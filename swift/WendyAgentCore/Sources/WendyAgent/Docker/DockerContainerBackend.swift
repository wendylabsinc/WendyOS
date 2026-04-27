import Foundation
import Logging

/// Manages the lifecycle of Linux containers running via Docker on a Mac agent.
///
/// When the agent receives a container request with `platform: "linux/..."`, it
/// delegates to this backend. The image is already in the local Docker registry
/// at `localhost:<registryPort>` (pushed by the CLI via the standard buildx pipeline).
actor DockerContainerBackend {
    private let docker = DockerCLI()
    private let logger = Logger(label: "sh.wendy.agent.docker-backend")

    /// Pull an image from the local registry into Docker.
    func pullImage(_ imageName: String) async throws {
        logger.info("Pulling image", metadata: ["image": "\(imageName)"])
        try await docker.pull(image: imageName)
    }

    /// Remove any stale container, then create and start a Docker container in
    /// attached mode. Returns the running Process and its stdout/stderr pipes.
    func createAndStart(
        appName: String,
        imageName: String,
        appConfig: WendyAppConfig?,
        terminationHandler: (@Sendable (Foundation.Process) -> Void)? = nil
    ) async throws -> (process: Foundation.Process, stdout: Pipe, stderr: Pipe) {
        let containerName = "wendy-\(appName)"

        // Remove any stale container with the same name.
        _ = try? await docker.rm(options: [.force], container: containerName)

        var options: [DockerCLI.RunOption] = [
            .rm,
            .name(containerName),
            .label(key: "wendy.managed", value: "true"),
            .label(key: "wendy.app-name", value: appName),
        ]

        // Map entitlements to Docker flags.
        if let entitlements = appConfig?.entitlements {
            options += dockerOptions(from: entitlements, appName: appName)
        }

        logger.info(
            "Starting Docker container",
            metadata: [
                "container": "\(containerName)",
                "image": "\(imageName)",
            ]
        )

        return try docker.runAttached(
            options: options,
            image: imageName,
            terminationHandler: terminationHandler
        )
    }

    /// Stop a running Docker container.
    func stop(appName: String) async throws {
        let containerName = "wendy-\(appName)"
        logger.info("Stopping Docker container", metadata: ["container": "\(containerName)"])
        _ = try? await docker.stop(container: containerName, timeout: 10)
    }

    /// Remove a Docker container (force).
    func remove(appName: String) async throws {
        let containerName = "wendy-\(appName)"
        logger.info("Removing Docker container", metadata: ["container": "\(containerName)"])
        _ = try? await docker.rm(options: [.force], container: containerName)
    }

    /// List Wendy-managed Docker containers.
    func listContainers() async throws -> [DockerCLI.ContainerInfo] {
        try await docker.ps(label: "wendy.managed=true")
    }

    // MARK: - Entitlement mapping

    /// Translate Wendy entitlements into Docker run options.
    private func dockerOptions(
        from entitlements: [WendyEntitlement],
        appName: String
    ) -> [DockerCLI.RunOption] {
        var options: [DockerCLI.RunOption] = []

        for entitlement in entitlements {
            switch entitlement.type {
            case "network":
                if entitlement.mode == "none" {
                    options.append(.network("none"))
                } else {
                    // --network=host doesn't work on Docker Desktop for Mac.
                    // Map explicit ports from the entitlement's ports array.
                    if let ports = entitlement.ports {
                        for port in ports {
                            options.append(
                                .publish(hostPort: port.host, containerPort: port.container)
                            )
                        }
                    }
                }

            case "persist":
                if let name = entitlement.name, let path = entitlement.path {
                    let volumeName = "wendy-\(appName)-\(name)"
                    options.append(.volume(hostOrName: volumeName, containerPath: path))
                }

            case "gpu", "bluetooth", "audio", "video", "camera", "usb", "i2c", "gpio":
                logger.warning(
                    "Entitlement '\(entitlement.type)' is not available for Linux containers on macOS (VM isolation)",
                    metadata: ["app_name": "\(appName)"]
                )

            default:
                logger.warning(
                    "Unknown entitlement type '\(entitlement.type)'",
                    metadata: ["app_name": "\(appName)"]
                )
            }
        }

        return options
    }
}
