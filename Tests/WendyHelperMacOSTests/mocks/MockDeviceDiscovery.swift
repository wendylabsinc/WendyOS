#if os(macOS)
    import WendyShared
    import Foundation
    import Logging

    /// Mock device discovery service for testing USB monitor
    actor MockUSBDeviceDiscovery: DeviceDiscovery {

        // Test control properties
        var mockUSBDevices: [USBDevice] = []
        var mockEthernetInterfaces: [EthernetInterface] = []
        var mockLANDevices: [LANDevice] = []

        var shouldFailUSBDiscovery = false
        var shouldFailEthernetDiscovery = false
        var shouldFailLANDiscovery = false

        // Call tracking
        var findUSBDevicesCallCount = 0
        var findEthernetInterfacesCallCount = 0
        var findLANDevicesCallCount = 0

        private let logger: Logger

        init(logger: Logger) {
            self.logger = logger
        }

        func findUSBDevices() async -> [USBDevice] {
            findUSBDevicesCallCount += 1

            if shouldFailUSBDiscovery {
                logger.error("Mock USB discovery failure")
                return []
            }

            for device in mockUSBDevices {
                if device.isWendyDevice {
                    logger.info("deviceName=\(device.name) [WendyShared] Wendy device found")
                } else {
                    logger.debug(
                        "device=\(device.name) - Vendor ID: \(device.vendorId), Product ID: \(device.productId) [WendyShared] Found device"
                    )
                }
            }

            return mockUSBDevices
        }

        func findEthernetInterfaces() async -> [EthernetInterface] {
            findEthernetInterfacesCallCount += 1

            if shouldFailEthernetDiscovery {
                logger.error("Mock ethernet discovery failure")
                return []
            }

            for interface in mockEthernetInterfaces {
                if interface.isWendyDevice {
                    logger.info(
                        "interface=\(interface.displayName) [WendyShared] Wendy interface found"
                    )
                }
            }

            return mockEthernetInterfaces
        }

        func findLANDevices() async throws -> [LANDevice] {
            findLANDevicesCallCount += 1

            if shouldFailLANDiscovery {
                throw MockDeviceDiscoveryError.lanDiscoveryFailed
            }

            return mockLANDevices
        }

        // Test helper methods
        func addMockUSBDevice(_ device: USBDevice) async {
            mockUSBDevices.append(device)
        }

        func addMockEthernetInterface(_ interface: EthernetInterface) async {
            mockEthernetInterfaces.append(interface)
        }

        func addMockLANDevice(_ device: LANDevice) async {
            mockLANDevices.append(device)
        }

        func clearMockDevices() async {
            mockUSBDevices.removeAll()
            mockEthernetInterfaces.removeAll()
            mockLANDevices.removeAll()
        }

        func setShouldFailUSBDiscovery(_ value: Bool) async {
            shouldFailUSBDiscovery = value
        }

        func setShouldFailEthernetDiscovery(_ value: Bool) async {
            shouldFailEthernetDiscovery = value
        }

        func setShouldFailLANDiscovery(_ value: Bool) async {
            shouldFailLANDiscovery = value
        }

        func resetCounts() async {
            findUSBDevicesCallCount = 0
            findEthernetInterfacesCallCount = 0
            findLANDevicesCallCount = 0
        }

        func reset() async {
            await resetCounts()
            await clearMockDevices()
            shouldFailUSBDiscovery = false
            shouldFailEthernetDiscovery = false
            shouldFailLANDiscovery = false
        }
    }

    /// Mock device creation helpers
    extension MockUSBDeviceDiscovery {
        func addMockWendyUSBDevice(
            name: String = "Wendy Device",
            vendorId: Int = 0x1D6B,
            productId: Int = 0x0104
        ) async {
            let device = USBDevice(name: name, vendorId: vendorId, productId: productId)
            await addMockUSBDevice(device)
        }

        func addMockRegularUSBDevice(
            name: String = "Regular Device",
            vendorId: Int = 0x1234,
            productId: Int = 0x5678
        ) async {
            let device = USBDevice(name: name, vendorId: vendorId, productId: productId)
            await addMockUSBDevice(device)
        }

        func addMockWendyEthernetInterface(
            name: String = "Wendy Ethernet",
            bsdName: String = "en0"
        ) async {
            let interface = EthernetInterface(
                name: bsdName,
                displayName: name,
                interfaceType: "Ethernet",
                macAddress: "00:11:22:33:44:55"
            )
            await addMockEthernetInterface(interface)
        }
    }

    /// Mock errors for device discovery testing
    enum MockDeviceDiscoveryError: Error, LocalizedError {
        case usbDiscoveryFailed
        case ethernetDiscoveryFailed
        case lanDiscoveryFailed

        var errorDescription: String? {
            switch self {
            case .usbDiscoveryFailed:
                return "Mock USB device discovery failed"
            case .ethernetDiscoveryFailed:
                return "Mock ethernet interface discovery failed"
            case .lanDiscoveryFailed:
                return "Mock LAN device discovery failed"
            }
        }
    }
#endif
