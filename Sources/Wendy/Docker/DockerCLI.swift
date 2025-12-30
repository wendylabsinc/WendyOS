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
            throw SubprocessError.nonZeroExit(
                command: arguments.description,
                exitCode: exitCode,
                terminationReason: result.terminationStatus.description,
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
        registryHostname: String = "host.docker.internal",
        registryPort: Int = 5000
    ) async throws {
        let arguments = [
            "buildx", "build",
            "--builder", defaultBuilderName,
            "--platform", "linux/arm64",
            "--provenance=false",
            "--sbom=false",
            "--push",
            "-t", "\(registryHostname):\(registryPort)/\(name):latest",
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
        // Create buildkitd.toml configuration
        // Include multiple registry configurations to handle different networking scenarios
        let configContent = """
            [registry."\(registryHostname):\(registryPort)"]
                http = true
                insecure = true

            """

        let wendyDir = FileManager.default.homeDirectoryForCurrentUser
            .appendingPathComponent(".wendy")

        try FileManager.default.createDirectory(at: wendyDir, withIntermediateDirectories: true)

        let configPath =
            wendyDir
            .appendingPathComponent("buildkit-config.toml")
            .path

        // Update the local config file if this registry isn't configured yet
        if var existingConfig = try? String(contentsOfFile: configPath, encoding: .utf8) {
            if !existingConfig.contains("\(registryHostname):\(registryPort)") {
                existingConfig += "\n\n" + configContent
                try existingConfig.write(toFile: configPath, atomically: true, encoding: .utf8)
            }
        } else {
            try configContent.write(toFile: configPath, atomically: true, encoding: .utf8)
        }

        // Read the desired config from local file
        let desiredConfig = try String(contentsOfFile: configPath, encoding: .utf8)

        if try await hasBuildxBuilder(builderName: defaultBuilderName) {
            let containerName = "buildx_buildkit_\(defaultBuilderName)0"

            // Check if the builder container is actually running
            let isRunning = await isContainerRunning(containerName: containerName)

            if !isRunning {
                // If it isn't, start it with bootstrap
                let bootstrapArguments: Subprocess.Arguments = [
                    "buildx", "inspect", "--bootstrap", defaultBuilderName,
                ]
                let bootstrapResult = try await Subprocess.run(
                    Subprocess.Executable.name(self.command),
                    arguments: bootstrapArguments,
                    output: .fileDescriptor(.standardOutput, closeAfterSpawningProcess: false),
                    error: .fileDescriptor(.standardError, closeAfterSpawningProcess: false)
                )

                guard bootstrapResult.terminationStatus.isSuccess else {
                    let exitCode: Int
                    switch bootstrapResult.terminationStatus {
                    case .exited(let code), .unhandledException(let code):
                        exitCode = Int(code)
                    }
                    throw SubprocessError.nonZeroExit(
                        command: bootstrapArguments.description,
                        exitCode: exitCode,
                        terminationReason: bootstrapResult.terminationStatus.description,
                        output: "",
                        error: ""
                    )
                }
            }

            // Get the container's current config
            let containerConfig = await getContainerFileContents(
                containerName: containerName,
                filePath: "/etc/buildkit/buildkitd.toml"
            )

            if containerConfig == desiredConfig {
                return
            }

            // If there is a change, copy the updated config to the container
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
                throw SubprocessError.nonZeroExit(
                    command: cpArguments.description,
                    exitCode: exitCode,
                    terminationReason: cpResult.terminationStatus.description,
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
                throw SubprocessError.nonZeroExit(
                    command: restartArguments.description,
                    exitCode: exitCode,
                    terminationReason: restartResult.terminationStatus.description,
                    output: "",
                    error: ""
                )
            }

            return
        }

        // Create builder with configuration
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
            throw SubprocessError.nonZeroExit(
                command: createArguments.description,
                exitCode: exitCode,
                terminationReason: createResult.terminationStatus.description,
                output: "",
                error: ""
            )
        }
    }

    /// Checks if a Docker container is currently running
    private func isContainerRunning(containerName: String) async -> Bool {
        let arguments = ["inspect", "-f", "{{.State.Running}}", containerName]
        do {
            let result = try await Subprocess.run(
                Subprocess.Executable.name(self.command),
                arguments: Subprocess.Arguments(arguments),
                output: .string(limit: 100, encoding: UTF8.self),
                error: .discarded
            )
            guard result.terminationStatus.isSuccess,
                let output = result.standardOutput
            else {
                return false
            }
            return output.trimmingCharacters(in: .whitespacesAndNewlines) == "true"
        } catch {
            return false
        }
    }

    /// Gets the contents of a file inside a Docker container using docker exec cat
    private func getContainerFileContents(
        containerName: String,
        filePath: String
    ) async -> String? {
        let arguments = ["exec", containerName, "cat", filePath]
        do {
            let result = try await Subprocess.run(
                Subprocess.Executable.name(self.command),
                arguments: Subprocess.Arguments(arguments),
                output: .string(limit: 100_000, encoding: UTF8.self),
                error: .discarded
            )
            guard result.terminationStatus.isSuccess,
                let output = result.standardOutput
            else {
                return nil
            }
            return output
        } catch {
            return nil
        }
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
