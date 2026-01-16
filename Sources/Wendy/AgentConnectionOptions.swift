import ArgumentParser
import Foundation
import Logging
import Noora
import WendyShared

#if canImport(Bluetooth)
import Bluetooth
#endif

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
        // Check for explicit Bluetooth option first
        if let bluetooth {
            return .bluetooth(deviceIdentifier: bluetooth)
        }

        // Check for explicit device/agent option
        if let device {
            return .grpc(device)
        }
        if let agent {
            return .grpc(agent)
        }

        // Check environment variable
        if let endpoint = ProcessInfo.processInfo.environment["WENDY_AGENT"],
            let endpoint = Endpoint(argument: endpoint)
        {
            return .grpc(endpoint)
        }

        // Check for default device
        if readDefault, let defaultDevice = Self.defaultDevice() {
            return .grpc(defaultDevice)
        }

        // In JSON mode, we cannot prompt for device selection
        if JSONMode.isEnabled {
            jsonModeRequiresArgument(
                argument: "device",
                description: "Provide --device <hostname:port>, --bluetooth <device>, or set WENDY_AGENT environment variable"
            )
        }

        // Discover both LAN and Bluetooth devices
        return try await discoverAndSelectDevice(title: title)
    }

    /// Discovers LAN and Bluetooth devices and lets user select one
    private func discoverAndSelectDevice(title: TerminalText?) async throws -> AgentConnectionType {
        let discovery = PlatformDeviceDiscovery(
            logger: Logger(label: "sh.wendy.cli.find-agent")
        )

        let devices = try await Noora().progressStep(
            message: "Searching for WendyOS devices (LAN + Bluetooth)",
            successMessage: nil,
            errorMessage: nil,
            showSpinner: true
        ) { _ in
            // Retry discovery until we find devices (up to 15 seconds total)
            let maxAttempts = 3
            for attempt in 1...maxAttempts {
                try Task.checkCancellation()

                // Discover LAN and Bluetooth in parallel
                async let lanDevicesTask = discovery.findAllDevices()

                #if canImport(Bluetooth)
                async let bluetoothDevicesTask = Self.discoverBluetoothDevices(scanDuration: 5)
                let bluetoothDevices = try await bluetoothDevicesTask
                #else
                let bluetoothDevices: [BluetoothDevice] = []
                #endif

                let lanCollection = try await lanDevicesTask

                // Combine into a single collection
                let combined = DevicesCollection(
                    usb: lanCollection.usbDevices,
                    ethernet: lanCollection.ethernetDevices,
                    lan: lanCollection.lanDevices,
                    bluetooth: bluetoothDevices
                )

                // Group and filter to devices with LAN or Bluetooth interfaces
                let grouped = combined.groupedDevices().filter { device in
                    device.interfaces.contains { interface in
                        switch interface {
                        case .lan, .bluetooth:
                            return true
                        default:
                            return false
                        }
                    }
                }

                if !grouped.isEmpty {
                    return grouped
                }

                // Wait before retrying (unless last attempt)
                if attempt < maxAttempts {
                    try await Task.sleep(for: .seconds(1))
                }
            }

            return []
        }

        if devices.isEmpty {
            throw NoDevicesFound()
        }

        let device: DevicesCollection.GroupedDevice = Noora().singleChoicePrompt(
            title: title,
            question: "Select a device",
            options: devices
        )

        printDeviceDetails(device)

        // Determine connection type based on available interfaces
        // Prefer LAN if available, otherwise use Bluetooth
        for interface in device.interfaces {
            if case .lan(let lanDevice) = interface {
                return .grpc(Endpoint(
                    host: lanDevice.hostname,
                    port: lanDevice.port
                ))
            }
        }

        for interface in device.interfaces {
            if case .bluetooth(let btDevice) = interface {
                // Use the address for matching, not displayName (which could be a fallback)
                return .bluetooth(deviceIdentifier: btDevice.address)
            }
        }

        throw InvalidEndpoint()
    }

    #if canImport(Bluetooth)
    /// Actor for safely collecting discovered Bluetooth devices
    private actor BluetoothDeviceCollector {
        private var devices: [BluetoothDevice] = []

        func add(_ device: BluetoothDevice) {
            if !devices.contains(where: { $0.id == device.id }) {
                devices.append(device)
            }
        }

        func getDevices() -> [BluetoothDevice] {
            return devices
        }
    }

    /// Discovers Bluetooth devices (simplified version for device selection)
    private static func discoverBluetoothDevices(scanDuration: Int = 5) async throws -> [BluetoothDevice] {
        let logger = Logger(label: "sh.wendy.cli.bluetooth.discover")
        let central = CentralManager()

        // Check current Bluetooth state
        let currentState = await central.state()
        switch currentState {
        case .poweredOn:
            break
        case .poweredOff, .unauthorized, .unsupported:
            logger.debug("Bluetooth not available: \(currentState)")
            return []
        default:
            // Wait for state to stabilize
            for await state in await central.stateUpdates() {
                switch state {
                case .poweredOn:
                    break
                case .poweredOff, .unauthorized, .unsupported:
                    return []
                default:
                    continue
                }
                break
            }
        }

        try await Task.sleep(for: .milliseconds(100))

        let collector = BluetoothDeviceCollector()
        let serviceUUID = UUID(uuidString: WendyBluetoothUUIDs.serviceUUID)!
        let targetServiceUUID = BluetoothUUID(serviceUUID)

        // Scan for 2 seconds
        try await withThrowingTaskGroup(of: Void.self) { group in
            group.addTask {
                let scanParameters = ScanParameters(allowDuplicates: true)
                for try await result in try await central.scan(filter: nil, parameters: scanParameters) {
                    let name = result.advertisementData.localName ?? result.peripheral.name ?? "Unknown"

                    // Only process devices with Wendy service UUID or name
                    guard result.advertisementData.serviceUUIDs.contains(targetServiceUUID) ||
                          name.lowercased().contains("wendy") else {
                        continue
                    }

                    let device = BluetoothDevice(
                        id: result.peripheral.id.rawValue,
                        displayName: result.advertisementData.localName ?? "WendyOS Device",
                        address: result.peripheral.id.rawValue,
                        rssi: result.rssi,
                        l2capPSM: nil
                    )

                    await collector.add(device)
                }
            }

            group.addTask {
                try await Task.sleep(for: .seconds(scanDuration))
            }

            _ = try await group.next()
            group.cancelAll()
        }

        try await central.stopScan()
        return await collector.getDevices()
    }
    #endif

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
