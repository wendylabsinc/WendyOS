import AppConfig
import ContainerRegistry
import ContainerdGRPC
import Foundation
import Logging
import NIOCore
import NIOFoundationCompat
import WendyAgentGRPC
import WendyShared
import _NIOFileSystem

struct WendyContainerService: Wendy_Agent_Services_V1_WendyContainerService.ServiceProtocol {
    let logger = Logger(label: "WendyContainerService")

    func listLayers(
        request: ServerRequest<Wendy_Agent_Services_V1_ListLayersRequest>,
        context: ServerContext
    ) async throws -> StreamingServerResponse<Wendy_Agent_Services_V1_LayerHeader> {
        return StreamingServerResponse { writer in
            try await Containerd.withClient { client in
                try await client.listContent { items in
                    try await writer.write(
                        contentsOf: items.map { item in
                            Wendy_Agent_Services_V1_LayerHeader.with { header in
                                header.digest = item.digest
                                header.size = item.size
                            }
                        }
                    )
                }
            }

            return Metadata()
        }
    }
    func listContainers(
        request: GRPCCore.ServerRequest<Wendy_Agent_Services_V1_ListContainersRequest>,
        context: GRPCCore.ServerContext
    ) async throws
        -> GRPCCore.StreamingServerResponse<Wendy_Agent_Services_V1_ListContainersResponse>
    {
        return StreamingServerResponse { writer in
            try await Containerd.withClient { client in
                let tasks = try await client.listTasks()
                let containers = try await client.listContainers()

                for container in containers {
                    try await writer.write(
                        .with {
                            $0.container.appName = container.id
                            $0.container.appVersion =
                                container.labels["sh.wendy/app.version"] ?? "0.0.0"

                            if let restartCount = container.labels["containerd.io/restart.count"],
                                let restartCount = UInt32(restartCount)
                            {
                                $0.container.failureCount = restartCount
                            }

                            if let task: Containerd_V1_Types_Process = tasks.first(where: {
                                $0.id == container.id
                            }) {
                                $0.container.runningState =
                                    task.status == .running ? .running : .stopped
                            } else {
                                $0.container.runningState = .stopped
                            }
                        }
                    )
                }
            }

            return Metadata()
        }
    }

    func writeLayer(
        request: StreamingServerRequest<Wendy_Agent_Services_V1_WriteLayerRequest>,
        context: ServerContext
    ) async throws -> StreamingServerResponse<Wendy_Agent_Services_V1_WriteLayerResponse> {
        return StreamingServerResponse { writer in
            nonisolated(unsafe) var iterator = request.messages.makeAsyncIterator()
            guard let firstChunk = try await iterator.next() else {
                throw RPCError(code: .aborted, message: "No initial chunk provided.")
            }

            try await Containerd.withClient { client in
                // Add labels to prevent garbage collection of uploaded layers
                let labels = [
                    "containerd.io/gc.root": "true",
                    "sh.wendy.layer": "true"
                ]
                try await client.writeLayer(ref: firstChunk.digest, labels: labels) { writer in
                    try await writer.write(data: firstChunk.data)

                    while let nextChunk = try await iterator.next() {
                        try await writer.write(data: nextChunk.data)
                    }
                }
            }

            return Metadata()
        }
    }

