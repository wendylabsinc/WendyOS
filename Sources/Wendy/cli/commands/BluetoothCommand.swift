import ArgumentParser
import AsyncAlgorithms
import Dispatch
import Foundation
import GRPCCore
import Logging
import Noora
import WendyAgentGRPC

#if os(macOS)
    import Darwin
#elseif canImport(Glibc)
    import Glibc
#elseif canImport(Musl)
    import Musl
#endif

@inline(__always) private func systemExit(_ code: Int32) -> Never {
    #if os(macOS)
        Darwin.exit(code)
    #elseif canImport(Glibc)
        Glibc.exit(code)
    #elseif canImport(Musl)
        Musl.exit(code)
    #else
        fatalError("Unsupported platform")
    #endif
}

struct BluetoothCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "bluetooth",
        abstract: "Manage Bluetooth connections.",
        subcommands: [
            ListCommand.self,
            ConnectCommand.self,
            DisconnectCommand.self,
            ForgetCommand.self,
        ]
    )

    // MARK: - List Command (Paired/Connected Devices)

    struct ListCommand: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            commandName: "list",
            abstract: "List paired or connected Bluetooth devices."
        )

        @Flag(name: .customLong("stream"), help: "Show live updates")
        var stream: Bool = false

        @OptionGroup var agentConnectionOptions: AgentConnectionOptions

        func run() async throws {
            try await withAgentClient(
                agentConnectionOptions,
                title: "For which device do you want to list Bluetooth devices?"
            ) { [stream] agent in
                if JSONMode.isEnabled && !stream {
                    // One-time JSON output
                    let devices = try await agent.listBluetoothDevices()
                    cliOutput.result(devices)
                } else {
                    if !JSONMode.isEnabled {
                        cliOutput.info("Press Ctrl+C to exit")
                    }

                    // Live table showing paired or connected devices
                    let scanner = PairedDevicesScanner(source: agent)

                    // // Use non-selectable live table
                    try await cliOutput.streamingTable(
                        initial: [],
                        updates: scanner
                    ) { devices -> (headers: [String], rows: [[String]]) in
                        return (
                            headers: ["Name", "Address", "Type", "Status"],
                            rows: devices.map { device in
                                [
                                    device.name, device.address, device.deviceTypeDisplay,
                                    device.connectedStatus,
                                ]
                            }
                        )
                    }
                }
            }
        }
    }

    // MARK: - Disconnect Command

    struct DisconnectCommand: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            commandName: "disconnect",
            abstract: "Disconnect from a Bluetooth device."
        )

        @Option(name: .long, help: "MAC address of the device to disconnect")
        var address: String?

        @Option(help: "Number of rows to display per page")
        var pageSize: Int = 10

        @OptionGroup var agentConnectionOptions: AgentConnectionOptions

        func run() async throws {
            try await withAgentGRPCClient(
                agentConnectionOptions,
                title: "Which device do you want to disconnect Bluetooth on?"
            ) { client in
                let agent = Wendy_Agent_Services_V1_WendyAgentService.Client(wrapping: client)

                let targetDevice: BluetoothDeviceInfo?

                if let providedAddress = address {
                    targetDevice = try await resolveBluetoothDevice(
                        address: providedAddress,
                        agent: agent
                    )
                } else if JSONMode.isEnabled {
                    jsonModeRequiresArgument(
                        argument: "address",
                        description:
                            "Provide --address <mac_address> to specify the device to disconnect"
                    )
                } else {
                    cliOutput.info("Press Ctrl+C to exit.")

                    let devices = try await fetchBluetoothDevices(
                        agent: agent
                    ) { $0.connected }

                    guard !devices.isEmpty else {
                        cliOutput.info("No connected Bluetooth devices found.")
                        return
                    }

                    let index = try await cliOutput.selectFromTable(
                        title: "Select device to disconnect",
                        headers: bluetoothDeviceHeaders,
                        rows: bluetoothDeviceRows(devices),
                        pageSize: min(pageSize, devices.count)
                    )

                    targetDevice = devices[index]
                }

                guard let targetDevice else {
                    cliOutput.warning("No Bluetooth device selected.")
                    return
                }

                let response = try await cliOutput.withProgress(
                    message: "Disconnecting from \(targetDevice.displayName)...",
                    successMessage: "Disconnected from \(targetDevice.displayName)",
                    errorMessage: "Failed to disconnect from \(targetDevice.displayName)"
                ) {
                    try await agent.disconnectBluetoothDevice(
                        .with { $0.address = targetDevice.address }
                    )
                }

                if response.hasStatus {
                    emitResponseStatusIfNeeded(response.status)
                }

                if !response.success {
                    let statusMessage = response.hasStatus ? response.status.message : ""
                    let trimmedStatus = statusMessage.trimmingCharacters(
                        in: .whitespacesAndNewlines
                    )
                    throw BluetoothCommandError.disconnectFailed(
                        targetDevice.displayName,
                        response.hasErrorMessage
                            ? response.errorMessage
                            : (!trimmedStatus.isEmpty ? trimmedStatus : "Unknown error")
                    )
                }

                if JSONMode.isEnabled {
                    cliOutput.result(
                        BluetoothOperationResult(
                            success: true,
                            device: targetDevice
                        )
                    )
                }
            }
        }
    }

    // MARK: - Forget Command

    struct ForgetCommand: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            commandName: "forget",
            abstract: "Forget a Bluetooth device."
        )

        @Option(name: .long, help: "MAC address of the device to forget")
        var address: String?

        @Option(help: "Number of rows to display per page")
        var pageSize: Int = 10

        @OptionGroup var agentConnectionOptions: AgentConnectionOptions

        func run() async throws {
            try await withAgentGRPCClient(
                agentConnectionOptions,
                title: "Which device do you want to forget Bluetooth on?"
            ) { client in
                let agent = Wendy_Agent_Services_V1_WendyAgentService.Client(wrapping: client)

                let targetDevice: BluetoothDeviceInfo?
                let needsConfirmation: Bool

                if let providedAddress = address {
                    targetDevice = try await resolveBluetoothDevice(
                        address: providedAddress,
                        agent: agent
                    )
                    needsConfirmation = false
                } else if JSONMode.isEnabled {
                    jsonModeRequiresArgument(
                        argument: "address",
                        description:
                            "Provide --address <mac_address> to specify the device to forget"
                    )
                } else {
                    cliOutput.info("Press Ctrl+C to exit.")

                    let devices = try await fetchBluetoothDevices(
                        agent: agent
                    ) { $0.paired || $0.connected }

                    guard !devices.isEmpty else {
                        cliOutput.info("No paired or connected Bluetooth devices found.")
                        return
                    }

                    let index = try await cliOutput.selectFromTable(
                        title: "Select device to forget",
                        headers: bluetoothDeviceHeaders,
                        rows: bluetoothDeviceRows(devices),
                        pageSize: min(pageSize, devices.count)
                    )

                    targetDevice = devices[index]
                    needsConfirmation = true
                }

                guard let targetDevice else {
                    cliOutput.warning("No Bluetooth device selected.")
                    return
                }

                if needsConfirmation {
                    let confirmed = try await confirmForget()
                    guard confirmed else {
                        cliOutput.info("Cancelled.")
                        return
                    }
                }

                let response = try await cliOutput.withProgress(
                    message: "Forgetting \(targetDevice.displayName)...",
                    successMessage: "Forgot \(targetDevice.displayName)",
                    errorMessage: "Failed to forget \(targetDevice.displayName)"
                ) {
                    try await agent.forgetBluetoothDevice(
                        .with { $0.address = targetDevice.address }
                    )
                }

                if response.hasStatus {
                    emitResponseStatusIfNeeded(response.status)
                }

                if !response.success {
                    let statusMessage = response.hasStatus ? response.status.message : ""
                    let trimmedStatus = statusMessage.trimmingCharacters(
                        in: .whitespacesAndNewlines
                    )
                    throw BluetoothCommandError.forgetFailed(
                        targetDevice.displayName,
                        response.hasErrorMessage
                            ? response.errorMessage
                            : (!trimmedStatus.isEmpty ? trimmedStatus : "Unknown error")
                    )
                }

                if JSONMode.isEnabled {
                    cliOutput.result(
                        BluetoothOperationResult(
                            success: true,
                            device: targetDevice
                        )
                    )
                }
            }
        }

        private func confirmForget() async throws -> Bool {
            let options = [
                "Yes, Forget Device",
                "Cancel",
            ]
            let index = try await cliOutput.selectFromTable(
                title: "Confirm forget device",
                headers: ["Confirm"],
                rows: options.map { [$0] },
                pageSize: options.count
            )
            return index == 0
        }
    }

    // MARK: - Connect Command (Scan & Connect)

    struct ConnectCommand: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            commandName: "connect",
            abstract: "Connect to a Bluetooth device."
        )

        @Option(name: .long, help: "MAC address of the device to connect to")
        var address: String?

        @Flag(name: .customLong("pair"), help: "Pair and trust the device before connecting")
        var pair: Bool = false

        @Flag(name: .customLong("trust"), help: "Alias for --pair")
        var trust: Bool = false

        @Option(help: "Number of rows to display per page")
        var pageSize: Int = 10

        @OptionGroup var agentConnectionOptions: AgentConnectionOptions

        func run() async throws {
            try await withAgentGRPCClient(
                agentConnectionOptions,
                title: "Which device do you want to connect Bluetooth on?"
            ) { client in
                let agent = Wendy_Agent_Services_V1_WendyAgentService.Client(wrapping: client)

                let targetAddress: String
                let targetDisplayName: String
                let shouldPairAndTrust = pair || trust

                if let providedAddress = address {
                    // Direct connection with provided address
                    targetAddress = providedAddress
                    targetDisplayName = providedAddress
                } else if JSONMode.isEnabled {
                    jsonModeRequiresArgument(
                        argument: "address",
                        description:
                            "Provide --address <mac_address> to specify the device to connect to"
                    )
                } else {
                    // Interactive: scan and select device
                    let target = try await scanAndSelectDevice(
                        agent: agent,
                        pageSize: pageSize
                    )
                    targetAddress = target.address
                    targetDisplayName = target.displayName
                }

                // Connect to the selected device
                let response = try await cliOutput.withProgress(
                    message: "Connecting to \(targetDisplayName)...",
                    successMessage: "Connected to \(targetDisplayName)",
                    errorMessage: "Failed to connect to \(targetDisplayName)"
                ) {
                    try await agent.connectBluetoothDevice(
                        .with {
                            $0.address = targetAddress
                            $0.pair = shouldPairAndTrust
                            $0.trust = shouldPairAndTrust
                        }
                    )
                }

                if response.hasStatus {
                    emitResponseStatusIfNeeded(response.status)
                }

                if !response.success {
                    let statusMessage = response.hasStatus ? response.status.message : ""
                    let trimmedStatus = statusMessage.trimmingCharacters(
                        in: .whitespacesAndNewlines
                    )
                    throw BluetoothCommandError.connectionFailed(
                        targetDisplayName,
                        response.hasErrorMessage
                            ? response.errorMessage
                            : (!trimmedStatus.isEmpty ? trimmedStatus : "Unknown error")
                    )
                }

                if JSONMode.isEnabled {
                    cliOutput.result(
                        BluetoothOperationResult(
                            success: true,
                            address: targetAddress,
                            displayName: targetDisplayName
                        )
                    )
                }
            }
        }

        private func scanAndSelectDevice(
            agent: Wendy_Agent_Services_V1_WendyAgentService.Client<GRPCTransport>,
            pageSize: Int
        ) async throws -> BluetoothDeviceInfo {
            let logger = Logger(label: "sh.wendy.cli.bluetooth.connect")

            // Start scanning
            cliOutput.info("Starting Bluetooth scan...")
            let scanResponse = try await agent.startBluetoothScan(.with { $0.timeoutSeconds = 0 })
            if scanResponse.hasStatus {
                emitResponseStatusIfNeeded(scanResponse.status)
            }
            if !scanResponse.success {
                let statusMessage = scanResponse.hasStatus ? scanResponse.status.message : ""
                let trimmedStatus = statusMessage.trimmingCharacters(in: .whitespacesAndNewlines)
                throw BluetoothCommandError.scanFailed(
                    scanResponse.hasErrorMessage
                        ? scanResponse.errorMessage
                        : (!trimmedStatus.isEmpty ? trimmedStatus : "Unknown error")
                )
            }

            let signalSource = installSigintHandler(agent: agent, logger: logger)

            // Use defer only for synchronous cleanup (signal handling)
            defer {
                signalSource.cancel()
                signal(SIGINT, SIG_DFL)
            }

            // Helper to stop scan with cancellation-aware fallback
            @Sendable func stopScanBestEffort() async {
                do {
                    _ = try await agent.stopBluetoothScan(.init())
                } catch {
                    // Best-effort cleanup; ignore stop failures.
                }
            }

            let scanner = DiscoveryScanner(source: agent)

            do {
                // Wait for devices to appear (with timeout)
                var initial: TableData?
                for attempt in 1...10 {
                    if let data = try await scanner.makeAsyncIterator().next(), !data.rows.isEmpty {
                        initial = data
                        break
                    }
                    if attempt < 10 {
                        cliOutput.info("Scanning for devices... (\(attempt * 2)s)")
                    }
                }

                guard let tableData = initial else {
                    throw BluetoothCommandError.noDevicesFound
                }

                let index = try await Noora().selectableTable(
                    tableData,
                    updates: scanner,
                    pageSize: pageSize
                )

                let devices = await scanner.currentDevices
                await stopScanBestEffort()
                return devices[index]
            } catch {
                await stopScanBestEffort()
                throw error
            }
        }

        private func installSigintHandler(
            agent: Wendy_Agent_Services_V1_WendyAgentService.Client<GRPCTransport>,
            logger: Logger
        ) -> DispatchSourceSignal {
            signal(SIGINT, SIG_IGN)

            let signalSource = DispatchSource.makeSignalSource(signal: SIGINT, queue: .global())
            signalSource.setEventHandler {
                logger.info("Received SIGINT, stopping Bluetooth scan")
                signalSource.cancel()
                Task {
                    await stopScanAndExit(agent: agent, logger: logger, exitCode: 130)
                }
            }
            signalSource.resume()
            return signalSource
        }

        private func stopScanAndExit(
            agent: Wendy_Agent_Services_V1_WendyAgentService.Client<GRPCTransport>,
            logger: Logger,
            exitCode: Int32
        ) async {
            logger.info(
                "Stopping Bluetooth scan before exit",
                metadata: ["exitCode": "\(exitCode)"]
            )
            await withTaskGroup(of: Void.self) { group in
                group.addTask {
                    _ = try? await agent.stopBluetoothScan(.init())
                }
                group.addTask {
                    try? await Task.sleep(for: .seconds(2))
                }
                _ = await group.next()
                group.cancelAll()
            }
            signal(SIGINT, SIG_DFL)
            raise(SIGINT)
            systemExit(exitCode)
        }
    }
}

