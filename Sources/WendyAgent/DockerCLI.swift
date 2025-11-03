import Foundation
import Subprocess

/// Represents the Docker CLI interface for managing container images and running containers.
public struct DockerCLI: Sendable {
    public let command: String

    public init(command: String = "docker") {
        self.command = command
    }

    /// Options for the Docker run command.
    public enum RunOption: Sendable {
        /// Remove the container when it exits.
        case rm

        /// Keep STDIN open even if not attached.
        case interactive

        /// Allocate a pseudo-TTY.
        case tty

        /// Publish a container's port to the host.
        case publishPort(hostPort: UInt16, containerPort: UInt16)

        /// Add Linux capabilities.
        case capAdd(String)

        /// Security options.
        case securityOpt(String)

        /// Connect a container to a network
        case network(String)

        /// Assign a name to the container
        case name(String)

        /// Run container in background and print container ID
        case detach

        case device(String)

        case gpus(String)

        /// Give extended privileges to this container
        case privileged

        /// Set restart policy to unless-stopped
        case restartUnlessStopped
        /// Set restart policy to no
        case restartNo
        /// Set restart policy to on-failure with max retries
        case restartOnFailure(Int)

        /// The arguments to pass to the Docker run command.
        var arguments: [String] {
            switch self {
            case .rm:
                return ["--rm"]
            case .interactive:
                return ["-i"]
            case .tty:
                return ["-t"]
            case .publishPort(let hostPort, let containerPort):
                return ["-p", "\(hostPort):\(containerPort)"]
            case .capAdd(let capability):
                return ["--cap-add=\(capability)"]
            case .securityOpt(let option):
                return ["--security-opt", option]
            case .network(let network):
                return ["--network", network]
            case .name(let name):
                return ["--name", name]
            case .detach:
                return ["--detach"]
            case .device(let device):
                return ["--device", device]
            case .privileged:
                return ["--privileged"]
            case .restartUnlessStopped:
                return ["--restart", "unless-stopped"]
            case .restartNo:
                return ["--restart", "no"]
            case .restartOnFailure(let retries):
                return ["--restart", "on-failure:\(retries)"]
            case .gpus(let gpus):
                return ["--gpus", gpus]
            }
        }
    }

    /// Options for the Docker rm command.
    public enum RmOption: Sendable {
        /// Force the removal of a running container (uses SIGKILL).
        case force

        /// The arguments to pass to the Docker rm command.
        var arguments: [String] {
            switch self {
            case .force:
                return ["--force"]
            }
        }
    }

    /// Load a Docker image from a tar archive.
    @discardableResult
    public func load(filePath: String) async throws -> String {
        let result = try await Subprocess.run(
            Subprocess.Executable.name(command),
            arguments: Subprocess.Arguments(["load", "-i", filePath]),
            output: .string(limit: .max)
        )
        return result.standardOutput ?? ""
    }

    /// Kill a Docker container.
    @discardableResult
    public func rm(options: [RmOption] = [], container: String) async throws -> String {
        let allArguments = ["rm"] + options.flatMap(\.arguments) + [container]
        let result = try await Subprocess.run(
            Subprocess.Executable.name(command),
            arguments: Subprocess.Arguments(allArguments),
            output: .string(limit: .max)
        )
        return result.standardOutput ?? ""
    }

    /// Stop a Docker container gracefully.
    /// - Parameters:
    ///   - container: The container name or ID to stop.
    ///   - timeoutSeconds: Optional timeout to wait before killing the container.
    /// - Returns: Standard output from the Docker CLI.
    @discardableResult
    public func stop(container: String, timeoutSeconds: Int? = nil) async throws -> String {
        var args = ["stop"]
        if let timeoutSeconds {
            args += ["--time", String(timeoutSeconds)]
        }
        args.append(container)
        let result = try await Subprocess.run(
            Subprocess.Executable.name(self.command),
            arguments: Subprocess.Arguments(args),
            output: .string(limit: .max)
        )
        return result.standardOutput ?? ""
    }

    /// Run a Docker container.
    @discardableResult
    public func run(
        options: [RunOption] = [],
        image: String,
        command: [String] = []
    ) async throws -> String {
        let allArguments = ["run"] + options.flatMap(\.arguments) + [image] + command
        let result = try await Subprocess.run(
            Subprocess.Executable.name(self.command),
            arguments: Subprocess.Arguments(allArguments),
            output: .string(limit: .max)
        )
        return result.standardOutput ?? ""
    }
}
