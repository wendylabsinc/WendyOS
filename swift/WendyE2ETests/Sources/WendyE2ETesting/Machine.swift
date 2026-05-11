import Foundation

public enum MachineOS: String, Sendable {
    case macOS
    case linux
    case windows
    case wendyOS

    public static var current: MachineOS {
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

public enum MachineTag: String, Sendable {
    case cli
    case agent
    case runner
}

public struct Machine: Sendable, Equatable {
    public let id: String
    public let name: String
    public let os: MachineOS
    public let tags: Set<MachineTag>
    public let user: String?
    public let address: String
    public let workingDirectory: String?
    public let env: [String: String]

    // MARK: - Creating Machines

    public init(
        id: String? = nil,
        name: String,
        os: MachineOS = .current,
        tags: Set<MachineTag> = [],
        user: String? = nil,
        address: String? = nil,
        workingDirectory: String? = nil,
        env: [String: String] = [:]
    ) {
        precondition(id?.isEmpty != true, "id must not be empty")
        precondition(!name.isEmpty, "name must not be empty")
        precondition(user?.isEmpty != true, "user must not be empty")
        precondition(address?.isEmpty != true, "address must not be empty")
        precondition(workingDirectory?.isEmpty != true, "workingDirectory must not be empty")
        for key in env.keys {
            precondition(
                Self.isValidEnvironmentKey(key),
                "env keys must be valid shell variable names"
            )
        }

        let resolvedAddress = address ?? Self.defaultAddress
        let resolvedWorkingDirectory =
            workingDirectory ?? (address == nil ? FileManager.default.currentDirectoryPath : nil)

        self.id =
            id
            ?? Self.defaultID(
                user: user,
                address: resolvedAddress,
                workingDirectory: resolvedWorkingDirectory
            )
        self.name = name
        self.os = os
        self.tags = tags
        self.user = user
        self.address = resolvedAddress
        self.workingDirectory = resolvedWorkingDirectory
        self.env = env
    }

    // MARK: - Known Machines

    public static var current: Machine {
        Machine(
            id: "current",
            name: "Current",
            os: .current,
            tags: [.runner]
        )
    }

    // MARK: - Private

    private static var defaultAddress: String {
        ProcessInfo.processInfo.hostName
    }

    private static func isValidEnvironmentKey(_ key: String) -> Bool {
        guard let first = key.first else {
            return false
        }
        guard first == "_" || first.isASCII && first.isLetter else {
            return false
        }

        return key.dropFirst().allSatisfy { character in
            character == "_" || character.isASCII && (character.isLetter || character.isNumber)
        }
    }

    private static func defaultID(
        user: String?,
        address: String,
        workingDirectory: String?
    ) -> String {
        let location = user.map { "\($0)@\(address)" } ?? address

        if let workingDirectory {
            return "\(location):\(workingDirectory)"
        }

        return "\(location):~"
    }
}

// MARK: - CustomStringConvertible

extension Machine: CustomStringConvertible {
    public var description: String {
        self.id
    }
}
