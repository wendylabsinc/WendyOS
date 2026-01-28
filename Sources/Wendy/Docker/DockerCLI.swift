//
//  DockerCLI.swift
//  wendy-agent
//
//  Created by Joannis Orlandos on 16/09/2025.
//

import Foundation
import Logging
import Subprocess

#if os(macOS)
    import Darwin
#elseif canImport(Glibc)
    import Glibc
#elseif canImport(Musl)
    import Musl
#endif

/// Errors related to Docker container operations
public enum DockerError: Error, LocalizedError {
    /// Container was not found (exit code 1 with "No such object" message)
    case containerNotFound(containerName: String)
    /// Docker daemon is not running or not accessible
    case daemonUnavailable(underlyingError: String)
    /// Permission denied when accessing Docker
    case permissionDenied(operation: String)
    /// File not found inside container
    case fileNotFound(containerName: String, filePath: String)
    /// Container exists but is not running
    case containerNotRunning(containerName: String)
    /// Generic Docker command failure
    case commandFailed(command: String, exitCode: Int, stderr: String)

    public var errorDescription: String? {
        switch self {
        case .containerNotFound(let name):
            return "Container '\(name)' not found"
        case .daemonUnavailable(let error):
            return "Docker daemon unavailable: \(error)"
        case .permissionDenied(let operation):
            return "Permission denied for Docker operation: \(operation)"
        case .fileNotFound(let container, let path):
            return "File '\(path)' not found in container '\(container)'"
        case .containerNotRunning(let name):
            return "Container '\(name)' is not running"
        case .commandFailed(let command, let exitCode, let stderr):
            return "Docker command '\(command)' failed with exit code \(exitCode): \(stderr)"
        }
    }
}

/// Compression mode for Docker image layers
public enum ImageCompressionMode: String, Sendable {
    /// zstd compression - 3-5x faster decompression than gzip, good compression ratio
    case zstd
    /// gzip compression - legacy default, slower decompression
    case gzip
    /// No compression - fastest for high-bandwidth connections (USB, fast LAN)
    case uncompressed
}

/// Manages a file-based lock to prevent parallel builds from interfering with each other
public final class BuildLock: Sendable {
    private let lockPath: String

    public static let shared = BuildLock()

    private init() {
        let wendyDir = FileManager.default.homeDirectoryForCurrentUser
            .appendingPathComponent(".wendy")
        self.lockPath = wendyDir.appendingPathComponent("build.lock").path
    }

    /// Acquires a shared lock for building
    /// Multiple builds can hold shared locks simultaneously, allowing parallel builds
    /// The lock is automatically released when the closure completes or throws
    public func withLock<T: Sendable>(_ operation: @Sendable () async throws -> T) async throws -> T
    {
        let fd = try acquireForBuild()
        defer { release(fd: fd) }
        return try await operation()
    }

    /// Checks if any builds are currently in progress
    /// Returns true if one or more builds hold shared locks
    public func isBuildInProgress() -> Bool {
        let fd = open(lockPath, O_RDWR)
        guard fd >= 0 else {
            // Lock file doesn't exist, no build in progress
            return false
        }
        defer { close(fd) }

        #if os(macOS) || canImport(Glibc) || canImport(Musl)
            // Try to acquire an exclusive lock (non-blocking)
            // If we can't get it, shared locks are held by running builds
            let result = flock(fd, LOCK_EX | LOCK_NB)
            if result != 0 {
                // Failed to get exclusive lock, builds are in progress
                return true
            }

            // We got the exclusive lock, meaning no builds are running
            // Release it and return false
            flock(fd, LOCK_UN)
        #endif

        return false
    }

    private func acquireForBuild() throws -> Int32 {
        // Ensure directory exists
        let wendyDir = (lockPath as NSString).deletingLastPathComponent
        try FileManager.default.createDirectory(
            atPath: wendyDir,
            withIntermediateDirectories: true
        )

        // Open or create the lock file
        let fd = open(lockPath, O_CREAT | O_RDWR, 0o644)
        guard fd >= 0 else {
            throw BuildLockError.unableToCreateLock
        }

        #if os(macOS) || canImport(Glibc) || canImport(Musl)
            // Acquire a shared lock, multiple builds can hold this simultaneously
            let result = flock(fd, LOCK_SH)
            if result != 0 {
                close(fd)
                throw BuildLockError.unableToCreateLock
            }
        #endif

        return fd
    }

