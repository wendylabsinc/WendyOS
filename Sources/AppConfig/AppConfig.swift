import ArgumentParser

public struct AppConfig: Codable {
    public let appId: String
    public let version: String
    public var language: String?
    public let entitlements: [Entitlement]

    public init(appId: String, version: String, language: String? = nil, entitlements: [Entitlement]) {
        self.appId = appId
        self.version = version
        self.language = language
        self.entitlements = entitlements
    }
}

public enum Entitlement: Codable, Sendable, Hashable {
    case network(NetworkEntitlements)
    case bluetooth(BluetoothEntitlements)
    case video(VideoEntitlements)
    case gpu(GPUEntitlements)
    case audio

    public func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)

        switch self {
        case .network(let entitlement):
            try container.encode(EntitlementType.network, forKey: .type)
            try entitlement.encode(to: encoder)
        case .video(let entitlement):
            try container.encode(EntitlementType.video, forKey: .type)
            try entitlement.encode(to: encoder)
        case .audio:
            try container.encode(EntitlementType.audio, forKey: .type)
        case .bluetooth(let entitlement):
            try container.encode(EntitlementType.bluetooth, forKey: .type)
            try entitlement.encode(to: encoder)
        case .gpu(let entitlement):
            try container.encode(EntitlementType.gpu, forKey: .type)
            try entitlement.encode(to: encoder)
        }
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        let type = try container.decode(EntitlementType.self, forKey: .type)

        switch type {
        case .network:
            self = .network(try NetworkEntitlements(from: decoder))
        case .video:
            self = .video(try VideoEntitlements(from: decoder))
        case .bluetooth:
            self = .bluetooth(try BluetoothEntitlements(from: decoder))
        case .gpu:
            self = .gpu(try GPUEntitlements(from: decoder))
        case .audio:
            self = .audio
        }
    }

    private enum CodingKeys: String, CodingKey {
        case type
    }
}

public enum EntitlementType: String, Codable, CaseIterable, ExpressibleByArgument, Sendable {
    case network
    case video
    case audio
    case bluetooth
    case gpu
}

public struct BluetoothEntitlements: Codable, Sendable, Hashable {
    public enum BluetoothMode: String, Codable, Sendable, Hashable {
        case bluez, kernel
    }

    public let mode: BluetoothMode

    public init(mode: BluetoothMode) {
        self.mode = mode
    }
}

public struct GPUEntitlements: Codable, Sendable, Hashable {
    public init() {}
}

public struct VideoEntitlements: Codable, Sendable, Hashable {
    public init() {}
}

public struct AudioEntitlements: Codable, Sendable, Hashable {
    public init() {}
}

public struct NetworkEntitlements: Codable, Sendable, Hashable {
    public let mode: NetworkMode

    public init(mode: NetworkMode) {
        self.mode = mode
    }
}

public enum NetworkMode: String, Codable, Sendable, Hashable {
    case host
    case none
}
