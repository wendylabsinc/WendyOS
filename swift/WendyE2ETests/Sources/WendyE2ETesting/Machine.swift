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
    public let ssh: String?
    public let workingDirectory: String?
    public let sshExecutable: String

    // MARK: - Creating Machines

    public init(
        id: String? = nil,
        name: String,
        os: MachineOS = .current,
        tags: Set<MachineTag> = [],
        ssh: String? = nil,
        workingDirectory: String? = nil,
        sshExecutable: String = "/usr/bin/ssh"
    ) {
        precondition(id?.isEmpty != true, "id must not be empty")
        precondition(!name.isEmpty, "name must not be empty")
        precondition(ssh?.isEmpty != true, "ssh must not be empty")
        precondition(workingDirectory?.isEmpty != true, "workingDirectory must not be empty")
        precondition(!sshExecutable.isEmpty, "sshExecutable must not be empty")

        let currentDirectoryPathOrNil = ssh == nil ? FileManager.default.currentDirectoryPath : nil
        let resolvedWorkingDirectory = workingDirectory ?? currentDirectoryPathOrNil

        self.id = id ?? Self.defaultID(ssh: ssh, workingDirectory: resolvedWorkingDirectory)
        self.name = name
        self.os = os
        self.tags = tags
        self.ssh = ssh
        self.workingDirectory = resolvedWorkingDirectory
        self.sshExecutable = sshExecutable
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

    private static func defaultID(ssh: String?, workingDirectory: String?) -> String {
        let location = ssh ?? "local"
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
