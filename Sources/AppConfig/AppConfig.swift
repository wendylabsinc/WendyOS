import ArgumentParser
import Foundation

/// Strongly typed representation of a `wendy.json` project configuration file.
///
/// `AppConfig` contains metadata, entitlement requests, and optional profile-based
/// build/run plans used by `wendy run` and `wendy build`.
public struct AppConfig: Codable, Sendable {
    /// Stable application identifier used across deploy/build workflows.
    public let appId: String
    /// User-managed app version string.
    public let version: String
    /// Human-friendly app name shown in UIs.
    public var name: String?
    /// Optional project description.
    public var description: String?
    /// Optional language hint (for example, `swift`, `python`, `rust`, or `cpp`).
    public var language: String?
    /// Environment variables applied at runtime unless overridden by profile settings.
    public var env: [String: String]?
    /// Default working directory used by host command execution.
    public var workingDir: String?
    /// Optional profile ID to prefer when multiple profiles match.
    public var defaultProfile: String?
    /// Requested runtime capabilities for device/container execution.
    public var entitlements: [Entitlement]
    /// Language-specific configuration for Python projects.
    public var python: PythonConfig?
    /// Optional profile definitions for local/device/remote build and run strategies.
    public var profiles: [Profile]?

    /// Python-specific settings for packaging and containerization.
    public struct PythonConfig: Codable, Sendable, Hashable {
        /// Container-oriented Python project settings.
        public struct PythonContainerConfig: Codable, Sendable, Hashable {
            /// Root directory that should be treated as Python source in a container build.
            public var sourceRoot: String
        }

        /// Optional container settings for Python projects.
        public var container: PythonContainerConfig?

        /// Creates Python configuration with a container source root.
        ///
        /// - Parameter sourceRoot: Path to the Python source root.
        public init(sourceRoot: String) {
            self.container = .init(sourceRoot: sourceRoot)
        }
    }

    /// Creates an app configuration value.
    ///
    /// - Parameters:
    ///   - appId: Stable application identifier.
    ///   - version: User-managed app version.
    ///   - name: Optional display name.
    ///   - description: Optional description text.
    ///   - language: Optional language hint.
    ///   - env: Optional base environment variables.
    ///   - workingDir: Optional working directory for host execution.
    ///   - defaultProfile: Optional profile ID to prefer during profile resolution.
    ///   - entitlements: Capability requests for container/device runtime.
    ///   - python: Optional Python-specific settings.
    ///   - profiles: Optional profile-based build/run definitions.
    public init(
        appId: String,
        version: String,
        name: String? = nil,
        description: String? = nil,
        language: String? = nil,
        env: [String: String]? = nil,
        workingDir: String? = nil,
        defaultProfile: String? = nil,
        entitlements: [Entitlement] = [],
        python: PythonConfig? = nil,
        profiles: [Profile]? = nil
    ) {
        self.appId = appId
        self.version = version
        self.name = name
        self.description = description
        self.language = language
        self.env = env
        self.workingDir = workingDir
        self.defaultProfile = defaultProfile
        self.entitlements = entitlements
        self.python = python
        self.profiles = profiles
    }

    private enum CodingKeys: String, CodingKey {
        case appId
        case version
        case name
        case description
        case language
        case env
        case workingDir
        case defaultProfile
        case entitlements
        case python
        case profiles
    }

    /// Decodes configuration and normalizes optional collection defaults.
    ///
    /// In particular, `entitlements` defaults to an empty array when omitted.
    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        self.appId = try container.decode(String.self, forKey: .appId)
        self.version = try container.decode(String.self, forKey: .version)
        self.name = try container.decodeIfPresent(String.self, forKey: .name)
        self.description = try container.decodeIfPresent(String.self, forKey: .description)
        self.language = try container.decodeIfPresent(String.self, forKey: .language)
        self.env = try container.decodeIfPresent([String: String].self, forKey: .env)
        self.workingDir = try container.decodeIfPresent(String.self, forKey: .workingDir)
        self.defaultProfile = try container.decodeIfPresent(String.self, forKey: .defaultProfile)
        self.entitlements =
            try container.decodeIfPresent([Entitlement].self, forKey: .entitlements) ?? []
        self.python = try container.decodeIfPresent(PythonConfig.self, forKey: .python)
        self.profiles = try container.decodeIfPresent([Profile].self, forKey: .profiles)

