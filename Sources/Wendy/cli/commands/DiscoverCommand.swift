import ArgumentParser
import AsyncAlgorithms
import Bluetooth
import Foundation
import Logging
import Noora
import WendyAgentGRPC
import WendyShared

struct DiscoverCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "discover",
        abstract: "Find connected Wendy devices"
    )

    enum DeviceType: String, ExpressibleByArgument {
        case usb, ethernet, lan, bluetooth, all
    }

    @Option(help: "Device types to list (usb, ethernet, lan, bluetooth, or all)")
    var type: DeviceType = .all

    @Flag(name: [.customShort("j"), .long], help: "Output in JSON format")
    var json: Bool = false

    @Flag(name: [.customShort("s"), .long], help: "Stream continuous discovery updates (use with --json for JSON stream)")
    var stream: Bool = false

    @Flag(help: "Skip resolving the agent's version")
    var skipResolveAgentVersion: Bool = false

    private func discoverDevices() async throws -> DevicesCollection {
        let logger = Logger(label: "sh.wendy.cli.devices")
        let discovery = PlatformDeviceDiscovery(logger: logger)
        // Collect devices based on the requested type
        var usbDevices: [USBDevice] = []
        var ethernetDevices: [EthernetInterface] = []
        var lanDevices: [LANDevice] = []
        var bluetoothDevices: [BluetoothDevice] = []

        // Bluetooth devices need inline version resolution since we can't reliably reconnect
        let resolveBluetoothVersionInline = !skipResolveAgentVersion

        switch type {
        case .usb:
            usbDevices = await discovery.findUSBDevices()
        case .ethernet:
            ethernetDevices = await discovery.findEthernetInterfaces()
        case .lan:
            lanDevices = try await discovery.findLANDevices()
        case .bluetooth:
            bluetoothDevices = try await discovery.findBluetoothDevices(
                resolveAgentVersion: resolveBluetoothVersionInline
            )
        case .all:
            // Fetch all types of devices
            async let _usbDevices = await discovery.findUSBDevices()
            async let _ethernetDevices = await discovery.findEthernetInterfaces()
            async let _lanDevices = try await discovery.findLANDevices()
            async let _bluetoothDevices = try await discovery.findBluetoothDevices(
                resolveAgentVersion: resolveBluetoothVersionInline
            )

            usbDevices = await _usbDevices
            ethernetDevices = await _ethernetDevices
            lanDevices = try await _lanDevices
            bluetoothDevices = try await _bluetoothDevices
        }

        // Display devices in the requested format
        var collection = DevicesCollection(
            usb: usbDevices,
            ethernet: ethernetDevices,
            lan: lanDevices,
            bluetooth: bluetoothDevices
        )

        if !skipResolveAgentVersion {
            collection = try await collection.resolveAgentVersions()
        }

        return collection
    }

    func run() async throws {
        let logger = Logger(label: "sh.wendy.cli.devices")

        switch (json, stream) {
        case (true, false):
            // Single JSON output
            let collection = try await discoverDevices()
            do {
                let jsonOutput = try collection.toJSON()
                print(jsonOutput)
            } catch {
                logger.error("Error serializing to JSON: \(error)")
            }
        case (true, true):
            // Streaming JSONL output (one compact JSON object per line)
            let deviceCache = DeviceCache()

            // Initial discovery
            let initialCollection = try await discoverDevices()
            await deviceCache.update(with: initialCollection)
            printJSON(await deviceCache.currentCollection(), compact: true)

            // Continuous updates
            while !Task.isCancelled {
                try await Task.sleep(for: .seconds(2))

                do {
                    let newDevices = try await withThrowingTaskGroup(
                        of: DevicesCollection.self
                    ) { group in
                        group.addTask {
                            try await discoverDevices()
                        }
                        group.addTask {
                            try await Task.sleep(for: .seconds(30))
                            throw CancellationError()
                        }
                        guard let result = try await group.next() else {
                            return DevicesCollection()
                        }
                        group.cancelAll()
                        return result
                    }
                    await deviceCache.update(with: newDevices)
                } catch {
                    await deviceCache.update(with: DevicesCollection())
                }

                printJSON(await deviceCache.currentCollection(), compact: true)
            }
        case (false, _):
            // Interactive table output (with or without --stream flag)
            let deviceCache = DeviceCache()
            let (updates, continuation) = AsyncStream<TableData>.makeStream()

            // Run discovery and table display concurrently using structured concurrency
            async let discoveryTask: Void = runTableDiscovery(
                deviceCache: deviceCache,
                resolveBluetoothVersionInline: !skipResolveAgentVersion,
                skipVersionResolution: skipResolveAgentVersion,
                continuation: continuation
            )

            // Display table (consumes stream until finished)
            let emptyResult = await deviceCache.groupedDevices()
            await Noora().table(emptyResult.tableData(), updates: updates)

            await discoveryTask
        }
    }

    private func runTableDiscovery(
        deviceCache: DeviceCache,
        resolveBluetoothVersionInline: Bool,
        skipVersionResolution: Bool,
        continuation: sending AsyncStream<TableData>.Continuation
    ) async {
        let logger = Logger(label: "sh.wendy.cli.devices")
        let discovery = PlatformDeviceDiscovery(logger: logger)

        // Transfer ownership of continuation for safe sharing across task group children
        // AsyncStream.Continuation is thread-safe but Swift's region isolation
        // doesn't know this, so we use nonisolated(unsafe) to opt out of checking
        nonisolated(unsafe) let sharedContinuation = consume continuation

        try? await withThrowingDiscardingTaskGroup { group in
            // Refresh task: update table every second so "Last Seen" counter ticks
            group.addTask {
                while !Task.isCancelled {
                    try await Task.sleep(for: .seconds(1))
                    let result = await deviceCache.groupedDevices()
                    sharedContinuation.yield(result.tableData())
                }
            }

            // BLE discovery task
            group.addTask {
                while !Task.isCancelled {
                    do {
                        let devices = try await discovery.findBluetoothDevices(
                            resolveAgentVersion: resolveBluetoothVersionInline
                        )
                        logger.debug("BLE discovery done: \(devices.count)")
                        await deviceCache.updateBLEDevices(with: devices)
                    } catch {
                        logger.debug("BLE discovery failed: \(error)")
                    }
                    try await Task.sleep(for: .seconds(2))
                }
            }

            // LAN discovery task - calls handler for each device as found
            group.addTask {
                while !Task.isCancelled {
                    do {
                        try await discovery.withLANDeviceDiscovery { device in
                            var resolvedDevice = device
                            if !skipVersionResolution {
                                let collection = DevicesCollection(lan: [device])
                                if let resolved = await collection.resolveLANDeviceAgentVersions().first {
                                    resolvedDevice = resolved
                                }
                            }
                            await deviceCache.updateFastDevices(with: DevicesCollection(lan: [resolvedDevice]))
                            logger.debug("LAN device found: \(resolvedDevice.hostname)")
                        }
                    } catch is CancellationError {
                        break
                    } catch {
                        logger.debug("LAN discovery failed: \(error)")
                    }
                    // Mark LAN cycle complete for stale tracking
                    await deviceCache.updateFastDevices(with: DevicesCollection())
                    // Short delay before next LAN discovery cycle
                    try await Task.sleep(for: .seconds(2))
                }
            }

            // USB/Ethernet discovery task (fast, run every 2 seconds)
            group.addTask {
                while !Task.isCancelled {
                    async let usbDevices = discovery.findUSBDevices()
                    async let ethernetDevices = discovery.findEthernetInterfaces()

                    let usb = await usbDevices
                    let ethernet = await ethernetDevices

                    if !usb.isEmpty {
                        await deviceCache.updateFastDevices(with: DevicesCollection(usb: usb))
                    }
                    if !ethernet.isEmpty {
                        await deviceCache.updateFastDevices(with: DevicesCollection(ethernet: ethernet))
                    }

                    try await Task.sleep(for: .seconds(2))
                }
            }
        }

        sharedContinuation.finish()
    }

    private func printJSON(_ collection: DevicesCollection, compact: Bool = false) {
        do {
            let encoder = JSONEncoder()
            encoder.outputFormatting = compact ? [.sortedKeys] : [.prettyPrinted, .sortedKeys]
            let data = try encoder.encode(collection)
            if let jsonOutput = String(data: data, encoding: .utf8) {
                print(jsonOutput)
                // Flush stdout to ensure immediate output for piping
                fflush(stdout)
            }
        } catch {
            FileHandle.standardError.write(Data("Error serializing to JSON: \(error)\n".utf8))
        }
    }
}

