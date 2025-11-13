struct OCI: Codable, Sendable {
    let ociVersion: String
    var process: Process
    let root: Root
    var hostname: String
    var mounts: [Mount]
    var linux: Linux

    init(args: [String], env: [String], workingDir: String, appName: String) {
        self = OCI(
            process: .init(
                user: .root,
                args: args,
                env: env,
                cwd: workingDir.isEmpty ? "/" : workingDir
            ),
            root: .init(path: "rootfs", readonly: false),
            hostname: appName,
            mounts: [
                .init(destination: "/proc", type: "proc", source: "proc"),
                // Needed for TTY support (requirement for DS2)
                .init(
                    destination: "/dev/pts",
                    type: "devpts",
                    source: "devpts",
                    options: [
                        "nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620",
                    ]
                ),
                .init(
                    destination: "/dev/shm",
                    type: "tmpfs",
                    source: "shm",
                    options: ["nosuid", "noexec", "nodev", "mode=1777", "size=65536k"]
                ),
                .init(
                    destination: "/dev/mqueue",
                    type: "mqueue",
                    source: "mqueue",
                    options: ["nosuid", "noexec", "nodev"]
                ),
            ],
            linux: .init(
                namespaces: [
                    .init(type: "pid"),
                    .init(type: "ipc"),
                    .init(type: "uts"),
                    .init(type: "mount"),
                ],
                networkMode: "host",
                capabilities: .init(
                    bounding: ["SYS_PTRACE"],
                    effective: ["SYS_PTRACE"],
                    inheritable: ["SYS_PTRACE"],
                    permitted: ["SYS_PTRACE"],
                ),
                seccomp: Seccomp(
                    defaultAction: "SCMP_ACT_ALLOW",
                    architectures: ["SCMP_ARCH_AARCH64"],
                    syscalls: []
                ),
                devices: []
            )
        )
    }

    init(
        ociVersion: String = "1.0.3",
        process: Process,
        root: Root,
        hostname: String,
        mounts: [Mount],
        linux: Linux
    ) {
        self.ociVersion = ociVersion
        self.process = process
        self.root = root
        self.hostname = hostname
        self.mounts = mounts
        self.linux = linux
    }
}

public struct Process: Codable, Sendable {
    struct User: Codable, Sendable {
        let uid: Int
        let gid: Int

        static let root = User(uid: 0, gid: 0)
    }

    let user: User
    let terminal: Bool
    let args: [String]
    let env: [String]
    let cwd: String

    init(
        user: User,
        terminal: Bool = false,
        args: [String],
        env: [String] = [
            "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
        ],
        cwd: String = "/"
    ) {
        self.user = user
        self.terminal = terminal
        self.args = args
        self.env = env
        self.cwd = cwd
    }
}

struct Root: Codable, Sendable {
    let path: String
    let readonly: Bool

    init(path: String = "rootfs", readonly: Bool = false) {
        self.path = path
        self.readonly = readonly
    }
}

public struct Mount: Codable, Sendable {
    let destination: String
    let type: String
    let source: String
    let options: [String]?

    init(destination: String, type: String, source: String, options: [String]? = nil) {
        self.destination = destination
        self.type = type
        self.source = source
        self.options = options
    }
}

public struct Linux: Codable, Sendable {
    var namespaces: [Namespace]
    var networkMode: String
    var capabilities: Capabilities
    var seccomp: Seccomp
    var devices: [Device]
    var resources: Resources?
    var cgroupsPath: String?
}

public struct Resources: Codable, Sendable {
    var devices: [DeviceAllowance]?
}

public struct DeviceAllowance: Codable, Sendable {
    let allow: Bool
    let type: String?
    let major: Int?
    let minor: Int?
    let access: String?

    init(
        allow: Bool,
        type: String? = nil,
        major: Int? = nil,
        minor: Int? = nil,
        access: String? = nil
    ) {
        self.allow = allow
        self.type = type
        self.major = major
        self.minor = minor
        self.access = access
    }
}

public struct Namespace: Codable, Sendable {
    let type: String
    var path: String?
}

public struct Capabilities: Codable, Sendable {
    var bounding: Set<String>
    var effective: Set<String>
    var inheritable: Set<String>
    var permitted: Set<String>
}

public struct Seccomp: Codable, Sendable {
    let defaultAction: String
    let architectures: [String]
    let syscalls: [Syscall]
}

public struct Syscall: Codable, Sendable {
    let names: [String]
    let action: String
    var args: [Argument]?

    struct Argument: Codable, Sendable {
        enum Op: String, Codable, Sendable {
            case NE = "SCMP_CMP_NE"
            case LT = "SCMP_CMP_LT"
            case LE = "SCMP_CMP_LE"
            case EQ = "SCMP_CMP_EQ"
            case GE = "SCMP_CMP_GE"
            case GT = "SCMP_CMP_GT"
            case MASKED_EQ = "SCMP_CMP_MASKED_EQ"
        }

        let index: UInt
        let value: UInt64
        let valueTwo: UInt64?
        let op: Op
    }
}

public struct Device: Codable, Sendable {
    let path: String
    let type: String
    let major: Int
    let minor: Int
    let fileMode: Int?
    let uid: Int?
    let gid: Int?
}