// MARK: - Bluetooth Device Model

struct BluetoothDeviceInfo: Codable, Sendable {
    let name: String
    let address: String
    let rssi: Int?
    let paired: Bool
    let connected: Bool
    let trusted: Bool
    let deviceType: String
    let icon: String?

    init(
        name: String,
        address: String,
        rssi: Int? = nil,
        paired: Bool = false,
        connected: Bool = false,
        trusted: Bool = false,
        deviceType: String = "",
        icon: String? = nil
    ) {
        self.name = name
        self.address = address
        self.rssi = rssi
        self.paired = paired
        self.connected = connected
        self.trusted = trusted
        self.deviceType = deviceType
        self.icon = icon
    }

    init(from proto: Wendy_Agent_Services_V1_ListBluetoothDevicesResponse.BluetoothDevice) {
        self.name = proto.name.isEmpty ? "Unknown" : proto.name
        self.address = proto.address
        self.rssi = proto.hasRssi ? Int(proto.rssi) : nil
        self.paired = proto.paired
        self.connected = proto.connected
        self.trusted = proto.trusted
        self.deviceType = proto.deviceType.isEmpty ? "unknown" : proto.deviceType
        self.icon = proto.hasIcon ? proto.icon : nil
    }

    // Initializer for Bluetooth L2CAP proto type
    init(from proto: Wendy_Agent_Services_V1_BluetoothDeviceInfo) {
        self.name = proto.name.isEmpty ? "Unknown" : proto.name
        self.address = proto.address
        self.rssi = proto.hasRssi ? Int(proto.rssi) : nil
        self.paired = proto.paired
        self.connected = proto.connected
        self.trusted = proto.trusted
        self.deviceType = proto.deviceType.isEmpty ? "unknown" : proto.deviceType
        self.icon = proto.hasIcon ? proto.icon : nil
    }

