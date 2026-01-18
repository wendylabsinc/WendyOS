import AsyncDNSResolver
import Foundation
import Logging

/// Helper for mDNS device discovery testing
public struct DeviceDiscoveryHelper: Sendable {
    private let logger: Logger

    public init(logger: Logger = Logger(label: "E2ETestHarness.DeviceDiscoveryHelper")) {
        self.logger = logger
    }

    /// Discovered WendyOS device information
    public struct DiscoveredDevice: Sendable, Equatable {
        public let id: String
        public let hostname: String
        public let port: Int
        public let txtRecords: [String: String]

        /// UUID from TXT records
        public var uuid: String? { txtRecords["uuid"] }

        /// Device name from TXT records
        public var name: String? { txtRecords["name"] }

        /// Agent version from TXT records
        public var version: String? { txtRecords["version"] }

        /// Platform from TXT records
        public var platform: String? { txtRecords["platform"] }
    }

    /// Discover WendyOS devices via mDNS
    public func discoverDevices() async throws -> [DiscoveredDevice] {
        var devices: [DiscoveredDevice] = []

        let resolver = try AsyncDNSResolver()

        // Query for _wendyos._udp service
        let ptrWendy = try await resolver.queryPTR(name: "_wendyos._udp.local")
        let ptrEdge = try await resolver.queryPTR(name: "_edgeos._udp.local")

        for name in (ptrWendy.names + ptrEdge.names) {
            do {
                guard let srv = try await resolver.querySRV(name: name).first else {
                    continue
                }

                // Parse TXT records
                var txtRecords: [String: String] = [:]
                if let txtResponse = try? await resolver.queryTXT(name: name).first {
                    // Parse key=value pairs from TXT record
                    let pairs = txtResponse.txt.components(separatedBy: ",")
                    for pair in pairs {
                        let parts = pair.split(separator: "=", maxSplits: 1)
                        if parts.count == 2 {
                            txtRecords[String(parts[0])] = String(parts[1])
                        }
                    }
                }

                let device = DiscoveredDevice(
                    id: txtRecords["uuid"] ?? name,
                    hostname: srv.host,
                    port: Int(srv.port),
                    txtRecords: txtRecords
                )

                // Prevent duplicates
                if !devices.contains(where: { $0.id == device.id || $0.hostname == device.hostname }) {
                    devices.append(device)
                    logger.debug("Discovered device", metadata: [
                        "hostname": "\(device.hostname)",
                        "port": "\(device.port)",
                        "uuid": "\(device.uuid ?? "unknown")"
                    ])
                }
            } catch {
                logger.warning("Failed to resolve device", metadata: [
                    "name": "\(name)",
                    "error": "\(error)"
                ])
            }
        }

        return devices
    }

    /// Wait for a specific device to appear via mDNS discovery
    public func waitForDevice(
        withHostname hostname: String,
        timeout: TimeInterval = 30
    ) async throws -> DiscoveredDevice {
        let startTime = Date()
        let timeoutDate = startTime.addingTimeInterval(timeout)

        logger.info("Waiting for device", metadata: [
            "hostname": "\(hostname)",
            "timeout": "\(timeout)s"
        ])

        while Date() < timeoutDate {
            let devices = try await discoverDevices()
            if let device = devices.first(where: { $0.hostname.contains(hostname) }) {
                logger.info("Device found", metadata: ["hostname": "\(device.hostname)"])
                return device
            }

            try await Task.sleep(for: .seconds(2))
        }

        throw DiscoveryError.deviceNotFound(hostname: hostname, timeout: timeout)
    }

    /// Wait for a device with a specific UUID to appear
    public func waitForDevice(
        withUUID uuid: String,
        timeout: TimeInterval = 30
    ) async throws -> DiscoveredDevice {
        let startTime = Date()
        let timeoutDate = startTime.addingTimeInterval(timeout)

        logger.info("Waiting for device", metadata: [
            "uuid": "\(uuid)",
            "timeout": "\(timeout)s"
        ])

        while Date() < timeoutDate {
            let devices = try await discoverDevices()
            if let device = devices.first(where: { $0.uuid == uuid }) {
                logger.info("Device found", metadata: ["uuid": "\(uuid)"])
                return device
            }

            try await Task.sleep(for: .seconds(2))
        }

        throw DiscoveryError.deviceNotFoundByUUID(uuid: uuid, timeout: timeout)
    }
}

/// Device discovery errors
public enum DiscoveryError: Error, CustomStringConvertible {
    case deviceNotFound(hostname: String, timeout: TimeInterval)
    case deviceNotFoundByUUID(uuid: String, timeout: TimeInterval)

    public var description: String {
        switch self {
        case .deviceNotFound(let hostname, let timeout):
            return "Device with hostname '\(hostname)' not found within \(timeout) seconds"
        case .deviceNotFoundByUUID(let uuid, let timeout):
            return "Device with UUID '\(uuid)' not found within \(timeout) seconds"
        }
    }
}