    private func release(fd: Int32) {
        #if os(macOS) || canImport(Glibc) || canImport(Musl)
            flock(fd, LOCK_UN)
        #endif
        close(fd)
    }
}

/// Errors related to build lock operations
public enum BuildLockError: Error, LocalizedError {
    case buildInProgress
    case unableToCreateLock

    public var errorDescription: String? {
        switch self {
        case .buildInProgress:
            return "Build in progress, please try again when it finishes"
        case .unableToCreateLock:
            return "Unable to create build lock file"
        }
    }
}

/// Represents the Docker CLI interface for managing container images and running containers.
public struct DockerCLI: Sendable {
    public let command: String
    private let logger = Logger(label: "sh.wendy.docker")

    public init(command: String = "docker") {
        self.command = command
    }

    public func getServerVersion() async throws -> String {
        let arguments = ["info", "--format", "{{.ServerVersion}}"]
        let result = try await Subprocess.run(
            Subprocess.Executable.name(self.command),
            arguments: Subprocess.Arguments(arguments),
            output: .string(limit: 1000, encoding: UTF8.self),
            error: .discarded
        )

        guard
            result.terminationStatus.isSuccess,
            let output = result.standardOutput
        else {
            let exitCode: Int
            switch result.terminationStatus {
            case .exited(let code), .unhandledException(let code):
                exitCode = Int(code)
            }
            throw SubprocessError(
                command: arguments.description,
                exitCode: exitCode,
                output: "",
                error: ""
            )
        }

        return output.trimmingCharacters(in: .whitespacesAndNewlines)
    }

    /// Build a Docker container.
    public func build(
        name: String,
        directory: String = "."
    ) async throws {
        let arguments = ["build", "-t", name, directory]
        let result = try await Subprocess.run(
            Subprocess.Executable.name(self.command),
            arguments: Subprocess.Arguments(arguments),
            output: .fileDescriptor(.standardOutput, closeAfterSpawningProcess: false),
            error: .fileDescriptor(.standardError, closeAfterSpawningProcess: false)
        )

        guard result.terminationStatus.isSuccess else {
            let exitCode: Int
            switch result.terminationStatus {
            case .exited(let code), .unhandledException(let code):
                exitCode = Int(code)
            }
            throw SubprocessError(
                command: ([self.command] + arguments).joined(separator: " "),
                exitCode: exitCode,
                output: "",
                error: ""
            )
        }
    }

    public func buildx(
        name: String,
        directory: String = ".",
        port: Int = 5000
    ) async throws {
        let arguments = [
            "buildx", "build", "--platform", "linux/arm64", "-t",
            "localhost:\(port)/\(name):latest", directory,
        ]
        let result = try await Subprocess.run(
            Subprocess.Executable.name(self.command),
            arguments: Subprocess.Arguments(arguments),
            output: .fileDescriptor(.standardOutput, closeAfterSpawningProcess: false),
            error: .fileDescriptor(.standardError, closeAfterSpawningProcess: false)
        )

        guard result.terminationStatus.isSuccess else {
            let exitCode: Int
            switch result.terminationStatus {
            case .exited(let code), .unhandledException(let code):
                exitCode = Int(code)
            }
            throw SubprocessError(
                command: ([self.command] + arguments).joined(separator: " "),
                exitCode: exitCode,
                output: "",
                error: ""
            )
        }
    }

    func push(
        name: String,
        port: Int = 5000
    ) async throws {
        let arguments = ["push", "localhost:\(port)/\(name):latest"]
        let result = try await Subprocess.run(
            Subprocess.Executable.name(self.command),
            arguments: Subprocess.Arguments(arguments),
            output: .fileDescriptor(.standardOutput, closeAfterSpawningProcess: false),
            error: .fileDescriptor(.standardError, closeAfterSpawningProcess: false)
        )

        guard result.terminationStatus.isSuccess else {
            let exitCode: Int
            switch result.terminationStatus {
            case .exited(let code), .unhandledException(let code):
                exitCode = Int(code)
            }
            throw SubprocessError(
                command: ([self.command] + arguments).joined(separator: " "),
                exitCode: exitCode,
                output: "",
                error: ""
            )
        }
    }