    var displayName: String {
        if name.isEmpty || name == "Unknown" {
            return address
        }
        return "\(name) (\(address))"
    }

    var rssiDescription: String {
        guard let rssi = rssi else {
            return "N/A"
        }
        switch rssi {
        case -50...0:
            return "Excellent (\(rssi) dBm)"
        case -60 ..< -50:
            return "Good (\(rssi) dBm)"
        case -70 ..< -60:
            return "Fair (\(rssi) dBm)"
        default:
            return "Weak (\(rssi) dBm)"
        }
    }

    var deviceTypeDisplay: String {
        switch deviceType.lowercased() {
        case "audio-headset", "audio-headphones":
            return "Headset"
        case "audio-card", "audio-speakers":
            return "Speaker"
        case "input-keyboard":
            return "Keyboard"
        case "input-mouse":
            return "Mouse"
        case "input-gaming":
            return "Gamepad"
        case "phone":
            return "Phone"
        case "computer":
            return "Computer"
        default:
            return deviceType.isEmpty ? "Unknown" : deviceType.capitalized
        }
    }

    var connectedStatus: String {
        connected ? "Connected" : "Disconnected"
    }
}

/// Result type for Bluetooth operations in JSON mode
struct BluetoothOperationResult: Codable, Sendable {
    let success: Bool
    let device: BluetoothDeviceInfo?
    let address: String?
    let displayName: String?

