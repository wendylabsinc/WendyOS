//
//  DockerCLI.swift
//  wendy-agent
//
//  Created by Joannis Orlandos on 16/09/2025.
//

import Foundation
import Subprocess

/// Represents the Docker CLI interface for managing container images and running containers.
public struct DockerCLI: Sendable {
    public let command: String

    public init(command: String = "docker") {
        self.command = command
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
            throw SubprocessError.nonZeroExit(
                command: ([self.command] + arguments).joined(separator: " "),
                exitCode: Int(result.terminationStatus.description) ?? -1,
                output: "",
                error: ""
            )
        }
    }

    public func buildx(
        name: String,
        directory: String = ".",
        port: Int = 8080
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
            throw SubprocessError.nonZeroExit(
                command: ([self.command] + arguments).joined(separator: " "),
                exitCode: Int(result.terminationStatus.description) ?? -1,
                output: "",
                error: ""
            )
        }
    }

    func push(
        name: String,
        port: Int = 8080
    ) async throws {
        let arguments = ["push", "localhost:\(port)/\(name):latest"]
        let result = try await Subprocess.run(
            Subprocess.Executable.name(self.command),
            arguments: Subprocess.Arguments(arguments),
            output: .fileDescriptor(.standardOutput, closeAfterSpawningProcess: false),
            error: .fileDescriptor(.standardError, closeAfterSpawningProcess: false)
        )

        guard result.terminationStatus.isSuccess else {
            throw SubprocessError.nonZeroExit(
                command: ([self.command] + arguments).joined(separator: " "),
                exitCode: Int(result.terminationStatus.description) ?? -1,
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

    public enum SubprocessError: Error, LocalizedError {
        case nonZeroExit(command: String, exitCode: Int, output: String, error: String)

        public var errorDescription: String? {
            switch self {
            case .nonZeroExit(let command, let exitCode, let output, let error):
                return """
                    Command '\(command)' failed with exit code \(exitCode): \(error)

                    \(output)
                    """
            }
        }
    }
}
