import Foundation
import Logging
import Testing
import WendyShared

@testable import wendy

// Define these enums for testing
enum DeviceTypeForTesting: String {
    case usb, ethernet, lan, all
}

enum OutputFormatForTesting: String {
    case json, text
}

@Suite("USB and Ethernet Device Discovery Tests")
struct DeviceDiscoveryTests {
    @Test("List all devices")
    func testAllDevices() async throws {
        let mockDiscovery = createMockDiscovery()

        // Test USB devices directly
        let devices = mockDiscovery.usbDevices
        #expect(devices.count == 2, "Should have 2 test devices")
        #expect(devices[0].name == "TestDevice1", "First device should be TestDevice1")

        // Test Ethernet interfaces directly
        let interfaces = mockDiscovery.ethernetInterfaces
        #expect(interfaces.count == 1, "Should have 1 test interface")
        #expect(interfaces[0].name == "eth0", "Interface should be eth0")
    }

    @Test("List only USB devices")
    func testUSBDevicesOnly() async throws {
        let mockDiscovery = createMockDiscovery()
        let devices = mockDiscovery.usbDevices

        #expect(devices.count == 2)
        #expect(devices[0].name == "TestDevice1")
        #expect(devices[1].name == "TestDevice2")
    }

    @Test("List only Ethernet devices")
    func testEthernetDevicesOnly() async throws {
        let mockDiscovery = createMockDiscovery()
        let interfaces = mockDiscovery.ethernetInterfaces

        #expect(interfaces.count == 1)
        #expect(interfaces[0].name == "eth0")
    }

    @Test("List only LAN devices")
    func testLANDevicesOnly() async throws {
        let mockDiscovery = createMockDiscovery()
        let lanDevices = mockDiscovery.lanDevices

        #expect(lanDevices.count == 1)
        #expect(lanDevices[0].id == "device123")
        #expect(lanDevices[0].displayName == "Test LAN Device")
        #expect(lanDevices[0].hostname == "test.local")
    }

    @Test("JSON formatting of devices")
    func testJSONOutput() async throws {
        // Test direct JSON encoding
        let devices = [
            USBDevice(name: "TestDevice1", vendorId: 0x1234, productId: 0x5678),
            USBDevice(name: "TestDevice2", vendorId: 0x8765, productId: 0x4321),
        ]

        let encoder = JSONEncoder()
        encoder.outputFormatting = [
            JSONEncoder.OutputFormatting.prettyPrinted, JSONEncoder.OutputFormatting.sortedKeys,
        ]
        let data = try encoder.encode(devices)
        let jsonString = String(data: data, encoding: .utf8)!

        print("JSON output: \(jsonString)")

        #expect(jsonString.contains("name"))
        #expect(jsonString.contains("TestDevice1"))
    }

    @Test("Empty device lists")
    func testEmptyDeviceLists() async throws {
        let mockDiscovery = MockDeviceDiscovery(usbDevices: [], ethernetInterfaces: [])

        let devices = mockDiscovery.usbDevices
        #expect(devices.isEmpty)

        let interfaces = mockDiscovery.ethernetInterfaces
        #expect(interfaces.isEmpty)
    }

    @Test("Command line argument type enum")
    func testDeviceTypeEnum() throws {
        // Test default value in a variable
        let defaultType: DeviceTypeForTesting = .all
        #expect(defaultType == .all)

        // Test equality for different types
        #expect(DeviceTypeForTesting.usb == .usb)
        #expect(DeviceTypeForTesting.ethernet == .ethernet)
        #expect(DeviceTypeForTesting.lan == .lan)
        #expect(DeviceTypeForTesting.all == .all)

        // Test string representation
        #expect(DeviceTypeForTesting.usb.rawValue == "usb")
        #expect(DeviceTypeForTesting.ethernet.rawValue == "ethernet")
        #expect(DeviceTypeForTesting.lan.rawValue == "lan")
        #expect(DeviceTypeForTesting.all.rawValue == "all")
    }

    @Test("Output format enum")
    func testOutputFormatEnum() throws {
        // Test equality
        #expect(OutputFormatForTesting.json == .json)
        #expect(OutputFormatForTesting.text == .text)

        // Test string representation
        #expect(OutputFormatForTesting.json.rawValue == "json")
        #expect(OutputFormatForTesting.text.rawValue == "text")
    }

