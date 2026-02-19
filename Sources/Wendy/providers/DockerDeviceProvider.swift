import CLIOutput
import Foundation
import Subprocess
import WendyShared

/// Build context for the Docker device provider
struct DockerBuildContext: Sendable {
    let containerName: String
}

/// Device provider for building and running Dockerfile-based projects via Docker Desktop.
struct DockerDeviceProvider: DeviceProvider, Sendable {
    let key = "docker"
    let displayName = "Docker Desktop"

    // MARK: - Availability

    func isAvailable() async -> Bool {
        do {
            let result = try await Subprocess.run(
                .name("docker"),
                arguments: ["--version"],
                output: .discarded,
                error: .discarded
            )
            return result.terminationStatus.isSuccess
        } catch {
            return false
        }
    }

    // MARK: - Requirements

    func checkRequirements(shouldAutoAccept: Bool) async throws {
        try await AppBuildHelpers.checkDockerIsRunning(shouldAutoAccept: shouldAutoAccept)
    }

    // MARK: - Discovery

    func discoverDevices() async throws -> [ExternalDevice] {
        let docker = DockerCLI()
        do {
            _ = try await docker.getServerVersion()
            return [
                ExternalDevice(
                    id: "docker",
                    displayName: "Docker Desktop",
                    providerKey: key,
                    agentVersion: Version.current
                )
            ]
        } catch {
            return []
        }
    }

    // MARK: - Build

    func canBuild(projectPath: URL) async -> Bool {
        let contents =
            (try? FileManager.default.contentsOfDirectory(atPath: projectPath.path)) ?? []
        return contents.contains { filename in
            let lowercased = filename.lowercased()
            return lowercased == "dockerfile"
                || lowercased.hasPrefix("dockerfile.")
                || lowercased.hasSuffix(".dockerfile")
        }
    }

    func build(
        for device: ExternalDevice,
        projectPath: URL,
        executable: String,
        debug: Bool
    ) async throws -> ProviderBuiltApp {
        let name = projectPath.lastPathComponent.lowercased()
        let docker = DockerCLI()

        try await AppBuildHelpers.executePhase(
            phase: "build",
            commandName: "wendy build",
            additionalProperties: [:]
        ) {
            try await docker.build(name: name)
        }

        return ProviderBuiltApp(
            provider: self,
            device: device,
            appName: name,
            context: DockerBuildContext(containerName: name)
        )
    }

    // MARK: - Run

    func run(
        _ builtApp: ProviderBuiltApp,
        detach: Bool,
        output: AsyncStream<ProviderRunOutput>.Continuation
    ) async throws {
        guard let ctx = builtApp.context as? DockerBuildContext else {
            throw CLIError.invalidArgument(
                name: "context",
                value: "unknown",
                reason: "Invalid build context for Docker provider"
            )
        }

        let docker = DockerCLI()
        output.yield(.started)
        try await docker.run(name: ctx.containerName, detach: detach)
        output.finish()
    }

    // MARK: - Stop

    func stop(_ builtApp: ProviderBuiltApp) async throws {
        guard let ctx = builtApp.context as? DockerBuildContext else { return }
        _ = try await Subprocess.run(
            .name("docker"),
            arguments: ["stop", ctx.containerName],
            output: .discarded,
            error: .discarded
        )
    }
}
