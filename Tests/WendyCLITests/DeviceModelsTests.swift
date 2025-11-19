import Foundation
import Testing
import WendyShared

@testable import wendy

@Suite("USB Device Tests")
struct USBDeviceTests {

    @Test("USBDevice initialization and validation")
    func testUSBDeviceInitialization() throws {
        // Test initialization with an Wendy device
        let wendyDevice = USBDevice(name: "Wendy Device", vendorId: 0x1234, productId: 0xABCD)
        #expect(wendyDevice.name == "Wendy Device")
        #expect(wendyDevice.vendorId == "0x1234")
        #expect(wendyDevice.productId == "0xABCD")
        #expect(wendyDevice.isWendyDevice)

        // Test initialization with a non-Wendy device
        let nonWendyDevice = USBDevice(
            name: "Generic USB Device",
            vendorId: 0x5678,
            productId: 0xDEF0
        )
        #expect(nonWendyDevice.name == "Generic USB Device")
        #expect(nonWendyDevice.vendorId == "0x5678")
        #expect(nonWendyDevice.productId == "0xDEF0")
        #expect(!nonWendyDevice.isWendyDevice)
    }

    @Test("Human readable string format")
    func testHumanReadableFormat() throws {
        let device = USBDevice(name: "Wendy Device", vendorId: 0x1234, productId: 0xABCD)
        let humanReadable = device.toHumanReadableString()

        #expect(humanReadable == "Wendy Device")
    }

    @Test("JSON serialization and Codable conformance")
    func testJSONSerializationAndCodable() throws {
        let device = USBDevice(name: "Wendy Device", vendorId: 0x1234, productId: 0xABCD)

        // Test custom toJSON() method
        let jsonString = try device.toJSON()

        // Verify JSON contains all fields with correct values
        #expect(jsonString.contains("\"name\" : \"Wendy Device\""))
        #expect(jsonString.contains("\"vendorId\" : \"0x1234\""))
        #expect(jsonString.contains("\"productId\" : \"0xABCD\""))
        #expect(jsonString.contains("\"isWendyDevice\" : true"))

        // Test Codable conformance
        // Encode to Data
        let encoder = JSONEncoder()
        let data = try encoder.encode(device)

        // Decode back to USBDevice
        let decoder = JSONDecoder()
        let decodedDevice = try decoder.decode(USBDevice.self, from: data)

        // Verify all properties match
        #expect(decodedDevice.name == device.name)
        #expect(decodedDevice.vendorId == device.vendorId)
        #expect(decodedDevice.productId == device.productId)
        #expect(decodedDevice.isWendyDevice == device.isWendyDevice)
    }

    @Test("Device list JSON serialization")
    func testDeviceListJSONSerialization() throws {
        // Create a collection of devices
        let devices = [
            USBDevice(name: "Wendy Device 1", vendorId: 0x1234, productId: 0xABCD),
            USBDevice(name: "Wendy Device 2", vendorId: 0x5678, productId: 0xDEF0),
        ]

        // Serialize the array to JSON
        let encoder = JSONEncoder()
        encoder.outputFormatting = [
            JSONEncoder.OutputFormatting.prettyPrinted, JSONEncoder.OutputFormatting.sortedKeys,
        ]
        let data = try encoder.encode(devices)
        let jsonString = String(data: data, encoding: .utf8)!

        // Verify the array is properly serialized
        #expect(jsonString.contains("\"name\" : \"Wendy Device 1\""))
        #expect(jsonString.contains("\"name\" : \"Wendy Device 2\""))
        #expect(jsonString.contains("\"vendorId\" : \"0x1234\""))
        #expect(jsonString.contains("\"vendorId\" : \"0x5678\""))

        // Verify we can deserialize it back
        let decodedDevices = try JSONDecoder().decode([USBDevice].self, from: data)
        #expect(decodedDevices.count == 2)
        #expect(decodedDevices[0].name == "Wendy Device 1")
        #expect(decodedDevices[1].name == "Wendy Device 2")
    }
}

@Suite("Ethernet Interface Tests")
struct EthernetInterfaceTests {

