import AppConfig
import ContainerRegistry
import ContainerdGRPC
import Foundation
import Logging
import Tracing
import WendyAgentGRPC
import WendyShared
import _NIOFileSystem

struct WendyContainerService: Wendy_Agent_Services_V1_WendyContainerService.ServiceProtocol {
    let logger = Logger(label: "WendyContainerService")
    let persistenceBasePath: URL

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
                    guard let appVersion = container.labels["sh.wendy/app.version"] else {
                        // If a container is not managed by Wendy Agent, skip it
                        continue
                    }

                    try await writer.write(
                        .with {
                            $0.container.appName = container.id
                            $0.container.appVersion = appVersion

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
                    "containerd.io/gc.root": Date().rfc3339Formatted(),
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
            return try await withSpan("runContainer") { span in
                span.attributes["container.app_name"] = request.message.appName
                span.attributes["container.image_name"] = request.message.imageName
                span.attributes["container.layers_count"] = request.message.layers.count

                return try await Containerd.withClient { client in
                    do {
                        let request = request.message

                        async let killed: () = {
                            try await withSpan("stopExistingContainer") { _ in
                                _ = try await stopContainer(
                                    request: .init(
                                        metadata: Metadata(),
                                        message: .with {
                                            $0.appName = request.appName
                                        }
                                    ),
                                    context: context
                                ).accepted.get()
                            }
                        }()

                        let (configHash, configSize) = try await withSpan("uploadImageConfig") {
                            innerSpan in
                            logger.info("Creating container config.json")
                            let cmdArgs =
                                request.cmd.split(separator: " ").map(String.init)
                                + request.userArgs
                            let config = ImageConfiguration(
                                architecture: "arm64",
                                os: "linux",
                                config: ImageConfigurationConfig(
                                    Cmd: cmdArgs,
                                    StopSignal: "SIGTERM"
                                ),
                                rootfs: ImageConfigurationRootFS(
                                    diff_ids: request.layers.map(\.diffID)
                                )
                            )
                            let result = try await client.uploadJSON(config)
                            innerSpan.attributes["config.hash"] = result.0
                            innerSpan.attributes["config.size"] = Int(result.1)
                            return result
                        }

                        let (manifestHash, manifestSize) = try await withSpan("uploadManifest") {
                            innerSpan in
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
                            let result = try await client.uploadJSON(manifest)
                            innerSpan.attributes["manifest.hash"] = result.0
                            innerSpan.attributes["manifest.size"] = Int(result.1)
                            return result
                        }

                        try await withSpan("createOrUpdateImage") { innerSpan in
                            innerSpan.attributes["image.name"] = request.imageName
                            do {
                                logger.info("Creating image \(request.imageName)")
                                try await client.createImage(
                                    named: request.imageName,
                                    manifestHash: manifestHash,
                                    manifestSize: manifestSize
                                )
                                innerSpan.attributes["image.action"] = "created"
                            } catch {
                                try await client.updateImage(
                                    named: request.imageName,
                                    manifestHash: manifestHash,
                                    manifestSize: manifestSize
                                )
                                innerSpan.attributes["image.action"] = "updated"
                            }
                        }

                        try await killed
                        logger.info(
                            "Killed running container",
                            metadata: [
                                "container-id": .stringConvertible(request.appName)
                            ]
                        )

                        try await withSpan("deleteOldTask") { _ in
                            try await client.deleteTask(containerID: request.appName)
                        }

                        try await withSpan("createContainer") { _ in
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
                                        $0.userArgs = request.userArgs
                                    }
                                ),
                                context: context
                            ).accepted.get()
                        }

                        let response = try await withSpan("startContainer") { _ in
                            try await self.startContainer(
                                request: .init(
                                    metadata: Metadata(),
                                    message: .with {
                                        $0.appName = request.appName
                                    }
                                ),
                                context: context
                            ).accepted.get()
                        }

                        span.setStatus(.init(code: .ok))
                        return try await response.producer(writer)
                    } catch let error as RPCError {
                        span.setStatus(.init(code: .error, message: error.description))
                        logger.error(
                            "Failed to run container",
                            metadata: [
                                "error": .stringConvertible(error.description)
                            ]
                        )
                        throw error
                    } catch {
                        span.setStatus(.init(code: .error, message: error.localizedDescription))
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
    }

    func createContainer(
        request: ServerRequest<Wendy_Agent_Services_V1_CreateContainerRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_CreateContainerResponse> {
        try await createContainerInternal(request: request.message, progress: nil)
        return ServerResponse(message: .init())
    }

    func createContainerWithProgress(
        request: ServerRequest<Wendy_Agent_Services_V1_CreateContainerRequest>,
        context: ServerContext
    ) async throws
        -> StreamingServerResponse<Wendy_Agent_Services_V1_CreateContainerProgressResponse>
    {
        let request = request.message
        return StreamingServerResponse { writer in
            try await createContainerInternal(request: request) { progress in
                try await writer.write(.with { $0.progress = progress })
            }

            try await writer.write(
                .with {
                    $0.completed = .init()
                }
            )

            return Metadata()
        }
    }

    private func createContainerInternal(
        request: Wendy_Agent_Services_V1_CreateContainerRequest,
        progress: (
            @Sendable (Wendy_Agent_Services_V1_CreateContainerProgress) async throws -> Void
        )?
    ) async throws {
        try await withSpan("createContainerInternal") { span in
            span.attributes["container.app_name"] = request.appName
            span.attributes["container.image_name"] = request.imageName

            try await Containerd.withClient { client in
                var labels = [String: String]()

                let images = Containerd_Services_Images_V1_Images.Client(wrapping: client.client)

                let image = try await withSpan("getImage") { innerSpan in
                    innerSpan.attributes["image.name"] = request.imageName
                    return try await images.get(
                        .with {
                            $0.name = request.imageName
                        }
                    ).image
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

                let hostname = try await String(
                    contentsOf: FilePath("/etc/hostname"),
                    maximumSizeAllowed: .bytes(256)
                )
                .trimmingCharacters(in: .whitespacesAndNewlines)

                // Build base environment variables
                // Note: GPU-related env vars (NVIDIA_VISIBLE_DEVICES, etc.) are now
                // handled by CDI and added during applyCDIDevice()
                let env = [
                    "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
                    "WENDY_HOSTNAME=\(hostname).local",
                ]

                // Infer command and workingDir from the image config, if not provided in the request.

                // Assume manifest.config.digest is the reference to the image config blob
                let manifest = try await client.readImageManifest(image: image)
                let configDescriptor = manifest.config
                let configData = try await client.fetchBlob(digest: configDescriptor.digest)
                let imageConfig = try JSONDecoder().decode(
                    ContainerRegistry.ImageConfiguration.self,
                    from: configData
                )

                // Set up command
                let requestCmdIsEmpty = request.cmd.trimmingCharacters(in: .whitespacesAndNewlines)
                    .isEmpty
                let hasUserArgs = !request.userArgs.isEmpty

                let finalArgs: [String]
                if !requestCmdIsEmpty {
                    finalArgs =
                        request.cmd.split(separator: " ").map(String.init) + request.userArgs
                } else if hasUserArgs {
                    // User args replace image CMD (Docker convention:
                    // `docker run image arg1 arg2` uses ENTRYPOINT + [arg1, arg2])
                    if let entrypoint = imageConfig.config?.Entrypoint, !entrypoint.isEmpty {
                        finalArgs = entrypoint + request.userArgs
                    } else if let cmd = imageConfig.config?.Cmd, let executable = cmd.first {
                        // No entrypoint, prefix user args with Cmd[0] (the executable)
                        // so we don't lose the binary path
                        finalArgs = [executable] + request.userArgs
                    } else {
                        finalArgs = request.userArgs
                    }
                } else {
                    // No user args, use image entrypoint + cmd as-is
                    if let entrypoint = imageConfig.config?.Entrypoint, !entrypoint.isEmpty {
                        if let extraCmd = imageConfig.config?.Cmd, !extraCmd.isEmpty {
                            finalArgs = entrypoint + extraCmd
                        } else {
                            finalArgs = entrypoint
                        }
                    } else if let extraCmd = imageConfig.config?.Cmd, !extraCmd.isEmpty {
                        finalArgs = extraCmd
                    } else {
                        finalArgs = []
                    }
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
                    args: finalArgs,
                    env: env,
                    workingDir: workingDir,
                    appName: request.appName
                )

                let dependencies = spec.applyEntitlements(
                    entitlements: appConfig.entitlements,
                    appName: request.appName,
                    availableDevices: try OCI.AvailableDevices.detect(),
                    persistenceBasePath: persistenceBasePath
                )

                for directory in dependencies.directoriesToCreate {
                    try FileManager.default.createDirectory(
                        at: directory,
                        withIntermediateDirectories: true
                    )
                }

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

                // Unpack the image from the content store into snapshots
                // This is required when images are pushed via registry but not yet unpacked
                let progressHandler = progress
                let (snapshotKey, _) = try await withSpan("unpackImage") { unpackSpan in
                    unpackSpan.attributes["image.name"] = request.imageName
                    return try await client.unpackImage(
                        named: request.imageName
                    ) { unpackProgress in
                        guard let progressHandler else { return }

                        let progressMessage = Wendy_Agent_Services_V1_CreateContainerProgress.with {
                            switch unpackProgress.phase {
                            case .start(let totalLayers, let totalBytes):
                                $0.phase = .unpacking
                                $0.totalLayers = Int32(totalLayers)
                                $0.layerSize = totalBytes
                                unpackSpan.attributes["unpack.total_layers"] = totalLayers
                                unpackSpan.attributes["unpack.total_bytes"] = Int(totalBytes)
                            case .layer(let index, let total, let size, let reused):
                                $0.phase = .applyingLayer
                                $0.layerIndex = Int32(index)
                                $0.totalLayers = Int32(total)
                                $0.layerSize = size
                                $0.reusedSnapshot = reused
                            case .complete(let totalLayers, _, _):
                                $0.phase = .complete
                                $0.totalLayers = Int32(totalLayers)
                            }
                        }

                        try await progressHandler(progressMessage)
                    }
                }

                if let progressHandler {
                    try await progressHandler(
                        .with {
                            $0.phase = .creatingContainer
                        }
                    )
                }

                try await withSpan("createOrUpdateContainer") { createSpan in
                    createSpan.attributes["container.app_name"] = request.appName
                    createSpan.attributes["container.runtime"] = runtime

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
                        createSpan.attributes["container.action"] = "created"
                    } catch let error as RPCError where error.code == .alreadyExists {
                        logger.debug("Container already exists, attempting to update")
                        do {
                            try await client.updateContainer(
                                imageName: request.imageName,
                                appName: request.appName,
                                snapshotKey: snapshotKey ?? "",
                                ociSpec: try JSONEncoder().encode(spec),
                                labels: labels,
                                runtime: runtime,
                                options: options
                            )
                            createSpan.attributes["container.action"] = "updated"
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
                            createSpan.attributes["container.action"] = "recreated"
                        }
                    }
                }
            }

            span.setStatus(.init(code: .ok))
        }
    }

    func startContainer(
        request: ServerRequest<Wendy_Agent_Services_V1_StartContainerRequest>,
        context: ServerContext
    ) async throws -> StreamingServerResponse<Wendy_Agent_Services_V1_RunContainerLayersResponse> {
        let request = request.message
        return StreamingServerResponse { writer in
            let appName = request.appName
            let logStream = try await ContainerLogManager.shared.startContainer(
                appName: appName,
                markExplicitStop: true
            )

            try await writer.write(
                .with {
                    $0.started = .init()
                }
            )

            for await chunk in logStream {
                do {
                    if chunk.isStderr {
                        try await writer.write(
                            .with {
                                $0.stderrOutput.data = chunk.data
                            }
                        )
                    } else {
                        try await writer.write(
                            .with {
                                $0.stdoutOutput.data = chunk.data
                            }
                        )
                    }
                } catch {
                    break
                }
            }

            return Metadata()
        }
    }

    func stopContainer(
        request: ServerRequest<Wendy_Agent_Services_V1_StopContainerRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_StopContainerResponse> {
        try await withSpan("stopContainer") { span in
            let appName = request.message.appName
            span.attributes["container.app_name"] = appName

            try await Containerd.withClient { client in
                logger.info(
                    "Stopping container",
                    metadata: ["container-id": .stringConvertible(appName)]
                )
                do {
                    try await client.stopTask(containerID: appName)
                    // Mark the container as explicitly stopped in the monitor
                    await ContainerMonitor.shared.markContainerStopped(appName)
                    logger.info(
                        "Stopped container",
                        metadata: ["container-id": .stringConvertible(appName)]
                    )
                    span.attributes["container.was_running"] = true
                } catch let error as RPCError where error.code == .notFound {
                    logger.info(
                        "Container wasn't running",
                        metadata: ["container-id": .stringConvertible(appName)]
                    )
                    span.attributes["container.was_running"] = false
                } catch let error as RPCError {
                    span.setStatus(.init(code: .error, message: error.description))
                    logger.error(
                        "Failed to stop container",
                        metadata: [
                            "container-id": .stringConvertible(appName),
                            "error": .stringConvertible(error.description),
                        ]
                    )
                    throw error
                }
            }

            span.setStatus(.init(code: .ok))
            return ServerResponse(message: .init())
        }
    }

    func deleteContainer(
        request: ServerRequest<Wendy_Agent_Services_V1_DeleteContainerRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_DeleteContainerResponse> {
        let request = request.message
        let appName = request.appName
        let deleteImage = request.deleteImage

        return try await withSpan("deleteContainer") { span in
            span.attributes["container.app_name"] = appName
            span.attributes["container.delete_image"] = deleteImage

            try await Containerd.withClient { client in
                logger.info(
                    "Deleting container",
                    metadata: [
                        "container-id": .stringConvertible(appName),
                        "delete-image": .stringConvertible(deleteImage),
                    ]
                )

                // Capture image name before deletion so we can optionally remove it
                var imageName: String? = nil
                do {
                    let container = try await client.getContainer(named: appName)
                    imageName = container.image
                    span.attributes["container.image_name"] = imageName
                } catch let error as RPCError where error.code == .notFound {
                    logger.info(
                        "Container not found prior to delete, continuing",
                        metadata: ["container-id": .stringConvertible(appName)]
                    )
                }

                // Stop and delete the container and its ephemeral snapshot
                try await withSpan("deleteContainerAndSnapshot") { _ in
                    do {
                        try await client.deleteContainer(named: appName)
                    } catch let error as RPCError where error.code == .notFound {
                        logger.info(
                            "Container already deleted",
                            metadata: ["container-id": .stringConvertible(appName)]
                        )
                    }
                }

                // Ensure monitor won't auto-restart it
                await ContainerMonitor.shared.markContainerStopped(appName)

                // Optionally remove the image to free disk space
                if deleteImage, let imageName {
                    try await withSpan("deleteImage") { imageSpan in
                        imageSpan.attributes["image.name"] = imageName
                        do {
                            try await client.deleteImage(named: imageName)
                            logger.info(
                                "Deleted container image",
                                metadata: [
                                    "container-id": .stringConvertible(appName),
                                    "image": .stringConvertible(imageName),
                                ]
                            )
                            imageSpan.attributes["image.deleted"] = true
                        } catch let error as RPCError where error.code == .notFound {
                            logger.info(
                                "Image already deleted",
                                metadata: [
                                    "container-id": .stringConvertible(appName),
                                    "image": .stringConvertible(imageName),
                                ]
                            )
                            imageSpan.attributes["image.deleted"] = false
                        }
                    }
                }
            }

            span.setStatus(.init(code: .ok))
            return ServerResponse(message: .init())
        }
    }

}
