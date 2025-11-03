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
            output: .discarded // TODO: Pipe into Noora?
            // TODO: Handle errors
        )

        if result.terminationStatus.isSuccess {
            return result.standardOutput
        } else {
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
