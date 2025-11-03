import ContainerRegistry
import Foundation
import GRPCCore
import Logging

public struct PullImage {
    public init() {
    }

    public func pullAndRun(
        image: String,
        appName: String,
        labels: [String: String],
        bearerToken: String,
        registry: String
    ) async throws {
        let logger = Logger(label: "sh.wendy.pull-image")
        let imageRef = try ImageReference(fromString: image, defaultRegistry: registry)

        // Create registry client without auth (will use default gcloud credentials if needed)
        let registry = try await RegistryClient(
            registry: imageRef.registry,
            auth: AuthHandler(bearer: bearerToken)
        )

        logger.info("Fetching manifest for \(imageRef.repository):\(imageRef.reference)")

        // Try to fetch as an index first (multi-platform images)
        var manifestReference = imageRef.reference
        do {
            let index = try await registry.getIndex(
                repository: imageRef.repository,
                reference: imageRef.reference
            )

            // Find the linux/arm64 or linux manifest
            if let linuxManifest = index.manifests.first(where: { desc in
                desc.platform?.os == "linux"
                    && (desc.platform?.architecture == "arm64"
                        || desc.mediaType.contains("image.manifest"))
            }) {
                logger.info("Found platform-specific manifest: \(linuxManifest.digest)")
                manifestReference = linuxManifest.digest
            }
        } catch {
            logger.debug("Not a multi-platform index, using direct reference")
        }

        // Get the manifest for the image
        let manifest = try await registry.getManifest(
            repository: imageRef.repository,
            reference: manifestReference
        )

        logger.info("✓ Downloaded manifest for \(image)")
        logger.info("  Digest: \(manifest.digest)")
        logger.info("  Layers: \(manifest.layers.count)")

        try await Containerd.withClient { containerd in
            async let configData = try await registry.getBlob(
                repository: imageRef.repository,
                digest: manifest.config.digest
            )
            for layer in manifest.layers {
                let content = try await containerd.collectContent()
                if content.contains(where: { $0.digest == layer.digest }) {
                    logger.debug("Layer \(layer.digest) already exists")
                    continue
                }
                // Add labels to prevent garbage collection of pulled layers
                let labels = [
                    "containerd.io/gc.root": "true",
                    "sh.wendy.layer": "true",
                    "sh.wendy.source": "registry"
                ]
                try await containerd.writeLayer(ref: layer.digest, labels: labels) { writer in
                    let data = try await registry.getBlob(
                        repository: imageRef.repository,
                        digest: layer.digest
                    )

                    for chunk in data.chunks(ofCount: 100_000) {
                        try await writer.write(data: chunk)
                    }
                }
            }

            try? await containerd.stopTask(containerID: appName)
            let (manifestHash, manifestSize) = try await containerd.uploadJSON(manifest)

            do {
                try await containerd.createImage(
                    named: appName,
                    manifestHash: manifestHash,
                    manifestSize: manifestSize
                )
            } catch {
                try await containerd.updateImage(
                    named: appName,
                    manifestHash: manifestHash,
                    manifestSize: manifestSize
                )
            }

            let (snapshotKey, mounts) = try await containerd.createSnapshot(
                imageName: appName,
                appName: appName,
                layers: manifest.layers.map { layer in
                    .with {
                        $0.diffID = layer.digest
                        $0.digest = layer.digest
                        $0.size = layer.size
                        $0.gzip = layer.mediaType.contains("gzip")
                    }
                }
            )

            // Decode the image config to extract entrypoint, cmd, env, etc.
            let decoder = JSONDecoder()
            let imageConfig = try decoder.decode(
                ImageConfiguration.self,
                from: try await configData
            )

            // Build OCI RuntimeSpec with process information from image config
            let args = imageConfig.config?.Entrypoint ?? imageConfig.config?.Cmd ?? ["/bin/sh"]
            let workingDir = imageConfig.config?.WorkingDir ?? "/"
            let env = imageConfig.config?.Env ?? []

            let spec = OCI(
                args: args,
                env: env,
                workingDir: workingDir,
                appName: appName
            )

            // spec.applyEntitlements(
            //     entitlements: appConfig.entitlements,
            //     appName: request.appName
            // )

            let runtimeSpecData = try JSONEncoder().encode(spec)

            do {
                logger.info(
                    "Creating container \(image) from \(imageRef.repository)"
                )
                try await containerd.createContainer(
                    imageName: appName,
                    appName: appName,
                    snapshotKey: snapshotKey ?? "",
                    ociSpec: runtimeSpecData,
                    labels: labels
                )
            } catch let error as RPCError where error.code == .alreadyExists {
                logger.debug("Container already exists, updating container")
                try await containerd.updateContainer(
                    imageName: appName,
                    appName: appName,
                    snapshotKey: snapshotKey ?? "",
                    ociSpec: runtimeSpecData
                )
            }

            func run(
                stdout: String?,
                stderr: String?
            ) async throws {
                do {
                    logger.info("Creating task")
                    try await containerd.createTask(
                        containerID: appName,
                        appName: appName,
                        snapshotName: snapshotKey ?? "",
                        mounts: mounts,
                        stdout: stdout,
                        stderr: stderr
                    )
                } catch let error as RPCError where error.code == .alreadyExists {
                    logger.info(
                        "Task already exists, re-creating it",
                        metadata: [
                            "container-id": .stringConvertible(appName)
                        ]
                    )
                    try? await containerd.deleteTask(containerID: appName)
                    logger.debug(
                        "Task removed",
                        metadata: [
                            "container-id": .stringConvertible(appName)
                        ]
                    )
                    try await containerd.createTask(
                        containerID: appName,
                        appName: appName,
                        snapshotName: snapshotKey ?? "",
                        mounts: mounts,
                        stdout: stdout,
                        stderr: stderr
                    )
                    logger.debug(
                        "Task created",
                        metadata: [
                            "container-id": .stringConvertible(appName)
                        ]
                    )
                }
                // try await containerd.withStdout { stdout, stderr in
                // try await run(stdout: stdout, stderr: stderr)
                // } onStdout: { bytes in
                //     try await writer.write(
                //         .with {
                //             $0.stdoutOutput.data = Data(buffer: bytes)
                //         }
                //     )
                // } onStderr: { bytes in
                //     try await writer.write(
                //         .with {
                //             $0.stderrOutput.data = Data(buffer: bytes)
                //         }
                //     )
                // }
            }

            logger.info("Running app")
            try await run(stdout: nil, stderr: nil)

            try await containerd.runTask(containerID: appName)
        }
    }
}
