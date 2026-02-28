import ArgumentParser
import AsyncAlgorithms
import CLIOutput
import Foundation
import Logging
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

    @Flag(help: "Stream the output as JSONL")
    var stream: Bool = false

    @Option(help: "Timeout in seconds")
    var timeout: TimeInterval = 10

    @Flag(help: "Skip resolving the agent's version")
    var skipResolveAgentVersion: Bool = false

    func run() async throws {
        let logger = Logger(label: "sh.wendy.cli.devices")

        if JSONMode.isEnabled && !stream {
            // Single JSON output with timeout
            let deviceCache = DeviceCache()
            do {
                try await withThrowingDiscardingTaskGroup { group in
                    // Timeout task
                    group.addTask {
                        try await Task.sleep(for: .seconds(self.timeout))
                        throw CancellationError()
                    }

                    // Discovery tasks
                    if type == .bluetooth || type == .all {
                        group.addTask {
                            let discovery = PlatformDeviceDiscovery(logger: logger)
                            while !Task.isCancelled {
                                let devices = try await discovery.findBluetoothDevices(
                                    resolveAgentVersion: !self.skipResolveAgentVersion
                                )
                                await deviceCache.updateBLEDevices(with: devices)
                                try await Task.sleep(for: .seconds(2))
                            }
                        }
                    }

                    if type == .lan || type == .all {
                        group.addTask {
                            let discovery = PlatformDeviceDiscovery(logger: logger)
                            while !Task.isCancelled {
                                try await discovery.withLANDeviceDiscovery { device in
                                    await deviceCache.updateFastDevices(
                                        with: DevicesCollection(lan: [device])
                                    )
                                }
                                try await Task.sleep(for: .seconds(2))
                            }
                        }
                    }

                    if type == .usb || type == .ethernet || type == .all {
                        group.addTask {
                            let discovery = PlatformDeviceDiscovery(logger: logger)
                            while !Task.isCancelled {
                                let usb = await discovery.findUSBDevices()
                                let ethernet = await discovery.findEthernetInterfaces()
                                await deviceCache.updateFastDevices(
                                    with: DevicesCollection(usb: usb, ethernet: ethernet)
                                )
                                try await Task.sleep(for: .seconds(2))
                            }
                        }
                    }

                    // External provider discovery
                    if type == .all {
                        group.addTask {
                            while !Task.isCancelled {
                                var allExternal = [ExternalDevice]()
                                for provider in DeviceProviderRegistry.availableProviders {
                                    if let devices = try? await provider.discoverDevices() {
                                        allExternal.append(contentsOf: devices)
                                    }
                                }
                                await deviceCache.updateExternalDevices(with: allExternal)
                                try await Task.sleep(for: .seconds(3))
                            }
                        }
                    }
                }
            } catch is CancellationError {
                // Timeout reached - this is expected
            }

            let collection = await deviceCache.currentCollection()
            cliOutput.result(collection)
        } else {
            // Streaming output (interactive table or JSON stream)
            let deviceCache = DeviceCache()
            let (updates, continuation) = AsyncStream<DevicesCollection>.makeStream()

            // Run discovery and output concurrently using structured concurrency
            async let discoveryTask: Void = DiscoverCommand.runStreamingDiscovery(
                deviceCache: deviceCache,
                resolveBluetoothVersionInline: !skipResolveAgentVersion,
                skipVersionResolution: skipResolveAgentVersion,
                continuation: continuation
            )

            let initial = await deviceCache.currentCollection()
            try await cliOutput.streamingTable(initial: initial, updates: updates) { collection in
                // Merge devices by name and render as table
                let grouped = collection.groupedDevices()
                let headers = ["Name", "Connection", "Interfaces", "Version"]
                let rows: [[String]] = grouped.map { device in
                    // Build connection info (hostname or RSSI)
                    var connectionParts: [String] = []
                    for iface in device.interfaces {
                        switch iface {
                        case .lan(let lan):
                            connectionParts.append(lan.hostname)
                        case .bluetooth(let ble):
                            connectionParts.append("BLE: \(ble.address)")
                            connectionParts.append("RSSI: \(ble.rssi)")
                        case .external(let ext):
                            connectionParts.append("\(ext.providerKey): \(ext.id)")
                        case .usb, .ethernet:
                            break
                        }
                    }
                    let connection = connectionParts.joined(separator: ", ")

                    // Build interfaces list
                    let interfaces = device.interfaces.map(\.shortDescription).joined(
                        separator: ", "
                    )

                    // Get version from any interface
                    let version = device.interfaces.compactMap(\.agentVersion).first ?? ""

                    return [device.name, connection, interfaces, version]
                }
                return (headers: headers, rows: rows)
            }

            await discoveryTask
        }
    }

    static func runStreamingDiscovery(
        deviceCache: DeviceCache,
        resolveBluetoothVersionInline: Bool,
        skipVersionResolution: Bool,
        continuation: sending AsyncStream<DevicesCollection>.Continuation
    ) async {
        let logger = Logger(label: "sh.wendy.cli.devices")
        let discovery = PlatformDeviceDiscovery(logger: logger)

        // Transfer ownership of continuation for safe sharing across task group children
        // AsyncStream.Continuation is Sendable and thread-safe
        let sharedContinuation = consume continuation

        try? await withThrowingDiscardingTaskGroup { group in
            // Refresh task: update every second
            group.addTask {
                while !Task.isCancelled {
                    try await Task.sleep(for: .seconds(1))
                    let collection = await deviceCache.currentCollection()
                    sharedContinuation.yield(collection)
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
                                if let resolved = await collection.resolveLANDeviceAgentVersions()
                                    .first
                                {
                                    resolvedDevice = resolved
                                }
                            }
                            await deviceCache.updateFastDevices(
                                with: DevicesCollection(lan: [resolvedDevice])
                            )
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
                        await deviceCache.updateFastDevices(
                            with: DevicesCollection(ethernet: ethernet)
                        )
                    }

                    try await Task.sleep(for: .seconds(2))
                }
            }

            // External provider discovery task (poll every 3 seconds)
            group.addTask {
                while !Task.isCancelled {
                    var allExternal = [ExternalDevice]()
                    for provider in DeviceProviderRegistry.availableProviders {
                        do {
                            let devices = try await provider.discoverDevices()
                            allExternal.append(contentsOf: devices)
                        } catch {
                            logger.debug("Provider \(provider.key) discovery failed: \(error)")
                        }
                    }
                    await deviceCache.updateExternalDevices(with: allExternal)
                    try await Task.sleep(for: .seconds(3))
                }
            }
        }

        sharedContinuation.finish()
    }
}

extension [DevicesCollection.GroupedDevice] {
    fileprivate var tableData: (headers: [String], rows: [[String]]) {
        return (
            headers: ["Name", "Hostname", "Interfaces", "Version"],
            rows: self.map { device in
                var hostname = ""
                for case .lan(let lanDevice) in device.interfaces {
                    hostname = lanDevice.hostname
                }
                return [
                    "\(device.name)",
                    "\(hostname)",
                    "\(device.interfaces.map { $0.shortDescription }.joined(separator: ", "))",
                    "\(device.interfaces.compactMap(\.agentVersion).first ?? "Unknown")",
                ]
            }
        )
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
                            host: device.hostname,
                            port: 50051,
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
                            device.os = version.os
                            device.osVersion = version.osVersion
                            device.cpuArchitecture = version.cpuArchitecture
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
        // Bluetooth agent version resolution is done via BLE L2CAP connection
        // This will be implemented in a subsequent PR
        return bluetoothDevices
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