    @Test("Ethernet interface initialization")
    func testEthernetInterfaceInitialization() throws {
        // Test initialization with an Wendy interface
        let wendyInterface = EthernetInterface(
            name: "wendy0",
            displayName: "Wendy Ethernet",
            macAddress: "11:22:33:44:55:66"
        )

        #expect(wendyInterface.name == "wendy0")
        #expect(wendyInterface.displayName == "Wendy Ethernet")
        #expect(wendyInterface.macAddress == "11:22:33:44:55:66")
        #expect(wendyInterface.isWendyDevice)

        // Test initialization with a non-Wendy interface
        let nonWendyInterface = EthernetInterface(
            name: "en0",
            displayName: "Wi-Fi",
            macAddress: "aa:bb:cc:dd:ee:ff"
        )

        #expect(nonWendyInterface.name == "en0")
        #expect(nonWendyInterface.displayName == "Wi-Fi")
        #expect(nonWendyInterface.macAddress == "aa:bb:cc:dd:ee:ff")
        #expect(!nonWendyInterface.isWendyDevice)
    }

    @Test("Human readable string format for interfaces")
    func testHumanReadableFormat() throws {
        // With MAC address
        let interface1 = EthernetInterface(
            name: "wendy0",
            displayName: "Wendy Ethernet",
            macAddress: "11:22:33:44:55:66"
        )

        let humanReadable1 = interface1.toHumanReadableString()
        #expect(
            humanReadable1 == "Wendy Ethernet @ wendy0 [11:22:33:44:55:66]"
        )

        // Without MAC address
        let interface2 = EthernetInterface(
            name: "wendy1",
            displayName: "Wendy PPP",
            macAddress: nil
        )

        let humanReadable2 = interface2.toHumanReadableString()
        #expect(humanReadable2 == "Wendy PPP @ wendy1")
    }

    @Test("Interface JSON serialization and Codable conformance")
    func testInterfaceJSONSerializationAndCodable() throws {
        let interface = EthernetInterface(
            name: "wendy0",
            displayName: "Wendy Ethernet",
            macAddress: "11:22:33:44:55:66"
        )

        // Test custom toJSON() method
        let jsonString = try interface.toJSON()

        // Verify JSON contains all fields with correct values
        #expect(jsonString.contains("\"name\" : \"wendy0\""))
        #expect(jsonString.contains("\"displayName\" : \"Wendy Ethernet\""))
        #expect(jsonString.contains("\"interfaceType\" : \"Ethernet\""))
        #expect(jsonString.contains("\"macAddress\" : \"11:22:33:44:55:66\""))
        #expect(jsonString.contains("\"isWendyDevice\" : true"))

        // Test Codable conformance
        // Encode to Data
        let encoder = JSONEncoder()
        let data = try encoder.encode(interface)

        // Decode back to EthernetInterface
        let decoder = JSONDecoder()
        let decodedInterface = try decoder.decode(EthernetInterface.self, from: data)

        // Verify all properties match
        #expect(decodedInterface.name == interface.name)
        #expect(decodedInterface.displayName == interface.displayName)
        #expect(decodedInterface.macAddress == interface.macAddress)
        #expect(decodedInterface.isWendyDevice == interface.isWendyDevice)
    }

    @Test("Interface list JSON serialization")
    func testInterfaceListJSONSerialization() throws {
        // Create a collection of interfaces
        let interfaces = [
            EthernetInterface(
                name: "wendy0",
                displayName: "Wendy Ethernet",
                macAddress: "11:22:33:44:55:66"
            ),
            EthernetInterface(
                name: "wendy1",
                displayName: "Wendy Wi-Fi",
                macAddress: "aa:bb:cc:dd:ee:ff"
            ),
        ]

        // Serialize the array to JSON
        let encoder = JSONEncoder()
        encoder.outputFormatting = [
            JSONEncoder.OutputFormatting.prettyPrinted, JSONEncoder.OutputFormatting.sortedKeys,
        ]
        let data = try encoder.encode(interfaces)
        let jsonString = String(data: data, encoding: .utf8)!

        // Verify the array is properly serialized
        #expect(jsonString.contains("\"name\" : \"wendy0\""))
        #expect(jsonString.contains("\"name\" : \"wendy1\""))
        #expect(jsonString.contains("\"displayName\" : \"Wendy Ethernet\""))
        #expect(jsonString.contains("\"displayName\" : \"Wendy Wi-Fi\""))

        // Verify we can deserialize it back
        let decodedInterfaces = try JSONDecoder().decode([EthernetInterface].self, from: data)
        #expect(decodedInterfaces.count == 2)
        #expect(decodedInterfaces[0].name == "wendy0")
        #expect(decodedInterfaces[1].name == "wendy1")
    }
}

@Suite("LAN Device Tests")
struct LANDeviceTests {

