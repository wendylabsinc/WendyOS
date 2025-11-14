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
                    "sh.wendy.layer": "true",
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

                    async let killed: () = {
                        _ = try await stopContainer(
                            request: .init(
                                metadata: Metadata(),
                                message: .with {
                                    $0.appName = request.appName
                                }
                            ),
                            context: context
                        ).accepted.get()
                    }()

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

                    try await killed
                    logger.info(
                        "Killed running container",
                        metadata: [
                            "container-id": .stringConvertible(request.appName)
                        ]
                    )
                    try await client.deleteTask(containerID: request.appName)

                    _ = try await self.createContainer(
                        request: .init(
                            metadata: Metadata(),
                            message: .with {
                                $0.imageName = request.imageName
                                $0.appName = request.appName
                                $0.cmd = request.cmd
                                $0.appConfig = request.appConfig
                                $0.workingDir = request.workingDir
                                $0.restartPolicy = request.restartPolicy
                            }
                        ),
                        context: context
                    ).accepted.get()

                    let response = try await self.startContainer(
                        request: .init(
                            metadata: Metadata(),
                            message: .with {
                                $0.appName = request.appName
                            }
                        ),
                        context: context
                    ).accepted.get()

                    return try await response.producer(writer)
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

    func createContainer(
        request: ServerRequest<Wendy_Agent_Services_V1_CreateContainerRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_CreateContainerResponse> {
        try await Containerd.withClient { client in
            var labels = [String: String]()
            let request = request.message

            let images = Containerd_Services_Images_V1_Images.Client(wrapping: client.client)
            let content = Containerd_Services_Content_V1_Content.Client(wrapping: client.client)

            let image = try await images.get(
                .with {
                    $0.name = request.imageName
                }
            ).image
            let manifest = try await content.read(
                .with {
                    $0.digest = image.target.digest
                }
            ) { manifest in
                var data = Data()
                for try await message in manifest.messages {
                    data.append(message.data)
                }
                return try JSONDecoder().decode(ImageManifest.self, from: data)
            }

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

            // Infer command and workingDir from the image config, if not provided in the request.

            // Assume manifest.config.digest is the reference to the image config blob
            let configDescriptor = manifest.config
            let configData = try await client.fetchBlob(digest: configDescriptor.digest)
            let imageConfig = try JSONDecoder().decode(
                ContainerRegistry.ImageConfiguration.self,
                from: configData
            )

            // Set up command
            let requestCmdIsEmpty = request.cmd.trimmingCharacters(in: .whitespacesAndNewlines)
                .isEmpty
            let args: [String]
            if requestCmdIsEmpty {
                // Compose from config.entrypoint + config.cmd if they exist (following Docker convention)
                if let entrypoint = imageConfig.config?.Entrypoint, !entrypoint.isEmpty {
                    if let extraCmd = imageConfig.config?.Cmd, !extraCmd.isEmpty {
                        args = entrypoint + extraCmd
                    } else {
                        args = entrypoint
                    }
                } else if let extraCmd = imageConfig.config?.Cmd, !extraCmd.isEmpty {
                    args = extraCmd
                } else {
                    // Fallback: try suggest something reasonable (e.g., "/bin/sh"?)
                    args = []
                }
            } else {
                args = request.cmd.split(separator: " ").map(String.init)
            }

            // Set up workingDir
            let workingDir: String
            if request.workingDir.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
                if let wd = imageConfig.config?.WorkingDir, !wd.isEmpty {
                    workingDir = wd
                } else {
                    workingDir = "/"
                }
            } else {
                workingDir = request.workingDir
            }

            var spec = OCI(
                args: args,
                env: env,
                workingDir: workingDir,
                appName: request.appName
            )

            spec.applyEntitlements(
                entitlements: appConfig.entitlements,
                appName: request.appName
            )

            // Apply CDI for GPU if requested

            // Use default runc runtime - GPU devices are injected via CDI
            let runtime = "io.containerd.runc.v2"
            let options: Containerd_Runc_V1_Options? = nil

            if wantsGPU {
                logger.debug(
                    "Applying NVIDIA CDI spec",
                    metadata: [
                        "app-name": .stringConvertible(request.appName),
                        "image-name": .stringConvertible(request.imageName),
                    ]
                )

                do {
                    let cdiManager = CDIManager(
                        specGenerator: CDISpecGenerator(
                            hardwareDiscoverer: SystemHardwareDiscoverer()
                        )
                    )

                    let nvidiaSpec = try await cdiManager.loadNVIDIACDISpec(
                        deviceName: "all"
                    )
                    try spec.applyCDIDevice(nvidiaSpec, deviceName: "all")

                    logger.info("Successfully applied NVIDIA CDI spec to container")
                } catch {
                    logger.error(
                        "Failed to apply NVIDIA CDI spec",
                        metadata: [
                            "error": .string(error.localizedDescription)
                        ]
                    )
                    throw error
                }
            }

            let snapshotKey: String?

            do {
                (snapshotKey, _) = try await client.createSnapshot(
                    imageName: request.imageName,
                    appName: request.appName,
                    layers: manifest.layers.map { layer in
                        return .with {
                            $0.digest = layer.digest
                            $0.size = layer.size
                            $0.gzip = layer.mediaType.contains("gzip")
                            $0.diffID = layer.digest.replacing("sha256:", with: "")
                        }
                    }
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
                        "image-name": .stringConvertible(request.imageName),
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

            return ServerResponse(message: .init())
        }
    }

    func startContainer(
        request: ServerRequest<Wendy_Agent_Services_V1_StartContainerRequest>,
        context: ServerContext
    ) async throws -> StreamingServerResponse<Wendy_Agent_Services_V1_RunContainerLayersResponse> {
        let request = request.message
        return StreamingServerResponse { writer in
            try await Containerd.withClient { client in
                func run(
                    stdout: String?,
                    stderr: String?
                ) async throws {
                    let container = try await client.getContainer(named: request.appName)
                    let snapshot = try await client.mountsSnapshot(named: container.snapshotKey)

                    _ = try await stopContainer(
                        request: .init(
                            metadata: Metadata(),
                            message: .with {
                                $0.appName = request.appName
                            }
                        ),
                        context: context
                    ).accepted.get()

                    do {
                        logger.info("Creating task")
                        try await client.createTask(
                            containerID: request.appName,
                            appName: request.appName,
                            mounts: snapshot.mounts,
                            stdout: stdout,
                            stderr: stderr,
                            runtime: container.runtime.name
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
                        // Mark the container as started in the monitor (reset explicitly stopped flag)
                        await containerMonitor.markContainerStarted(request.appName)
                    } catch let error as RPCError where error.code == .notFound {
                        logger.info("Container wasn't running")
                    } catch let error as RPCError {
                        logger.error(
                            "Failed to kill container",
                            metadata: [
                                "container-id": .stringConvertible(request.appName)
                            ]
                        )
                        try await client.createTask(
                            containerID: request.appName,
                            appName: request.appName,
                            mounts: snapshot.mounts,
                            stdout: stdout,
                            stderr: stderr,
                            runtime: container.runtime.name
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
                // Mark the container as explicitly stopped in the monitor
                await containerMonitor.markContainerStopped(appName)
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