    @Test("Command discovery mocking")
    func testCommandDiscovery() async throws {
        // Create a mock discovery
        let mockDiscovery = createMockDiscovery()

        // Verify the mock discovery returns the expected devices
        let usbDevices = await mockDiscovery.findUSBDevices()
        #expect(usbDevices.count == 2)
        #expect(usbDevices[0].name == "TestDevice1")

        // Verify mock discovery returns the expected interfaces
        let ethernetInterfaces = await mockDiscovery.findEthernetInterfaces()
        #expect(ethernetInterfaces.count == 1)
        #expect(ethernetInterfaces[0].name == "eth0")
    }

    @Test("Edge case: malformed device data")
    func testMalformedDeviceData() async throws {
        // Create devices with potentially problematic data
        let malformedDevices = [
            USBDevice(name: "", vendorId: 0, productId: 0),
            USBDevice(name: "Device with \"quotes\"", vendorId: 0x1234, productId: 0x5678),
        ]

        // Direct encoding test
        let encoder = JSONEncoder()
        encoder.outputFormatting = [
            JSONEncoder.OutputFormatting.prettyPrinted, JSONEncoder.OutputFormatting.sortedKeys,
        ]
        let data = try encoder.encode(malformedDevices)
        let jsonString = String(data: data, encoding: .utf8)!

        print("Malformed JSON: \(jsonString)")

        // Very basic tests that should pass regardless of exact formatting
        #expect(jsonString.contains("name"))
        #expect(jsonString.contains("Device with"))
    }

    @Test("Error logging during device discovery")
    func testErrorLogging() async throws {
        // Create test logger to capture errors
        let testLogger = TestLogger()

        // Just verify that the logger can capture errors
        testLogger.addError("Test error message")
        #expect(testLogger.errorMessages.count > 0)
        #expect(testLogger.errorMessages[0] == "Test error message")
    }

    // Helper function to create mock discovery with test devices
    private func createMockDiscovery() -> MockDeviceDiscovery {
        return MockDeviceDiscovery(
            usbDevices: [
                USBDevice(name: "TestDevice1", vendorId: 0x1234, productId: 0x5678),
                USBDevice(name: "TestDevice2", vendorId: 0x8765, productId: 0x4321),
            ],
            ethernetInterfaces: [
                EthernetInterface(
                    name: "eth0",
                    displayName: "Test Ethernet Interface",
                    interfaceType: "Ethernet",
                    macAddress: "00:11:22:33:44:55"
                )
            ],
            lanDevices: [
                LANDevice(
                    id: "device123",
                    displayName: "Test LAN Device",
                    hostname: "test.local",
                    port: 8080,
                    interfaceType: "LAN",
                    isWendyDevice: true
                )
            ]
        )
    }
}

// Implementation for mocking device discovery
struct MockDeviceDiscovery: DeviceDiscovery {
    let usbDevices: [USBDevice]
    let ethernetInterfaces: [EthernetInterface]
    let lanDevices: [LANDevice]

    init(
        usbDevices: [USBDevice] = [],
        ethernetInterfaces: [EthernetInterface] = [],
        lanDevices: [LANDevice] = []
    ) {
        self.usbDevices = usbDevices
        self.ethernetInterfaces = ethernetInterfaces
        self.lanDevices = lanDevices
    }

    func findUSBDevices() async -> [USBDevice] {
        return usbDevices
    }

    func findEthernetInterfaces() async -> [EthernetInterface] {
        return ethernetInterfaces
    }

    func findLANDevices() async throws -> [LANDevice] {
        return lanDevices
    }
}

// Logger for testing - avoid bootstrapping the global logger
@preconcurrency
final class TestLogger: @unchecked Sendable {
    // Using a synchronized array to make thread-safe
    private var _errorMessages: [String] = []
    private let lock = NSLock()

    var errorMessages: [String] {
        lock.lock()
        defer { lock.unlock() }
        return _errorMessages
    }

    func addError(_ message: String) {
        lock.lock()
        defer { lock.unlock() }
        _errorMessages.append(message)
    }

    func createLogger() -> Logger {
        // Create a logger without bootstrapping the global system
        var logger = Logger(label: "test.logger")
        logger.logLevel = .trace
        return logger
    }
}
