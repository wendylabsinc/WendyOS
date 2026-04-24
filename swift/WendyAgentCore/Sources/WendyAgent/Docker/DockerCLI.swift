import Foundation
import Logging

/// A thin wrapper around the `docker` CLI for managing containers and images.
///
/// Adapted from the legacy Swift agent's DockerCLI. Uses Foundation.Process
/// instead of the Subprocess package since the Mac prototype doesn't depend on
/// swift-subprocess.
struct DockerCLI: Sendable {
    struct AvailabilityCheckResult: Sendable {
        let isAvailable: Bool
        let failureMessage: String?
    }

    private struct ExecutableResolution: Sendable {
        let requested: String
        let resolvedPath: String?
        let searchedPaths: [String]
    }

    private let logger = Logger(label: "sh.wendy.agent.docker-cli")
    private let executable: String
    private let startupCommandTimeout: Duration
    private let environment: [String: String]

    init(
        executable: String = "docker",
        startupCommandTimeout: Duration = Self.defaultStartupCommandTimeout,
        environment: [String: String] = ProcessInfo.processInfo.environment
    ) {
        self.executable = executable
        self.startupCommandTimeout = startupCommandTimeout
        self.environment = environment
    }

    // MARK: - Run options

    /// Options for `docker run`.
    enum RunOption: Sendable {
        case rm
        case interactive
        case tty
        case detach
        case name(String)
        case label(key: String, value: String)
        case publish(hostPort: UInt16, containerPort: UInt16)
        case volume(hostOrName: String, containerPath: String)
        case env(key: String, value: String)
        case network(String)
        case restartUnlessStopped
        case restartNo
        case restartOnFailure(Int)

        var arguments: [String] {
            switch self {
            case .rm: ["--rm"]
            case .interactive: ["-i"]
            case .tty: ["-t"]
            case .detach: ["--detach"]
            case .name(let n): ["--name", n]
            case .label(let k, let v): ["--label", "\(k)=\(v)"]
            case .publish(let h, let c): ["-p", "\(h):\(c)"]
            case .volume(let src, let dst): ["-v", "\(src):\(dst)"]
            case .env(let k, let v): ["-e", "\(k)=\(v)"]
            case .network(let n): ["--network", n]
            case .restartUnlessStopped: ["--restart", "unless-stopped"]
            case .restartNo: ["--restart", "no"]
            case .restartOnFailure(let n): ["--restart", "on-failure:\(n)"]
            }
        }
    }

    /// Options for `docker rm`.
    enum RmOption: Sendable {
        case force
        var arguments: [String] {
            switch self {
            case .force: ["--force"]
            }
        }
    }

    // MARK: - Availability

    /// Probes whether the `docker` CLI is functional and returns a diagnostic
    /// message on failure.
    func checkAvailability() async -> AvailabilityCheckResult {
        do {
            _ = try await run(
                arguments: ["version", "--format", "{{.Server.Version}}"],
                timeout: self.startupCommandTimeout
            )
            return AvailabilityCheckResult(isAvailable: true, failureMessage: nil)
        } catch {
            return AvailabilityCheckResult(
                isAvailable: false,
                failureMessage: String(describing: error)
            )
        }
    }

    /// Returns `true` if the `docker` CLI is functional.
    func checkAvailable() async -> Bool {
        await self.checkAvailability().isAvailable
    }

    // MARK: - Registry

    /// The host port for the local Docker registry.
    /// Uses 5555 instead of the default 5000 to avoid conflicts with macOS
    /// AirPlay Receiver, which binds *:5000 by default on every Mac.
    static let registryPort: UInt16 = 5555
    private static let defaultStartupCommandTimeout: Duration = .seconds(5)

    /// Ensures a local Docker registry container is running on the registry port.
    /// If one named `wendy-registry` already exists and is running, this is a no-op.
    func ensureRegistry() async throws {
        // Check if the registry container is already running.
        let psOutput = try await run(
            arguments: [
                "ps", "--filter", "name=wendy-registry", "--format", "{{.Status}}",
            ],
            timeout: self.startupCommandTimeout
        )
        if psOutput.contains("Up") {
            return
        }

        // Remove stale container if it exists but isn't running.
        _ = try? await run(
            arguments: ["rm", "-f", "wendy-registry"],
            timeout: self.startupCommandTimeout
        )

        _ = try await run(
            arguments: [
                "run", "-d",
                "-p", "\(Self.registryPort):5000",
                "--name", "wendy-registry",
                "--restart", "unless-stopped",
                "registry:2",
            ],
            timeout: self.startupCommandTimeout
        )
    }

    // MARK: - Image operations