extension DevicesCollection {
    private func resolveUSBDeviceAgentVersions() async -> [USBDevice] {
        // TODO: Agent version resolution unsupported
        return usbDevices
    }

    private func resolveEthernetDeviceAgentVersions() async -> [EthernetInterface] {
        // TODO: Agent version resolution unsupported
        return ethernetDevices
    }

    func resolveLANDeviceAgentVersions() async -> [LANDevice] {
        await withTaskGroup(of: LANDevice?.self) { group in
            for device in lanDevices {
                group.addTask {
                    do {
                        return try await withGRPCClient(
                            AgentConnectionOptions.Endpoint(host: device.hostname, port: 50051),
                            security: .plaintext
                        ) { client in
                            let agent = Wendy_Agent_Services_V1_WendyAgentService.Client(
                                wrapping: client
                            )
                            let version = try await agent.getAgentVersion(
                                request: .init(message: .init())
                            )
                            var device = device
                            device.agentVersion = version.version
                            return device
                        }
                    } catch {
                        return device
                    }
                }
            }

            return await group.reduce(into: [LANDevice]()) { devices, device in
                if let device {
                    devices.append(device)
                }
            }
        }
    }

    private func resolveBluetoothDeviceAgentVersions() async -> [BluetoothDevice] {
        await withTaskGroup(of: BluetoothDevice.self) { group in
            for device in bluetoothDevices {
                group.addTask {
                    let peripheral = Peripheral(
                        id: BluetoothDeviceID(device.id),
                        name: device.displayName
                    )
                    var updatedDevice = device
                    updatedDevice.agentVersion = try? await withRetry(maxAttempts: 3) {
                        try await BluetoothAgentClient.withConnection(
                            to: peripheral,
                            connectionTimeout: .seconds(10)
                        ) { client in
                            try await client.getAgentVersion()
                        }
                    }
                    return updatedDevice
                }
            }

            return await group.reduce(into: [BluetoothDevice]()) { devices, device in
                devices.append(device)
            }
        }
    }