        if let profiles, !profiles.isEmpty {
            var seen = Set<String>()
            var duplicates = Set<String>()

            for profile in profiles {
                if !seen.insert(profile.id).inserted {
                    duplicates.insert(profile.id)
                }
            }

            if !duplicates.isEmpty {
                throw DecodingError.dataCorruptedError(
                    forKey: .profiles,
                    in: container,
                    debugDescription:
                        "Duplicate profile id(s): \(duplicates.sorted().joined(separator: ", ")). "
                        + "Profile ids must be unique."
                )
            }
        }
    }

    /// A conditional build/run strategy selected by destination and environment.
    public struct Profile: Codable, Sendable, Hashable {
        /// Unique profile identifier.
        public let id: String
        /// Selection conditions used by profile resolution.
        public var when: When
        /// Optional tie-breaker priority; larger numbers win.
        public var priority: Int?
        /// Environment variables applied for this profile.
        public var env: [String: String]?
        /// Optional build plan for this profile.
        public var build: Build?
        /// Optional run plan for this profile.
        public var run: Run?
        /// Optional entitlement overrides applied for this profile.
        ///
        /// - Note: When set, this value replaces top-level `entitlements` for
        ///   this profile. It does not merge with them.
        public var entitlements: [Entitlement]?
        /// Optional prerequisite labels for tooling/UX.
        public var requires: [String]?
        /// Optional OpenTelemetry configuration for this profile.
        public var otel: OTel?
        /// Optional lifecycle hooks that run around build/run phases.
        ///
        /// Hooks always run on the host machine that invokes `wendy run`.
        /// They are useful for setup/teardown tasks such as generating assets,
        /// validating prerequisites, or cleaning temporary state.
        ///
        /// Hooks are currently executed during profile-based `wendy run` flows
        /// (`--local`, `--device`, or `--profile`). They are not executed by
        /// `wendy build`.
        ///
        /// For dependency installation:
        /// - Use `hooks.preBuild` for host-side setup work before a run
        ///   (for example `npm ci` or `uv sync`).
        /// - Use `build.type == .command` when dependency setup should be part of
        ///   the profile build step.
        /// - Use Dockerfile `RUN` instructions for dependencies that must exist
        ///   inside a device/container image.
        public var hooks: Hooks?

        /// Creates a profile.
        ///
        /// - Note: At least one of `build` or `run` must be provided.
        public init(
            id: String,
            when: When,
            priority: Int? = nil,
            env: [String: String]? = nil,
            build: Build? = nil,
            run: Run? = nil,
            entitlements: [Entitlement]? = nil,
            requires: [String]? = nil,
            otel: OTel? = nil,
            hooks: Hooks? = nil
        ) {
            precondition(
                build != nil || run != nil,
                "Profile '\(id)' must define at least one of 'build' or 'run'."
            )
            self.id = id
            self.when = when
            self.priority = priority
            self.env = env
            self.build = build
            self.run = run
            self.entitlements = entitlements
            self.requires = requires
            self.otel = otel
            self.hooks = hooks
        }

        /// Decodes a profile and validates that at least one execution section exists.
        ///
        /// A profile must define at least one of `build` or `run`.
        public init(from decoder: Decoder) throws {
            let container = try decoder.container(keyedBy: CodingKeys.self)
            self.id = try container.decode(String.self, forKey: .id)
            self.when = try container.decode(When.self, forKey: .when)
            self.priority = try container.decodeIfPresent(Int.self, forKey: .priority)
            self.env = try container.decodeIfPresent([String: String].self, forKey: .env)
            self.build = try container.decodeIfPresent(Build.self, forKey: .build)
            self.run = try container.decodeIfPresent(Run.self, forKey: .run)
            self.entitlements = try container.decodeIfPresent(
                [Entitlement].self,
                forKey: .entitlements
            )
            self.requires = try container.decodeIfPresent([String].self, forKey: .requires)
            self.otel = try container.decodeIfPresent(OTel.self, forKey: .otel)
            self.hooks = try container.decodeIfPresent(Hooks.self, forKey: .hooks)

            if build == nil && run == nil {
                throw DecodingError.dataCorrupted(
                    DecodingError.Context(
                        codingPath: container.codingPath,
                        debugDescription:
                            "Profile '\(id)' must define at least one of 'build' or 'run'."
                    )
                )
            }
        }

        /// Matching conditions used to determine when a profile applies.
        public struct When: Codable, Sendable, Hashable {
            /// Primary execution destination (local/device/remote).
            public var target: ProfileTarget
            /// Optional OS matcher (for example, `macos`, `linux`).
            public var os: String?
            /// Optional architecture matcher (for example, `arm64`).
            public var arch: String?
            /// Optional trait matcher set.
            public var traits: [String]?
            /// Optional device-specific matcher.
            public var device: DeviceSelector?

            /// Creates profile matching conditions.
            public init(
                target: ProfileTarget,
                os: String? = nil,
                arch: String? = nil,
                traits: [String]? = nil,
                device: DeviceSelector? = nil
            ) {
                self.target = target
                self.os = os
                self.arch = arch
                self.traits = traits
                self.device = device
            }
        }

        /// Device selector constraints used when `when.target == .device`.
        public struct DeviceSelector: Codable, Sendable, Hashable {
            /// Optional device platform matcher.
            public var platform: String?
            /// Optional regex applied to device hostnames.
            public var hostnameRegex: String?

            /// Creates device selector constraints.
            public init(platform: String? = nil, hostnameRegex: String? = nil) {
                self.platform = platform
                self.hostnameRegex = hostnameRegex
            }
        }

        /// Build configuration for a profile.
        public struct Build: Codable, Sendable, Hashable {
            /// Build strategy.
            public var type: BuildType
            /// Optional Dockerfile path when using Docker builds.
            public var dockerfile: String?
            /// Optional build platform override (for example, `linux/arm64`).
            public var platform: String?
            /// Optional Docker build arguments.
            public var buildArgs: [String: String]?
            /// Optional environment variables for build command execution.
            ///
            /// This applies to `build.type == .command` and to lifecycle build hooks
            /// (`hooks.preBuild` and `hooks.postBuild`).
            public var env: [String: String]?
            /// Host command used when `type == .command`.
            public var command: String?
            /// Shell executable used for `command`.
            public var shell: String?
            /// Working directory override for build command execution.
            public var cwd: String?
            /// Extra arguments appended to `command`.
            public var args: [String]?
            /// Optional declared input paths.
            public var inputs: [String]?
            /// Optional declared output paths.
            public var outputs: [String]?

            /// Creates a profile build configuration.
            public init(
                type: BuildType,
                dockerfile: String? = nil,
                platform: String? = nil,
                buildArgs: [String: String]? = nil,
                env: [String: String]? = nil,
                command: String? = nil,
                shell: String? = nil,
                cwd: String? = nil,
                args: [String]? = nil,
                inputs: [String]? = nil,
                outputs: [String]? = nil
            ) {
                self.type = type
                self.dockerfile = dockerfile
                self.platform = platform
                self.buildArgs = buildArgs
                self.env = env
                self.command = command
                self.shell = shell
                self.cwd = cwd
                self.args = args
                self.inputs = inputs
                self.outputs = outputs
            }
        }

        /// Run configuration for a profile.
        public struct Run: Codable, Sendable, Hashable {
            /// Run strategy.
            public var type: RunType
            /// Host command used when running on local host.
            public var command: String?
            /// Shell executable used for `command`.
            public var shell: String?
            /// Working directory override for run command execution.
            public var cwd: String?
            /// Optional argument list for executable-style host runs.
            public var args: [String]?
            /// Optional environment overrides for run execution.
            public var env: [String: String]?
            /// Optional container runtime overrides for device/container runs.
            public var container: Container?

            /// Creates a profile run configuration.
            public init(
                type: RunType,
                command: String? = nil,
                shell: String? = nil,
                cwd: String? = nil,
                args: [String]? = nil,
                env: [String: String]? = nil,
                container: Container? = nil
            ) {
                self.type = type
                self.command = command
                self.shell = shell
                self.cwd = cwd
                self.args = args
                self.env = env
                self.container = container
            }
        }

        /// Lifecycle hook definitions for profile execution phases.
        ///
        /// Hooks run in the order listed for each phase:
        /// - `preBuild` before `build`
        /// - `postBuild` after a successful `build`
        /// - `preRun` before `run`
        /// - `postRun` after a successful `run`
        /// - `preStop` before stopping an attached device container due to cancellation/failure
        ///
        /// ## Dependency Setup Guidance
        ///
        /// Hooks can be used for dependency setup (for example `npm ci`,
        /// `pnpm install --frozen-lockfile`, `uv sync`, or
        /// `uv pip install -r requirements.txt`), especially in `preBuild`.
        ///
        /// Prefer idempotent commands so repeated `wendy run` invocations stay
        /// predictable. If dependency installation is heavy and should not run
        /// on every local run, move it to `build.type == .command` or your
        /// Dockerfile instead.
        ///
        /// ## Example
        ///
        /// ```json
        /// {
        ///   "id": "device-jetson",
        ///   "when": { "target": "device", "device": { "platform": "jetson" } },
        ///   "build": {
        ///     "type": "docker",
        ///     "platform": "{{target.platform}}",
        ///     "buildArgs": {
        ///       "TARGET_PLATFORM": "{{target.platform}}",
        ///       "DEVICE_HOST": "{{device.host}}"
        ///     },
        ///     "env": {
        ///       "DOCKER_BUILDKIT": "1"
        ///     }
        ///   },
        ///   "run": { "type": "container" },
        ///   "hooks": {
        ///     "preBuild": [
        ///       { "name": "Generate assets", "command": "make assets" }
        ///     ],
        ///     "preRun": [
        ///       { "command": "echo Running on {{target.platform}} for {{device.host}}" }
        ///     ],
        ///     "preStop": [
        ///       { "command": "echo Cleaning up local dev state", "continueOnError": true }
        ///     ]
        ///   }
        /// }
        /// ```
        public struct Hooks: Codable, Sendable, Hashable {
            /// Commands to run before profile build execution.
            public var preBuild: [Hook]?
            /// Commands to run after successful profile build execution.
            public var postBuild: [Hook]?
            /// Commands to run before profile run execution.
            public var preRun: [Hook]?
            /// Commands to run after successful profile run execution.
            public var postRun: [Hook]?
            /// Commands to run before attached container stop on cancellation/failure.
            public var preStop: [Hook]?

            /// Creates lifecycle hook groups.
            public init(
                preBuild: [Hook]? = nil,
                postBuild: [Hook]? = nil,
                preRun: [Hook]? = nil,
                postRun: [Hook]? = nil,
                preStop: [Hook]? = nil
            ) {
                self.preBuild = preBuild
                self.postBuild = postBuild
                self.preRun = preRun
                self.postRun = postRun
                self.preStop = preStop
            }
        }

        /// A single lifecycle hook command.
        ///
        /// You can define either:
        /// - `command` (+ optional `args`) to run via `shell`, or
        /// - `args` where `args[0]` is the executable path/name.
        ///
        /// Supported interpolation placeholders:
        /// - `{{profile.id}}`
        /// - `{{host.os}}`, `{{host.arch}}`
        /// - `{{target.kind}}`, `{{target.os}}`, `{{target.arch}}`, `{{target.platform}}`
        /// - `{{device.host}}`, `{{device.port}}`, `{{device.platform}}`
        ///   (`device.host` is sanitized to hostname-safe characters for template use)
        ///
        /// The same values are also available as environment variables:
        /// `WENDY_PROFILE_ID`, `WENDY_HOST_OS`, `WENDY_HOST_ARCH`,
        /// `WENDY_TARGET_KIND`, `WENDY_TARGET_OS`, `WENDY_TARGET_ARCH`,
        /// `WENDY_TARGET_PLATFORM`, `WENDY_DEVICE_HOST`, `WENDY_DEVICE_PORT`,
        /// `WENDY_DEVICE_PLATFORM`.
        ///
        /// `WENDY_DEVICE_HOST` preserves the raw discovered host value.
        public struct Hook: Codable, Sendable, Hashable {
            /// Optional display name shown in CLI output.
            public var name: String?
            /// Optional shell command.
            public var command: String?
            /// Shell executable used when `command` is provided.
            public var shell: String?
            /// Working directory override for hook command execution.
            public var cwd: String?
            /// Optional argument list to append to `command`,
            /// or executable-style invocation when `command` is omitted.
            public var args: [String]?
            /// Optional hook-scoped environment overrides.
            public var env: [String: String]?
            /// Continue profile execution if this hook fails.
            public var continueOnError: Bool?

            /// Creates a lifecycle hook command.
            public init(
                name: String? = nil,
                command: String? = nil,
                shell: String? = nil,
                cwd: String? = nil,
                args: [String]? = nil,
                env: [String: String]? = nil,
                continueOnError: Bool? = nil
            ) {
                self.name = name
                self.command = command
                self.shell = shell
                self.cwd = cwd
                self.args = args
                self.env = env
                self.continueOnError = continueOnError
            }
        }

        /// Container runtime override values for a profile run.
        public struct Container: Codable, Sendable, Hashable {
            /// Container command override.
            public var cmd: [String]?
            /// Container entrypoint override.
            /// - Note: This field is currently ignored by the runtime/agent and is
            ///   reserved for future use. Configuration values set here will have
            ///   no effect.
            public var entrypoint: [String]?
            /// Container working directory override.
            public var workingDir: String?

            /// Creates container runtime overrides.
            public init(
                cmd: [String]? = nil,
                entrypoint: [String]? = nil,
                workingDir: String? = nil
            ) {
                self.cmd = cmd
                self.entrypoint = entrypoint
                self.workingDir = workingDir
            }
        }

        /// OpenTelemetry settings applied for this profile.
        public struct OTel: Codable, Sendable, Hashable {
            /// Enables or disables OpenTelemetry wiring.
            ///
            /// When omitted, this defaults to `true` to preserve existing behavior.
            public var enabled: Bool?
            /// Collector endpoint URL.
            public var endpoint: String?
            /// Reported service name.
            public var serviceName: String?

            /// Creates profile OpenTelemetry settings.
            public init(enabled: Bool? = nil, endpoint: String? = nil, serviceName: String? = nil) {
                self.enabled = enabled
                self.endpoint = endpoint
                self.serviceName = serviceName
            }
        }
    }

    /// Top-level profile destination classes.
    public enum ProfileTarget: String, Codable, Sendable, Hashable {
        /// Execute on the current machine.
        case local
        /// Execute against a Wendy device.
        case device
        /// Execute against a remote/cloud environment.
        case remote
    }

    /// Build strategy for a profile.
    public enum BuildType: String, Codable, Sendable, Hashable {
        /// No build step.
        case none
        /// Build using Docker.
        case docker
        /// Build using a host command.
        case command
    }

    /// Run strategy for a profile.
    public enum RunType: String, Codable, Sendable, Hashable {
        /// Run on host (local shell/executable).
        case host
        /// Run in a container (typically device/container runtime).
        case container
    }

    /// Validates wendy.json data and returns warnings for unknown keys in entitlements.
    ///
    /// Call this after decoding to surface potential key typos while remaining tolerant.
    ///
    /// - Parameter data: Raw JSON data for `wendy.json`.
    /// - Returns: Warning messages for unknown entitlement keys.
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

