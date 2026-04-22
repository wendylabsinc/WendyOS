import Foundation
import Logging
import Subprocess

/// A thin wrapper around the `docker` CLI for managing containers and images.
///
/// One-shot commands run via `swift-subprocess`, which drains stdout and stderr
/// concurrently while the child process is running rather than buffering the
/// full output at termination. Attached container runs still use
/// `Foundation.Process` today (see ``runAttached(options:image:command:terminationHandler:)``)
/// and will be migrated in a follow-up.
struct DockerCLI: Sendable {
    private let logger = Logger(label: "sh.wendy.agent.docker-cli")
    private let executable: String
    private let startupCommandTimeout: Duration

    init(
        executable: String = "docker",
        startupCommandTimeout: Duration = Self.defaultStartupCommandTimeout
    ) {
        self.executable = executable
        self.startupCommandTimeout = startupCommandTimeout
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

    /// Returns `true` if the `docker` CLI is functional.
    func checkAvailable() async -> Bool {
        do {
            _ = try await run(
                arguments: ["version", "--format", "{{.Server.Version}}"],
                timeout: self.startupCommandTimeout
            )
            return true
        } catch {
            return false
        }
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

        let process = Foundation.Process()
        process.executableURL = URL(fileURLWithPath: "/usr/bin/env")
        process.arguments = [self.executable] + allArgs
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

    /// Upper bound on collected stdout/stderr bytes for a single one-shot command.
    ///
    /// `swift-subprocess` drains output concurrently while the child runs, so
    /// this cap only exists to keep us from accumulating unbounded output in
    /// memory if a docker command misbehaves. The limit is intentionally high
    /// because outputs like `docker pull` progress or `docker ps` listings are
    /// still expected to fit comfortably. If we ever need to stream truly
    /// unbounded output we should use the streaming body variant directly
    /// instead of raising this further.
    private static let maxCollectedOutputBytes = 16 * 1024 * 1024

    /// Run a docker command and return its stdout as a trimmed string.
    ///
    /// Implementation notes:
    /// - Uses `swift-subprocess`, which reads stdout/stderr concurrently while
    ///   the child runs instead of blocking on `readDataToEndOfFile()` at
    ///   termination. This avoids both the stdout-deadlock class of bugs and
    ///   the "buffer everything until exit" pattern.
    /// - Output is bounded by ``maxCollectedOutputBytes``; exceeding the cap
    ///   is surfaced as a `commandFailed` so callers see it the same way as
    ///   any other docker failure.
    /// - When `timeout` fires we cancel the subprocess task, which triggers
    ///   `swift-subprocess`'s teardown sequence (graceful signal followed by
    ///   kill) rather than a single bare terminate.
    @discardableResult
    private func run(arguments: [String], timeout: Duration? = nil) async throws -> String {
        let executable = self.executable
        let runCommand: @Sendable () async throws -> String = {
            let record: ExecutionRecord<StringOutput<UTF8>, StringOutput<UTF8>>
            do {
                record = try await Subprocess.run(
                    .name(executable),
                    arguments: Arguments(arguments),
                    output: .string(limit: Self.maxCollectedOutputBytes),
                    error: .string(limit: Self.maxCollectedOutputBytes)
                )
            } catch let error as SubprocessError {
                throw DockerError.commandFailed(
                    executable: executable,
                    args: arguments,
                    status: -1,
                    stderr: String(describing: error)
                )
            }

            let stdout = (record.standardOutput ?? "")
                .trimmingCharacters(in: .whitespacesAndNewlines)

            switch record.terminationStatus {
            case .exited(0):
                return stdout
            case .exited(let code):
                let stderr = (record.standardError ?? "")
                    .trimmingCharacters(in: .whitespacesAndNewlines)
                throw DockerError.commandFailed(
                    executable: executable,
                    args: arguments,
                    status: Int32(code),
                    stderr: stderr
                )
            case .signaled(let signal):
                let stderr = (record.standardError ?? "")
                    .trimmingCharacters(in: .whitespacesAndNewlines)
                throw DockerError.commandFailed(
                    executable: executable,
                    args: arguments,
                    status: -Int32(signal),
                    stderr: stderr
                )
            }
        }

        guard let timeout else {
            return try await runCommand()
        }

        do {
            return try await withThrowingTaskGroup(of: String.self) { group in
                group.addTask { try await runCommand() }
                group.addTask {
                    try await Task.sleep(for: timeout)
                    throw DockerError.commandTimedOut(
                        executable: executable,
                        args: arguments,
                        timeout: timeout
                    )
                }

                defer { group.cancelAll() }

                guard let result = try await group.next() else {
                    throw DockerError.commandFailed(
                        executable: executable,
                        args: arguments,
                        status: -1,
                        stderr: "docker command did not produce a result"
                    )
                }
                return result
            }
        } catch {
            if case .commandTimedOut = error as? DockerError {
                self.logger.warning(
                    "Docker command timed out",
                    metadata: [
                        "command": "\(([executable] + arguments).joined(separator: " "))",
                        "timeout": "\(timeout)",
                    ]
                )
            }
            throw error
        }
    }
}

enum DockerError: Error, CustomStringConvertible {
    case commandFailed(executable: String, args: [String], status: Int32, stderr: String)
    case commandTimedOut(executable: String, args: [String], timeout: Duration)

    var description: String {
        switch self {
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
