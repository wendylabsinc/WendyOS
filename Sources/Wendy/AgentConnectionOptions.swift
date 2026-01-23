import ArgumentParser
import Bluetooth
import Foundation
import Logging
import Noora
import WendyShared

/// Represents the selected device connection type
enum SelectedDevice: Sendable {
    case lan(host: String, port: Int, defaultDevice: Bool)
    case bluetooth(peripheral: Peripheral, address: String)

    var isLAN: Bool {
        if case .lan = self { return true }
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

        init(host: String, port: Int, defaultDevice: Bool = false) {
            self.host = host
            self.port = port
            self.defaultDevice = defaultDevice
        }

        init?(argument: String) {
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

    @Option(
        name: .shortAndLong,
        help:
            """
            Alias for the `--device` option. (Deprecated)
            If both `--device` and `--device` are provided, the `--device` option takes precedence.
            """
    )
    var agent: Endpoint?

    init() {}

    init(
        endpoint: Endpoint?
    ) {
        self.agent = nil
        self.device = endpoint
    }

    static func defaultDevice() -> Endpoint? {
        let config = getConfig()
        if let defaultDevice = config.defaultDevice {
            return Endpoint(host: defaultDevice, port: 50051, defaultDevice: true)
        }
        return nil
    }

    func read(
        title: TerminalText?,
        readDefault: Bool = true
    ) async throws -> Endpoint {
        if let device {
            return device
        }

        if let agent {
            return agent
        }

        if let endpoint = ProcessInfo.processInfo.environment["WENDY_AGENT"],
            let endpoint = Endpoint(argument: endpoint)
        {
            return endpoint
        }

        if readDefault, let defaultDevice = Self.defaultDevice() {
            return defaultDevice
        }

        // In JSON mode, we cannot prompt for device selection
        if JSONMode.isEnabled {
            jsonModeRequiresArgument(
                argument: "device",
                description:
                    "Provide --device <hostname:port> or set WENDY_AGENT environment variable"
            )
        }

        let discovery = PlatformDeviceDiscovery(
            logger: Logger(label: "sh.wendy.cli.find-agent")
        )
        let lanDevices = try await Noora().progressStep(
            message: "Searching for WendyOS devices",
            successMessage: nil,
            errorMessage: nil,
            showSpinner: true
        ) { _ in
            while true {
                try Task.checkCancellation()
                let devices = try await discovery.findAllDevices()
                    .groupedDevices()
                    .filter { $0.interfaces.contains(where: { $0.type == .lan }) }

                if !devices.isEmpty {
                    return devices
                }

                try await Task.sleep(for: .seconds(1))
            }
        }

        let device = Noora().singleChoicePrompt(
            title: title,
            question: "Select a device",
            options: lanDevices
        )

        printDeviceDetails(device)

        for interface in device.interfaces {
            if case .lan(let lanDevice) = interface {
                return Endpoint(
                    host: lanDevice.hostname,
                    port: lanDevice.port
                )
            }
        }

        throw CLIError.invalidEndpoint("No valid endpoint found")
    }

    var endpoint: Endpoint {
        get throws {
            if let device {
                return device
            }

            if let agent {
                return agent
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
            Noora().info(.alert("\(device.name) (version: \(version))"))
        } else {
            Noora().info(.alert("\(device.name)"))
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

        Noora().table(
            headers: ["Interface", "Details"],
            rows: rows
        )
    }
}

// MARK: - Device Selection with Bluetooth Support

extension AgentConnectionOptions {
    /// Read device selection, including Bluetooth devices when no LAN devices are available
    /// or when explicitly requested
    func readWithBluetooth(
        title: TerminalText?,
        readDefault: Bool = true,
        preferBluetooth: Bool = false,
        includeBluetooth: Bool = true
    ) async throws -> SelectedDevice {
        // If explicit device specified via CLI, use it as LAN
        if let device {
            return .lan(host: device.host, port: device.port, defaultDevice: false)
        }

        if let agent {
            return .lan(host: agent.host, port: agent.port, defaultDevice: false)
        }

        if let endpoint = ProcessInfo.processInfo.environment["WENDY_AGENT"],
            let endpoint = Endpoint(argument: endpoint)
        {
            return .lan(host: endpoint.host, port: endpoint.port, defaultDevice: false)
        }

        if readDefault, !preferBluetooth, let defaultDevice = Self.defaultDevice() {
            return .lan(host: defaultDevice.host, port: defaultDevice.port, defaultDevice: true)
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
            return try await NooraRenderer().selectFromStreamingTable(
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