    @Test("LANDevice initialization and validation")
    func testLANDeviceInitialization() throws {
        // Test initialization with an Wendy device
        let wendyDevice = LANDevice(
            id: "device123",
            displayName: "Wendy LAN Device",
            hostname: "wendy.local",
            port: 8080,
            interfaceType: "LAN",
            isWendyDevice: true
        )

        #expect(wendyDevice.id == "device123")
        #expect(wendyDevice.displayName == "Wendy LAN Device")
        #expect(wendyDevice.hostname == "wendy.local")
        #expect(wendyDevice.port == 8080)
        #expect(wendyDevice.interfaceType == "LAN")
        #expect(wendyDevice.isWendyDevice)
    }

    @Test("Human readable string format for LAN devices")
    func testHumanReadableFormat() throws {
        let device = LANDevice(
            id: "device123",
            displayName: "Wendy LAN Device",
            hostname: "wendy.local",
            port: 8080,
            interfaceType: "LAN",
            isWendyDevice: true
        )

        let humanReadable = device.toHumanReadableString()
        #expect(humanReadable == "Wendy LAN Device @ wendy.local:8080")
    }

    @Test("LANDevice JSON serialization and Codable conformance")
    func testLANDeviceJSONSerializationAndCodable() throws {
        let device = LANDevice(
            id: "device123",
            displayName: "Wendy LAN Device",
            hostname: "wendy.local",
            port: 8080,
            interfaceType: "LAN",
            isWendyDevice: true
        )

        // Test custom toJSON() method
        let jsonString = try device.toJSON()

        // Verify JSON contains all fields with correct values
        #expect(jsonString.contains("\"id\" : \"device123\""))
        #expect(jsonString.contains("\"displayName\" : \"Wendy LAN Device\""))
        #expect(jsonString.contains("\"hostname\" : \"wendy.local\""))
        #expect(jsonString.contains("\"port\" : 8080"))
        #expect(jsonString.contains("\"interfaceType\" : \"LAN\""))
        #expect(jsonString.contains("\"isWendyDevice\" : true"))

        // Test Codable conformance
        // Encode to Data
        let encoder = JSONEncoder()
        let data = try encoder.encode(device)

        // Decode back to LANDevice
        let decoder = JSONDecoder()
        let decodedDevice = try decoder.decode(LANDevice.self, from: data)

        // Verify all properties match
        #expect(decodedDevice.id == device.id)
        #expect(decodedDevice.displayName == device.displayName)
        #expect(decodedDevice.hostname == device.hostname)
        #expect(decodedDevice.port == device.port)
        #expect(decodedDevice.interfaceType == device.interfaceType)
        #expect(decodedDevice.isWendyDevice == device.isWendyDevice)
    }

    @Test("LAN device list JSON serialization")
    func testLANDeviceListJSONSerialization() throws {
        // Create a collection of LAN devices
        let devices = [
            LANDevice(
                id: "device123",
                displayName: "Wendy LAN Device 1",
                hostname: "wendy1.local",
                port: 8080,
                interfaceType: "LAN",
                isWendyDevice: true
            ),
            LANDevice(
                id: "device456",
                displayName: "Wendy LAN Device 2",
                hostname: "wendy2.local",
                port: 8081,
                interfaceType: "LAN",
                isWendyDevice: true
            ),
        ]

        // Serialize the array to JSON
        let encoder = JSONEncoder()
        encoder.outputFormatting = [
            JSONEncoder.OutputFormatting.prettyPrinted, JSONEncoder.OutputFormatting.sortedKeys,
        ]
        let data = try encoder.encode(devices)
        let jsonString = String(data: data, encoding: .utf8)!

        // Verify the array is properly serialized
        #expect(jsonString.contains("\"id\" : \"device123\""))
        #expect(jsonString.contains("\"id\" : \"device456\""))
        #expect(jsonString.contains("\"displayName\" : \"Wendy LAN Device 1\""))
        #expect(jsonString.contains("\"displayName\" : \"Wendy LAN Device 2\""))

        // Verify we can deserialize it back
        let decodedDevices = try JSONDecoder().decode([LANDevice].self, from: data)
        #expect(decodedDevices.count == 2)
        #expect(decodedDevices[0].id == "device123")
        #expect(decodedDevices[1].id == "device456")
    }
}

@Suite("DeviceFormatter Tests")
struct DeviceFormatterTests {