    /// Build and push a Docker container in a single operation using buildx.
    /// This is more efficient than separate build and push as it streams layers directly to the registry.
    /// Uses host.docker.internal:PORT for cross-platform support (works on both Linux and Docker Desktop).
    /// Disables provenance and SBOM to ensure Docker v2 manifest format (not OCI image index).
    /// Uses registry cache to preserve build cache across builder recreations.
    /// Acquires a build lock to prevent parallel builds from interfering with builder restarts.
    public func buildxAndPush(
        name: String,
        directory: String = ".",
        registryHostname: String = "host.docker.internal",
        registryPort: Int = 5000,
        compression: ImageCompressionMode = .zstd,
        onOutput: @escaping @Sendable (String) async throws -> Void
    ) async throws {
        // Acquire shared build lock, allows parallel builds but prevents builder restarts
        try await BuildLock.shared.withLock {
            // Build the --output flag based on compression mode
            // Using OCI media types is required for zstd compression
            let outputFlag: String
            switch compression {
            case .zstd:
                outputFlag = "type=image,push=true,compression=zstd,oci-mediatypes=true"
            case .gzip:
                outputFlag = "type=image,push=true,compression=gzip"
            case .uncompressed:
                outputFlag = "type=image,push=true,compression=uncompressed,oci-mediatypes=true"
            }

            let arguments = [
                "buildx", "build",
                "--builder", self.defaultBuilderName,
                "--platform", "linux/arm64",
                "--provenance=false",
                "--sbom=false",
                "--output", outputFlag,
                "-t", "\(registryHostname):\(registryPort)/\(name):latest",
                directory,
            ]

            try await run(
                executable: .name(self.command),
                arguments: Subprocess.Arguments(arguments)
            ) { string in
                try await onOutput(string)
            }
        }
    }

    /// Returns the current Docker context name
    public func currentContext() async -> String? {
        let arguments = ["context", "show"]
        do {
            let result = try await Subprocess.run(
                Subprocess.Executable.name(self.command),
                arguments: Subprocess.Arguments(arguments),
                output: .string(limit: 1000, encoding: UTF8.self),
                error: .discarded
            )
            guard result.terminationStatus.isSuccess,
                let output = result.standardOutput
            else {
                return nil
            }
            return output.trimmingCharacters(in: .whitespacesAndNewlines)
        } catch {
            return nil
        }
    }

    public func listBuildxBuilders() async throws -> [String] {
        let arguments = ["buildx", "ls", "--format", "{{.Name}}"]
        let result = try await Subprocess.run(
            Subprocess.Executable.name(self.command),
            arguments: Subprocess.Arguments(arguments),
            output: .string(limit: 100_000, encoding: UTF8.self),
            error: .fileDescriptor(.standardError, closeAfterSpawningProcess: false)
        )

        guard
            result.terminationStatus.isSuccess,
            let output = result.standardOutput
        else {
            let exitCode: Int
            switch result.terminationStatus {
            case .exited(let code), .unhandledException(let code):
                exitCode = Int(code)
            }
            throw SubprocessError(
                command: ([self.command] + arguments).joined(separator: " "),
                exitCode: exitCode,
                output: "",
                error: ""
            )
        }

        return output.split(separator: "\n").map { String($0) }
    }

    public var defaultBuilderName: String {
        return "wendy-builder"
    }

    public func hasBuildxBuilder(builderName: String) async throws -> Bool {
        let builders = try await listBuildxBuilders()
        return builders.contains(builderName)
    }

