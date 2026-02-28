import ArgumentParser
import Foundation

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

    /// Validates wendy.json data and returns warnings for unknown keys in entitlements.
    /// Call this after decoding to check for potential typos or invalid configuration.
    public static func validateJSON(_ data: Data) -> [String] {
        var warnings: [String] = []

        guard let json = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
            let entitlements = json["entitlements"] as? [[String: Any]]
        else {
            return warnings
        }

        for (index, entitlement) in entitlements.enumerated() {
            guard let typeString = entitlement["type"] as? String,
                let type = EntitlementType(rawValue: typeString)
            else {
                continue
            }

            let presentKeys = Set(entitlement.keys)
            let allowedKeys = Entitlement.allowedKeys(for: type)
            let unknownKeys = presentKeys.subtracting(allowedKeys)

            if !unknownKeys.isEmpty {
                let sortedUnknown = unknownKeys.sorted()
                let sortedAllowed = allowedKeys.sorted()
                warnings.append(
                    "Unknown key(s) in entitlement[\(index)] (\(type)): \(sortedUnknown.joined(separator: ", ")). "
                        + "Allowed keys are: \(sortedAllowed.joined(separator: ", "))"
                )
            }
        }

        return warnings
    }
}

public enum Entitlement: Codable, Sendable, Hashable {
    case network(NetworkEntitlements)
    case bluetooth(BluetoothEntitlements)
    case video(VideoEntitlements)
    case gpu(GPUEntitlements)
    case persist(PersistenceEntitlements)
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
        case .persist(let entitlement):
            try container.encode(EntitlementType.persist, forKey: .type)
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
        case .persist:
            self = .persist(try PersistenceEntitlements(from: decoder))
        }
    }

    private enum CodingKeys: String, CodingKey {
        case type
    }

    /// Returns the set of allowed keys for this entitlement type
    public static func allowedKeys(for type: EntitlementType) -> Set<String> {
        switch type {
        case .network:
            return ["type", "mode"]
        case .video:
            return ["type", "mode", "allowlist"]
        case .bluetooth:
            return ["type", "mode"]
        case .gpu:
            return ["type"]
        case .audio:
            return ["type"]
        case .persist:
            return ["type", "name", "path"]
        }
    }
}

public enum EntitlementType: String, Codable, CaseIterable, ExpressibleByArgument, Sendable {
    case network
    case video
    case audio
    case bluetooth
    case gpu
    case persist
}

public struct PersistenceEntitlements: Codable, Sendable, Hashable {
    /// The name of the volume to persist
    public let name: String

    /// The path inside the container to mount the persisted volume at
    public let path: String

    public init(name: String, path: String) {
        self.name = name
        self.path = path
    }
}

public struct BluetoothEntitlements: Codable, Sendable, Hashable {
    @available(*, deprecated, message: "BluetoothMode is no longer used. Bluetooth is now a yes/no entitlement.")
    public enum BluetoothMode: String, Codable, Sendable, Hashable {
        case bluez, kernel
    }

    /// Deprecated: mode is ignored. Kept for backward compatibility with existing wendy.json files.
    public let mode: BluetoothMode?

    public init(mode: BluetoothMode? = nil) {
        self.mode = mode
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        self.mode = try container.decodeIfPresent(BluetoothMode.self, forKey: .mode)
    }

    public func encode(to encoder: Encoder) throws {
        // Only encode "type" (handled by Entitlement.encode) — mode is deprecated
    }

    private enum CodingKeys: String, CodingKey {
        case mode
    }
}

public struct GPUEntitlements: Codable, Sendable, Hashable {
    public init() {}
}

public struct VideoEntitlements: Codable, Sendable, Hashable {
    /// Deprecated: Video mode is no longer used. Video is now a yes/no entitlement.
    @available(*, deprecated, message: "VideoMode is no longer used. Video is now a yes/no entitlement.")
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

    /// Deprecated: mode is ignored. Kept for backward compatibility with existing wendy.json files.
    public var mode: VideoMode?

    /// Deprecated: allowlist is ignored. Kept for backward compatibility with existing wendy.json files.
    public var allowlist: [String]

    public init(mode: VideoMode? = nil, allowlist: [String] = []) {
        self.mode = mode
        self.allowlist = allowlist
    }

    private enum CodingKeys: String, CodingKey {
        case mode
        case allowlist
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        self.mode = try container.decodeIfPresent(VideoMode.self, forKey: .mode)
        self.allowlist =
            try container.decodeIfPresent([String].self, forKey: .allowlist) ?? []
    }

    public func encode(to encoder: Encoder) throws {
        // Only encode "type" (handled by Entitlement.encode) — mode and allowlist are deprecated
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