    /// Pull an image from a registry.
    @discardableResult
    func pull(image: String) async throws -> String {
        try await run(arguments: ["pull", image])
    }

    // MARK: - Container lifecycle

    /// Run a container in **attached mode** (not detached). Returns the Process
    /// and its stdout/stderr pipes so the caller can stream output.
    func runAttached(
        options: [RunOption],
        image: String,
        command: [String] = [],
        terminationHandler: (@Sendable (Foundation.Process) -> Void)? = nil
    ) throws -> (process: Foundation.Process, stdout: Pipe, stderr: Pipe) {
        let allArgs = ["run"] + options.flatMap(\.arguments) + [image] + command

        let resolvedExecutable = try self.resolveExecutablePath()

        let process = Foundation.Process()
        process.executableURL = URL(fileURLWithPath: resolvedExecutable)
        process.arguments = allArgs
        process.environment = self.makeProcessEnvironment(resolvedExecutable: resolvedExecutable)
        process.terminationHandler = terminationHandler

        let stdoutPipe = Pipe()
        let stderrPipe = Pipe()
        process.standardOutput = stdoutPipe
        process.standardError = stderrPipe

        try process.run()
        return (process, stdoutPipe, stderrPipe)
    }

    /// Stop a running container.
    @discardableResult
    func stop(container: String, timeout: Int? = nil) async throws -> String {
        var args = ["stop"]
        if let timeout {
            args += ["--time", String(timeout)]
        }
        args.append(container)
        return try await run(arguments: args)
    }

    /// Remove a container.
    @discardableResult
    func rm(options: [RmOption] = [], container: String) async throws -> String {
        let args = ["rm"] + options.flatMap(\.arguments) + [container]
        return try await run(arguments: args)
    }

    // MARK: - Listing

    /// Parsed container info from `docker ps`.
    struct ContainerInfo: Sendable {
        let id: String
        let names: String
        let state: String
        let status: String
    }

    /// List containers matching a label filter.
    func ps(label: String) async throws -> [ContainerInfo] {
        let output = try await run(arguments: [
            "ps", "-a",
            "--filter", "label=\(label)",
            "--format", "{{.ID}}\t{{.Names}}\t{{.State}}\t{{.Status}}",
        ])
        return
            output
            .split(separator: "\n")
            .compactMap { line -> ContainerInfo? in
                let cols = line.split(separator: "\t", maxSplits: 3).map(String.init)
                guard cols.count == 4 else { return nil }
                return ContainerInfo(id: cols[0], names: cols[1], state: cols[2], status: cols[3])
            }
    }

    // MARK: - Private

    /// Run a docker command and return its stdout as a trimmed string.
    @discardableResult
    private func run(arguments: [String], timeout: Duration? = nil) async throws -> String {
        let resolvedExecutable = try self.resolveExecutablePath()

        let process = Foundation.Process()
        process.executableURL = URL(fileURLWithPath: resolvedExecutable)
        process.arguments = arguments
        process.environment = self.makeProcessEnvironment(resolvedExecutable: resolvedExecutable)

        let stdoutPipe = Pipe()
        let stderrPipe = Pipe()
        process.standardOutput = stdoutPipe
        process.standardError = stderrPipe

        let resultTask = Task<String, Error> {
            try await withCheckedThrowingContinuation { continuation in
                process.terminationHandler = { p in
                    let stdout =
                        String(
                            data: stdoutPipe.fileHandleForReading.readDataToEndOfFile(),
                            encoding: .utf8
                        )?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""

                    if p.terminationStatus == 0 {
                        continuation.resume(returning: stdout)
                    } else {
                        let stderr =
                            String(
                                data: stderrPipe.fileHandleForReading.readDataToEndOfFile(),
                                encoding: .utf8
                            )?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
                        continuation.resume(
                            throwing: DockerError.commandFailed(
                                executable: resolvedExecutable,
                                args: arguments,
                                status: p.terminationStatus,
                                stderr: stderr
                            )
                        )
                    }
                }

                do {
                    try process.run()
                } catch {
                    continuation.resume(throwing: error)
                }
            }
        }

        do {
            guard let timeout else {
                return try await resultTask.value
            }

            return try await withThrowingTaskGroup(of: String.self) { group in
                group.addTask {
                    try await resultTask.value
                }
                group.addTask {
                    try await Task.sleep(for: timeout)
                    throw DockerError.commandTimedOut(
                        executable: resolvedExecutable,
                        args: arguments,
                        timeout: timeout
                    )
                }

                guard let result = try await group.next() else {
                    throw DockerError.commandFailed(
                        executable: resolvedExecutable,
                        args: arguments,
                        status: -1,
                        stderr: "docker command did not produce a result"
                    )
                }
                group.cancelAll()
                return result
            }
        } catch {
            if case .commandTimedOut = error as? DockerError {
                self.logger.warning(
                    "Docker command timed out",
                    metadata: [
                        "command": "\(([resolvedExecutable] + arguments).joined(separator: " "))",
                        "timeout": "\(timeout.map { String(describing: $0) } ?? "none")",
                    ]
                )
            }

            if process.isRunning {
                process.terminate()
            }
            resultTask.cancel()
            _ = try? await resultTask.value
            throw error
        }
    }