    @Test("DeviceFormatter formats USB devices correctly")
    func testUSBDeviceFormatting() throws {
        // Create test USB devices
        let devices = [
            USBDevice(name: "Wendy USB 1", vendorId: 0x1234, productId: 0x5678),
            USBDevice(name: "Wendy USB 2", vendorId: 0x9ABC, productId: 0xDEF0),
        ]

        // Test text formatting
        let textOutput = DeviceFormatter.formatCollection(
            devices,
            as: .text,
            collectionName: "USB Devices"
        )
        #expect(textOutput.contains("USB Devices:"))
        #expect(textOutput.contains("Wendy USB 1"))
        #expect(textOutput.contains("Wendy USB 2"))

        // Test JSON formatting
        let jsonOutput = DeviceFormatter.formatCollection(
            devices,
            as: .json,
            collectionName: "USB Devices"
        )
        #expect(jsonOutput.contains("\"name\" : \"Wendy USB 1\""))
        #expect(jsonOutput.contains("\"name\" : \"Wendy USB 2\""))
    }

    @Test("DeviceFormatter formats LAN devices correctly")
    func testLANDeviceFormatting() throws {
        // Create test LAN devices
        let devices = [
            LANDevice(
                id: "device123",
                displayName: "Wendy LAN 1",
                hostname: "wendy1.local",
                port: 8080,
                interfaceType: "LAN",
                isWendyDevice: true
            ),
            LANDevice(
                id: "device456",
                displayName: "Wendy LAN 2",
                hostname: "wendy2.local",
                port: 8081,
                interfaceType: "LAN",
                isWendyDevice: true
            ),
        ]

        // Test text formatting
        let textOutput = DeviceFormatter.formatCollection(
            devices,
            as: .text,
            collectionName: "LAN Interfaces"
        )
        #expect(textOutput.contains("LAN Interfaces:"))
        #expect(textOutput.contains("Wendy LAN 1"))
        #expect(textOutput.contains("Wendy LAN 2"))

        // Test JSON formatting
        let jsonOutput = DeviceFormatter.formatCollection(
            devices,
            as: .json,
            collectionName: "LAN Interfaces"
        )
        #expect(jsonOutput.contains("\"displayName\" : \"Wendy LAN 1\""))
        #expect(jsonOutput.contains("\"displayName\" : \"Wendy LAN 2\""))
    }

    @Test("DeviceFormatter handles empty collections")
    func testEmptyCollectionFormatting() throws {
        // Test empty USB devices
        let emptyUSBDevices: [USBDevice] = []
        let emptyUSBOutput = DeviceFormatter.formatCollection(
            emptyUSBDevices,
            as: .text,
            collectionName: "USB Devices"
        )
        #expect(emptyUSBOutput == "No Wendy USB Devices found.")

        // Test empty Ethernet interfaces
        let emptyEthernetInterfaces: [EthernetInterface] = []
        let emptyEthernetOutput = DeviceFormatter.formatCollection(
            emptyEthernetInterfaces,
            as: .text,
            collectionName: "Ethernet Interfaces"
        )
        #expect(emptyEthernetOutput == "No Wendy Ethernet Interfaces found.")

        // Test empty LAN devices
        let emptyLANDevices: [LANDevice] = []
        let emptyLANOutput = DeviceFormatter.formatCollection(
            emptyLANDevices,
            as: .text,
            collectionName: "LAN Interfaces"
        )
        #expect(emptyLANOutput == "No Wendy LAN Interfaces found.")
    }
}

@Suite("DevicesCollection Tests")
struct DevicesCollectionTests {

