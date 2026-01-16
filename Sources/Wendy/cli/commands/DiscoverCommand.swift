import ArgumentParser
import AsyncAlgorithms
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

        switch type {
        case .usb:
            usbDevices = await discovery.findUSBDevices()
        case .ethernet:
            ethernetDevices = await discovery.findEthernetInterfaces()
        case .lan:
            lanDevices = try await discovery.findLANDevices()
        case .bluetooth:
            bluetoothDevices = try await discovery.findBluetoothDevices()
        case .all:
            // Fetch all types of devices
            async let _usbDevices = await discovery.findUSBDevices()
            async let _ethernetDevices = await discovery.findEthernetInterfaces()
            async let _lanDevices = try await discovery.findLANDevices()
            async let _bluetoothDevices = try await discovery.findBluetoothDevices()

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

            // Track devices with last-seen timestamps to avoid flickering
            let deviceCache = DeviceCache(staleTimeout: .seconds(30))
            await deviceCache.update(with: collection)

            let updates = AsyncTimerSequence(interval: .seconds(2), clock: .continuous)
                .map { _ in
                    let newDevices = try await discoverDevices()
                    await deviceCache.update(with: newDevices)
                    return await deviceCache.groupedDevices().tableData
                }

            await Noora().table(collection.groupedDevices().tableData, updates: updates)
        }
    }
}

/// Cache for discovered devices that prevents flickering by keeping devices
/// visible for a period after they were last seen
private actor DeviceCache {
    private var usbDevices: [String: (device: USBDevice, lastSeen: ContinuousClock.Instant)] = [:]
    private var ethernetDevices: [String: (device: EthernetInterface, lastSeen: ContinuousClock.Instant)] = [:]
    private var lanDevices: [String: (device: LANDevice, lastSeen: ContinuousClock.Instant)] = [:]
    private var bluetoothDevices: [String: (device: BluetoothDevice, lastSeen: ContinuousClock.Instant)] = [:]
    private let staleTimeout: Duration

    init(staleTimeout: Duration) {
        self.staleTimeout = staleTimeout
    }

    func update(with collection: DevicesCollection) {
        let now = ContinuousClock.now

        // Update USB devices
        for device in collection.usbDevices {
            let key = "\(device.vendorId)-\(device.productId)-\(device.serialNumber ?? "")"
            usbDevices[key] = (device, now)
        }

        // Update Ethernet devices
        for device in collection.ethernetDevices {
            let key = device.name
            ethernetDevices[key] = (device, now)
        }

        // Update LAN devices
        for device in collection.lanDevices {
            let key = device.hostname
            // Preserve agent version if we already have it
            var updatedDevice = device
            if let existing = lanDevices[key], updatedDevice.agentVersion == nil {
                updatedDevice.agentVersion = existing.device.agentVersion
            }
            lanDevices[key] = (updatedDevice, now)
        }

        // Update Bluetooth devices
        for device in collection.bluetoothDevices {
            let key = device.id
            // Preserve agent version if we already have it
            var updatedDevice = device
            if let existing = bluetoothDevices[key], updatedDevice.agentVersion == nil {
                updatedDevice.agentVersion = existing.device.agentVersion
            }
            bluetoothDevices[key] = (updatedDevice, now)
        }

        // Remove stale devices
        removeStaleDevices(olderThan: now)
    }

    private func removeStaleDevices(olderThan now: ContinuousClock.Instant) {
        let cutoff = now - staleTimeout

        usbDevices = usbDevices.filter { $0.value.lastSeen > cutoff }
        ethernetDevices = ethernetDevices.filter { $0.value.lastSeen > cutoff }
        lanDevices = lanDevices.filter { $0.value.lastSeen > cutoff }
        bluetoothDevices = bluetoothDevices.filter { $0.value.lastSeen > cutoff }
    }

    func groupedDevices() -> [DevicesCollection.GroupedDevice] {
        let collection = DevicesCollection(
            usb: usbDevices.values.map(\.device),
            ethernet: ethernetDevices.values.map(\.device),
            lan: lanDevices.values.map(\.device),
            bluetooth: bluetoothDevices.values.map(\.device)
        )
        return collection.groupedDevices()
    }
}

extension [DevicesCollection.GroupedDevice] {
    fileprivate var tableData: TableData {
        return TableData(
            columns: [
                TableColumn(title: "Name"),
                TableColumn(title: "Hostname"),
                TableColumn(title: "Interfaces"),
                TableColumn(title: "Version"),
            ],
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
