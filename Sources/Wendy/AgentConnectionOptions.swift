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
    case external(ExternalDevice)

    init(endpoint: TargetOptions.Endpoint) {
        switch endpoint.remote {
        case .local:
            self = .external(
                ExternalDevice(
                    id: "local",
                    displayName: "Local (This Device)",
                    providerKey: "local"
                )
            )
        case .docker:
            self = .external(
                ExternalDevice(
                    id: "docker",
                    displayName: "Docker Desktop",
                    providerKey: "docker"
                )
            )
        case .grpc(let host, let port):
            self = .lan(host: host, port: port, defaultDevice: endpoint.defaultDevice)
        case .bluetooth(let uuid):
            let peripheral = Peripheral(
                id: BluetoothDeviceID(uuid),
                name: "WendyOS Device \(uuid)"
            )
            self = .bluetooth(peripheral: peripheral, address: uuid)
        case .external(let device):
            self = .external(device)
        }
    }

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

struct TargetOptions: ParsableArguments {
    struct Endpoint: ExpressibleByArgument, CustomStringConvertible, Sendable {
        enum Remote: Sendable, Equatable {
            case local
            case docker
            case grpc(host: String, port: Int)
            case bluetooth(uuid: String)
            case external(ExternalDevice)
        }

        var remote: Remote
        var defaultDevice: Bool

        init(remote: Remote, defaultDevice: Bool = false) {
            self.remote = remote
            self.defaultDevice = defaultDevice
        }

        init(host: String, port: Int, defaultDevice: Bool = false) {
            self.remote = .grpc(host: host, port: port)
            self.defaultDevice = defaultDevice
        }

        init?(argument: String) {
            if argument == "local" {
                self.remote = .local
                self.defaultDevice = false
                return
            }
            if argument == "docker" {
                self.remote = .docker
                self.defaultDevice = false
                return
            }
            if let uuid = UUID(uuidString: argument) {
                self.remote = .bluetooth(uuid: uuid.uuidString)
                self.defaultDevice = false
                return
            }
            // Create a dummy URL to use URLComponents parsing capabilities
            var urlString = argument
            let hasScheme = urlString.contains("://")

            // Only allow wendy:// scheme or no scheme
            if hasScheme {
                if !urlString.starts(with: "wendy://") {
                    return nil
                }
            } else {
                urlString = "wendy://" + urlString
            }

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

            self.remote = .grpc(host: cleanHost, port: components.port ?? 50051)
            self.defaultDevice = false
        }

        static var defaultValueDescription: String {
            "localhost:50051"
        }

        public var description: String {
            switch remote {
            case .local:
                return "Local (This Device)"
            case .docker:
                return "Docker Container"
            case .grpc(let host, let port):
                return "\(host):\(port)"
            case .bluetooth(let uuid):
                return uuid
            case .external(let device):
                return "\(device.displayName) [\(device.providerKey)]"
            }
        }
    }

    @Option(
        name: .shortAndLong,
        help:
            """
            The host (and optional port) of the WendyOS Agent to connect to (format: host or host:port).
            IPv6 addresses must be enclosed in square brackets, e.g. [2001:db8::1] or [2001:db8::1]:50051.
            """
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

        if !rows.isEmpty {
            cliOutput.table(
                headers: ["Interface", "Details"],
                rows: rows
            )
        }
    }
}

// MARK: - Device Selection with Bluetooth Support

extension TargetOptions {
    /// Read device selection, including Bluetooth devices when no LAN devices are available
    /// or when explicitly requested
    func read(
        title: String?,
        readDefault: Bool = true,
        preferBluetooth: Bool = false,
        includeLocalProviders: Bool = false,
        includeBluetooth: Bool = true
    ) async throws -> SelectedDevice {
        // If explicit device specified via CLI, use it as LAN
        if let device {
            return SelectedDevice(endpoint: device)
        }

        if let endpoint = ProcessInfo.processInfo.environment["WENDY_AGENT"],
            let endpoint = Endpoint(argument: endpoint)
        {
            return SelectedDevice(endpoint: endpoint)
        }

        if readDefault, !preferBluetooth, let defaultDevice = Self.defaultDevice() {
            return SelectedDevice(endpoint: defaultDevice)
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
            let initial = [
                DevicesCollection.GroupedDevice(
                    name: "No device detected yet",
                    interfaces: []
                )
            ]
            return try await cliOutput.selectFromStreamingTable(
                initial: initial,
                updates: stream.map { collection -> [DevicesCollection.GroupedDevice] in
                    var devices = collection.groupedDevices()

                    if !includeLocalProviders {
                        devices.removeAll { $0.isLocalhost || $0.isDocker }
                    }

                    return devices.sorted().filter { device in
                        let interfaces = device.interfaces.map(\.type)
                        if interfaces.contains(.lan) {
                            return true
                        }
                        if interfaces.contains(.bluetooth), includeBluetooth {
                            return true
                        }
                        if interfaces.contains(.external) {
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
            case .external(let externalDevice):
                return .external(externalDevice)
            default:
                ()
            }
        }

        throw CLIError.invalidEndpoint("No valid endpoint found")
    }
}