    init(success: Bool, device: BluetoothDeviceInfo) {
        self.success = success
        self.device = device
        self.address = device.address
        self.displayName = device.displayName
    }

    init(success: Bool, address: String, displayName: String) {
        self.success = success
        self.device = nil
        self.address = address
        self.displayName = displayName
    }
}

private let bluetoothDeviceHeaders = [
    "Name",
    "Address",
    "Type",
    "Status",
]

private func bluetoothDeviceRows(_ devices: [BluetoothDeviceInfo]) -> [[String]] {
    devices.map { bluetoothDevice in
        [
            "\(bluetoothDevice.name)",
            "\(bluetoothDevice.address)",
            "\(bluetoothDevice.deviceTypeDisplay)",
            "\(bluetoothDevice.connectedStatus)",
        ]
    }
}

private func fetchBluetoothDevices(
    agent: Wendy_Agent_Services_V1_WendyAgentService.Client<GRPCTransport>,
    filter: (BluetoothDeviceInfo) -> Bool
) async throws -> [BluetoothDeviceInfo] {
    let response = try await agent.listBluetoothDevices(
        .with { $0.pairedOnly = false }
    )

    return response.devices
        .map { BluetoothDeviceInfo(from: $0) }
        .filter(filter)
        .sorted { bluetoothDevice1, bluetoothDevice2 in
            if bluetoothDevice1.connected != bluetoothDevice2.connected {
                return bluetoothDevice1.connected
            }
            return bluetoothDevice1.name < bluetoothDevice2.name
        }
}

