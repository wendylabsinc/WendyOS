import DBUS
import Logging
import NIO
import Subprocess

#if canImport(FoundationEssentials)
    import FoundationEssentials
#else
    import Foundation
#endif

/// Protocol for decoding DBus messages into Swift types
public protocol DBusDecodable {
    static func decode(from message: DBusMessage) throws -> Self
}

// MARK: - DBus Decodable Implementations

extension String: DBusDecodable {
    public static func decode(from message: DBusMessage) throws -> String {
        guard let value = message.body.first else {
            throw NetworkConnectionError.noReply
        }

        switch value {
        case .string(let string):
            return string
        case .variant(let variant):
            if case .string(let string) = variant.value {
                return string
            }
            throw NetworkConnectionError.invalidType
        default:
            throw NetworkConnectionError.invalidType
        }
    }
}

extension UInt32: DBusDecodable {
    public static func decode(from message: DBusMessage) throws -> UInt32 {
        guard let bodyValue = message.body.first else {
            throw NetworkConnectionError.noReply
        }

        switch bodyValue {
        case .uint32(let value):
            return value
        case .variant(let variant):
            if case .uint32(let value) = variant.value {
                return value
            }
            throw NetworkConnectionError.invalidType
        default:
            throw NetworkConnectionError.invalidType
        }
    }
}

extension Int8: DBusDecodable {
    public static func decode(from message: DBusMessage) throws -> Int8 {
        guard let bodyValue = message.body.first else {
            throw NetworkConnectionError.noReply
        }

        switch bodyValue {
        case .byte(let byte):
            return Int8(byte)
        case .int16(let value):
            guard value >= Int16(Int8.min) && value <= Int16(Int8.max) else {
                throw NetworkConnectionError.invalidType
            }
            return Int8(value)
        case .int32(let value):
            guard value >= Int32(Int8.min) && value <= Int32(Int8.max) else {
                throw NetworkConnectionError.invalidType
            }
            return Int8(value)
        case .int64(let value):
            guard value >= Int64(Int8.min) && value <= Int64(Int8.max) else {
                throw NetworkConnectionError.invalidType
            }
            return Int8(value)
        case .variant(let variant):
            if case .byte(let byte) = variant.value {
                return Int8(byte)
            }
            if case .int16(let value) = variant.value,
                value >= Int16(Int8.min) && value <= Int16(Int8.max)
            {
                return Int8(value)
            }
            if case .int32(let value) = variant.value,
                value >= Int32(Int8.min) && value <= Int32(Int8.max)
            {
                return Int8(value)
            }
            if case .int64(let value) = variant.value,
                value >= Int64(Int8.min) && value <= Int64(Int8.max)
            {
                return Int8(value)
            }
            throw NetworkConnectionError.invalidType
        default:
            throw NetworkConnectionError.invalidType
        }
    }
}

extension Bool: DBusDecodable {
    public static func decode(from message: DBusMessage) throws -> Bool {
        guard let bodyValue = message.body.first else {
            throw NetworkConnectionError.noReply
        }

        switch bodyValue {
        case .boolean(let value):
            return value
        case .variant(let variant):
            if case .boolean(let value) = variant.value {
                return value
            }
            throw NetworkConnectionError.invalidType
        default:
            throw NetworkConnectionError.invalidType
        }
    }
}