/// Requested runtime capabilities for an app/profile.
///
/// Serialized as tagged objects in `wendy.json` using the `type` field.
public enum Entitlement: Codable, Sendable, Hashable {
    /// Network namespace/capability configuration.
    case network(NetworkEntitlements)
    /// Bluetooth capability configuration.
    case bluetooth(BluetoothEntitlements)
    /// Video device access configuration.
    case video(VideoEntitlements)
    /// GPU access capability configuration.
    case gpu(GPUEntitlements)
    /// Persistent volume mapping configuration.
    case persist(PersistenceEntitlements)
    /// Audio capability request.
    case audio

    /// Encodes the tagged entitlement payload.
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

    /// Decodes a tagged entitlement payload by `type`.
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

    /// Returns the set of allowed JSON keys for an entitlement payload type.
    ///
    /// - Parameter type: Entitlement type discriminator.
    /// - Returns: Keys accepted for that payload.
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

/// String discriminator values for entitlement payloads in `wendy.json`.
public enum EntitlementType: String, Codable, CaseIterable, ExpressibleByArgument, Sendable {
    case network
    case video
    case audio
    case bluetooth
    case gpu
    case persist
}

/// Persistent volume entitlement settings.
public struct PersistenceEntitlements: Codable, Sendable, Hashable {
    /// The name of the volume to persist
    public let name: String