    func runContainer(
        request: ServerRequest<Wendy_Agent_Services_V1_RunContainerLayersRequest>,
        context: ServerContext
    ) async throws -> StreamingServerResponse<Wendy_Agent_Services_V1_RunContainerLayersResponse> {
        return StreamingServerResponse { writer in
            try await Containerd.withClient { client in
                do {
                    let request = request.message
                    var labels = [String: String]()

                    do {
                        let restartPolicy = request.restartPolicy
                        let restartPolicyLabel = "containerd.io/restart.policy"

                        switch restartPolicy.mode {
                        case .default, .unlessStopped, .UNRECOGNIZED:
                            labels[restartPolicyLabel] = "unless-stopped"
                        case .no:
                            labels[restartPolicyLabel] = "no"
                        case .onFailure:
                            labels[restartPolicyLabel] =
                                "on-failure:\(restartPolicy.onFailureMaxRetries)"
                        }
                    }

                    async let killed: Void = try await client.stopTask(containerID: request.appName)

                    logger.info("Creating container config.json")
                    let config = ImageConfiguration(
                        architecture: "arm64",
                        os: "linux",
                        config: ImageConfigurationConfig(
                            Cmd: request.cmd.split(separator: " ").map(String.init),
                            StopSignal: "SIGTERM"
                        ),
                        rootfs: ImageConfigurationRootFS(diff_ids: request.layers.map(\.diffID))
                    )
                    let (configHash, configSize) = try await client.uploadJSON(config)

                    logger.debug("Creating container manifest")
                    let manifest = ImageManifest(
                        mediaType: "application/vnd.oci.image.manifest.v1+json",
                        config: ContentDescriptor(
                            mediaType: "application/vnd.oci.image.config.v1+json",
                            digest: "sha256:\(configHash)",
                            size: configSize
                        ),
                        layers: request.layers.map { layer in
                            return ContentDescriptor(
                                mediaType: layer.gzip
                                    ? "application/vnd.oci.image.layer.v1.tar+gzip"
                                    : "application/vnd.oci.image.layer.v1.tar",
                                digest: layer.digest,
                                size: layer.size
                            )
                        }
                    )
                    let (manifestHash, manifestSize) = try await client.uploadJSON(manifest)

                    do {
                        logger.info("Creating image \(request.imageName)")
                        try await client.createImage(
                            named: request.imageName,
                            manifestHash: manifestHash,
                            manifestSize: manifestSize
                        )
                    } catch {
                        try await client.updateImage(
                            named: request.imageName,
                            manifestHash: manifestHash,
                            manifestSize: manifestSize
                        )
                    }

                    let appConfig: AppConfig

                    if request.appConfig.isEmpty {
                        appConfig = AppConfig(
                            appId: request.appName,
                            version: "0.0.0",
                            entitlements: []
                        )
                    } else {
                        appConfig = try JSONDecoder().decode(
                            AppConfig.self,
                            from: request.appConfig
                        )
                    }

                    let wantsGPU = appConfig.entitlements.contains(where: {
                        if case .gpu = $0 {
                            return true
                        } else {
                            return false
                        }
                    })

                    labels["sh.wendy/app.version"] = appConfig.version

                    // Build environment variables from entitlements
                    var env = [
                        "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
                    ]
                    env.append(contentsOf: appConfig.entitlements.environmentVariables())

                    var spec = OCI(
                        args: request.cmd.split(separator: " ").map(String.init),
                        env: env,
                        workingDir: request.workingDir.isEmpty ? "/" : request.workingDir,
                        appName: request.appName
                    )

                    spec.applyEntitlements(
                        entitlements: appConfig.entitlements,
                        appName: request.appName
                    )

                    // Use default runc runtime - GPU devices are manually injected in OCI spec
                    let runtime = "io.containerd.runc.v2"

                    // Options will point us to the nvidia runtime if GPU entitlements are present
                    let options: Containerd_Runc_V1_Options?
                    
                    if wantsGPU {
                        options = Containerd_Runc_V1_Options.with {
                            $0.binaryName = "/usr/bin/nvidia-container-runtime"
                            $0.systemdCgroup = true
                        }
                        logger.debug(
                            "Creating container with GPU options",
                            metadata: [
                                "app-name": .stringConvertible(request.appName),
                                "image-name": .stringConvertible(request.imageName)
                            ]
                        )
                    } else {
                        options = nil
                    }

                    let snapshotKey: String?
                    let mounts: [Containerd_Types_Mount]

                    do {
                        (snapshotKey, mounts) = try await client.createSnapshot(
                            imageName: request.imageName,
                            appName: request.appName,
                            layers: request.layers
                        )
                    } catch let error as RPCError {
                        logger.error(
                            "Failed to create snapshot",
                            metadata: [
                                "error": .stringConvertible(error.description)
                            ]
                        )
                        throw error
                    }

                    do {
                        logger.info(
                            "Creating container",
                            metadata: [
                                "app-name": .stringConvertible(request.appName),
                                "image-name": .stringConvertible(request.imageName)
                            ]
                        )
                        try await client.createContainer(
                            imageName: request.imageName,
                            appName: request.appName,
                            snapshotKey: snapshotKey ?? "",
                            ociSpec: try JSONEncoder().encode(spec),
                            labels: labels,
                            runtime: runtime,
                            options: options
                        )
                    } catch let error as RPCError where error.code == .alreadyExists {
                        logger.debug("Container already exists, attempting to update")
                        do {
                            try await client.updateContainer(
                                imageName: request.imageName,
                                appName: request.appName,
                                snapshotKey: snapshotKey ?? "",
                                ociSpec: try JSONEncoder().encode(spec),
                                runtime: runtime,
                                options: options
                            )
                        } catch let updateError as RPCError
                            where updateError.code == .invalidArgument
                            && updateError.message.contains("Runtime.Name field is immutable")
                        {
                            logger.info("Runtime changed, deleting and recreating container")
                            try await client.deleteContainer(named: request.appName)
                            try await client.createContainer(
                                imageName: request.imageName,
                                appName: request.appName,
                                snapshotKey: snapshotKey ?? "",
                                ociSpec: try JSONEncoder().encode(spec),
                                labels: labels,
                                runtime: runtime
                            )
                        }
                    }

                    do {
                        try await killed
                        logger.info(
                            "Killed running container",
                            metadata: [
                                "container-id": .stringConvertible(request.appName)
                            ]
                        )
                        try await client.deleteTask(containerID: request.appName)
                    } catch let error as RPCError where error.code == .notFound {
                        logger.info("Container wasn't running")
                    } catch let error as RPCError {
                        logger.error(
                            "Failed to kill container",
                            metadata: [
                                "container-id": .stringConvertible(request.appName),
                                "error": .stringConvertible(error.description),
                            ]
                        )
                        throw error
                    } catch {
                        logger.error(
                            "Failed to kill container",
                            metadata: [
                                "container-id": .stringConvertible(request.appName),
                                "error": .stringConvertible(error.localizedDescription),
                            ]
                        )
                        throw error
                    }

                    func run(
                        stdout: String?,
                        stderr: String?
                    ) async throws {
                        do {
                            logger.info("Creating task")
                            try await client.createTask(
                                containerID: request.appName,
                                appName: request.appName,
                                snapshotName: snapshotKey ?? "",
                                mounts: mounts,
                                stdout: stdout,
                                stderr: stderr,
                                runtime: runtime
                            )
                        } catch let error as RPCError where error.code == .alreadyExists {
                            logger.info(
                                "Task already exists, re-creating it",
                                metadata: [
                                    "container-id": .stringConvertible(request.appName)
                                ]
                            )
                            try await client.deleteTask(containerID: request.appName)
                            logger.debug(
                                "Task removed",
                                metadata: [
                                    "container-id": .stringConvertible(request.appName)
                                ]
                            )
                            try await client.createTask(
                                containerID: request.appName,
                                appName: request.appName,
                                snapshotName: snapshotKey ?? "",
                                mounts: mounts,
                                stdout: stdout,
                                stderr: stderr,
                                runtime: runtime
                            )
                            logger.debug(
                                "Task created",
                                metadata: [
                                    "container-id": .stringConvertible(request.appName)
                                ]
                            )
                        }

                        logger.info("Starting task")
                        try await client.runTask(containerID: request.appName)

                        try await writer.write(
                            .with {
                                $0.started = .init()
                            }
                        )
                    }

                    logger.info("Running app")
                    try await client.withStdout { stdout, stderr in
                        try await run(stdout: stdout, stderr: stderr)
                    } onStdout: { bytes in
                        try await writer.write(
                            .with {
                                $0.stdoutOutput.data = Data(buffer: bytes)
                            }
                        )
                    } onStderr: { bytes in
                        try await writer.write(
                            .with {
                                $0.stderrOutput.data = Data(buffer: bytes)
                            }
                        )
                    }

                    return Metadata()
                } catch let error as RPCError {
                    logger.error(
                        "Failed to run container",
                        metadata: [
                            "error": .stringConvertible(error.description)
                        ]
                    )
                    throw error
                } catch {
                    logger.error(
                        "Failed to run container",
                        metadata: [
                            "error": .stringConvertible(error.localizedDescription)
                        ]
                    )
                    throw RPCError(code: .aborted, message: "\(error)")
                }
            }
        }
    }

    func stopContainer(
        request: ServerRequest<Wendy_Agent_Services_V1_StopContainerRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_StopContainerResponse> {
        try await Containerd.withClient { client in
            let appName = request.message.appName
            logger.info(
                "Stopping container",
                metadata: ["container-id": .stringConvertible(appName)]
            )
            do {
                try await client.stopTask(containerID: appName)
                logger.info(
                    "Stopped container",
                    metadata: ["container-id": .stringConvertible(appName)]
                )
            } catch let error as RPCError where error.code == .notFound {
                logger.info(
                    "Container wasn't running",
                    metadata: ["container-id": .stringConvertible(appName)]
                )
            } catch let error as RPCError {
                logger.error(
                    "Failed to stop container",
                    metadata: [
                        "container-id": .stringConvertible(appName),
                        "error": .stringConvertible(error.description),
                    ]
                )
                throw error
            }

            return ServerResponse(message: .init())
        }
    }
}
