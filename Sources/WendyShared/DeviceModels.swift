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

    /// The number of unique devices (counting each unique device name once)
    public var deviceCount: Int {
        return uniqueDeviceNames.count
    }

    /// Normalize device name for grouping similar devices
    private func normalizeDeviceName(_ name: String) -> String {
        // Convert to lowercase, remove common prefixes, and normalize separators
        var normalized = name.lowercased()

        // Remove common prefixes
        let prefixes = ["wendyos device", "wendy device", "device"]
        for prefix in prefixes {
            if normalized.hasPrefix(prefix + " ") {
                normalized = String(normalized.dropFirst(prefix.count + 1))
            }
        }

        // Normalize all separators (spaces, underscores, hyphens) to hyphens
        normalized = normalized.replacingOccurrences(of: "_", with: "-")
        normalized = normalized.replacingOccurrences(of: " ", with: "-")

        // Remove any duplicate hyphens that might result
        while normalized.contains("--") {
            normalized = normalized.replacingOccurrences(of: "--", with: "-")
        }

        // Remove extra whitespace and trim hyphens from ends
        normalized = normalized.trimmingCharacters(in: CharacterSet(charactersIn: "- "))

        return normalized
    }

    /// Get unique device names across all interfaces (using normalized names)
    private var uniqueDeviceNames: Set<String> {
        var names = Set<String>()
        usbDevices.forEach { names.insert(normalizeDeviceName($0.displayName)) }
        ethernetDevices.forEach { names.insert(normalizeDeviceName($0.displayName)) }
        lanDevices.forEach { names.insert(normalizeDeviceName($0.displayName)) }
        return names
    }

    /// Groups all devices by their normalized name
    private func groupedDevices() -> [(name: String, interfaces: [InterfaceInfo])] {
        // Use normalized names as keys, but track the best display name
        var deviceGroups: [String: (displayName: String, interfaces: [InterfaceInfo])] = [:]

        // Helper to choose the best display name (prefer shorter, cleaner names)
        func betterDisplayName(_ name1: String, _ name2: String) -> String {
            // Prefer names without "WendyOS Device" prefix
            if name1.hasPrefix("WendyOS Device") && !name2.hasPrefix("WendyOS Device") {
                return name2
            }
            if !name1.hasPrefix("WendyOS Device") && name2.hasPrefix("WendyOS Device") {
                return name1
            }
            // Otherwise prefer shorter names or the first one
            return name1.count <= name2.count ? name1 : name2
        }

        // Group USB devices
        for device in usbDevices {
            let normalizedName = normalizeDeviceName(device.displayName)
            var group = deviceGroups[normalizedName] ?? (displayName: device.displayName, interfaces: [])
            group.displayName = betterDisplayName(group.displayName, device.displayName)
            group.interfaces.append(InterfaceInfo(
                type: "USB",
                identifier: "Vendor: \(device.vendorId), Product: \(device.productId)",
                agentVersion: device.agentVersion
            ))
            deviceGroups[normalizedName] = group
        }

        // Group Ethernet devices
        for device in ethernetDevices {
            let normalizedName = normalizeDeviceName(device.displayName)
            var group = deviceGroups[normalizedName] ?? (displayName: device.displayName, interfaces: [])
            group.displayName = betterDisplayName(group.displayName, device.displayName)
            var identifier = "\(device.name)"
            if let mac = device.macAddress {
                identifier += " (MAC: \(mac))"
            }
            group.interfaces.append(InterfaceInfo(
                type: "Ethernet",
                identifier: identifier,
                agentVersion: device.agentVersion
            ))
            deviceGroups[normalizedName] = group
        }

        // Group LAN devices
        for device in lanDevices {
            let normalizedName = normalizeDeviceName(device.displayName)
            var group = deviceGroups[normalizedName] ?? (displayName: device.displayName, interfaces: [])
            group.displayName = betterDisplayName(group.displayName, device.displayName)
            group.interfaces.append(InterfaceInfo(
                type: "LAN",
                identifier: "\(device.hostname):\(device.port)",
                agentVersion: device.agentVersion
            ))
            deviceGroups[normalizedName] = group
        }

        // Sort by display name for consistent output
        return deviceGroups.map { ($1.displayName, $1.interfaces) }.sorted { $0.0 < $1.0 }
    }
    /// Information about a specific interface for a device
    private struct InterfaceInfo {
        let type: String
        let identifier: String
        let agentVersion: String?
    }

    public func toJSON() throws -> String {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        let data = try encoder.encode(self)
        return String(data: data, encoding: .utf8) ?? "{}"
    }

    public func toHumanReadableString() -> String {
        let grouped = groupedDevices()

        if grouped.isEmpty {
            return "No devices found."
        }

        var result = ""

        for (deviceName, interfaces) in grouped {
            if !result.isEmpty {
                result += "\n"
            }

            // Get the first available agent version (should be the same across interfaces)
            let agentVersion = interfaces.first(where: { $0.agentVersion != nil })?.agentVersion

            // Add device name with agent version if available
            result += "\n\(deviceName)"
            if let version = agentVersion {
                result += " (Agent: \(version))"
            }

            // List all interfaces for this device
            for interface in interfaces {
                result += "\n   \(interface.type): \(interface.identifier)"
            }
        }

        return result
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
