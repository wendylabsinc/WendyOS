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
    case peripherals(PeripheralEntitlements)

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
        case .peripherals(let entitlement):
            try container.encode(EntitlementType.peripherals, forKey: .type)
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
        case .peripherals:
            self = .peripherals(try PeripheralEntitlements(from: decoder))
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
    case peripherals
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

public struct PeripheralEntitlements: Codable, Sendable, Hashable {
    public let gpio: Bool
    public let spi: Bool
    public let i2c: Bool
    public let usbSerial: Bool
    public let usbBus: Bool

    public init(
        gpio: Bool = true,
        spi: Bool = true,
        i2c: Bool = true,
        usbSerial: Bool = true,
        usbBus: Bool = false
    ) {
        self.gpio = gpio
        self.spi = spi
        self.i2c = i2c
        self.usbSerial = usbSerial
        self.usbBus = usbBus
    }

    /// Convenience initializer for all peripherals enabled
    public static var all: PeripheralEntitlements {
        PeripheralEntitlements(gpio: true, spi: true, i2c: true, usbSerial: true, usbBus: true)
    }
}