    /// The path inside the container to mount the persisted volume at
    public let path: String

    /// Creates persistence entitlement settings.
    ///
    /// - Parameters:
    ///   - name: Volume name.
    ///   - path: Mount path inside the container.
    public init(name: String, path: String) {
        self.name = name
        self.path = path
    }
}

/// Bluetooth entitlement settings.
public struct BluetoothEntitlements: Codable, Sendable, Hashable {
    /// Allowed Bluetooth backend modes.
    public enum BluetoothMode: String, Codable, Sendable, Hashable {
        /// Use BlueZ userspace integration.
        case bluez, kernel
    }

    /// Selected Bluetooth mode.
    public let mode: BluetoothMode

    /// Creates Bluetooth entitlement settings.
    public init(mode: BluetoothMode) {
        self.mode = mode
    }
}

/// GPU entitlement marker type.
public struct GPUEntitlements: Codable, Sendable, Hashable {
    /// Creates GPU entitlement settings.
    public init() {}
}

/// Video device entitlement settings.
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
    /// Explicit allowed device paths when `mode == .allowlist`.
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

    /// Decodes video entitlement settings with defaults.
    ///
    /// `mode` defaults to `.all`, and `allowlist` defaults to empty.
    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        self.mode = try container.decodeIfPresent(VideoMode.self, forKey: .mode) ?? .all
        self.allowlist =
            try container.decodeIfPresent([String].self, forKey: .allowlist) ?? []
    }

    /// Encodes video entitlement settings.
    public func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode(mode, forKey: .mode)
        try container.encode(allowlist, forKey: .allowlist)
    }
}

/// Audio entitlement marker type.
public struct AudioEntitlements: Codable, Sendable, Hashable {
    /// Creates audio entitlement settings.
    public init() {}
}

/// Network entitlement settings.
public struct NetworkEntitlements: Codable, Sendable, Hashable {
    /// Selected network mode.
    public let mode: NetworkMode

    /// Creates network entitlement settings.
    public init(mode: NetworkMode) {
        self.mode = mode
    }
}

/// Network namespace modes for `NetworkEntitlements`.
public enum NetworkMode: String, Codable, Sendable, Hashable {
    /// Use host networking.
    case host
    /// Disable network access.
    case none
}