private func resolveBluetoothDevice(
    address: String,
    agent: Wendy_Agent_Services_V1_WendyAgentService.Client<GRPCTransport>
) async throws -> BluetoothDeviceInfo {
    let match = try await fetchBluetoothDevices(agent: agent) { device in
        device.address.caseInsensitiveCompare(address) == .orderedSame
    }

    if let device = match.first {
        return device
    }

    return BluetoothDeviceInfo(
        name: "Unknown",
        address: address,
        rssi: nil,
        paired: false,
        connected: false,
        trusted: false,
        deviceType: "",
        icon: nil
    )
}

// MARK: - Paired/Connected Devices Scanner (for list command)

actor PairedDevicesScanner: nonisolated AsyncSequence {
    nonisolated let source: AgentClient

    init(source: AgentClient) {
        self.source = source
    }

    nonisolated func makeAsyncIterator() -> AsyncIterator {
        AsyncIterator(scanner: self)
    }

    struct AsyncIterator: AsyncIteratorProtocol {
        let scanner: PairedDevicesScanner

        func next() async throws -> [BluetoothDeviceInfo]? {
            // Poll interval for paired devices status
            try await Task.sleep(for: .seconds(2))
            return try await scanner.source.listBluetoothDevices()
        }
    }
}

// MARK: - Discovery Scanner (for connect command)

