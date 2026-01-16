import Foundation

/// UUIDs for the WendyOS Bluetooth service
public enum WendyBluetoothUUIDs {
    /// The main service UUID for WendyOS devices
    public static let serviceUUID = "E7A90001-1234-5678-90AB-CDEF01234567"

    /// Characteristic for sending commands to the agent
    public static let commandCharacteristicUUID = "E7A90002-1234-5678-90AB-CDEF01234567"

    /// Characteristic for receiving responses from the agent
    public static let responseCharacteristicUUID = "E7A90003-1234-5678-90AB-CDEF01234567"

    /// L2CAP PSM for bidirectional communication
    /// BlueZ typically assigns PSM 128 (0x80) for unprivileged L2CAP channels
    public static let l2capPSM: UInt16 = 128
}

/// Bluetooth device model for discovery
public struct BluetoothDevice: Device, Encodable, Sendable {
    public let id: String
    public let displayName: String
    public let address: String
    public let rssi: Int
    public let isWendyDevice: Bool
    public var agentVersion: String?
    /// The L2CAP PSM advertised by the device for connection
    public var l2capPSM: UInt16?

    public init(
        id: String,
        displayName: String,
        address: String,
        rssi: Int,
        isWendyDevice: Bool = true,
        agentVersion: String? = nil,
        l2capPSM: UInt16? = nil
    ) {
        self.id = id
        self.displayName = displayName
        self.address = address
        self.rssi = rssi
        self.isWendyDevice = isWendyDevice
        self.agentVersion = agentVersion
        self.l2capPSM = l2capPSM
    }

    public func toHumanReadableString() -> String {
        let version = agentVersion.map { "v\($0)" } ?? ""
        return "\(displayName) [\(address)] RSSI: \(rssi) \(version)".trimmingCharacters(
            in: .whitespacesAndNewlines
        )
    }
}