    /// Creates a buildx builder with insecure registry support for the specified port.
    /// Returns the name of the created builder.
    public func prepareBuildxBuilder(
        registryHostname: String,
        registryPort: Int
    ) async throws {
        let wendyDir = FileManager.default.homeDirectoryForCurrentUser
            .appendingPathComponent(".wendy")

        try FileManager.default.createDirectory(at: wendyDir, withIntermediateDirectories: true)

        let configPath =
            wendyDir
            .appendingPathComponent("buildkit-config.toml")
            .path

        // Parse existing config or create new one using structured TOML handling
        var config = BuildkitConfig.loadOrCreate(from: configPath)

        // Add registry if not already configured
        if !config.hasRegistry(hostname: registryHostname, port: registryPort) {
            config.addRegistry(hostname: registryHostname, port: registryPort)
            let tomlOutput = config.toTOML()
            try tomlOutput.write(toFile: configPath, atomically: true, encoding: .utf8)
        }

        // Read back for comparison (ensures consistent encoding from TOMLKit)
        let desiredConfig = try String(contentsOfFile: configPath, encoding: .utf8)

        if try await hasBuildxBuilder(builderName: defaultBuilderName) {
            let containerName = "buildx_buildkit_\(defaultBuilderName)0"

            // Check if the builder container is actually running
            // containerNotFound is treated as "not running" since the builder exists but container may not
            var isRunning: Bool
            do {
                isRunning = try await isContainerRunning(containerName: containerName)
            } catch DockerError.containerNotFound {
                isRunning = false
            }

            if !isRunning {
                // If it isn't, start it with bootstrap
                let bootstrapArguments: Subprocess.Arguments = [
                    "buildx", "inspect", "--bootstrap", defaultBuilderName,
                ]
                let bootstrapResult = try await Subprocess.run(
                    Subprocess.Executable.name(self.command),
                    arguments: bootstrapArguments,
                    output: .discarded,
                    error: .discarded
                )

                // Check if the container is now running after bootstrap
                do {
                    isRunning = try await isContainerRunning(containerName: containerName)
                } catch {
                    isRunning = false
                }

                // If bootstrap failed OR the container still doesn't exist, the builder
                // is likely orphaned (pointing to a dead Docker context like desktop-linux
                // when the user switched to OrbStack). Remove it and recreate.
                if !bootstrapResult.terminationStatus.isSuccess || !isRunning {
                    try? await removeBuildxBuilder(name: defaultBuilderName)
                    return try await createBuildxBuilder(configPath: configPath)
                }
            }

            // Get the container's current config
            // fileNotFound means config needs to be updated
            let containerConfig: String?
            do {
                containerConfig = try await getContainerFileContents(
                    containerName: containerName,
                    filePath: "/etc/buildkit/buildkitd.toml"
                )
            } catch DockerError.fileNotFound {
                containerConfig = nil
            }

            if containerConfig == desiredConfig {
                return
            }

            // Config needs updating, check if any builds are in progress before modifying
            if BuildLock.shared.isBuildInProgress() {
                throw BuildLockError.buildInProgress
            }

            // Copy the updated config to the container
            let cpArguments: Subprocess.Arguments = [
                "cp",
                configPath,
                "\(containerName):/etc/buildkit/buildkitd.toml",
            ]
            let cpResult = try await Subprocess.run(
                Subprocess.Executable.name(self.command),
                arguments: cpArguments,
                output: .fileDescriptor(.standardOutput, closeAfterSpawningProcess: false),
                error: .fileDescriptor(.standardError, closeAfterSpawningProcess: false)
            )

            guard cpResult.terminationStatus.isSuccess else {
                let exitCode: Int
                switch cpResult.terminationStatus {
                case .exited(let code), .unhandledException(let code):
                    exitCode = Int(code)
                }
                throw SubprocessError(
                    command: cpArguments.description,
                    exitCode: exitCode,
                    output: "",
                    error: ""
                )
            }

            // Restart to apply the new config
            let restartArguments: Subprocess.Arguments = [
                "restart", containerName,
            ]
            let restartResult = try await Subprocess.run(
                Subprocess.Executable.name(self.command),
                arguments: restartArguments,
                output: .fileDescriptor(.standardOutput, closeAfterSpawningProcess: false),
                error: .fileDescriptor(.standardError, closeAfterSpawningProcess: false)
            )

            guard restartResult.terminationStatus.isSuccess else {
                let exitCode: Int
                switch restartResult.terminationStatus {
                case .exited(let code), .unhandledException(let code):
                    exitCode = Int(code)
                }
                throw SubprocessError(
                    command: restartArguments.description,
                    exitCode: exitCode,
                    output: "",
                    error: ""
                )
            }

            return
        }

        try await createBuildxBuilder(configPath: configPath)
    }

