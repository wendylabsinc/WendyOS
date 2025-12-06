import Logging

public protocol DeviceDiscovery: Sendable {
    func findUSBDevices() async -> [USBDevice]
    func findEthernetInterfaces() async -> [EthernetInterface]
    func findLANDevices() async throws -> [LANDevice]
}

extension DeviceDiscovery {
    public func findAllDevices() async throws -> DevicesCollection {
        async let usbDevices = findUSBDevices()
        async let ethernetDevices = findEthernetInterfaces()
        async let lanDevices = findLANDevices()

        return try await DevicesCollection(
            usb: usbDevices,
            ethernet: ethernetDevices,
            lan: lanDevices
        )
    }
}