import ArgumentParser
import AsyncAlgorithms
import CLIOutput
import Dispatch
import Foundation
import GRPCCore
import Logging
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

        @Option(
            name: .customLong("timeout"),
            help: "Seconds to wait for initial device list when streaming"
        )
        var timeout: TimeInterval = 30

        @OptionGroup var agentConnectionOptions: AgentConnectionOptions

        func run() async throws {
            try await withAgentClient(
                agentConnectionOptions,
                title: "For which device do you want to list Bluetooth devices?"
            ) { [stream] client in
                if JSONMode.isEnabled && !stream {
                    actor Devices {
                        var devices = [Wendy_Agent_Services_V1_DiscoveredBluetoothPeripheral]()
                        var hasDevices = false

                        func set(
                            _ newDevices: [Wendy_Agent_Services_V1_DiscoveredBluetoothPeripheral]
                        ) {
                            self.devices = newDevices
                            if !newDevices.isEmpty {
                                hasDevices = true
                            }
                        }

                        func getDevices() -> [Wendy_Agent_Services_V1_DiscoveredBluetoothPeripheral]
                        {
                            return devices
                        }

                        func didFindDevices() -> Bool {
                            return hasDevices
                        }
                    }
                    let start = Date()
                    let devices = Devices()

                    do {
                        try await client.withBluetoothPeripherals { peripherals in
                            await devices.set(peripherals)

                            // Only timeout if no devices have been found yet
                            let hasDevices = await devices.didFindDevices()
                            if start.addingTimeInterval(timeout) < Date() && !hasDevices {
                                throw BluetoothCommandError.noDevicesFound
                            }
                        }
                    } catch is CancellationError {
                        // User cancelled, output what we have
                    }

                    // Output JSON result
                    try await cliOutput.result(await devices.getDevices())
                } else {
                    if !JSONMode.isEnabled {
                        cliOutput.info("Press Ctrl+C to exit")
                    }

                    return try await client.withBluetoothPeripheralsStream { stream in
                        // Use non-selectable live table
                        return try await cliOutput.streamingTable(
                            initial: [],
                            updates: stream
                        ) { devices -> (headers: [String], rows: [[String]]) in
                            return (
                                headers: ["Name", "Address", "Type", "Status"],
                                rows: devices.map { device in
                                    [
                                        device.name,
                                        device.address,
                                        device.deviceType,
                                        device.connected ? "Connected" : "Not Connected",
                                    ]
                                }
                            )
                        }
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
            try await withAgentClient(
                agentConnectionOptions,
                title: "Which device do you want to disconnect Bluetooth on?"
            ) { client in
                let targetDevice: String
                let displayName: String

                if let address {
                    targetDevice = address
                    displayName = address
                } else if JSONMode.isEnabled {
                    jsonModeRequiresArgument(
                        argument: "address",
                        description:
                            "Provide --address <mac_address> to specify the device to disconnect"
                    )
                } else {
                    cliOutput.info("Press Ctrl+C to exit.")

                    let device = try await client.withBluetoothPeripheralsStream { stream in
                        // Use non-selectable live table
                        return try await cliOutput.selectFromStreamingTable(
                            initial: [],
                            updates: stream,
                            pageSize: pageSize
                        ) { devices -> (headers: [String], rows: [[String]]) in
                            return (
                                headers: ["Name", "Address", "Type", "Status"],
                                rows: devices.filter(\.connected).map { device in
                                    [
                                        device.name,
                                        device.address,
                                        device.deviceType,
                                        device.connected ? "Connected" : "Not Connected",
                                    ]
                                }
                            )
                        }
                    }

                    targetDevice = device.address
                    displayName = device.name
                }

                let response = try await cliOutput.withProgress(
                    message: "Disconnecting from \(displayName)...",
                    successMessage: "Disconnected from \(displayName)",
                    errorMessage: "Failed to disconnect from \(displayName)"
                ) {
                    try await client.disconnectBluetoothPeripheral(
                        address: targetDevice
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
            try await withAgentClient(
                agentConnectionOptions,
                title: "Which device do you want to forget Bluetooth on?"
            ) { client in
                let targetDevice: String
                let displayName: String
                let needsConfirmation: Bool

                if let address {
                    targetDevice = address
                    displayName = address
                    needsConfirmation = false
                } else if JSONMode.isEnabled {
                    jsonModeRequiresArgument(
                        argument: "address",
                        description:
                            "Provide --address <mac_address> to specify the device to forget"
                    )
                } else {
                    cliOutput.info("Press Ctrl+C to exit.")

                    let device = try await client.withBluetoothPeripheralsStream { stream in
                        // Use non-selectable live table
                        return try await cliOutput.selectFromStreamingTable(
                            initial: [],
                            updates: stream,
                            pageSize: pageSize
                        ) { devices -> (headers: [String], rows: [[String]]) in
                            return (
                                headers: ["Name", "Address", "Type", "Status"],
                                rows: devices.filter(\.paired).map { device in
                                    [
                                        device.name,
                                        device.address,
                                        device.deviceType,
                                        device.connected ? "Connected" : "Not Connected",
                                    ]
                                }
                            )
                        }
                    }
                    needsConfirmation = true
                    displayName = device.name
                    targetDevice = device.address
                }

                if needsConfirmation {
                    let confirmed = try await confirmForget()
                    guard confirmed else {
                        cliOutput.info("Cancelled.")
                        return
                    }
                }

                try await cliOutput.withProgress(
                    message: "Forgetting \(displayName)...",
                    successMessage: "Forgot \(displayName)",
                    errorMessage: "Failed to forget \(displayName)"
                ) {
                    try await client.forgetBluetoothPeripheral(
                        address: targetDevice
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
            try await withAgentClient(
                agentConnectionOptions,
                title: "Which device do you want to connect Bluetooth on?"
            ) { client in
                let targetAddress: String
                let targetDisplayName: String
                let shouldPairAndTrust = pair || trust

                if let address {
                    // Direct connection with provided address
                    targetAddress = address
                    targetDisplayName = address
                } else if JSONMode.isEnabled {
                    jsonModeRequiresArgument(
                        argument: "address",
                        description:
                            "Provide --address <mac_address> to specify the device to connect to"
                    )
                } else {
                    // Interactive: scan and select device

                    let targetDevice = try await client.withBluetoothPeripheralsStream { stream in
                        // Use non-selectable live table
                        return try await cliOutput.selectFromStreamingTable(
                            initial: [],
                            updates: stream,
                            pageSize: pageSize
                        ) { devices -> (headers: [String], rows: [[String]]) in
                            return (
                                headers: ["Name", "Address", "Type", "Status"],
                                rows: devices.filter { !$0.connected }.map { device in
                                    [
                                        device.name,
                                        device.address,
                                        device.deviceType,
                                        device.connected ? "Connected" : "Not Connected",
                                    ]
                                }
                            )
                        }
                    }
                    targetAddress = targetDevice.address
                    targetDisplayName = targetDevice.name
                }

                // Connect to the selected device
                try await cliOutput.withProgress(
                    message: "Connecting to \(targetDisplayName)...",
                    successMessage: "Connected to \(targetDisplayName)",
                    errorMessage: "Failed to connect to \(targetDisplayName)"
                ) {
                    try await client.connectBluetoothPeripheral(
                        address: targetAddress,
                        pair: shouldPairAndTrust,
                        trust: shouldPairAndTrust
                    )
                }
            }
        }
    }
}

extension AgentClient {
    fileprivate func withBluetoothPeripheralsStream<T: Sendable>(
        perform:
            @escaping (AsyncStream<[Wendy_Agent_Services_V1_DiscoveredBluetoothPeripheral]>)
            async throws -> T
    ) async throws -> T {
        let (stream, continuation) = AsyncStream.makeStream(
            of: [Wendy_Agent_Services_V1_DiscoveredBluetoothPeripheral].self
        )

        return try await withThrowingTaskGroup(of: Void.self) { group in
            group.addTask {
                defer { continuation.finish() }
                try await self.withBluetoothPeripherals { peripherals in
                    continuation.yield(peripherals)
                }
            }
            defer { continuation.finish() }
            defer { group.cancelAll() }
            return try await perform(stream)
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

extension Wendy_Agent_Services_V1_DiscoveredBluetoothPeripheral: Comparable, Encodable {
    var displayName: String {
        if name.isEmpty {
            return address
        } else {
            return "\(name) (\(address))"
        }
    }

    private enum CodingKeys: String, CodingKey {
        case name
        case address
        case deviceType
        case rssi
        case connected
    }

    public func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode(name, forKey: .name)
        try container.encode(address, forKey: .address)
        try container.encode(deviceType, forKey: .deviceType)
        try container.encode(rssi, forKey: .rssi)
        try container.encode(connected, forKey: .connected)
    }

    public static func < (
        lhs: Wendy_Agent_Services_V1_DiscoveredBluetoothPeripheral,
        rhs: Wendy_Agent_Services_V1_DiscoveredBluetoothPeripheral
    ) -> Bool {
        if lhs.rssi < rhs.rssi {
            return true
        } else if lhs.rssi > rhs.rssi {
            return false
        }
        return lhs.displayName.localizedCaseInsensitiveCompare(rhs.displayName) == .orderedAscending
    }
}
