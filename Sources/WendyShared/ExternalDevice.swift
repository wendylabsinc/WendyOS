import Foundation

/// A device discovered by a DeviceProvider plugin (e.g. Android via ADB, ESP32, AWS).
/// Lives in WendyShared so GroupedDevice and InterfaceInfo can reference it.
public struct ExternalDevice: Device, Encodable, Sendable, Hashable {
    public let id: String              // e.g. "adb:HVA12345"
    public let displayName: String     // e.g. "Pixel 4"
    public let providerKey: String     // e.g. "android"
    public let connectionInfo: [String: String]
    public let isWendyDevice: Bool
    public var agentVersion: String?
    public var os: String?
    public var osVersion: String?
    public var cpuArchitecture: String?

    public init(
        id: String,
        displayName: String,
        providerKey: String,
        connectionInfo: [String: String] = [:],
        isWendyDevice: Bool = false,
        agentVersion: String? = nil,
        os: String? = nil,
        osVersion: String? = nil,
        cpuArchitecture: String? = nil
    ) {
        self.id = id
        self.displayName = displayName
        self.providerKey = providerKey
        self.connectionInfo = connectionInfo
        self.isWendyDevice = isWendyDevice
        self.agentVersion = agentVersion
        self.os = os
        self.osVersion = osVersion
        self.cpuArchitecture = cpuArchitecture
    }

    public func toHumanReadableString() -> String {
        var result = "\(displayName) [\(providerKey)]"
        if let os {
            result += " \(os)"
            if let osVersion {
                result += " \(osVersion)"
            }
        }
        if let cpuArchitecture {
            result += " (\(cpuArchitecture))"
        }
        return result
    }
}
