import Foundation
import GRPCCore
import Logging

/// Manages the lifecycle of Linux containers running via Docker on a Mac agent.
///
/// When the agent receives a container request with `platform: "linux/..."`, it
/// delegates to this backend. The image is already in the local Docker registry
/// at `localhost:<registryPort>` (pushed by the CLI via the standard buildx pipeline).
///
/// On Docker Desktop, `docker pull localhost:...` may be treated as an HTTPS
/// registry lookup, while `127.0.0.1:...` is part of Docker's default insecure
/// loopback range. We therefore rewrite loopback registry references to
/// `127.0.0.1` before pulling/running so the daemon can talk to the local
/// plaintext registry without requiring manual daemon reconfiguration.
actor DockerContainerBackend {
    private let docker = DockerCLI()
    private let logger = Logger(label: "sh.wendy.agent.docker-backend")

    /// Pull an image from the local registry into Docker.
    func pullImage(_ imageName: String) async throws {
        let dockerImageName = Self.rewriteLoopbackRegistryHost(in: imageName)
        self.logImageRewriteIfNeeded(original: imageName, effective: dockerImageName)

        logger.info(
            "Pulling image",
            metadata: [
                "image": "\(dockerImageName)",
                "requested_image": "\(imageName)",
            ]
        )

        do {
            try await docker.pull(image: dockerImageName)
        } catch {
            logger.error(
                "Docker image pull failed",
                metadata: [
                    "image": "\(dockerImageName)",
                    "requested_image": "\(imageName)",
                    "error": "\(String(describing: error))",
                ]
            )
            throw Self.makeRPCError(
                action: "pull Docker image",
                requestedImageName: imageName,
                effectiveImageName: dockerImageName,
                error: error
            )
        }
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
        let dockerImageName = Self.rewriteLoopbackRegistryHost(in: imageName)
        self.logImageRewriteIfNeeded(original: imageName, effective: dockerImageName)

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
                "image": "\(dockerImageName)",
                "requested_image": "\(imageName)",
            ]
        )

        do {
            return try docker.runAttached(
                options: options,
                image: dockerImageName,
                terminationHandler: terminationHandler
            )
        } catch {
            logger.error(
                "Docker container start failed",
                metadata: [
                    "container": "\(containerName)",
                    "image": "\(dockerImageName)",
                    "requested_image": "\(imageName)",
                    "error": "\(String(describing: error))",
                ]
            )
            throw Self.makeRPCError(
                action: "start Docker container from image",
                requestedImageName: imageName,
                effectiveImageName: dockerImageName,
                error: error
            )
        }
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

    static func rewriteLoopbackRegistryHostForTesting(_ imageName: String) -> String {
        Self.rewriteLoopbackRegistryHost(in: imageName)
    }

    private func logImageRewriteIfNeeded(original: String, effective: String) {
        guard original != effective else { return }

        logger.info(
            "Rewriting loopback registry host for Docker Desktop",
            metadata: [
                "requested_image": "\(original)",
                "effective_image": "\(effective)",
                "reason": "Docker treats 127.0.0.1 as a default insecure loopback registry, but localhost may be forced through HTTPS",
            ]
        )
    }

    private static func rewriteLoopbackRegistryHost(in imageName: String) -> String {
        guard let separator = imageName.firstIndex(of: "/") else { return imageName }

        let registry = String(imageName[..<separator])
        let remainder = String(imageName[separator...])

        switch registry.lowercased() {
        case "localhost":
            return "127.0.0.1\(remainder)"
        case let registry where registry.hasPrefix("localhost:"):
            return "127.0.0.1\(registry.dropFirst("localhost".count))\(remainder)"
        case "[::1]":
            return "127.0.0.1\(remainder)"
        case let registry where registry.hasPrefix("[::1]:"):
            return "127.0.0.1\(registry.dropFirst("[::1]".count))\(remainder)"
        default:
            return imageName
        }
    }

    private static func makeRPCError(
        action: String,
        requestedImageName: String,
        effectiveImageName: String,
        error: Error
    ) -> RPCError {
        var message = "Failed to \(action) \(requestedImageName)"
        if effectiveImageName != requestedImageName {
            message += " (using \(effectiveImageName) inside Docker)"
        }
        message += ": \(String(describing: error))"
        return RPCError(code: .internalError, message: message)
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
