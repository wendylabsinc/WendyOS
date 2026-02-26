import ArgumentParser
import Bluetooth
import CLIOutput
import Foundation
import Logging
import WendyShared

/// Represents the selected device connection type
enum SelectedDevice: Sendable {
    case lan(host: String, port: Int, defaultDevice: Bool)
    case bluetooth(peripheral: Peripheral, address: String)

    var isLAN: Bool {
        if case .lan = self { return true }
        return false
    }

    var isDefaultDevice: Bool {
        if case .lan(_, _, let defaultDevice) = self {
            return defaultDevice
        }
        return false
    }

    var isBluetooth: Bool {
        if case .bluetooth = self { return true }
        return false
    }
}

struct AgentConnectionOptions: ParsableArguments {
    struct Endpoint: ExpressibleByArgument {
        let host: String
        var port: Int
        var defaultDevice: Bool

        /// Returns true if the host is an IPv6 link-local address (fe80::).
        var isIPv6LinkLocal: Bool {
            host.lowercased().hasPrefix("fe80:")
        }

        /// The IPv6 scope ID (interface name) if present, e.g. "eth0" from "fe80::1%eth0".
        var scopeID: String? {
            guard let percentIndex = host.firstIndex(of: "%") else { return nil }
            let scope = String(host[host.index(after: percentIndex)...])
            return scope.isEmpty ? nil : scope
        }

        /// The host address without the scope ID suffix.
        var hostWithoutScope: String {
            if let percentIndex = host.firstIndex(of: "%") {
                return String(host[..<percentIndex])
            }
            return host
        }

        init(host: String, port: Int, defaultDevice: Bool = false) {
            self.host = host
            self.port = port
            self.defaultDevice = defaultDevice
        }

        init?(argument: String) {
            // Handle IPv6 link-local with scope ID before URL parsing,
            // since URLComponents mangles the %interface suffix.
            // Formats: [fe80::1%eth0]:port, fe80::1%eth0, [fe80::1%25eth0]:port
            var input = argument

            // Strip wendy:// scheme if present
            if input.hasPrefix("wendy://") {
                input = String(input.dropFirst("wendy://".count))
            } else if input.contains("://") {
                return nil  // Unknown scheme
            }

            // Check for bracketed IPv6 with scope ID: [fe80::1%eth0]:port or [fe80::1%25eth0]:port
            if input.hasPrefix("[") {
                guard let closeBracket = input.firstIndex(of: "]") else { return nil }
                var ipv6Part = String(input[input.index(after: input.startIndex)..<closeBracket])
                let afterBracket = String(input[input.index(after: closeBracket)...])

                // Decode %25 -> % (URL-encoded scope ID)
                ipv6Part = ipv6Part.replacingOccurrences(of: "%25", with: "%")

                let port: Int
                if afterBracket.hasPrefix(":"), let p = Int(afterBracket.dropFirst()) {
                    port = p
                } else {
                    port = 50051
                }

                self.host = ipv6Part
                self.port = port
                self.defaultDevice = false
                return
            }

            // Check for bare IPv6 link-local with scope ID: fe80::1%eth0
            if input.lowercased().hasPrefix("fe80:") && input.contains("%") {
                // Decode %25 -> % if URL-encoded
                input = input.replacingOccurrences(of: "%25", with: "%")
                self.host = input
                self.port = 50051
                self.defaultDevice = false
                return
            }

            // Fall back to URLComponents for everything else (hostnames, IPv4, global IPv6)
            let urlString = "wendy://" + input
            guard let components = URLComponents(string: urlString),
                let host = components.host, !host.isEmpty
            else {
                return nil
            }

            // Handle IPv6 addresses by removing the brackets if present
            var cleanHost = host
            if cleanHost.first == "[" && cleanHost.last == "]" {
                cleanHost = String(cleanHost.dropFirst().dropLast())
            }

            self.host = cleanHost
            self.port = components.port ?? 50051
            self.defaultDevice = false
        }

        static var defaultValueDescription: String {
            "localhost:50051"
        }

        var description: String {
            "\(host):\(port)"
        }
    }

    @Option(
        name: .shortAndLong,
        help:
            "The host and port of the Wendy Agent to connect to (format: host or host:port). IPv6 addresses must be enclosed in square brackets, e.g. [2001:db8::1] or [2001:db8::1]:8080. Defaults to the `WENDY_AGENT` environment variable."
    )
    var device: Endpoint?

    init() {}

    init(
        endpoint: Endpoint?
    ) {
        self.device = endpoint
    }

    static func defaultDevice() -> Endpoint? {
        let config = getConfig()
        if let defaultDevice = config.defaultDevice {
            return Endpoint(host: defaultDevice, port: 50051, defaultDevice: true)
        }
        return nil
    }