    func resolveAgentVersions() async throws -> DevicesCollection {
        return await withTaskGroup(of: DevicesCollection.self) { group in
            group.addTask {
                let devices = await resolveUSBDeviceAgentVersions()
                return DevicesCollection(usb: devices)
            }

            group.addTask {
                let devices = await resolveEthernetDeviceAgentVersions()
                return DevicesCollection(ethernet: devices)
            }

            group.addTask {
                let devices = await resolveLANDeviceAgentVersions()
                return DevicesCollection(lan: devices)
            }

            group.addTask {
                let devices = await resolveBluetoothDeviceAgentVersions()
                return DevicesCollection(bluetooth: devices)
            }

            var collection = DevicesCollection()

            for await devices in group {
                collection.usbDevices.append(contentsOf: devices.usbDevices)
                collection.ethernetDevices.append(contentsOf: devices.ethernetDevices)
                collection.lanDevices.append(contentsOf: devices.lanDevices)
                collection.bluetoothDevices.append(contentsOf: devices.bluetoothDevices)
            }

            return collection
        }
    }
}

/// Retry an async operation with exponential backoff
private func withRetry<T>(
    maxAttempts: Int,
    initialDelay: Duration = .milliseconds(100),
    operation: () async throws -> T
) async throws -> T {
    precondition(maxAttempts > 0, "maxAttempts must be positive")
    var attempt = 0
    while true {
        attempt += 1
        do {
            return try await operation()
        } catch {
            guard attempt < maxAttempts else {
                throw error
            }
            let delay = initialDelay * (1 << (attempt - 1))
            try await Task.sleep(for: delay)
        }
    }
}