    @Test("DevicesCollection initialization and handling heterogeneous devices")
    func testDevicesCollectionInitialization() throws {
        // Create test devices
        let usbDevice = USBDevice(name: "Wendy USB", vendorId: 0x1234, productId: 0x5678)
        let ethernetInterface = EthernetInterface(
            name: "wendy0",
            displayName: "Wendy Ethernet",
            macAddress: "11:22:33:44:55:66"
        )
        let lanDevice = LANDevice(
            id: "device123",
            displayName: "Wendy LAN Device",
            hostname: "wendy.local",
            port: 8080,
            interfaceType: "LAN",
            isWendyDevice: true
        )

        // Test initialization with specific device types
        let collection1 = DevicesCollection(
            usb: [usbDevice],
            ethernet: [ethernetInterface],
            lan: [lanDevice]
        )

        // Test initialization with generic device array
        var devices: [any Device] = []
        devices.append(usbDevice)
        devices.append(ethernetInterface)
        devices.append(lanDevice)
        // The 'devices' constructor was removed, so we use the named parameters instead
        let collection2 = DevicesCollection(
            usb: [usbDevice],
            ethernet: [ethernetInterface],
            lan: [lanDevice]
        )

        // Verify both collections can generate proper JSON
        let json1 = try collection1.toJSON()
        let json2 = try collection2.toJSON()

        // Both collections should contain the same devices
        #expect(json1.contains("\"name\" : \"Wendy USB\""))
        #expect(json1.contains("\"name\" : \"wendy0\""))
        #expect(json1.contains("\"id\" : \"device123\""))
        #expect(json2.contains("\"name\" : \"Wendy USB\""))
        #expect(json2.contains("\"name\" : \"wendy0\""))
        #expect(json2.contains("\"id\" : \"device123\""))

        // Test that the human readable output contains all device types
        let humanReadable = collection1.toHumanReadableString()
        #expect(humanReadable.contains("USB Devices:"))
        #expect(humanReadable.contains("Ethernet Interfaces:"))
        #expect(humanReadable.contains("LAN Devices:"))
        #expect(humanReadable.contains("Wendy USB"))
        #expect(humanReadable.contains("Wendy Ethernet"))
        #expect(humanReadable.contains("Wendy LAN Device"))
    }

    @Test("DevicesCollection handles empty collections properly")
    func testEmptyDevicesCollection() throws {
        // Create an empty collection - use the correct constructor
        let emptyCollection = DevicesCollection()

        // Test JSON output
        let json = try emptyCollection.toJSON()

        // Check for empty arrays with the exact formatting from the output
        #expect(json.contains("\"usbDevices\" : ["))
        #expect(json.contains("\"ethernetDevices\" : ["))
        #expect(json.contains("\"lanDevices\" : ["))

        // Test human readable output
        let humanReadable = emptyCollection.toHumanReadableString()
        #expect(humanReadable == "No devices found.")
    }

    @Test("DevicesCollection properly groups device types")
    func testDeviceTypeGrouping() throws {
        // Create multiple devices of each type
        let usbDevices = [
            USBDevice(name: "Wendy USB 1", vendorId: 0x1234, productId: 0x5678),
            USBDevice(name: "Wendy USB 2", vendorId: 0x9ABC, productId: 0xDEF0),
        ]

        let ethernetInterfaces = [
            EthernetInterface(
                name: "Wendy0",
                displayName: "Wendy Ethernet 1",
                macAddress: "11:22:33:44:55:66"
            ),
            EthernetInterface(
                name: "Wendy1",
                displayName: "Wendy Ethernet 2",
                macAddress: "AA:BB:CC:DD:EE:FF"
            ),
        ]

        let lanDevices = [
            LANDevice(
                id: "device123",
                displayName: "Wendy LAN 1",
                hostname: "wendy1.local",
                port: 8080,
                interfaceType: "LAN",
                isWendyDevice: true
            ),
            LANDevice(
                id: "device456",
                displayName: "Wendy LAN 2",
                hostname: "wendy2.local",
                port: 8081,
                interfaceType: "LAN",
                isWendyDevice: true
            ),
        ]

        // Create a collection with multiple devices
        let collection = DevicesCollection(
            usb: usbDevices,
            ethernet: ethernetInterfaces,
            lan: lanDevices
        )

        // Verify JSON contains all devices properly grouped
        let json = try collection.toJSON()
        #expect(json.contains("\"usbDevices\""))
        #expect(json.contains("\"ethernetDevices\""))
        #expect(json.contains("\"lanDevices\""))
        #expect(json.contains("Wendy USB 1"))
        #expect(json.contains("Wendy USB 2"))
        #expect(json.contains("Wendy Ethernet 1"))
        #expect(json.contains("Wendy Ethernet 2"))
        #expect(json.contains("Wendy LAN 1"))
        #expect(json.contains("Wendy LAN 2"))

        // Verify human readable output contains all devices properly grouped
        let humanReadable = collection.toHumanReadableString()
        #expect(humanReadable.contains("USB Devices:"))
        #expect(humanReadable.contains("Ethernet Interfaces:"))
        #expect(humanReadable.contains("LAN Devices:"))
        #expect(humanReadable.contains("Wendy USB 1"))
        #expect(humanReadable.contains("Wendy USB 2"))
        #expect(humanReadable.contains("Wendy Ethernet 1"))
        #expect(humanReadable.contains("Wendy Ethernet 2"))
        #expect(humanReadable.contains("Wendy LAN 1"))
        #expect(humanReadable.contains("Wendy LAN 2"))
    }
}
