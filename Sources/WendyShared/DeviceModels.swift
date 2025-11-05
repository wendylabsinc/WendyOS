import Foundation

public enum OutputFormat {
    case text
    case json
}

// Add to DeviceModels.swift or create a separate file like Device.swift in the domain folder
public protocol Device: Codable, Hashable {
    var isWendyDevice: Bool { get }
    func toHumanReadableString() -> String
}

// Add a protocol extension for common functionality
extension Device {
    public static func formatEmpty(type: String) -> String {
        return "No Wendy \(type) found."
    }
}

public struct DevicesCollection: Encodable, Sendable {
    public var usbDevices: [USBDevice]
    public var ethernetDevices: [EthernetInterface]
    public var lanDevices: [LANDevice]

    public init(usb: [USBDevice] = [], ethernet: [EthernetInterface] = [], lan: [LANDevice] = []) {
        self.usbDevices = usb
        self.ethernetDevices = ethernet
        self.lanDevices = lan
    }

    /// Whether the collection contains no devices
    public var isEmpty: Bool {
        return usbDevices.isEmpty && ethernetDevices.isEmpty && lanDevices.isEmpty
    }

    /// The number of devices in the collection
    public var deviceCount: Int {
        return usbDevices.count + ethernetDevices.count + lanDevices.count
    }

    public func toJSON() throws -> String {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        let data = try encoder.encode(self)
        return String(data: data, encoding: .utf8) ?? "{}"
    }

    public func toHumanReadableString() -> String {
        var result = ""

        // Add USB devices section
        if !usbDevices.isEmpty {
            result += "\nUSB Devices:"
            for device in usbDevices {
                result += "\n" + device.toHumanReadableString()
            }
        }

        // Add Ethernet devices section
        if !ethernetDevices.isEmpty {
            if !result.isEmpty {
                result += "\n"
            }
            result += "\nEthernet Interfaces:"
            for device in ethernetDevices {
                result += "\n" + device.toHumanReadableString()
            }
        }

        // Add LAN devices section
        if !lanDevices.isEmpty {
            if !result.isEmpty {
                result += "\n"
            }
            result += "\nLAN Devices:"
            for device in lanDevices {
                result += "\n" + device.toHumanReadableString()
            }
        }

        return result.isEmpty ? "No devices found." : result
    }
}

public struct LANDevice: Device, Encodable, Sendable {
    public let id: String
    public let displayName: String
    public let hostname: String
    public let port: Int
    public let interfaceType: String
    public let isWendyDevice: Bool
    public var agentVersion: String?

    public init(
        id: String,
        displayName: String,
        hostname: String,
        port: Int,
        interfaceType: String,
        isWendyDevice: Bool,
        agentVersion: String? = nil
    ) {
        self.id = id
        self.displayName = displayName
        self.hostname = hostname
        self.port = port
        self.interfaceType = interfaceType
        self.isWendyDevice = isWendyDevice
        self.agentVersion = agentVersion
    }

    public func toJSON() throws -> String {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        let data = try encoder.encode(self)
        return String(data: data, encoding: .utf8) ?? "{}"
    }

    public func toHumanReadableString() -> String {
        return
            "\(displayName)\(agentVersion.map { " (\($0))" } ?? "") @ \(hostname):\(port) [\(id)]"
    }

    public static func formatCollection(
        _ interfaces: [LANDevice],
        as format: OutputFormat
    ) -> String {
        return DeviceFormatter.formatCollection(
            interfaces,
            as: format,
            collectionName: "LAN Interfaces"
        )
    }
}

public struct EthernetInterface: Device, Encodable, Sendable {
    public let name: String
    public let displayName: String
    public let interfaceType: String
    public let macAddress: String?
    public let isWendyDevice: Bool
    public var agentVersion: String?

    public init(name: String, displayName: String, interfaceType: String, macAddress: String?) {
        self.name = name
        self.displayName = displayName
        self.interfaceType = interfaceType
        self.macAddress = macAddress
        self.isWendyDevice = displayName.contains("Wendy") || name.contains("Wendy")
        self.agentVersion = nil
    }

    public func toJSON() throws -> String {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        let data = try encoder.encode(self)
        return String(data: data, encoding: .utf8) ?? "{}"
    }

    public func toHumanReadableString() -> String {
        var result = "- \(displayName) (\(name)) [\(interfaceType)]"
        if let mac = macAddress {
            result += "\n  MAC Address: \(mac)"
        }
        return result
    }

    public static func formatCollection(
        _ interfaces: [EthernetInterface],
        as format: OutputFormat
    ) -> String {
        return DeviceFormatter.formatCollection(
            interfaces,
            as: format,
            collectionName: "Ethernet Interfaces"
        )
    }
}

public struct USBDevice: Device, Encodable, Sendable {
    public let name: String
    public let displayName: String
    public let vendorId: String
    public let productId: String
    public let isWendyDevice: Bool
    public var agentVersion: String?

    public init(name: String, vendorId: Int, productId: Int) {
        self.name = name
        self.displayName = name
        self.vendorId = String(format: "0x%04X", vendorId)
        self.productId = String(format: "0x%04X", productId)
        self.isWendyDevice = name.contains("Wendy")
        self.agentVersion = nil
    }

    public func toJSON() throws -> String {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        let data = try encoder.encode(self)
        return String(data: data, encoding: .utf8) ?? "{}"
    }

    public func toHumanReadableString() -> String {
        return
            "\(name)\(agentVersion.map { " (\($0))" } ?? "") - Vendor ID: \(vendorId), Product ID: \(productId)"
    }

    public static func formatCollection(_ devices: [USBDevice], as format: OutputFormat) -> String {
        return DeviceFormatter.formatCollection(devices, as: format, collectionName: "USB Devices")
    }
}

public struct DeviceFormatter {
    public static func formatCollection<T: Device>(
        _ devices: [T],
        as format: OutputFormat,
        collectionName: String
    ) -> String {
        switch format {
        case .text:
            if devices.isEmpty {
                return "No Wendy \(collectionName) found."
            }

            var result = "\n\(collectionName):"
            for device in devices {
                result += "\n" + device.toHumanReadableString()
            }
            return result

        case .json:
            do {
                let encoder = JSONEncoder()
                encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
                let data = try encoder.encode(devices)
                return String(data: data, encoding: .utf8) ?? "{}"
            } catch {
                return "Error serializing to JSON"
            }
        }
    }
}
