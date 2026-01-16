import ArgumentParser
import AsyncAlgorithms
import Foundation
import Logging
import Noora
import WendyAgentGRPC
import WendyShared

#if canImport(Bluetooth)
    import Bluetooth
#endif

/// Cache for BLE devices to keep them visible even when they stop advertising
/// BLE advertisements are less reliable than LAN mDNS, so we keep devices visible longer
private actor BLEDeviceCache {
    struct CachedDevice {
        var device: BluetoothDevice
        var lastSeen: ContinuousClock.Instant
    }

    private var devices: [String: CachedDevice] = [:]
    private let visibilityDuration: Duration

    init(visibilityDuration: Duration) {
        self.visibilityDuration = visibilityDuration
    }

    /// Updates the cache with newly discovered devices and returns all non-expired devices
    func updateAndGetDevices(_ newDevices: [BluetoothDevice]) -> [BluetoothDevice] {
        let now = ContinuousClock.now

        // Update cache with new devices
        for device in newDevices {
            if var existing = devices[device.id] {
                // Update existing device (keep version if we had one and new one doesn't)
                if existing.device.agentVersion != nil && device.agentVersion == nil {
                    // Keep existing device but update lastSeen (rssi is immutable, device will be replaced on next full discovery)
                    existing.lastSeen = now
                    devices[device.id] = existing
                } else {
                    existing.device = device
                    existing.lastSeen = now
                    devices[device.id] = existing
                }
            } else {
                devices[device.id] = CachedDevice(device: device, lastSeen: now)
            }
        }

        // Remove expired devices and return non-expired ones
        devices = devices.filter { _, cached in
            now - cached.lastSeen < visibilityDuration
        }

        return devices.values.map(\.device)
    }

    /// Clears all cached devices
    func clear() {
        devices.removeAll()
    }
}

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

    @Option(name: .shortAndLong, help: "Bluetooth scan duration in seconds")
    var timeout: Int = 10

    @Flag(help: "Skip resolving the agent's version")
    var skipResolveAgentVersion: Bool = false

    /// BLE device visibility duration - keeps devices visible even after they stop advertising
    private static let bleDeviceVisibilityDuration: Duration = .seconds(30)

    /// Shared BLE device cache to persist devices across discovery cycles
    private static let bleCache = BLEDeviceCache(visibilityDuration: bleDeviceVisibilityDuration)

    private func discoverDevices() async throws -> DevicesCollection {
        let logger = Logger(label: "sh.wendy.cli.devices")
        let discovery = PlatformDeviceDiscovery(logger: logger)
        // Collect devices based on the requested type
        var usbDevices: [USBDevice] = []
        var ethernetDevices: [EthernetInterface] = []
        var lanDevices: [LANDevice] = []
        var bluetoothDevices: [BluetoothDevice] = []

        switch type {
        case .usb:
            usbDevices = await discovery.findUSBDevices()
        case .ethernet:
            ethernetDevices = await discovery.findEthernetInterfaces()
        case .lan:
            lanDevices = try await discovery.findLANDevices()
        case .bluetooth:
            let freshDevices = try await discoverBluetoothDevices()
            // Use cache to keep BLE devices visible longer than they advertise
            bluetoothDevices = await Self.bleCache.updateAndGetDevices(freshDevices)
        case .all:
            // Fetch all types of devices
            async let _usbDevices = await discovery.findUSBDevices()
            async let _ethernetDevices = await discovery.findEthernetInterfaces()
            async let _lanDevices = try await discovery.findLANDevices()
            async let _bluetoothDevices = try await discoverBluetoothDevices()

            usbDevices = await _usbDevices
            ethernetDevices = await _ethernetDevices
            lanDevices = try await _lanDevices
            // Use cache to keep BLE devices visible longer than they advertise
            bluetoothDevices = await Self.bleCache.updateAndGetDevices(try await _bluetoothDevices)
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

    #if canImport(Bluetooth)
        /// Intermediate structure to hold discovered device info before version resolution
        private struct DiscoveredBluetoothDevice {
            let peripheral: Peripheral
            var device: BluetoothDevice
            let l2capPSM: UInt16?
        }

        private func discoverBluetoothDevices() async throws -> [BluetoothDevice] {
            let logger = Logger(label: "sh.wendy.cli.bluetooth.discover")
            let central = CentralManager()

            // Check current Bluetooth state first (don't wait for updates if already ready)
            let currentState = await central.state()
            logger.debug("Bluetooth state: \(currentState)")
            switch currentState {
            case .poweredOn:
                // Already ready, proceed
                break
            case .poweredOff, .unauthorized, .unsupported:
                logger.debug("Bluetooth not available: \(currentState)")
                return []
            default:
                // State is unknown or resetting, wait for it to stabilize
                logger.debug("Waiting for Bluetooth to become ready...")
                for await state in await central.stateUpdates() {
                    logger.debug("Bluetooth state update: \(state)")
                    switch state {
                    case .poweredOn:
                        break
                    case .poweredOff, .unauthorized, .unsupported:
                        logger.debug("Bluetooth not available: \(state)")
                        return []
                    default:
                        continue
                    }
                    break
                }
            }

            // Small delay to let CoreBluetooth fully initialize
            try await Task.sleep(for: .milliseconds(100))
            logger.debug("Starting Bluetooth scan...")

            let serviceUUID = UUID(uuidString: WendyBluetoothUUIDs.serviceUUID)!
            // Don't use CoreBluetooth's native service filter - it has issues with case sensitivity
            // for custom 128-bit UUIDs. We filter manually below instead.
            let filter: ScanFilter? = nil
            let targetServiceUUID = BluetoothUUID(serviceUUID)

            // Use an actor to safely collect discovered devices
            actor DeviceCollector {
                var devices: [DiscoveredBluetoothDevice] = []
                var totalSeen: Int = 0

                func sawDevice() {
                    totalSeen += 1
                }

                func add(_ device: DiscoveredBluetoothDevice) -> Bool {
                    if devices.contains(where: { $0.device.id == device.device.id }) {
                        return false
                    }
                    devices.append(device)
                    return true
                }

                func getDevices() -> [DiscoveredBluetoothDevice] {
                    return devices
                }

                func getTotalSeen() -> Int {
                    return totalSeen
                }
            }

            let collector = DeviceCollector()

            let bluetoothServiceUUID = BluetoothUUID(serviceUUID)

            // Phase 1: Scan for devices (don't connect during scan - it causes flakiness)
            // Use allowDuplicates to catch devices that advertise intermittently
            let scanParameters = ScanParameters(allowDuplicates: true)
            try await withThrowingTaskGroup(of: Void.self) { group in
                group.addTask {
                    for try await result in try await central.scan(
                        filter: filter,
                        parameters: scanParameters
                    ) {
                        await collector.sawDevice()
                        let name =
                            result.advertisementData.localName ?? result.peripheral.name
                            ?? "Unknown"
                        let serviceUUIDs = result.advertisementData.serviceUUIDs.map {
                            $0.description
                        }

                        // Log all devices for debugging
                        logger.debug(
                            "Saw device",
                            metadata: [
                                "name": "\(name)",
                                "id": "\(result.peripheral.id.rawValue)",
                                "serviceUUIDs": "\(serviceUUIDs)",
                            ]
                        )

                        // Only process devices with Wendy service UUID or name
                        guard
                            result.advertisementData.serviceUUIDs.contains(targetServiceUUID)
                                || name.lowercased().contains("wendy")
                        else {
                            continue
                        }

                        // Extract PSM from service data if available
                        var l2capPSM: UInt16? = nil
                        if let psmData = result.advertisementData.serviceData[bluetoothServiceUUID],
                            psmData.count >= 2
                        {
                            l2capPSM = psmData.withUnsafeBytes { $0.load(as: UInt16.self) }
                        }

                        let device = BluetoothDevice(
                            id: result.peripheral.id.rawValue,
                            displayName: result.advertisementData.localName ?? result.peripheral.name ?? "WendyOS Device",
                            address: result.peripheral.id.rawValue,
                            rssi: result.rssi,
                            l2capPSM: l2capPSM
                        )

                        let discovered = DiscoveredBluetoothDevice(
                            peripheral: result.peripheral,
                            device: device,
                            l2capPSM: l2capPSM
                        )

                        if await collector.add(discovered) {
                            logger.debug(
                                "Found Bluetooth device",
                                metadata: [
                                    "name": "\(device.displayName)",
                                    "id": "\(device.id)",
                                ]
                            )
                        }
                    }
                }

                group.addTask {
                    try await Task.sleep(for: .seconds(self.timeout))
                }

                // Wait for timeout
                _ = try await group.next()
                group.cancelAll()
            }

            try await central.stopScan()
            let totalSeen = await collector.getTotalSeen()
            logger.debug("Scan complete. Total devices seen: \(totalSeen)")

            // Phase 2: Resolve versions for discovered devices (after scan completes)
            // Do this in parallel with a timeout per device
            let discoveredDevices = await collector.getDevices()
            logger.debug("Wendy devices found: \(discoveredDevices.count)")

            guard !skipResolveAgentVersion && !discoveredDevices.isEmpty else {
                return discoveredDevices.map(\.device)
            }

            logger.debug("Resolving versions for \(discoveredDevices.count) device(s) in parallel")

            let resolvedDevices = await withTaskGroup(of: BluetoothDevice.self) { group in
                for discovered in discoveredDevices {
                    group.addTask {
                        var device = discovered.device

                        do {
                            // 5 second timeout per device
                            let response = try await withThrowingTaskGroup(
                                of: BluetoothResponse.self
                            ) { timeoutGroup in
                                timeoutGroup.addTask {
                                    try await withBluetoothConnection(
                                        central: central,
                                        peripheral: discovered.peripheral,
                                        l2capPSM: discovered.l2capPSM
                                    ) { channel in
                                        try await channel.send(
                                            BluetoothAgentCommand.agentVersion.toData()
                                        )
                                        for try await data in channel.incoming() {
                                            return try BluetoothResponse.from(data: data)
                                        }
                                        throw BluetoothConnectionError.noResponse
                                    }
                                }

                                timeoutGroup.addTask {
                                    try await Task.sleep(for: .seconds(5))
                                    throw BluetoothConnectionError.noResponse
                                }

                                let result = try await timeoutGroup.next()!
                                timeoutGroup.cancelAll()
                                return result
                            }

                            if case .agentVersion(let version) = response {
                                device.agentVersion = version
                                logger.debug(
                                    "Resolved version",
                                    metadata: [
                                        "device": "\(device.displayName)",
                                        "version": "\(version)",
                                    ]
                                )
                            }
                        } catch {
                            logger.debug(
                                "Failed to resolve Bluetooth version",
                                metadata: [
                                    "device": "\(device.displayName)",
                                    "error": "\(error)",
                                ]
                            )
                        }

                        return device
                    }
                }

                var results: [BluetoothDevice] = []
                for await device in group {
                    results.append(device)
                }
                return results
            }

            return resolvedDevices
        }
    #else
        private func discoverBluetoothDevices() async throws -> [BluetoothDevice] {
            return []
        }
    #endif

    func run() async throws {
        let logger = Logger(label: "sh.wendy.cli.devices")

        if JSONMode.isEnabled {
            let collection = try await discoverDevices()
            do {
                let jsonOutput = try collection.toJSON()
                print(jsonOutput)
            } catch {
                logger.error("Error serializing to JSON: \(error)")
            }
        } else {
            let collection = try await Noora().progressStep(message: "Discovering Wendy devices") {
                progress in
                try await discoverDevices()
            }
            let updates = AsyncTimerSequence(interval: .seconds(2), clock: .continuous)
                .map { _ in
                    try await discoverDevices().groupedDevices().tableData
                }

            await Noora().table(collection.groupedDevices().tableData, updates: updates)
        }
    }
}

extension [DevicesCollection.GroupedDevice] {
    fileprivate var tableData: TableData {
        return TableData(
            columns: [
                TableColumn(title: "Name"),
                TableColumn(title: "Hostname (LAN)"),
                TableColumn(title: "RSSI (BLE)"),
                TableColumn(title: "Interfaces"),
                TableColumn(title: "Version"),
            ],
            rows: self.map { device in
                var hostname = ""
                var rssi = ""

                for interface in device.interfaces {
                    switch interface {
                    case .lan(let lanDevice):
                        hostname = lanDevice.hostname
                    case .bluetooth(let btDevice):
                        rssi = "\(btDevice.rssi) dBm"
                    default:
                        break
                    }
                }

                return [
                    "\(device.name)",
                    "\(hostname)",
                    "\(rssi)",
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

    private func resolveLANDeviceAgentVersions() async -> [LANDevice] {
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

            var collection = DevicesCollection()
            // Bluetooth versions are resolved during discovery, so just pass through
            collection.bluetoothDevices = self.bluetoothDevices

            for await devices in group {
                collection.usbDevices.append(contentsOf: devices.usbDevices)
                collection.ethernetDevices.append(contentsOf: devices.ethernetDevices)
                collection.lanDevices.append(contentsOf: devices.lanDevices)
            }

            return collection
        }
    }
}
