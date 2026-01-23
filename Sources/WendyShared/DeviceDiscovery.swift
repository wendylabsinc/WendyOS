import Logging

public protocol DeviceDiscovery: Sendable {
    func findUSBDevices() async -> [USBDevice]
    func findEthernetInterfaces() async -> [EthernetInterface]
    func findLANDevices() async throws -> [LANDevice]
    func findBluetoothDevices(resolveAgentVersion: Bool) async throws -> [BluetoothDevice]

    /// Discover LAN devices and call the handler for each one as it's found
    func withLANDeviceDiscovery(_ handler: (LANDevice) async throws -> Void) async throws
}

extension DeviceDiscovery {
    /// Convenience method that calls findBluetoothDevices with resolveAgentVersion: false
    public func findBluetoothDevices() async throws -> [BluetoothDevice] {
        try await findBluetoothDevices(resolveAgentVersion: false)
    }

    /// Default implementation that wraps findLANDevices
    public func withLANDeviceDiscovery(_ handler: (LANDevice) async throws -> Void) async throws {
        let devices = try await findLANDevices()
        for device in devices {
            try await handler(device)
        }
    }
}

extension DeviceDiscovery {
    public func findAllDevices() async throws -> DevicesCollection {
        try await findDevices(includeBluetooth: true)
    }

    /// Find devices with optional Bluetooth scanning.
    /// BLE scan takes 5+ seconds, so skip when not needed (e.g., `wendy run`).
    public func findDevices(includeBluetooth: Bool) async throws -> DevicesCollection {
        if includeBluetooth {
            async let usbDevices = findUSBDevices()
            async let ethernetDevices = findEthernetInterfaces()
            async let lanDevices = findLANDevices()
            async let bluetoothDevices = findBluetoothDevices()

            return try await DevicesCollection(
                usb: usbDevices,
                ethernet: ethernetDevices,
                lan: lanDevices,
                bluetooth: bluetoothDevices
            )
        } else {
            async let usbDevices = findUSBDevices()
            async let ethernetDevices = findEthernetInterfaces()
            async let lanDevices = findLANDevices()

            return try await DevicesCollection(
                usb: usbDevices,
                ethernet: ethernetDevices,
                lan: lanDevices,
                bluetooth: []
            )
        }
    }
}