    private func resolveExecutablePath() throws -> String {
        let resolution = self.resolveExecutable()
        if let resolvedPath = resolution.resolvedPath {
            return resolvedPath
        }
        throw DockerError.executableNotFound(
            executable: resolution.requested,
            searchedPaths: resolution.searchedPaths
        )
    }

    func resolveExecutableForTesting() -> (resolvedPath: String?, searchedPaths: [String]) {
        let resolution = self.resolveExecutable()
        return (resolution.resolvedPath, resolution.searchedPaths)
    }

    func processEnvironmentForTesting(resolvedExecutable: String) -> [String: String] {
        self.makeProcessEnvironment(resolvedExecutable: resolvedExecutable)
    }

    private func resolveExecutable() -> ExecutableResolution {
        if self.executable.contains("/") {
            let resolvedPath = FileManager.default.isExecutableFile(atPath: self.executable)
                ? self.executable : nil
            return ExecutableResolution(
                requested: self.executable,
                resolvedPath: resolvedPath,
                searchedPaths: [self.executable]
            )
        }

        let searchPaths = self.buildSearchPaths()
        let resolvedPath = searchPaths.first { FileManager.default.isExecutableFile(atPath: $0) }
        return ExecutableResolution(
            requested: self.executable,
            resolvedPath: resolvedPath,
            searchedPaths: searchPaths
        )
    }

    private func buildSearchPaths() -> [String] {
        let pathEntries = (self.environment["PATH"] ?? "")
            .split(separator: ":")
            .map(String.init)
            .filter { !$0.isEmpty }

        let candidates = pathEntries.map { URL(fileURLWithPath: $0).appendingPathComponent(self.executable).path }
            + Self.fallbackExecutablePaths(for: self.executable)

        var seen = Set<String>()
        var uniqueCandidates: [String] = []
        for candidate in candidates where seen.insert(candidate).inserted {
            uniqueCandidates.append(candidate)
        }
        return uniqueCandidates
    }

    private func makeProcessEnvironment(resolvedExecutable: String) -> [String: String] {
        var environment = self.environment

        let existingPathEntries = (environment["PATH"] ?? "")
            .split(separator: ":")
            .map(String.init)
            .filter { !$0.isEmpty }

        let helperDirectories = [
            URL(fileURLWithPath: resolvedExecutable).deletingLastPathComponent().path
        ] + Self.fallbackExecutablePaths(for: self.executable).map {
            URL(fileURLWithPath: $0).deletingLastPathComponent().path
        }

        var seen = Set<String>()
        var mergedPathEntries: [String] = []
        for pathEntry in helperDirectories + existingPathEntries where seen.insert(pathEntry).inserted {
            mergedPathEntries.append(pathEntry)
        }

        if !mergedPathEntries.isEmpty {
            environment["PATH"] = mergedPathEntries.joined(separator: ":")
        }

        return environment
    }

    private static func fallbackExecutablePaths(for executable: String) -> [String] {
        [
            "/usr/local/bin/\(executable)",
            "/opt/homebrew/bin/\(executable)",
            "/Applications/Docker.app/Contents/Resources/bin/\(executable)",
        ]
    }
}

enum DockerError: Error, CustomStringConvertible {
    case executableNotFound(executable: String, searchedPaths: [String])
    case commandFailed(executable: String, args: [String], status: Int32, stderr: String)
    case commandTimedOut(executable: String, args: [String], timeout: Duration)

    var description: String {
        switch self {
        case .executableNotFound(let executable, let searchedPaths):
            return
                "Could not find \(executable) executable. Looked in: \(searchedPaths.joined(separator: ", "))"
        case .commandFailed(let executable, let args, let status, let stderr):
            let cmd = ([executable] + args).joined(separator: " ")
            if stderr.isEmpty {
                return "\(cmd) exited with status \(status)"
            }
            return "\(cmd) exited with status \(status): \(stderr)"
        case .commandTimedOut(let executable, let args, let timeout):
            let cmd = ([executable] + args).joined(separator: " ")
            return "\(cmd) timed out after \(timeout)"
        }
    }
}
