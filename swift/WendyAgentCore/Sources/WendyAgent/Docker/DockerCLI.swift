import Foundation
import Logging

/// A thin wrapper around the `docker` CLI for managing containers and images.
///
/// Adapted from the legacy Swift agent's DockerCLI. Uses Foundation.Process
/// instead of the Subprocess package since the Mac prototype doesn't depend on
/// swift-subprocess.
struct DockerCLI: Sendable {
    private let logger = Logger(label: "sh.wendy.agent.docker-cli")

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
            _ = try await run(arguments: ["version", "--format", "{{.Server.Version}}"])
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

    /// Ensures a local Docker registry container is running on the registry port.
    /// If one named `wendy-registry` already exists and is running, this is a no-op.
    func ensureRegistry() async throws {
        // Check if the registry container is already running.
        let psOutput = try await run(arguments: [
            "ps", "--filter", "name=wendy-registry", "--format", "{{.Status}}",
        ])
        if psOutput.contains("Up") {
            return
        }

        // Remove stale container if it exists but isn't running.
        _ = try? await run(arguments: ["rm", "-f", "wendy-registry"])

        _ = try await run(arguments: [
            "run", "-d",
            "-p", "\(Self.registryPort):5000",
            "--name", "wendy-registry",
            "--restart", "unless-stopped",
            "registry:2",
        ])
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
        command: [String] = []
    ) throws -> (process: Foundation.Process, stdout: Pipe, stderr: Pipe) {
        let allArgs = ["run"] + options.flatMap(\.arguments) + [image] + command

        let process = Foundation.Process()
        process.executableURL = URL(fileURLWithPath: "/usr/bin/env")
        process.arguments = ["docker"] + allArgs

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
        return output
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
    private func run(arguments: [String]) async throws -> String {
        let process = Foundation.Process()
        process.executableURL = URL(fileURLWithPath: "/usr/bin/env")
        process.arguments = ["docker"] + arguments

        let stdoutPipe = Pipe()
        let stderrPipe = Pipe()
        process.standardOutput = stdoutPipe
        process.standardError = stderrPipe

        return try await withCheckedThrowingContinuation { continuation in
            process.terminationHandler = { p in
                let stdout = String(
                    data: stdoutPipe.fileHandleForReading.readDataToEndOfFile(),
                    encoding: .utf8
                )?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""

                if p.terminationStatus == 0 {
                    continuation.resume(returning: stdout)
                } else {
                    let stderr = String(
                        data: stderrPipe.fileHandleForReading.readDataToEndOfFile(),
                        encoding: .utf8
                    )?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
                    continuation.resume(
                        throwing: DockerError.commandFailed(
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
}

enum DockerError: Error, CustomStringConvertible {
    case commandFailed(args: [String], status: Int32, stderr: String)

    var description: String {
        switch self {
        case .commandFailed(let args, let status, let stderr):
            let cmd = (["docker"] + args).joined(separator: " ")
            if stderr.isEmpty {
                return "\(cmd) exited with status \(status)"
            }
            return "\(cmd) exited with status \(status): \(stderr)"
        }
    }
}
