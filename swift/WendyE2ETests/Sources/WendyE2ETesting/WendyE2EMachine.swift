import Foundation

public enum WendyE2EMachineOS: String, Sendable {
    case macOS
    case linux
    case windows
    case wendyOS

    public static var current: WendyE2EMachineOS {
        #if os(macOS)
            .macOS
        #elseif os(Linux)
            .linux
        #elseif os(Windows)
            .windows
        #else
            .linux
        #endif
    }

    public init?(environmentValue value: String) {
        switch value.lowercased() {
        case "macos", "mac", "darwin":
            self = .macOS
        case "linux":
            self = .linux
        case "windows", "win":
            self = .windows
        case "wendyos", "wendy-os", "wendy_os":
            self = .wendyOS
        default:
            return nil
        }
    }
}

public enum WendyE2EMachineTag: String, Sendable {
    case cli
    case agent
    case runner
}

public struct WendyE2EMachine: Sendable, Equatable {
    public let id: String
    public let name: String
    public let os: WendyE2EMachineOS
    public let tags: Set<WendyE2EMachineTag>
    public let isLocal: Bool
    public let user: String?
    public let address: String

    // MARK: - Creating Machines

    public init(
        id: String,
        name: String,
        os: WendyE2EMachineOS = .current,
        tags: Set<WendyE2EMachineTag> = [],
        user: String? = nil,
        address: String? = nil
    ) {
        precondition(!id.isEmpty, "id must not be empty")
        precondition(!name.isEmpty, "name must not be empty")
        precondition(user?.isEmpty != true, "user must not be empty")
        precondition(address?.isEmpty != true, "address must not be empty")

        let resolvedAddress = address ?? Self.defaultAddress

        self.id = id
        self.name = name
        self.os = os
        self.tags = tags
        self.isLocal = address == nil
        self.user = user
        self.address = resolvedAddress
    }

    // MARK: - Known Machines

    public static var current: WendyE2EMachine {
        WendyE2EMachine(
            id: "current",
            name: "Current",
            os: .current,
            tags: [.runner]
        )
    }

    public static var cli: WendyE2EMachine {
        WendyE2EMachine(
            id: "cli",
            name: "CLI",
            os: WendyE2EEnvironment.cliOS ?? .current,
            tags: [.cli],
            user: WendyE2EEnvironment.cliUser,
            address: WendyE2EEnvironment.cliAddress
        )
    }

    public static var agent: WendyE2EMachine {
        WendyE2EMachine(
            id: "agent",
            name: "Agent",
            os: WendyE2EEnvironment.agentOS ?? .current,
            tags: [.agent],
            user: WendyE2EEnvironment.agentUser,
            address: WendyE2EEnvironment.agentAddress
        )
    }

    // MARK: - Private

    private static var defaultAddress: String {
        ProcessInfo.processInfo.hostName
    }
}

// MARK: - CustomStringConvertible

extension WendyE2EMachine: CustomStringConvertible {
    public var description: String {
        self.id
    }
}
