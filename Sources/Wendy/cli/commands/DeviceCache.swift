import CLIOutput
import Foundation
import WendyShared

/// An interface with its last-seen timestamp
struct InterfaceWithTiming {
    let interface: DevicesCollection.InterfaceInfo
    let lastSeen: ContinuousClock.Instant

    func timeSinceLastSeen(relativeTo now: ContinuousClock.Instant) -> Duration {
        return now - lastSeen
    }
}

/// A grouped device with per-interface timing information
struct DeviceWithTiming {
    let name: String
    let interfaces: [InterfaceWithTiming]

    /// The most recent lastSeen across all interfaces
    var lastSeen: ContinuousClock.Instant {
        interfaces.map(\.lastSeen).max() ?? .now
    }

    /// Returns how long ago this device was last seen (using most recent interface)
    func timeSinceLastSeen(relativeTo now: ContinuousClock.Instant) -> Duration {
        return now - lastSeen
    }
}

/// Result of grouping devices with timing, includes per-type discovery timestamps
struct DevicesWithTimingResult {
    let devices: [DeviceWithTiming]
    /// When fast discovery (LAN/USB/Ethernet) last completed
    let lastFastDiscoveryTime: ContinuousClock.Instant
    /// When BLE discovery last completed
    let lastBLEDiscoveryTime: ContinuousClock.Instant
}

/// Cache for discovered devices that prevents flickering by keeping devices
/// visible for a period after they were last seen
actor DeviceCache {
    private var usbDevices: [String: (device: USBDevice, lastSeen: ContinuousClock.Instant)] = [:]
    private var ethernetDevices:
        [String: (device: EthernetInterface, lastSeen: ContinuousClock.Instant)] = [:]
    private var lanDevices: [String: (device: LANDevice, lastSeen: ContinuousClock.Instant)] = [:]
    private var bluetoothDevices:
        [String: (device: BluetoothDevice, lastSeen: ContinuousClock.Instant)] = [:]
    private var externalDevices:
        [String: (device: ExternalDevice, lastSeen: ContinuousClock.Instant)] = [:]

    /// Stale timeout for BLE devices (slower discovery)
    private let bleStaleTimeout: Duration = .seconds(60)
    /// Stale timeout for fast discovery types (LAN/USB/Ethernet)
    private let fastStaleTimeout: Duration = .seconds(30)

    /// Tracks the last time each device type was updated
    private var lastBLEUpdateTime: ContinuousClock.Instant = .now
    private var lastFastUpdateTime: ContinuousClock.Instant = .now

    init() {}

    /// Update cache with fast discovery results (USB, Ethernet, LAN)
    func updateFastDevices(with collection: DevicesCollection) {
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

        // Remove stale fast devices
        removeStaleDevices(olderThan: now)

        // Track when fast discovery happened
        lastFastUpdateTime = now
    }

    /// Update cache with BLE discovery results
    func updateBLEDevices(with devices: [BluetoothDevice]) {
        let now = ContinuousClock.now

        for device in devices {
            let key = device.id
            // Preserve agent version if we already have it
            var updatedDevice = device
            if let existing = bluetoothDevices[key], updatedDevice.agentVersion == nil {
                updatedDevice.agentVersion = existing.device.agentVersion
            }
            bluetoothDevices[key] = (updatedDevice, now)
        }

        // Remove stale BLE devices
        removeStaleDevices(olderThan: now)

        // Track when BLE discovery happened
        lastBLEUpdateTime = now
    }

    /// Update cache with external provider devices
    func updateExternalDevices(with devices: [ExternalDevice]) {
        let now = ContinuousClock.now

        for device in devices {
            let key = device.id
            externalDevices[key] = (device, now)
        }

        removeStaleDevices(olderThan: now)
        lastFastUpdateTime = now
    }

    /// Update cache with all device types (for backwards compatibility with JSON streaming)
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

        // Track when this update happened
        lastFastUpdateTime = now
        lastBLEUpdateTime = now
    }

    private func removeStaleDevices(olderThan now: ContinuousClock.Instant) {
        let fastCutoff = now - fastStaleTimeout
        let bleCutoff = now - bleStaleTimeout

        usbDevices = usbDevices.filter { $0.value.lastSeen > fastCutoff }
        ethernetDevices = ethernetDevices.filter { $0.value.lastSeen > fastCutoff }
        lanDevices = lanDevices.filter { $0.value.lastSeen > fastCutoff }
        bluetoothDevices = bluetoothDevices.filter { $0.value.lastSeen > bleCutoff }
        externalDevices = externalDevices.filter { $0.value.lastSeen > fastCutoff }
    }

    func groupedDevices() -> DevicesWithTimingResult {
        let collection = DevicesCollection(
            usb: usbDevices.values.map(\.device),
            ethernet: ethernetDevices.values.map(\.device),
            lan: lanDevices.values.map(\.device),
            bluetooth: bluetoothDevices.values.map(\.device),
            external: externalDevices.values.map(\.device)
        )

        // Build lookup using unique keys that match how we store devices
        // USB: vendorId-productId-serialNumber, Ethernet: name, LAN: hostname, BLE: id, External: id
        var usbLastSeen: [String: ContinuousClock.Instant] = [:]
        var ethernetLastSeen: [String: ContinuousClock.Instant] = [:]
        var lanLastSeen: [String: ContinuousClock.Instant] = [:]
        var bleLastSeen: [String: ContinuousClock.Instant] = [:]
        var externalLastSeen: [String: ContinuousClock.Instant] = [:]

        for (key, value) in usbDevices {
            usbLastSeen[key] = value.lastSeen
        }
        for (key, value) in ethernetDevices {
            ethernetLastSeen[key] = value.lastSeen
        }
        for (key, value) in lanDevices {
            lanLastSeen[key] = value.lastSeen
        }
        for (key, value) in bluetoothDevices {
            bleLastSeen[key] = value.lastSeen
        }
        for (key, value) in externalDevices {
            externalLastSeen[key] = value.lastSeen
        }

        let devices = collection.groupedDevices().map { groupedDevice in
            // Build per-interface timing
            let interfacesWithTiming = groupedDevice.interfaces.map {
                interface -> InterfaceWithTiming in
                let lastSeen: ContinuousClock.Instant
                switch interface {
                case .usb(let device):
                    let key = "\(device.vendorId)-\(device.productId)-\(device.serialNumber ?? "")"
                    lastSeen = usbLastSeen[key] ?? .now
                case .ethernet(let device):
                    lastSeen = ethernetLastSeen[device.name] ?? .now
                case .lan(let device):
                    lastSeen = lanLastSeen[device.hostname] ?? .now
                case .bluetooth(let device):
                    lastSeen = bleLastSeen[device.id] ?? .now
                case .external(let device):
                    lastSeen = externalLastSeen[device.id] ?? .now
                }
                return InterfaceWithTiming(interface: interface, lastSeen: lastSeen)
            }

            return DeviceWithTiming(name: groupedDevice.name, interfaces: interfacesWithTiming)
        }

        return DevicesWithTimingResult(
            devices: devices,
            lastFastDiscoveryTime: lastFastUpdateTime,
            lastBLEDiscoveryTime: lastBLEUpdateTime
        )
    }

    /// Returns the current cached devices as a DevicesCollection (for JSON output)
    func currentCollection() -> DevicesCollection {
        DevicesCollection(
            usb: usbDevices.values.map(\.device),
            ethernet: ethernetDevices.values.map(\.device),
            lan: lanDevices.values.map(\.device),
            bluetooth: bluetoothDevices.values.map(\.device),
            external: externalDevices.values.map(\.device)
        )
    }
}