actor DiscoveryScanner: nonisolated AsyncSequence {
    typealias Element = TableData

    nonisolated let source: Wendy_Agent_Services_V1_WendyAgentService.Client<GRPCTransport>
    private(set) var currentDevices: [BluetoothDeviceInfo] = []

    init(source: Wendy_Agent_Services_V1_WendyAgentService.Client<GRPCTransport>) {
        self.source = source
    }

    func setDevices(_ devices: [BluetoothDeviceInfo]) {
        self.currentDevices = devices
    }

    nonisolated func makeAsyncIterator() -> AsyncIterator {
        AsyncIterator(scanner: self)
    }

    struct AsyncIterator: AsyncIteratorProtocol {
        let scanner: DiscoveryScanner

        func next() async throws -> TableData? {
            // Poll interval for discovery
            try await Task.sleep(for: .seconds(2))

            let response = try await scanner.source.listBluetoothDevices(
                .with { $0.pairedOnly = false }
            )

            let devices = response.devices
                .map { BluetoothDeviceInfo(from: $0) }
                .sorted { bluetoothDevice1, bluetoothDevice2 in
                    // Sort by: paired first, then by RSSI
                    if bluetoothDevice1.paired != bluetoothDevice2.paired {
                        return bluetoothDevice1.paired
                    }
                    return (bluetoothDevice1.rssi ?? -100) > (bluetoothDevice2.rssi ?? -100)
                }

            await scanner.setDevices(devices)

            let rows: [TableRow] = devices.map { bluetoothDevice in
                let statusIcon = bluetoothDevice.paired ? "○" : "◌"
                return [
                    "\(statusIcon) \(bluetoothDevice.name)",
                    "\(bluetoothDevice.address)",
                    "\(bluetoothDevice.deviceTypeDisplay)",
                    "\(bluetoothDevice.rssiDescription)",
                ]
            }

            return TableData(
                columns: [
                    TableColumn(title: "Name"),
                    TableColumn(title: "Address"),
                    TableColumn(title: "Type"),
                    TableColumn(title: "Signal"),
                ],
                rows: rows
            )
        }
    }
}

// MARK: - Errors

enum BluetoothCommandError: Error, LocalizedError {
    case noDevicesFound
    case scanFailed(String)
    case connectionFailed(String, String)
    case disconnectFailed(String, String)
    case forgetFailed(String, String)

    var errorDescription: String? {
        switch self {
        case .noDevicesFound:
            return "No Bluetooth devices found"
        case .scanFailed(let reason):
            return "Failed to start Bluetooth scan: \(reason)"
        case .connectionFailed(let address, let reason):
            return "Failed to connect to \(address): \(reason)"
        case .disconnectFailed(let address, let reason):
            return "Failed to disconnect from \(address): \(reason)"
        case .forgetFailed(let address, let reason):
            return "Failed to forget device \(address): \(reason)"
        }
    }
}
