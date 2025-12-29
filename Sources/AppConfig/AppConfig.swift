import ArgumentParser

public struct AppConfig: Codable {
    public let appId: String
    public let version: String
    public var language: String?
    public var entitlements: [Entitlement]
    public var python: PythonConfig?

    public struct PythonConfig: Codable, Sendable, Hashable {
        public struct PythonContainerConfig: Codable, Sendable, Hashable {
            public var sourceRoot: String
        }

        public var container: PythonContainerConfig?

        public init(sourceRoot: String) {
            self.container = .init(sourceRoot: sourceRoot)
        }
    }

    public init(
        appId: String,
        version: String,
        language: String? = nil,
        entitlements: [Entitlement]
    ) {
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
    /// Video entitlement modes for V4L2 device access.
    public enum VideoMode: String, Codable, Sendable, Hashable, CaseIterable,
        CustomStringConvertible
    {
        /// Bind and allow all detected V4L2 device nodes.
        case all

        /// Bind and allow only the explicit device list.
        case allowlist

        public var description: String {
            switch self {
            case .all:
                return "All"
            case .allowlist:
                return "Allowlist"
            }
        }
    }

    public var mode: VideoMode
    public var allowlist: [String]

    /// Defaults to `.all` mode and a single `/dev/video0` whitelist entry.
    public init(mode: VideoMode = .all, allowlist: [String] = []) {
        self.mode = mode
        self.allowlist = allowlist
    }

    private enum CodingKeys: String, CodingKey {
        case mode
        case allowlist
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        self.mode = try container.decodeIfPresent(VideoMode.self, forKey: .mode) ?? .all
        self.allowlist =
            try container.decodeIfPresent([String].self, forKey: .allowlist) ?? []
    }

    public func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode(mode, forKey: .mode)
        try container.encode(allowlist, forKey: .allowlist)
    }
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