extension DevicesWithTimingResult {
    func tableData() -> (headers: [String], rows: [[String]]) {
        let now = ContinuousClock.now
        // Only show "Last Seen" for interfaces stale by more than 20 seconds
        let staleDisplayThreshold: Duration = .seconds(20)

        // Helper to determine the appropriate discovery time for an interface
        func lastDiscoveryTime(
            for interface: DevicesCollection.InterfaceInfo
        ) -> ContinuousClock.Instant {
            switch interface {
            case .bluetooth:
                return lastBLEDiscoveryTime
            case .usb, .ethernet, .lan, .external:
                return lastFastDiscoveryTime
            }
        }

        // An interface is stale if it was last seen before its type's last discovery time
        // (meaning it was not found in the most recent discovery for that type)
        func isStale(_ interfaceWithTiming: InterfaceWithTiming) -> Bool {
            let discoveryTime = lastDiscoveryTime(for: interfaceWithTiming.interface)
            return interfaceWithTiming.lastSeen < discoveryTime
        }

        // Check if interface is stale enough to display (more than threshold)
        func isStaleEnoughToDisplay(_ interfaceWithTiming: InterfaceWithTiming) -> Bool {
            guard isStale(interfaceWithTiming) else { return false }
            let timeSinceSeen = now - interfaceWithTiming.lastSeen
            return timeSinceSeen > staleDisplayThreshold
        }

        // Check if any interface across all devices is stale enough to display
        let hasStaleInterfaces = devices.contains { device in
            device.interfaces.contains { isStaleEnoughToDisplay($0) }
        }

        // Build headers - add "Last Seen" column only when there are stale interfaces
        var headers = ["Name", "Connection", "Interfaces", "Version"]
        if hasStaleInterfaces {
            headers.append("Last Seen")
        }

        let rows: [[String]] = devices.map { deviceWithTiming in
            // Build connection info showing LAN hostname and/or BLE RSSI
            var connectionParts: [String] = []

            for interfaceWithTiming in deviceWithTiming.interfaces {
                switch interfaceWithTiming.interface {
                case .lan(let lanDevice):
                    connectionParts.append("\(lanDevice.hostname) (LAN)")
                case .bluetooth(let btDevice):
                    if btDevice.rssi != 0 {
                        connectionParts.append("RSSI: \(btDevice.rssi) dBm (BLE)")
                    } else {
                        connectionParts.append("(BLE)")
                    }
                case .external(let ext):
                    connectionParts.append("\(ext.providerKey): \(ext.id)")
                case .usb, .ethernet:
                    break
                }
            }

            let connection =
                connectionParts.isEmpty ? "-" : connectionParts.joined(separator: ", ")

            // Get agent version from any interface
            let agentVersion =
                deviceWithTiming.interfaces
                .compactMap { $0.interface.agentVersion }
                .first ?? "Unknown"

            var row: [String] = [
                deviceWithTiming.name,
                connection,
                deviceWithTiming.interfaces.map { $0.interface.shortDescription }.joined(
                    separator: ", "
                ),
                agentVersion,
            ]

            // Add "Last Seen" value if we're showing that column
            // Only show interfaces that are stale enough to display
            if hasStaleInterfaces {
                let staleInterfaceTimings = deviceWithTiming.interfaces.compactMap {
                    iface -> String? in
                    guard isStaleEnoughToDisplay(iface) else {
                        return nil  // Don't show interfaces that are current or barely stale
                    }
                    let seconds = Int((now - iface.lastSeen).components.seconds)
                    return "\(iface.interface.shortDescription): \(seconds)s ago"
                }

                let lastSeenText =
                    staleInterfaceTimings.isEmpty
                    ? "-" : staleInterfaceTimings.joined(separator: ", ")
                row.append(lastSeenText)
            }

            return row
        }

        return (headers: headers, rows: rows)
    }
}