/// NetworkManager provides an interface to interact with NetworkManager over DBus
public actor NetworkManager: NetworkConnectionManager {
    private let logger = Logger(label: "NetworkManager")

    // Connection configuration
    private let socketPath: String
    private let uid: String

    public init(
        uid: String,
        socketPath: String = "/var/run/dbus/system_bus_socket"
    ) {
        self.uid = uid
        self.socketPath = socketPath
    }

    /// Execute a DBus request and handle the response
    private func executeDBusRequest<T: DBusDecodable & Sendable>(
        _ request: DBusRequest
    ) async throws -> T {
        // Create socket address
        let address = try SocketAddress(unixDomainSocketPath: socketPath)

        return try await DBusClient.withConnection(
            to: address,
            auth: .external(userID: uid)
        ) { [self] connection in
            // Helper function to log signal details
            func logSignalDetails(_ signal: DBusMessage) {
                self.logger.debug(
                    "DETAILED SIGNAL: MessageType=\(signal.messageType), Serial=\(signal.serial)"
                )

                if !signal.body.isEmpty {
                    // Detailed body logging based on message body content type
                    self.logger.debug("Signal body contents:")
                    for (index, value) in signal.body.enumerated() {
                        self.logger.debug(
                            "Body element",
                            metadata: [
                                "index": "\(index)",
                                "value": "\(value)",
                            ]
                        )

                        // Try to extract more detailed information from body elements
                        switch value {
                        case .string(let message):
                            if message.contains("error") || message.contains("permission")
                                || message.contains("denied") || message.contains("failed")
                            {
                                self.logger.error("⚠️ IMPORTANT ERROR MESSAGE: \(message)")
                            }
                            self.logger.debug(
                                "String value",
                                metadata: [
                                    "value": "\(message)"
                                ]
                            )
                        case .uint32(let code):
                            self.logger.debug(
                                "UInt32 value",
                                metadata: [
                                    "value": "\(code)"
                                ]
                            )
                        case .variant(let variant):
                            self.logger.debug(
                                "Variant value",
                                metadata: [
                                    "value": "\(variant.value)"
                                ]
                            )
                        case .dictionary(let dict):
                            self.logger.debug(
                                "Dictionary entries",
                                metadata: [
                                    "count": "\(dict.count)"
                                ]
                            )
                            for (key, dictValue) in dict {
                                self.logger.debug(
                                    "Dictionary entry",
                                    metadata: [
                                        "key": "\(key)",
                                        "value": "\(dictValue)",
                                    ]
                                )
                            }
                        case .array(let array):
                            self.logger.debug(
                                "Array elements",
                                metadata: [
                                    "count": "\(array.count)"
                                ]
                            )
                            for (i, element) in array.enumerated() {
                                self.logger.debug(
                                    "Array element",
                                    metadata: [
                                        "index": "\(i)",
                                        "value": "\(element)",
                                    ]
                                )
                            }
                        default:
                            self.logger.debug(
                                "Other value type",
                                metadata: [
                                    "value": "\(value)"
                                ]
                            )
                        }
                    }
                } else {
                    self.logger.debug("Signal body is empty")
                }
            }

            do {
                guard let reply = try await connection.send(request) else {
                    throw NetworkConnectionError.noReply
                }
                guard case .methodReturn = reply.messageType else {
                    // Extract error details from the body if possible
                    var errorDetails = "No details available"
                    if let bodyValue = reply.body.first, case .string(let message) = bodyValue {
                        errorDetails = message
                    }

                    self.logger.error("DBus connection error: \(errorDetails)")
                    self.logger.debug("Full error reply: \(reply)")
                    throw NetworkConnectionError.connectionFailed
                }
                return try T.decode(from: reply)
            } catch {
                self.logger.error("Failed to write request to DBus: \(error)")
                throw NetworkConnectionError.connectionFailed
            }
        }
    }

    /// Helper to decode an array of strings from DBus message
    private func decodeStringArray(from message: DBusMessage) throws -> [String] {
        guard let bodyValue = message.body.first else {
            throw NetworkConnectionError.noReply
        }

        guard let array = bodyValue.array else {
            throw NetworkConnectionError.invalidType
        }

        return array.compactMap(\.objectPath)
    }

    /// Helper to decode an array of bytes from DBus message
    private func decodeByteArray(from message: DBusMessage) throws -> [UInt8] {
        guard let bodyValue = message.body.first else {
            throw NetworkConnectionError.noReply
        }
        guard let array = bodyValue.array else {
            throw NetworkConnectionError.invalidType
        }

        return array.compactMap(\.byte)
    }

    /// Get all network devices
    public func getNetworkDevices() async throws -> [String] {
        let message: DBusMessage = try await executeDBusRequest(
            .createMethodCall(
                destination: "org.freedesktop.NetworkManager",
                path: "/org/freedesktop/NetworkManager",
                interface: "org.freedesktop.NetworkManager",
                method: "GetDevices",
            )
        )

        return try decodeStringArray(from: message)
    }

    /// Get device type for a specific device path
    public func getDeviceType(devicePath: String) async throws -> UInt32 {
        return try await executeDBusRequest(
            .createMethodCall(
                destination: "org.freedesktop.NetworkManager",
                path: devicePath,
                interface: "org.freedesktop.DBus.Properties",
                method: "Get",
                body: [
                    .string("org.freedesktop.NetworkManager.Device"),
                    .string("DeviceType"),
                ]
            )
        )
    }

    /// Get all access points for a WiFi device
    public func getAccessPoints(devicePath: String) async throws -> [String] {
        let message: DBusMessage = try await executeDBusRequest(
            .createMethodCall(
                destination: "org.freedesktop.NetworkManager",
                path: devicePath,
                interface: "org.freedesktop.NetworkManager.Device.Wireless",
                method: "GetAllAccessPoints"
            )
        )

        return try decodeStringArray(from: message)
    }

    /// Get SSID for an access point
    public func getSSID(apPath: String) async throws -> String {
        let message: DBusMessage = try await executeDBusRequest(
            .createMethodCall(
                destination: "org.freedesktop.NetworkManager",
                path: apPath,
                interface: "org.freedesktop.DBus.Properties",
                method: "Get",
                body: [
                    .string("org.freedesktop.NetworkManager.AccessPoint"),
                    .string("Ssid"),
                ]
            )
        )

        let ssidBytes = try decodeByteArray(from: message)

        guard let ssid = String(bytes: ssidBytes, encoding: .utf8) else {
            throw NetworkConnectionError.invalidSSID
        }

        return ssid
    }

    /// Get the signal strength for an access point
    public func getSignalStrength(apPath: String) async throws -> Int8 {
        return try await executeDBusRequest(
            .createMethodCall(
                destination: "org.freedesktop.NetworkManager",
                path: apPath,
                interface: "org.freedesktop.DBus.Properties",
                method: "Get",
                body: [
                    .string("org.freedesktop.NetworkManager.AccessPoint"),
                    .string("Strength"),
                ]
            )
        )
    }

    /// Find the WiFi device path among the available devices
    private func findWiFiDevice() async throws -> String {
        // Get all devices
        let devicePaths = try await getNetworkDevices()
        self.logger.debug("Found \(devicePaths.count) network devices")

        if devicePaths.isEmpty {
            throw NetworkConnectionError.noWiFiDevice
        }

        // Try to find a WiFi device (type 2)
        for devicePath in devicePaths {
            do {
                let deviceType = try await getDeviceType(devicePath: devicePath)
                if deviceType == 2 {  // NM_DEVICE_TYPE_WIFI = 2
                    self.logger.debug("Found WiFi device at path: \(devicePath)")
                    return devicePath
                }
            } catch {
                // Log the error but continue trying other devices
                self.logger.warning("Failed to get device type for \(devicePath): \(error)")
                continue
            }
        }

        // If we get here, no WiFi device was found
        throw NetworkConnectionError.noWiFiDevice
    }

    /// List all available WiFi networks
    public func listWiFiNetworks() async throws -> [WiFiNetwork] {
        // Find the WiFi device
        let wifiDevicePath = try await findWiFiDevice()

        // Get all access points
        self.logger.debug("Getting access points for WiFi device: \(wifiDevicePath)")
        let accessPointPaths = try await getAccessPoints(devicePath: wifiDevicePath)
        self.logger.debug("Found \(accessPointPaths.count) access points")

        if accessPointPaths.isEmpty {
            return []
        }

        // Get SSIDs and signal strength for each access point
        var networks: [WiFiNetwork] = []
        for apPath in accessPointPaths {
            do {
                let ssid = try await getSSID(apPath: apPath)
                var signalStrength: Int8? = nil

                do {
                    // Try to get signal strength, but don't fail if it's not available
                    signalStrength = try await getSignalStrength(apPath: apPath)
                } catch {
                    self.logger.warning(
                        "Failed to get signal strength for AP: \(apPath)",
                        metadata: ["error": "\(error)"]
                    )
                }

                networks.append(
                    WiFiNetwork(
                        ssid: ssid,
                        path: apPath,
                        signalStrength: signalStrength,
                        isSecured: true
                    )
                )
                self.logger.debug(
                    "Added network: \(ssid) (signal: \(signalStrength?.description ?? "unknown"))"
                )
            } catch {
                self.logger.warning(
                    "Failed to get SSID for AP: \(apPath)",
                    metadata: ["error": "\(error)"]
                )
                continue
            }
        }

        return networks
    }

    /// Connect to a WiFi network
    public func connectToNetwork(ssid: String, password: String) async throws {
        do {
            // Get available networks
            let networks = try await listWiFiNetworks()

            self.logger.debug(
                "Searching for network with SSID: '\(ssid)' among \(networks.count) networks"
            )

            guard networks.first(where: { $0.ssid == ssid }) != nil else {
                self.logger.error("Network not found: '\(ssid)'")
                self.logger.debug("Available networks: \(networks.map { $0.ssid })")
                throw NetworkConnectionError.networkNotFound
            }

            // Reminder for later
            self.logger.info("Using nmcli to connect to network '\(ssid)' (workaround)")

            self.logger.debug("Executing nmcli command to connect to '\(ssid)'")

            do {
                let result = try await Subprocess.run(
                    .path("/usr/bin/nmcli"),
                    arguments: [
                        "device", "wifi", "connect", ssid, "password", password,
                    ],
                    output: .string(limit: 1024 * 1024),
                    error: .string(limit: 1024 * 1024)
                )

                let output = result.standardOutput ?? ""
                let errorOutput = result.standardError ?? ""

                self.logger.debug("nmcli output: \(output)")
                if !errorOutput.isEmpty {
                    self.logger.debug("nmcli stderr: \(errorOutput)")
                }

                // Check exit status
                if !result.terminationStatus.isSuccess {
                    // Parse the error to provide appropriate error types
                    let combinedOutput = output + " " + errorOutput

                    if combinedOutput.contains("password")
                        || combinedOutput.contains("Secrets were required")
                    {
                        self.logger.error(
                            "WiFi authentication error: Invalid password or authentication failure"
                        )
                        throw NetworkConnectionError.authenticationFailed
                    } else if combinedOutput.contains("not found")
                        || combinedOutput.contains("No network with SSID")
                    {
                        self.logger.error("Network '\(ssid)' not found")
                        throw NetworkConnectionError.networkNotFound
                    } else if combinedOutput.contains("permission")
                        || combinedOutput.contains("authorized")
                    {
                        self.logger.error("Permission denied")
                        throw NetworkConnectionError.authenticationFailed
                    } else if combinedOutput.contains("timeout") {
                        self.logger.error("Connection attempt timed out")
                        throw NetworkConnectionError.timeout
                    } else {
                        self.logger.error(
                            "nmcli failed with termination status: \(result.terminationStatus)"
                        )
                        self.logger.error("Output: \(output)")
                        if !errorOutput.isEmpty {
                            self.logger.error("Error output: \(errorOutput)")
                        }
                        throw NetworkConnectionError.connectionFailed
                    }
                }

                self.logger.debug("Successfully connected to WiFi network: \(ssid)")

            } catch let error as NetworkConnectionError {
                // Just re-throw NetworkConnectionErrors
                throw error
            } catch {
                self.logger.error("Failed to execute nmcli: \(error)")
                throw NetworkConnectionError.connectionFailed
            }

        } catch let error as NetworkConnectionError {
            // Just re-throw NetworkConnectionErrors
            throw error
        } catch {
            // Wrap other errors
            self.logger.error("Unexpected error connecting to WiFi: \(error)")
            throw NetworkConnectionError.connectionFailed
        }
    }

    /// Setup WiFi (alias for connectToNetwork for backward compatibility)
    public func setupWiFi(ssid: String, password: String) async throws {
        try await connectToNetwork(ssid: ssid, password: password)
    }

    /// Get the current active WiFi connection information
    public func getCurrentConnection() async throws -> WiFiConnection? {
        // Find the WiFi device
        let wifiDevicePath = try await findWiFiDevice()

        do {
            // Get the active connection path with enhanced debugging
            let message: DBusMessage = try await executeDBusRequest(
                .createMethodCall(
                    destination: "org.freedesktop.NetworkManager",
                    path: wifiDevicePath,
                    interface: "org.freedesktop.DBus.Properties",
                    method: "Get",
                    body: [
                        .string("org.freedesktop.NetworkManager.Device"),
                        .string("ActiveConnection"),
                    ]
                )
            )

            // Debug the response body
            self.logger.debug("ActiveConnection response: \(message)")
            if let firstBodyValue = message.body.first {
                self.logger.debug("ActiveConnection value type: \(type(of: firstBodyValue))")
            }

            // More robust extraction of connection path
            var activeConnectionPath = "/"
            if let bodyValue = message.body.first {
                switch bodyValue {
                case .objectPath(let path):
                    activeConnectionPath = path
                case .string(let path):
                    activeConnectionPath = path
                case .variant(let variant):
                    switch variant.value {
                    case .objectPath(let path):
                        activeConnectionPath = path
                    case .string(let path):
                        activeConnectionPath = path
                    default:
                        self.logger.error(
                            "Unexpected variant type for ActiveConnection: \(variant.value)"
                        )
                    }
                default:
                    self.logger.error("Unexpected type for ActiveConnection: \(bodyValue)")
                    // Try to extract a string from the body
                    let bodyString = String(describing: bodyValue)
                    if bodyString.contains("/org/freedesktop/NetworkManager/ActiveConnection") {
                        // Try to extract path using regex-like approach
                        if let range = bodyString.range(
                            of: "/org/freedesktop/NetworkManager/ActiveConnection[^ \"']+"
                        ) {
                            activeConnectionPath = String(bodyString[range])
                            self.logger.debug(
                                "Extracted path using fallback method: \(activeConnectionPath)"
                            )
                        }
                    }
                }
            }

            // If there's no active connection, return nil
            if activeConnectionPath == "/" {
                self.logger.debug("No active connection found")
                return nil
            }

            // Verify the path looks valid
            if !activeConnectionPath.hasPrefix("/org/freedesktop/NetworkManager") {
                self.logger.error("Invalid ActiveConnection path: \(activeConnectionPath)")
                throw NetworkConnectionError.invalidType
            }

            self.logger.debug("Found active connection at path: \(activeConnectionPath)")

            // Get the connection ID (SSID)
            let ssidMessage: DBusMessage = try await executeDBusRequest(
                .createMethodCall(
                    destination: "org.freedesktop.NetworkManager",
                    path: activeConnectionPath,
                    interface: "org.freedesktop.DBus.Properties",
                    method: "Get",
                    body: [
                        .string("org.freedesktop.NetworkManager.Connection.Active"),
                        .string("Id"),
                    ]
                )
            )

            // Debug the response
            self.logger.debug("SSID response: \(ssidMessage)")

            guard let bodyValue = ssidMessage.body.first else {
                throw NetworkConnectionError.invalidType
            }

            // More robust extraction of SSID
            let ssid: String

            if let string = bodyValue.string {
                ssid = string
            } else if let array = bodyValue.array {
                let bytes = array.compactMap(\.byte)
                if let string = String(bytes: bytes, encoding: .utf8) {
                    ssid = string
                } else {
                    throw NetworkConnectionError.invalidSSID
                }
            } else {
                throw NetworkConnectionError.invalidSSID
            }

            return WiFiConnection(
                ssid: ssid,
                connectionPath: activeConnectionPath,
                ipAddress: nil,
                state: .connected
            )
        } catch {
            // Handle specific errors for better debugging
            if let nmError = error as? NetworkConnectionError {
                if nmError == .invalidType {
                    self.logger.error(
                        "Invalid DBus response type when getting WiFi status. This might indicate NetworkManager version mismatch or interface changes."
                    )
                }
                throw nmError
            }

            // Log and rethrow other errors
            self.logger.error("Error getting current WiFi connection: \(error)")
            throw error
        }
    }

    /// Disconnect from the current WiFi network
    public func disconnectFromNetwork() async throws -> Bool {
        // Find the WiFi device
        let wifiDevicePath = try await findWiFiDevice()

        // Check if there's an active connection
        guard (try await getCurrentConnection()) != nil else {
            throw NetworkConnectionError.noActiveConnection
        }

        // Disconnect by bringing the device down
        return try await executeDBusRequest(
            .createMethodCall(
                destination: "org.freedesktop.NetworkManager",
                path: wifiDevicePath,
                interface: "org.freedesktop.NetworkManager.Device",
                method: "Disconnect",
            )
        )
    }
}

// MARK: - DBusMessage Conformance

// Add conformance for DBusMessage
extension DBusMessage: DBusDecodable {
    public static func decode(from message: DBusMessage) throws -> DBusMessage {
        return message
    }
}
