import DBUS
import Logging
import NIO

#if canImport(FoundationEssentials)
    import FoundationEssentials
#else
    import Foundation
#endif

/// Factory for creating appropriate network manager instances based on availability
public actor NetworkConnectionManagerFactory {
    private let logger = Logger(label: "NetworkConnectionManagerFactory")
    private let uid: String
    private let socketPath: String

    /// Cached detection result
    private var cachedManager: NetworkManagerType?

    public enum NetworkManagerType: String, Sendable {
        case connMan = "ConnMan"
        case networkManager = "NetworkManager"
        case none = "None"
    }

    /// User preference for network manager (if set)
    public enum Preference: Sendable {
        case auto
        case preferConnMan
        case preferNetworkManager
        case forceConnMan
        case forceNetworkManager
    }

    public init(
        uid: String,
        socketPath: String = "/var/run/dbus/system_bus_socket"
    ) {
        self.uid = uid
        self.socketPath = socketPath
    }

    /// Check if a D-Bus service is available
    private func isServiceAvailable(_ serviceName: String) async -> Bool {
        do {
            let address = try SocketAddress(unixDomainSocketPath: socketPath)

            return try await DBusClient.withConnection(
                to: address,
                auth: .external(userID: uid)
            ) { connection in
                let request = DBusRequest.createMethodCall(
                    destination: "org.freedesktop.DBus",
                    path: "/org/freedesktop/DBus",
                    interface: "org.freedesktop.DBus",
                    method: "NameHasOwner",
                    body: [.string(serviceName)]
                )

                guard let reply = try await connection.send(request),
                    case .methodReturn = reply.messageType,
                    let bodyValue = reply.body.first
                else {
                    return false
                }

                switch bodyValue {
                case .boolean(let hasOwner):
                    return hasOwner
                case .variant(let variant):
                    if case .boolean(let hasOwner) = variant.value {
                        return hasOwner
                    }
                    return false
                default:
                    return false
                }
            }
        } catch {
            logger.debug(
                "Failed to check service availability",
                metadata: [
                    "service": "\(serviceName)",
                    "error": "\(error)",
                ]
            )
            return false
        }
    }

    /// Detect which network manager is available
    private func detectNetworkManager() async -> NetworkManagerType {
        if let cached = cachedManager {
            logger.debug(
                "Using cached network manager detection",
                metadata: [
                    "manager": "\(cached.rawValue)"
                ]
            )
            return cached
        }

        logger.debug("Detecting available network manager")

        let connManAvailable = await isServiceAvailable("net.connman")
        let networkManagerAvailable = await isServiceAvailable("org.freedesktop.NetworkManager")

        let detected: NetworkManagerType
        if connManAvailable {
            detected = .connMan
            logger.debug("ConnMan detected and will be used")
        } else if networkManagerAvailable {
            detected = .networkManager
            logger.debug("NetworkManager detected and will be used")
        } else {
            detected = .none
            logger.warning("No network manager detected")
        }

        cachedManager = detected
        return detected
    }

    /// Create a network connection manager instance based on preference and availability
    public func createNetworkManager(
        preference: Preference = .auto
    ) async throws -> NetworkConnectionManager {
        // Handle force preferences without auto-detection
        switch preference {
        case .forceConnMan:
            // Check if ConnMan is available first
            if await isServiceAvailable("net.connman") {
                logger.debug("Creating ConnMan instance (forced)")
                return ConnMan(uid: uid, socketPath: socketPath)
            }
            logger.error("ConnMan is not available, cannot use forced setting")
            throw NetworkConnectionError.managerNotAvailable

        case .forceNetworkManager:
            // Check if NetworkManager is available first
            if await isServiceAvailable("org.freedesktop.NetworkManager") {
                logger.debug("Creating NetworkManager instance (forced)")
                return NetworkManager(uid: uid, socketPath: socketPath)
            }
            logger.error("NetworkManager is not available, cannot use forced setting")
            throw NetworkConnectionError.managerNotAvailable

        case .preferConnMan:
            logger.debug("ConnMan preferred by configuration")
            // Check if ConnMan is available first
            if await isServiceAvailable("net.connman") {
                logger.debug("Creating ConnMan instance (preferred and available)")
                return ConnMan(uid: uid, socketPath: socketPath)
            }
            // Fall back to NetworkManager if available
            if await isServiceAvailable("org.freedesktop.NetworkManager") {
                logger.debug("ConnMan not available, falling back to NetworkManager")
                return NetworkManager(uid: uid, socketPath: socketPath)
            }
            logger.error("No network manager available")
            throw NetworkConnectionError.managerNotAvailable

        case .preferNetworkManager:
            logger.debug("NetworkManager preferred by configuration")
            // Check if NetworkManager is available first
            if await isServiceAvailable("org.freedesktop.NetworkManager") {
                logger.debug("Creating NetworkManager instance (preferred and available)")
                return NetworkManager(uid: uid, socketPath: socketPath)
            }
            // Fall back to ConnMan if available
            if await isServiceAvailable("net.connman") {
                logger.debug("NetworkManager not available, falling back to ConnMan")
                return ConnMan(uid: uid, socketPath: socketPath)
            }
            logger.error("No network manager available")
            throw NetworkConnectionError.managerNotAvailable

        case .auto:
            // For auto, use the full detection logic with caching
            let detected = await detectNetworkManager()

            switch detected {
            case .connMan:
                logger.debug("Creating ConnMan instance (auto-detected)")
                return ConnMan(uid: uid, socketPath: socketPath)

            case .networkManager:
                logger.debug("Creating NetworkManager instance (auto-detected)")
                return NetworkManager(uid: uid, socketPath: socketPath)

            case .none:
                logger.error("No network manager available")
                throw NetworkConnectionError.managerNotAvailable
            }
        }
    }

    /// Clear the cached detection result (useful for testing or after system changes)
    public func clearCache() {
        cachedManager = nil
        logger.debug("Cleared network manager detection cache")
    }

    /// Get the currently detected network manager type, returns cached manager when available
    public func getDetectedType() async -> NetworkManagerType {
        return await detectNetworkManager()
    }
}