    var endpoint: Endpoint {
        get throws {
            if let device {
                return device
            }

            if let endpoint = ProcessInfo.processInfo.environment["WENDY_AGENT"],
                let endpoint = Endpoint(argument: endpoint)
            {
                return endpoint
            }

            return Endpoint(host: "edgeos-device.local", port: 50051)
        }
    }

    // MARK: - Device presentation helpers

    private func printDeviceDetails(_ device: DevicesCollection.GroupedDevice) {
        // Show the selected device name and version (if available)
        if let version = device.interfaces.compactMap(\.agentVersion).first {
            cliOutput.info("\(device.name) (version: \(version))")
        } else {
            cliOutput.info("\(device.name)")
        }

        let rows = device.interfaces.map { interface -> [String] in
            let parts = interface.description.split(
                separator: ":",
                maxSplits: 1,
                omittingEmptySubsequences: false
            )

            let interfaceLabel =
                parts.first.map(String.init)?.trimmingCharacters(
                    in: .whitespaces
                ) ?? ""
            let details =
                parts.count > 1
                ? parts[1].trimmingCharacters(in: .whitespaces)
                : ""

            return [interfaceLabel, details]
        }

        cliOutput.table(
            headers: ["Interface", "Details"],
            rows: rows
        )
    }
}

// MARK: - Device Selection with Bluetooth Support

extension AgentConnectionOptions {
    /// Read device selection, including Bluetooth devices when no LAN devices are available
    /// or when explicitly requested
    func read(
        title: String?,
        readDefault: Bool = true,
        preferBluetooth: Bool = false,
        includeBluetooth: Bool = true
    ) async throws -> SelectedDevice {
        // If explicit device specified via CLI, use it as LAN
        if let device {
            return .lan(host: device.host, port: device.port, defaultDevice: false)
        }

        if let endpoint = ProcessInfo.processInfo.environment["WENDY_AGENT"],
            let endpoint = Endpoint(argument: endpoint)
        {
            return .lan(host: endpoint.host, port: endpoint.port, defaultDevice: false)
        }

        if readDefault, !preferBluetooth, let defaultDevice = Self.defaultDevice() {
            return .lan(host: defaultDevice.host, port: defaultDevice.port, defaultDevice: true)
        }

        // In JSON mode, we cannot use interactive device selection
        if JSONMode.isEnabled {
            throw CLIError.invalidEndpoint(
                "No device specified. Use --device <host> or set the WENDY_AGENT environment variable."
            )
        }

        let (stream, continuation) = AsyncStream<DevicesCollection>.makeStream()
        let device = try await withThrowingTaskGroup(of: Void.self) {
            group -> DevicesCollection.GroupedDevice in
            group.addTask {
                await DiscoverCommand.runStreamingDiscovery(
                    deviceCache: DeviceCache(),
                    resolveBluetoothVersionInline: false,
                    skipVersionResolution: true,
                    continuation: continuation
                )
            }

            defer { group.cancelAll() }
            let emptyDevice = DevicesCollection.GroupedDevice(
                name: "No device detected yet",
                interfaces: []
            )
            return try await cliOutput.selectFromStreamingTable(
                initial: [emptyDevice],
                updates: stream.map { collection -> [DevicesCollection.GroupedDevice] in
                    if collection.isEmpty {
                        return [emptyDevice]
                    }
                    return collection.groupedDevices().filter { device in
                        let interfaces = device.interfaces.map(\.type)
                        if interfaces.contains(.lan) {
                            return true
                        }
                        if interfaces.contains(.bluetooth), includeBluetooth {
                            return true
                        }
                        return false
                    }
                },
                pageSize: 20,
                renderTable: { devices in
                    return (
                        headers: ["Name", "Interfaces"],
                        rows: devices.map { device in
                            return [
                                device.name,
                                device.interfaces
                                    .map { $0.type.rawValue }
                                    .joined(separator: ", "),
                            ]
                        }
                    )
                }
            )
        }

        printDeviceDetails(device)

        for interface in device.interfaces {
            switch interface {
            case .lan(let lanDevice):
                return .lan(
                    host: lanDevice.hostname,
                    port: lanDevice.port,
                    defaultDevice: false
                )
            case .bluetooth(let btDevice):
                let peripheral = Peripheral(
                    id: BluetoothDeviceID(btDevice.id),
                    name: btDevice.displayName
                )
                return .bluetooth(peripheral: peripheral, address: btDevice.address)
            default:
                continue
            }
        }

        throw CLIError.invalidEndpoint("No valid endpoint found")
    }
}