    /// Creates a new buildx builder with the given configuration file.
    private func createBuildxBuilder(configPath: String) async throws {
        let createArguments: Subprocess.Arguments = [
            "buildx", "create",
            "--name", defaultBuilderName,
            "--driver", "docker-container",
            "--config", configPath,
            "--bootstrap",  // Start the builder immediately to load config
        ]

        let createResult = try await Subprocess.run(
            Subprocess.Executable.name(self.command),
            arguments: createArguments,
            output: .fileDescriptor(.standardOutput, closeAfterSpawningProcess: false),
            error: .fileDescriptor(.standardError, closeAfterSpawningProcess: false)
        )

        guard createResult.terminationStatus.isSuccess else {
            let exitCode: Int
            switch createResult.terminationStatus {
            case .exited(let code), .unhandledException(let code):
                exitCode = Int(code)
            }
            throw SubprocessError(
                command: createArguments.description,
                exitCode: exitCode,
                output: "",
                error: ""
            )
        }
    }

    /// Checks if a Docker container is currently running
    private func isContainerRunning(containerName: String) async throws -> Bool {
        let arguments = ["inspect", "-f", "{{.State.Running}}", containerName]
        let result:
            Subprocess.CollectedResult<
                Subprocess.StringOutput<UTF8>, Subprocess.StringOutput<UTF8>
            >
        do {
            result = try await Subprocess.run(
                Subprocess.Executable.name(self.command),
                arguments: Subprocess.Arguments(arguments),
                output: .string(limit: 100, encoding: UTF8.self),
                error: .string(limit: 10_000, encoding: UTF8.self)
            )
        } catch {
            logger.error(
                "Failed to execute docker inspect",
                metadata: [
                    "container": "\(containerName)",
                    "error": "\(error.localizedDescription)",
                ]
            )
            throw DockerError.daemonUnavailable(underlyingError: error.localizedDescription)
        }

        let stderr =
            result.standardError?.trimmingCharacters(in: CharacterSet.whitespacesAndNewlines) ?? ""

        guard result.terminationStatus.isSuccess else {
            let exitCode: Int
            switch result.terminationStatus {
            case .exited(let code), .unhandledException(let code):
                exitCode = Int(code)
            }

            // Parse stderr to determine the specific error type
            if stderr.contains("No such object") || stderr.contains("not found") {
                logger.debug(
                    "Container not found",
                    metadata: ["container": "\(containerName)"]
                )
                throw DockerError.containerNotFound(containerName: containerName)
            } else if stderr.contains("permission denied") || stderr.contains("Permission denied") {
                logger.warning(
                    "Permission denied accessing Docker",
                    metadata: [
                        "container": "\(containerName)",
                        "stderr": "\(stderr)",
                    ]
                )
                throw DockerError.permissionDenied(
                    operation: "inspect container '\(containerName)'"
                )
            } else if stderr.contains("Cannot connect to the Docker daemon")
                || stderr.contains("Is the docker daemon running")
            {
                logger.error(
                    "Docker daemon unavailable",
                    metadata: ["stderr": "\(stderr)"]
                )
                throw DockerError.daemonUnavailable(underlyingError: stderr)
            } else {
                logger.warning(
                    "Docker inspect failed",
                    metadata: [
                        "container": "\(containerName)",
                        "exitCode": "\(exitCode)",
                        "stderr": "\(stderr)",
                    ]
                )
                throw DockerError.commandFailed(
                    command: "docker inspect",
                    exitCode: exitCode,
                    stderr: stderr
                )
            }
        }

        guard let output = result.standardOutput else {
            logger.warning(
                "Docker inspect returned no output",
                metadata: ["container": "\(containerName)"]
            )
            return false
        }

        return output.trimmingCharacters(in: CharacterSet.whitespacesAndNewlines) == "true"
    }

