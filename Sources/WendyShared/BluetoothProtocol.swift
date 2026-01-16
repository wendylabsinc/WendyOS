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

/// Commands that can be sent over Bluetooth from CLI to Agent
public enum BluetoothAgentCommand: Codable, Sendable {
    case wifiList
    case wifiConnect(ssid: String, password: String)
    case wifiStatus
    case wifiDisconnect
    case appsList
    case appsStop(appName: String)
    case appsRemove(appName: String, purgeImage: Bool)
    case agentVersion
    case hardwareList

    private enum CodingKeys: String, CodingKey {
        case type
        case ssid
        case password
        case appName
        case purgeImage
    }

    private enum CommandType: String, Codable {
        case wifiList
        case wifiConnect
        case wifiStatus
        case wifiDisconnect
        case appsList
        case appsStop
        case appsRemove
        case agentVersion
        case hardwareList
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        let type = try container.decode(CommandType.self, forKey: .type)

        switch type {
        case .wifiList:
            self = .wifiList
        case .wifiConnect:
            let ssid = try container.decode(String.self, forKey: .ssid)
            let password = try container.decode(String.self, forKey: .password)
            self = .wifiConnect(ssid: ssid, password: password)
        case .wifiStatus:
            self = .wifiStatus
        case .wifiDisconnect:
            self = .wifiDisconnect
        case .appsList:
            self = .appsList
        case .appsStop:
            let appName = try container.decode(String.self, forKey: .appName)
            self = .appsStop(appName: appName)
        case .appsRemove:
            let appName = try container.decode(String.self, forKey: .appName)
            let purgeImage = try container.decodeIfPresent(Bool.self, forKey: .purgeImage) ?? false
            self = .appsRemove(appName: appName, purgeImage: purgeImage)
        case .agentVersion:
            self = .agentVersion
        case .hardwareList:
            self = .hardwareList
        }
    }

    public func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)

        switch self {
        case .wifiList:
            try container.encode(CommandType.wifiList, forKey: .type)
        case .wifiConnect(let ssid, let password):
            try container.encode(CommandType.wifiConnect, forKey: .type)
            try container.encode(ssid, forKey: .ssid)
            try container.encode(password, forKey: .password)
        case .wifiStatus:
            try container.encode(CommandType.wifiStatus, forKey: .type)
        case .wifiDisconnect:
            try container.encode(CommandType.wifiDisconnect, forKey: .type)
        case .appsList:
            try container.encode(CommandType.appsList, forKey: .type)
        case .appsStop(let appName):
            try container.encode(CommandType.appsStop, forKey: .type)
            try container.encode(appName, forKey: .appName)
        case .appsRemove(let appName, let purgeImage):
            try container.encode(CommandType.appsRemove, forKey: .type)
            try container.encode(appName, forKey: .appName)
            try container.encode(purgeImage, forKey: .purgeImage)
        case .agentVersion:
            try container.encode(CommandType.agentVersion, forKey: .type)
        case .hardwareList:
            try container.encode(CommandType.hardwareList, forKey: .type)
        }
    }

    /// Encode the command to JSON data
    public func toData() throws -> Data {
        try JSONEncoder().encode(self)
    }

    /// Decode a command from JSON data
    public static func from(data: Data) throws -> BluetoothAgentCommand {
        try JSONDecoder().decode(BluetoothAgentCommand.self, from: data)
    }
}

/// Response from the agent over Bluetooth
public enum BluetoothResponse: Codable, Sendable {
    case wifiList(networks: [WiFiNetworkInfo])
    case wifiConnect(success: Bool, errorMessage: String?)
    case wifiStatus(connected: Bool, ssid: String?, errorMessage: String?)
    case wifiDisconnect(success: Bool, errorMessage: String?)
    case appsList(apps: [AppInfo])
    case appsStop(success: Bool, errorMessage: String?)
    case appsRemove(success: Bool, errorMessage: String?)
    case agentVersion(version: String)
    case hardwareList(capabilities: [BluetoothHardwareInfo])
    case error(message: String)

    private enum CodingKeys: String, CodingKey {
        case type
        case networks
        case success
        case errorMessage
        case connected
        case ssid
        case apps
        case version
        case capabilities
        case message
    }

    private enum ResponseType: String, Codable {
        case wifiList
        case wifiConnect
        case wifiStatus
        case wifiDisconnect
        case appsList
        case appsStop
        case appsRemove
        case agentVersion
        case hardwareList
        case error
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        let type = try container.decode(ResponseType.self, forKey: .type)

