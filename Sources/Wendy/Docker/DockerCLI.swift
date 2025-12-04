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
            let exitCode: Int
            switch result.terminationStatus {
            case .exited(let code), .unhandledException(let code):
                exitCode = Int(code)
            }
            throw SubprocessError.nonZeroExit(
                command: ([self.command] + arguments).joined(separator: " "),
                exitCode: exitCode,
                terminationReason: result.terminationStatus.description,
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
            throw SubprocessError.nonZeroExit(
                command: ([self.command] + arguments).joined(separator: " "),
                exitCode: exitCode,
                terminationReason: result.terminationStatus.description,
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
            throw SubprocessError.nonZeroExit(
                command: ([self.command] + arguments).joined(separator: " "),
                exitCode: exitCode,
                terminationReason: result.terminationStatus.description,
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
    public func buildxAndPush(
        name: String,
        directory: String = ".",
        port: Int = 5000,
        builder: String
    ) async throws {
        let arguments = [
            "buildx", "build",
            "--builder", builder,
            "--platform", "linux/arm64",
            "--provenance=false",
            "--sbom=false",
            "--push",
            "-t", "host.docker.internal:\(port)/\(name):latest",
            directory,
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
            throw SubprocessError.nonZeroExit(
                command: ([self.command] + arguments).joined(separator: " "),
                exitCode: exitCode,
                terminationReason: result.terminationStatus.description,
                output: "",
                error: ""
            )
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
            throw SubprocessError.nonZeroExit(
                command: ([self.command] + arguments).joined(separator: " "),
                exitCode: exitCode,
                terminationReason: result.terminationStatus.description,
                output: "",
                error: ""
            )
        }

        return output.split(separator: "\n").map { String($0) }
    }

    public func builderName(forPort port: Int) -> String {
        return "wendy-builder-\(port)"
    }

    public func hasBuildxBuilder(builderName: String) async throws -> Bool {
        let builders = try await listBuildxBuilders()
        return builders.contains(builderName)
    }

    /// Creates a buildx builder with insecure registry support for the specified port.
    /// Returns the name of the created builder.
    public func createBuildxBuilder(
        port: Int
    ) async throws {
        let builderName = builderName(forPort: port)

        // Create buildkitd.toml configuration
        // Include multiple registry configurations to handle different networking scenarios
        let configContent = """
            [registry."host.docker.internal:\(port)"]
              http = true
              insecure = true

            [registry."localhost:\(port)"]
              http = true
              insecure = true

            [registry."127.0.0.1:\(port)"]
              http = true
              insecure = true
            """

        let tempDir = FileManager.default.temporaryDirectory
        let configPath = tempDir.appendingPathComponent("buildkitd-\(builderName).toml")
        try configContent.write(to: configPath, atomically: true, encoding: .utf8)

        // Create builder with configuration
        let createArguments = [
            "buildx", "create",
            "--name", builderName,
            "--driver", "docker-container",
            "--config", configPath.path,
            "--bootstrap",  // Start the builder immediately to load config
        ]

        let createResult = try await Subprocess.run(
            Subprocess.Executable.name(self.command),
            arguments: Subprocess.Arguments(createArguments),
            output: .fileDescriptor(.standardOutput, closeAfterSpawningProcess: false),
            error: .fileDescriptor(.standardError, closeAfterSpawningProcess: false)
        )

        guard createResult.terminationStatus.isSuccess else {
            let exitCode: Int
            switch createResult.terminationStatus {
            case .exited(let code), .unhandledException(let code):
                exitCode = Int(code)
            }
            throw SubprocessError.nonZeroExit(
                command: ([self.command] + createArguments).joined(separator: " "),
                exitCode: exitCode,
                terminationReason: createResult.terminationStatus.description,
                output: "",
                error: ""
            )
        }

        // Config has been loaded into builder container, safe to delete now
        try? FileManager.default.removeItem(at: configPath)
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
            throw SubprocessError.nonZeroExit(
                command: ([self.command] + arguments).joined(separator: " "),
                exitCode: exitCode,
                terminationReason: result.terminationStatus.description,
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
        case nonZeroExit(
            command: String,
            exitCode: Int,
            terminationReason: String,
            output: String,
            error: String
        )

        public var errorDescription: String? {
            switch self {
            case .nonZeroExit(let command, _, let terminationReason, let output, let error):
                return """
                    Command '\(command)' failed with \(terminationReason): \(error)

                    \(output)
                    """
            }
        }
    }
}