    /// Gets the contents of a file inside a Docker container using docker exec cat
    private func getContainerFileContents(
        containerName: String,
        filePath: String
    ) async throws -> String {
        let arguments = ["exec", containerName, "cat", filePath]
        let result:
            Subprocess.CollectedResult<
                Subprocess.StringOutput<UTF8>, Subprocess.StringOutput<UTF8>
            >
        do {
            result = try await Subprocess.run(
                Subprocess.Executable.name(self.command),
                arguments: Subprocess.Arguments(arguments),
                output: .string(limit: 100_000, encoding: UTF8.self),
                error: .string(limit: 10_000, encoding: UTF8.self)
            )
        } catch {
            logger.error(
                "Failed to execute docker exec",
                metadata: [
                    "container": "\(containerName)",
                    "filePath": "\(filePath)",
                    "error": "\(error.localizedDescription)",
                ]
            )
            throw DockerError.daemonUnavailable(underlyingError: error.localizedDescription)
        }

        let stderr =
            result.standardError?.trimmingCharacters(in: CharacterSet.whitespacesAndNewlines) ?? ""

        guard result.terminationStatus.isSuccess else {
            let exitCode: Int
            switch result.terminationStatus {
            case .exited(let code), .unhandledException(let code):
                exitCode = Int(code)
            }

            // Parse stderr to determine the specific error type
            if stderr.contains("No such container") || stderr.contains("not found") {
                logger.debug(
                    "Container not found for file read",
                    metadata: [
                        "container": "\(containerName)",
                        "filePath": "\(filePath)",
                    ]
                )
                throw DockerError.containerNotFound(containerName: containerName)
            } else if stderr.contains("is not running") {
                logger.debug(
                    "Container not running for file read",
                    metadata: [
                        "container": "\(containerName)",
                        "filePath": "\(filePath)",
                    ]
                )
                throw DockerError.containerNotRunning(containerName: containerName)
            } else if stderr.contains("No such file or directory") {
                logger.debug(
                    "File not found in container",
                    metadata: [
                        "container": "\(containerName)",
                        "filePath": "\(filePath)",
                    ]
                )
                throw DockerError.fileNotFound(containerName: containerName, filePath: filePath)
            } else if stderr.contains("permission denied") || stderr.contains("Permission denied") {
                logger.warning(
                    "Permission denied reading file from container",
                    metadata: [
                        "container": "\(containerName)",
                        "filePath": "\(filePath)",
                        "stderr": "\(stderr)",
                    ]
                )
                throw DockerError.permissionDenied(
                    operation: "read file '\(filePath)' from container '\(containerName)'"
                )
            } else if stderr.contains("Cannot connect to the Docker daemon")
                || stderr.contains("Is the docker daemon running")
            {
                logger.error(
                    "Docker daemon unavailable",
                    metadata: ["stderr": "\(stderr)"]
                )
                throw DockerError.daemonUnavailable(underlyingError: stderr)
            } else {
                logger.warning(
                    "Docker exec cat failed",
                    metadata: [
                        "container": "\(containerName)",
                        "filePath": "\(filePath)",
                        "exitCode": "\(exitCode)",
                        "stderr": "\(stderr)",
                    ]
                )
                throw DockerError.commandFailed(
                    command: "docker exec cat",
                    exitCode: exitCode,
                    stderr: stderr
                )
            }
        }

        guard let output = result.standardOutput else {
            logger.warning(
                "Docker exec cat returned no output",
                metadata: [
                    "container": "\(containerName)",
                    "filePath": "\(filePath)",
                ]
            )
            return ""
        }

        return output
    }

    /// Removes a buildx builder.
    public func removeBuildxBuilder(
        name: String
    ) async throws {
        let arguments = ["buildx", "rm", name]

        let result = try await Subprocess.run(
            Subprocess.Executable.name(self.command),
            arguments: Subprocess.Arguments(arguments),
            output: .fileDescriptor(.standardOutput, closeAfterSpawningProcess: false),
            error: .fileDescriptor(.standardError, closeAfterSpawningProcess: false)
        )

        guard result.terminationStatus.isSuccess else {
            let exitCode: Int
            switch result.terminationStatus {
            case .exited(let code), .unhandledException(let code):
                exitCode = Int(code)
            }
            throw SubprocessError(
                command: ([self.command] + arguments).joined(separator: " "),
                exitCode: exitCode,
                output: "",
                error: ""
            )
        }
    }

    /// Export a Docker container.
    public func save(
        name: String,
        output: String
    ) async throws {
        _ = try await Subprocess.run(
            Subprocess.Executable.name(self.command),
            arguments: Subprocess.Arguments(["save", name, "-o", output]),
            output: .discarded
        )
    }

}