        switch type {
        case .wifiList:
            let networks = try container.decode([WiFiNetworkInfo].self, forKey: .networks)
            self = .wifiList(networks: networks)
        case .wifiConnect:
            let success = try container.decode(Bool.self, forKey: .success)
            let errorMessage = try container.decodeIfPresent(String.self, forKey: .errorMessage)
            self = .wifiConnect(success: success, errorMessage: errorMessage)
        case .wifiStatus:
            let connected = try container.decode(Bool.self, forKey: .connected)
            let ssid = try container.decodeIfPresent(String.self, forKey: .ssid)
            let errorMessage = try container.decodeIfPresent(String.self, forKey: .errorMessage)
            self = .wifiStatus(connected: connected, ssid: ssid, errorMessage: errorMessage)
        case .wifiDisconnect:
            let success = try container.decode(Bool.self, forKey: .success)
            let errorMessage = try container.decodeIfPresent(String.self, forKey: .errorMessage)
            self = .wifiDisconnect(success: success, errorMessage: errorMessage)
        case .appsList:
            let apps = try container.decode([AppInfo].self, forKey: .apps)
            self = .appsList(apps: apps)
        case .appsStop:
            let success = try container.decode(Bool.self, forKey: .success)
            let errorMessage = try container.decodeIfPresent(String.self, forKey: .errorMessage)
            self = .appsStop(success: success, errorMessage: errorMessage)
        case .appsRemove:
            let success = try container.decode(Bool.self, forKey: .success)
            let errorMessage = try container.decodeIfPresent(String.self, forKey: .errorMessage)
            self = .appsRemove(success: success, errorMessage: errorMessage)
        case .agentVersion:
            let version = try container.decode(String.self, forKey: .version)
            self = .agentVersion(version: version)
        case .hardwareList:
            let capabilities = try container.decode([BluetoothHardwareInfo].self, forKey: .capabilities)
            self = .hardwareList(capabilities: capabilities)
        case .error:
            let message = try container.decode(String.self, forKey: .message)
            self = .error(message: message)
        }
    }

    public func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)

        switch self {
        case .wifiList(let networks):
            try container.encode(ResponseType.wifiList, forKey: .type)
            try container.encode(networks, forKey: .networks)
        case .wifiConnect(let success, let errorMessage):
            try container.encode(ResponseType.wifiConnect, forKey: .type)
            try container.encode(success, forKey: .success)
            try container.encodeIfPresent(errorMessage, forKey: .errorMessage)
        case .wifiStatus(let connected, let ssid, let errorMessage):
            try container.encode(ResponseType.wifiStatus, forKey: .type)
            try container.encode(connected, forKey: .connected)
            try container.encodeIfPresent(ssid, forKey: .ssid)
            try container.encodeIfPresent(errorMessage, forKey: .errorMessage)
        case .wifiDisconnect(let success, let errorMessage):
            try container.encode(ResponseType.wifiDisconnect, forKey: .type)
            try container.encode(success, forKey: .success)
            try container.encodeIfPresent(errorMessage, forKey: .errorMessage)
        case .appsList(let apps):
            try container.encode(ResponseType.appsList, forKey: .type)
            try container.encode(apps, forKey: .apps)
        case .appsStop(let success, let errorMessage):
            try container.encode(ResponseType.appsStop, forKey: .type)
            try container.encode(success, forKey: .success)
            try container.encodeIfPresent(errorMessage, forKey: .errorMessage)
        case .appsRemove(let success, let errorMessage):
            try container.encode(ResponseType.appsRemove, forKey: .type)
            try container.encode(success, forKey: .success)
            try container.encodeIfPresent(errorMessage, forKey: .errorMessage)
        case .agentVersion(let version):
            try container.encode(ResponseType.agentVersion, forKey: .type)
            try container.encode(version, forKey: .version)
        case .hardwareList(let capabilities):
            try container.encode(ResponseType.hardwareList, forKey: .type)
            try container.encode(capabilities, forKey: .capabilities)
        case .error(let message):
            try container.encode(ResponseType.error, forKey: .type)
            try container.encode(message, forKey: .message)
        }
    }

    /// Encode the response to JSON data
    public func toData() throws -> Data {
        try JSONEncoder().encode(self)
    }

    /// Decode a response from JSON data
    public static func from(data: Data) throws -> BluetoothResponse {
        try JSONDecoder().decode(BluetoothResponse.self, from: data)
    }
}

/// WiFi network information for Bluetooth responses
public struct WiFiNetworkInfo: Codable, Sendable, Equatable, CustomStringConvertible {
    public let ssid: String
    public let signalStrength: Int?

    public init(ssid: String, signalStrength: Int?) {
        self.ssid = ssid
        self.signalStrength = signalStrength
    }

    public var description: String {
        if let signal = signalStrength {
            return "\(ssid) (Signal: \(signal))"
        }
        return ssid
    }
}

/// Application information for Bluetooth responses
public struct AppInfo: Codable, Sendable {
    public let appName: String
    public let appVersion: String
    public let state: String
    public let failureCount: Int

    public init(appName: String, appVersion: String, state: String, failureCount: Int) {
        self.appName = appName
        self.appVersion = appVersion
        self.state = state
        self.failureCount = failureCount
    }
}

/// Hardware capability information for Bluetooth responses
public struct BluetoothHardwareInfo: Codable, Sendable {
    public let type: String
    public let name: String
    public let available: Bool

    public init(type: String, name: String, available: Bool) {
        self.type = type
        self.name = name
        self.available = available
    }
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
