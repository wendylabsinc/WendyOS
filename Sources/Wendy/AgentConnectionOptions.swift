import ArgumentParser
import Foundation
import Logging
import Noora
import WendyShared

/// The type of connection to use for agent communication
enum AgentConnectionType: Sendable {
    case grpc(AgentConnectionOptions.Endpoint)
    case bluetooth(deviceIdentifier: String)
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

    @Option(
        name: .shortAndLong,
        help: "Connect to a device over Bluetooth by name or address. Use 'wendy discover --type bluetooth' to find devices."
    )
    var bluetooth: String?

    public init() {}

    public init(
        endpoint: Endpoint?
    ) {
        self.agent = nil
        self.device = endpoint
        self.bluetooth = nil
    }

    public init(bluetoothDevice: String) {
        self.agent = nil
        self.device = nil
        self.bluetooth = bluetoothDevice
    }

    /// Returns the connection type based on provided options
    var connectionType: AgentConnectionType? {
        if let bluetooth {
            return .bluetooth(deviceIdentifier: bluetooth)
        }
        if let device {
            return .grpc(device)
        }
        if let agent {
            return .grpc(agent)
        }
        return nil
    }

    static func defaultDevice() -> Endpoint? {
        let config = getConfig()
        if let defaultDevice = config.defaultDevice {
            return Endpoint(host: defaultDevice, port: 50051, defaultDevice: true)
        }
        return nil
    }

    /// Reads the connection type from options, environment, or interactive prompt
    func readConnectionType(
        title: TerminalText?,
        readDefault: Bool = true
    ) async throws -> AgentConnectionType {
        // Check for Bluetooth first
        if let bluetooth {
            return .bluetooth(deviceIdentifier: bluetooth)
        }

        // Fall back to gRPC endpoint
        let endpoint = try await readGRPCEndpoint(title: title, readDefault: readDefault)
        return .grpc(endpoint)
    }

    func read(
        title: TerminalText?,
        readDefault: Bool = true
    ) async throws -> Endpoint {
        // For backwards compatibility, return endpoint (ignores bluetooth option)
        return try await readGRPCEndpoint(title: title, readDefault: readDefault)
    }

    private func readGRPCEndpoint(
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
                description: "Provide --device <hostname:port>, --bluetooth <device>, or set WENDY_AGENT environment variable"
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
                    .filter { device in
                        device.interfaces.contains(where: { interface in
                            switch interface {
                            case .lan:
                                return true
                            default:
                                // This function is for gRPC endpoints only, so we only look for LAN devices
                                return false
                            }
                        })
                    }

                if !devices.isEmpty {
                    return devices
                }

                try await Task.sleep(for: .seconds(1))
            }
        }

        let device: DevicesCollection.GroupedDevice = Noora().singleChoicePrompt(
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

        throw InvalidEndpoint()
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

struct InvalidEndpoint: Error {}
struct NoDevicesFound: Error, CustomStringConvertible, CustomDebugStringConvertible {
    var description: String {
        "No Wendy devices found"
    }

    var debugDescription: String {
        "No Wendy devices found"
    }
}
