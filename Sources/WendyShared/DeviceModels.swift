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
        for device in usbDevices {
            names.insert(normalizeDeviceName(device.displayName))
        }
        for device in ethernetDevices {
            names.insert(normalizeDeviceName(device.displayName))
        }
        for device in lanDevices {
            names.insert(normalizeDeviceName(device.displayName))
        }
        return names
    }

    public struct GroupedDevice: Sendable, Hashable, CustomStringConvertible {
        public let name: String
        public let interfaces: [InterfaceInfo]

        public var description: String {
            return "\(name) [\(interfaces.map(\.type).joined(separator: ", "))]"
        }

        public init(name: String, interfaces: [InterfaceInfo]) {
            self.name = name
            self.interfaces = interfaces
        }
    }

    /// Groups all devices by their normalized name
    public func groupedDevices() -> [GroupedDevice] {
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
            var group =
                deviceGroups[normalizedName] ?? (displayName: device.displayName, interfaces: [])
            group.displayName = betterDisplayName(group.displayName, device.displayName)

            group.interfaces.append(.usb(device))
            deviceGroups[normalizedName] = group
        }

        // Group Ethernet devices
        for device in ethernetDevices {
            let normalizedName = normalizeDeviceName(device.displayName)
            var group =
                deviceGroups[normalizedName] ?? (displayName: device.displayName, interfaces: [])
            group.displayName = betterDisplayName(group.displayName, device.displayName)

            group.interfaces.append(.ethernet(device))
            deviceGroups[normalizedName] = group
        }

        // Group LAN devices
        for device in lanDevices {
            let normalizedName = normalizeDeviceName(device.displayName)
            var group =
                deviceGroups[normalizedName] ?? (displayName: device.displayName, interfaces: [])
            group.displayName = betterDisplayName(group.displayName, device.displayName)
            group.interfaces.append(.lan(device))
            deviceGroups[normalizedName] = group
        }

        // Sort by display name for consistent output
        return deviceGroups.map { GroupedDevice(name: $1.displayName, interfaces: $1.interfaces) }.sorted { $0.name < $1.name }
    }

    /// Information about a specific interface for a device
    public enum InterfaceInfo: Sendable, Hashable, CustomStringConvertible {
        case usb(USBDevice)
        case ethernet(EthernetInterface)
        case lan(LANDevice)

        public var type: String {
            switch self {
            case .usb: return "USB"
            case .ethernet: return "Ethernet"
            case .lan: return "LAN"
            }
        }

        public var description: String {
            var string = ""
            switch self {
            case .usb(let device):
                if let usbVersion = device.usbVersion {
                    string += "\(usbVersion):"
                } else {
                    string += "USB:"
                }
                string += " VID: \(device.vendorId), PID: \(device.productId)"
                if let serialNumber = device.serialNumber {
                    string += ", S/N: \(serialNumber)"
                }
                if let maxPowerMilliamps = device.maxPowerMilliamps {
                    string += " (Max Power: \(maxPowerMilliamps)mA)"
                }
                return string
            case .ethernet(let device):
                string += "Ethernet: \(device.name)"

                if let speed = device.linkSpeed {
                    string += ", \(speed)"
                }
                return string
            case .lan(let device):
                string += "LAN: \(device.hostname):\(device.port)"
                return string
            }
        }

        public var identifier: String {
            switch self {
            case .usb(let device): return device.displayName
            case .ethernet(let device): return device.displayName
            case .lan(let device): return device.displayName
            }
        }

        public var agentVersion: String? {
            switch self {
            case .usb(let device): return device.agentVersion
            case .ethernet(let device): return device.agentVersion
            case .lan(let device): return device.agentVersion
            }
        }
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

        for device in grouped {
            if !result.isEmpty {
                result += "\n"
            }

            // Get the first available agent version (should be the same across interfaces)
            let agentVersion = device.interfaces.first(where: { $0.agentVersion != nil })?.agentVersion

            // Add device name with agent version if available
            result += "\n\(device.name)"
            if let version = agentVersion {
                result += " (version: \(version))"
            }

            // List all interfaces for this device
            for interface in device.interfaces {
                result += "\n   \(interface.description)"
            }
        }

        return result
    }
}

public struct LANDevice: Device, Encodable, Sendable, CustomStringConvertible {
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
        let version = agentVersion.map { "v\($0)" }
        let metadata = [version].compactMap { $0 }.joined(separator: " ")
        return "\(displayName) @ \(hostname):\(port) \(metadata)".trimmingCharacters(
            in: .whitespacesAndNewlines
        )
    }

    public var description: String { 
        displayName
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
    public let macAddress: String?
    public let linkSpeed: String?
    public let isWendyDevice: Bool
    public var agentVersion: String?

    public init(
        name: String,
        displayName: String,
        interfaceType: String,
        macAddress: String?,
        linkSpeed: String? = nil
    ) {
        self.name = name
        self.displayName = displayName
        self.macAddress = macAddress
        self.linkSpeed = linkSpeed
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
        let version = agentVersion.map { " v\($0)" }
        let mac = macAddress.map { "[\($0)]" }
        let speed = linkSpeed.map { "[\($0)]" }
        let metadata = [version, mac, speed].compactMap { $0 }.joined(separator: " ")
        return "\(displayName) @ \(name) \(metadata)".trimmingCharacters(
            in: .whitespacesAndNewlines
        )
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
    public let usbVersion: String?
    public let serialNumber: String?
    public let maxPowerMilliamps: Int?
    public let isWendyDevice: Bool
    public var agentVersion: String?

    public init(
        name: String,
        vendorId: Int,
        productId: Int,
        usbVersion: String? = nil,
        serialNumber: String? = nil,
        maxPowerMilliamps: Int? = nil
    ) {
        self.name = name
        self.displayName = name
        self.vendorId = String(format: "0x%04X", vendorId)
        self.productId = String(format: "0x%04X", productId)
        self.usbVersion = usbVersion
        self.serialNumber = serialNumber
        self.maxPowerMilliamps = maxPowerMilliamps
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
        let version = agentVersion.map { "v\($0)" }
        let metadata = [version].compactMap { $0 }.joined(separator: " ")
        return "\(name) \(metadata)".trimmingCharacters(in: .whitespacesAndNewlines)
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
