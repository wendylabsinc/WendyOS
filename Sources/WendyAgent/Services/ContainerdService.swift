import ContainerRegistry
import ContainerdGRPC
import Crypto
import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import Logging
import NIOCore
import NIOPosix
import WendyAgentGRPC

#if canImport(Musl)
    import Musl
#endif

// MARK: - FIFO Management Protocol

/// Protocol for managing FIFO operations, allowing for testing and mocking
public protocol FIFOManager: Sendable {
    /// Creates a FIFO (named pipe) at the specified path
    func createFIFO(path: String, permissions: mode_t) throws

    /// Opens a FIFO for reading and returns the file descriptor
    func openForReading(path: String) throws -> Int32

    /// Removes a FIFO from the filesystem
    func removeFIFO(path: String)
}

/// Production implementation using real system calls
public struct SystemFIFOManager: FIFOManager {
    public init() {}

    public func createFIFO(path: String, permissions: mode_t) throws {
        guard mkfifo(path, permissions) == 0 else {
            throw RPCError(
                code: .internalError,
                message: "Failed to create FIFO at \(path): errno \(errno)"
            )
        }
    }

    public func openForReading(path: String) throws -> Int32 {
        let fd = open(path, O_RDONLY | O_NONBLOCK)
        guard fd >= 0 else {
            throw RPCError(
                code: .internalError,
                message: "Failed to open FIFO at \(path) for reading: errno \(errno)"
            )
        }
        return fd
    }

    public func removeFIFO(path: String) {
        unlink(path)
    }
}

// MARK: - Containerd Client

struct NamespaceInterceptor: ClientInterceptor {
    let namespace: String

    init(namespace: String = "default") {
        self.namespace = namespace
    }

    func intercept<Input, Output>(
        request: StreamingClientRequest<Input>,
        context: ClientContext,
        next: (StreamingClientRequest<Input>, ClientContext) async throws ->
            StreamingClientResponse<Output>
    ) async throws -> StreamingClientResponse<Output> where Input: Sendable, Output: Sendable {
        var request = request
        request.metadata.addString(namespace, forKey: "containerd-namespace")
        return try await next(request, context)
    }
}

public struct Containerd: Sendable {
    let client: GRPCClient<HTTP2ClientTransport.Posix>
    let logger = Logger(label: "Containerd")
    let fifoManager: FIFOManager

    // Protocol-based dependencies for testing (optional, default to wrapping client)
    private let _containersClient:
        (any Containerd_Services_Containers_V1_Containers.ClientProtocol)?
    private let _tasksClient: (any Containerd_Services_Tasks_V1_Tasks.ClientProtocol)?

    // Computed properties that return either injected protocols or create from client
    private var containersClient: any Containerd_Services_Containers_V1_Containers.ClientProtocol {
        _containersClient ?? Containerd_Services_Containers_V1_Containers.Client(wrapping: client)
    }

    private var tasksClient: any Containerd_Services_Tasks_V1_Tasks.ClientProtocol {
        _tasksClient ?? Containerd_Services_Tasks_V1_Tasks.Client(wrapping: client)
    }

    // Protocol-based service dependencies for image unpacking (testable)
    let imagesService: ContainerdImagesService
    let contentService: ContainerdContentService
    let snapshotsService: ContainerdSnapshotsService
    let diffsService: ContainerdDiffsService

    /// Initialize a Containerd client
    /// - Parameters:
    ///   - client: The gRPC client for containerd
    ///   - fifoManager: The FIFO manager (defaults to SystemFIFOManager for production)
    ///   - imagesService: Images service implementation (defaults to gRPC client wrapper)
    ///   - contentService: Content service implementation (defaults to gRPC client wrapper)
    ///   - snapshotsService: Snapshots service implementation (defaults to gRPC client wrapper)
    ///   - diffsService: Diffs service implementation (defaults to gRPC client wrapper)
    public init(
        client: GRPCClient<HTTP2ClientTransport.Posix>,
        fifoManager: FIFOManager = SystemFIFOManager(),
        imagesService: ContainerdImagesService? = nil,
        contentService: ContainerdContentService? = nil,
        snapshotsService: ContainerdSnapshotsService? = nil,
        diffsService: ContainerdDiffsService? = nil
    ) {
        self.client = client
        self.fifoManager = fifoManager
        self._containersClient = nil
        self._snapshotsClient = nil
        self._tasksClient = nil
        self.imagesService = imagesService ?? GRPCImagesService(client: client)
        self.contentService = contentService ?? GRPCContentService(client: client)
        self.snapshotsService = snapshotsService ?? GRPCSnapshotsService(client: client)
        self.diffsService = diffsService ?? GRPCDiffsService(client: client)
    }

    /// Initialize a Containerd client with injected protocol dependencies (for testing)
    /// - Parameters:
    ///   - client: The gRPC client (not used when mocks are injected, but required for initialization)
    ///   - containersClient: Mock or real containers client
    ///   - snapshotsClient: Mock or real snapshots client (for container management)
    ///   - tasksClient: Mock or real tasks client
    ///   - imagesService: Mock or real images service (for image unpacking)
    ///   - contentService: Mock or real content service (for image unpacking)
    ///   - snapshotsService: Mock or real snapshots service (for image unpacking)
    ///   - diffsService: Mock or real diffs service (for image unpacking)
    ///   - fifoManager: The FIFO manager (defaults to SystemFIFOManager for production)
    internal init(
        client: GRPCClient<HTTP2ClientTransport.Posix>,
        containersClient: any Containerd_Services_Containers_V1_Containers.ClientProtocol,
        snapshotsClient: any Containerd_Services_Snapshots_V1_Snapshots.ClientProtocol,
        tasksClient: any Containerd_Services_Tasks_V1_Tasks.ClientProtocol,
        imagesService: ContainerdImagesService? = nil,
        contentService: ContainerdContentService? = nil,
        snapshotsService: ContainerdSnapshotsService? = nil,
        diffsService: ContainerdDiffsService? = nil,
        fifoManager: FIFOManager = SystemFIFOManager()
    ) {
        self.client = client
        self.fifoManager = fifoManager
        self._containersClient = containersClient
        self._snapshotsClient = snapshotsClient
        self._tasksClient = tasksClient
        self.imagesService = imagesService ?? GRPCImagesService(client: client)
        self.contentService = contentService ?? GRPCContentService(client: client)
        self.snapshotsService = snapshotsService ?? GRPCSnapshotsService(client: client)
        self.diffsService = diffsService ?? GRPCDiffsService(client: client)
    }

    public static func withClient<R: Sendable>(
        _ run: @escaping (Containerd) async throws -> R
    ) async throws -> R {
        let transport = try HTTP2ClientTransport.Posix(
            target: .unixDomainSocket(path: "/run/containerd/containerd.sock"),
            transportSecurity: .plaintext
        )
        return try await withGRPCClient(
            transport: transport,
            interceptors: [NamespaceInterceptor()]
        ) { client in
            let client = Containerd(client: client)
            return try await run(client)
        }
    }

    public struct LayerWriter: Sendable {
        let ref: String
        let writer: RPCWriter<Containerd_Services_Content_V1_WriteContentRequest>
        fileprivate var offset: Int64 = 0

        init(ref: String, writer: RPCWriter<Containerd_Services_Content_V1_WriteContentRequest>) {
            self.ref = ref
            self.writer = writer
        }

        public mutating func write(data: Data) async throws {
            try await writer.write(
                .with {
                    $0.data = data
                    $0.offset = offset
                    $0.ref = ref
                    $0.action = .write
                }
            )
            offset += Int64(data.count)
        }
    }

    public func uploadJSON(_ config: some Encodable) async throws -> (digest: String, size: Int64) {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys]
        encoder.dateEncodingStrategy = .iso8601
        let encoded = try encoder.encode(config)
        let digest = SHA256.hash(data: encoded)
            .map { String(format: "%02x", $0) }.joined()
        let size = Int64(encoded.count)
        do {
            try await writeLayer(ref: digest) { writer in
                try await writer.write(data: encoded)
            }
        } catch let error as RPCError where error.code == .alreadyExists {
            // Ignore
        }
        return (digest, size)
    }

    public func writeLayer(
        ref: String,
        labels: [String: String] = [:],
        withWriter: @Sendable @escaping (inout LayerWriter) async throws -> Void
    ) async throws {
        let content = Containerd_Services_Content_V1_Content.Client(wrapping: client)
        try await content.write { writer in
            var layerWriter = LayerWriter(ref: ref, writer: writer)
            try await withWriter(&layerWriter)

            try await writer.write(
                .with {
                    $0.ref = ref
                    $0.offset = layerWriter.offset
                    $0.action = .commit
                    if !labels.isEmpty {
                        $0.labels = labels
                    }
                }
            )
        } onResponse: { response in
            for try await _ in response.messages {}
        }
    }

    public func listContent(
        withContent:
            @Sendable @escaping ([Containerd_Services_Content_V1_Info]) async throws ->
            Void
    ) async throws {
        let content = Containerd_Services_Content_V1_Content.Client(wrapping: client)
        return try await content.list(
            request: .init(
                message: .with { req in
                    // No filters
                }
            )
        ) { response in
            for try await items in response.messages {
                try await withContent(items.info)
            }
        }
    }

    public func collectContent() async throws -> [Containerd_Services_Content_V1_Info] {
        let content = Containerd_Services_Content_V1_Content.Client(wrapping: client)
        return try await content.list(
            request: .init(
                message: .with { req in
                    // No filters
                }
            )
        ) { response in
            var allItems = [Containerd_Services_Content_V1_Info]()
            for try await items in response.messages {
                allItems.append(contentsOf: items.info)
            }
            return allItems
        }
    }

    public func deleteImage(named name: String) async throws {
        let images = Containerd_Services_Images_V1_Images.Client(wrapping: client)
        _ = try await images.delete(
            .with {
                $0.name = name
            }
        )
    }

    public func createImage(
        named name: String,
        manifestHash: String,
        manifestSize: Int64
    ) async throws {
        let images = Containerd_Services_Images_V1_Images.Client(wrapping: client)
        try await images.create(
            .with {
                $0.image = .with {
                    $0.name = name
                    $0.target = .with {
                        $0.mediaType = "application/vnd.oci.image.manifest.v1+json"
                        $0.digest = "sha256:\(manifestHash)"
                        $0.size = manifestSize
                    }
                }
            }
        ) { res in
            if case .failure(let error) = res.accepted {
                logger.error(
                    "Failed to create image",
                    metadata: [
                        "image-name": .stringConvertible(name),
                        "manifest-digest": .stringConvertible("sha256:\(manifestHash)"),
                        "manifest-size": .stringConvertible(manifestSize),
                        "error": .stringConvertible(error.description),
                    ]
                )
                throw error
            }
        }
    }

    public func fetchBlob(digest: String) async throws -> Data {
        let content = Containerd_Services_Content_V1_Content.Client(wrapping: client)
        return try await content.read(
            .with {
                $0.digest = digest
            }
        ) { response in
            var data = Data()
            for try await message in response.messages {
                data.append(message.data)
            }
            return data
        }
    }

    public func updateImage(
        named name: String,
        manifestHash: String,
        manifestSize: Int64
    ) async throws {
        let images = Containerd_Services_Images_V1_Images.Client(wrapping: client)
        try await images.update(
            .with {
                $0.image = .with {
                    $0.name = name
                    $0.target = .with {
                        $0.mediaType = "application/vnd.oci.image.manifest.v1+json"
                        $0.digest = "sha256:\(manifestHash)"
                        $0.size = manifestSize
                    }
                }
            }
        ) { res in
            if case .failure(let error) = res.accepted {
                logger.error(
                    "Failed to update image",
                    metadata: [
                        "image-name": .stringConvertible(name),
                        "manifest-digest": .stringConvertible("sha256:\(manifestHash)"),
                        "manifest-size": .stringConvertible(manifestSize),
                        "error": .stringConvertible(error.description),
                    ]
                )
                throw error
            }
        }
    }

    /// Deletes a container and its associated ephemeral snapshot.
    ///
    /// This method ensures complete cleanup following containerd's proper lifecycle:
    /// 1. Retrieving the container's snapshot key and snapshotter before deletion
    /// 2. Deleting any associated task (if running)
    /// 3. Deleting the container from containerd
    /// 4. Attempting to remove the ephemeral snapshot from the snapshotter
    ///
    /// ## Snapshot Cleanup Behavior
    ///
    /// - **Ephemeral snapshots (UUID keys)**: Deleted when container is removed
    /// - **Image layer snapshots (ChainID keys)**: Preserved (shared across containers)
    /// - **Missing snapshots**: Handled gracefully (logged at debug level, operation succeeds)
    /// - **Missing containers**: Returns early without error (idempotent operation)
    /// - **Snapshot deletion failures**: Logged as warnings (containerd's GC will clean up orphans)
    ///
    /// ## Safety Validations
    ///
    /// Before attempting snapshot deletion, this method validates:
    /// - The snapshot key is a valid UUID (ephemeral), not a ChainID (shared layer)
    ///
    /// Containerd itself enforces additional safety checks:
    /// - Rejects deletion if snapshot has children (shouldn't happen for ephemeral snapshots)
    /// - Rejects deletion if snapshot is in use by another container
    ///
    /// ## Snapshot Types
    ///
    /// - **Image layer snapshots**: Identified by ChainID (deterministic hash of layer content).
    ///   These are shared across containers and should NOT be deleted.
    /// - **Ephemeral container snapshots**: Identified by UUID. These are the writable layer
    ///   for each container and SHOULD be deleted when the container is removed.
    ///
    /// ## Error Handling Philosophy
    ///
    /// Once the container is deleted, snapshot deletion errors are logged as warnings rather
    /// than thrown. This prevents partial cleanup failures from propagating. Orphaned snapshots
    /// will be cleaned up by containerd's garbage collector.
    ///
    /// ## Manual Testing
    ///
    /// To manually verify snapshot cleanup behavior:
    ///
    /// ```bash
    /// # 1. Create and run a container
    /// wendy run
    ///
    /// # 2. List snapshots before deletion (note UUID-based snapshot)
    /// ctr snapshots ls
    ///
    /// # 3. Delete the container
    /// ctr containers rm <container-id>
    ///
    /// # 4. Verify ephemeral snapshot was removed
    /// ctr snapshots ls  # UUID snapshot gone, ChainID snapshots remain
    /// ```
    ///
    /// - Parameter name: The container ID to delete
    /// - Throws: RPCError if task or container deletion fails (snapshot errors are logged only)
    public func deleteContainer(named name: String) async throws {
        let containers = containersClient
        let snapshots = snapshotsClient

        // First, get the container to retrieve its snapshot key
        let container: Containerd_Services_Containers_V1_Container
        do {
            container = try await containers.get(
                .with {
                    $0.id = name
                }
            ).container
        } catch let error as RPCError where error.code == .notFound {
            // Container doesn't exist, nothing to delete
            logger.debug(
                "Container not found, nothing to delete",
                metadata: ["container-id": .stringConvertible(name)]
            )
            return
        }

        let snapshotKey = container.snapshotKey
        let snapshotter = container.snapshotter

        // Delete any associated task first (proper containerd lifecycle)
        // Catch all errors and log warnings - a stuck task shouldn't prevent container cleanup
        do {
            try await deleteTask(containerID: name)
            logger.debug(
                "Deleted task before container",
                metadata: ["container-id": .stringConvertible(name)]
            )
        } catch let error as RPCError where error.code == .notFound {
            // No task exists, this is fine - container might not be running
            logger.debug(
                "No task to delete",
                metadata: ["container-id": .stringConvertible(name)]
            )
        } catch let error as RPCError {
            // Task deletion failed for other reasons (timeout, stuck process, permissions)
            // Log warning but continue with container deletion
            logger.warning(
                "Failed to delete task before container deletion, continuing anyway",
                metadata: [
                    "container-id": .stringConvertible(name),
                    "error-code": .stringConvertible(String(describing: error.code)),
                    "error-message": .stringConvertible(error.message),
                ]
            )
        }

        // Delete the container
        do {
            _ = try await containers.delete(
                .with {
                    $0.id = name
                }
            )
            logger.debug(
                "Deleted container",
                metadata: ["container-id": .stringConvertible(name)]
            )
        } catch let error as RPCError where error.code == .notFound {
            // Container was deleted between get and delete (race condition)
            // Continue to snapshot cleanup - another process may have failed to clean it up
            logger.debug(
                "Container already deleted, will still attempt snapshot cleanup",
                metadata: ["container-id": .stringConvertible(name)]
            )
        }

        // Delete the associated ephemeral snapshot if it exists
        // Only delete if the snapshot key is not empty
        if !snapshotKey.isEmpty {
            // Validate snapshotter is also set (data consistency check)
            // Per containerd spec: snapshotKey empty means no snapshot, so snapshotter should also be set
            guard !snapshotter.isEmpty else {
                logger.warning(
                    "Container has snapshot key but missing snapshotter field (data inconsistency)",
                    metadata: [
                        "container-id": .stringConvertible(name),
                        "snapshot-key": .stringConvertible(snapshotKey),
                    ]
                )
                return
            }

            // Validate that this is an ephemeral snapshot (UUID), not a shared layer (ChainID)
            guard Self.isEphemeralSnapshotKey(snapshotKey) else {
                logger.warning(
                    "Snapshot key does not appear to be ephemeral (UUID), skipping deletion to preserve shared layers",
                    metadata: [
                        "container-id": .stringConvertible(name),
                        "snapshot-key": .stringConvertible(snapshotKey),
                        "snapshotter": .stringConvertible(snapshotter),
                    ]
                )
                return
            }

            // Try to delete the ephemeral snapshot
            // Containerd will reject the deletion if the snapshot:
            // - Has children (should never happen for ephemeral/active snapshots)
            // - Is in use by another container
            // - Has other restrictions
            do {
                _ = try await snapshots.remove(
                    .with {
                        $0.key = snapshotKey
                        $0.snapshotter = snapshotter
                    }
                )
                logger.debug(
                    "Deleted ephemeral snapshot",
                    metadata: [
                        "container-id": .stringConvertible(name),
                        "snapshot-key": .stringConvertible(snapshotKey),
                        "snapshotter": .stringConvertible(snapshotter),
                    ]
                )
            } catch let error as RPCError where error.code == .notFound {
                // Snapshot was already deleted (race condition) - this is OK
                logger.debug(
                    "Ephemeral snapshot not found during cleanup",
                    metadata: [
                        "container-id": .stringConvertible(name),
                        "snapshot-key": .stringConvertible(snapshotKey),
                        "snapshotter": .stringConvertible(snapshotter),
                    ]
                )
            } catch let error as RPCError {
                // Other errors (failedPrecondition, invalidArgument, permissionDenied, etc.)
                // Log a warning but don't fail - container is already deleted
                // Note: Snapshot is now orphaned but containerd's GC should eventually clean it up
                logger.warning(
                    "Failed to delete ephemeral snapshot after container deletion",
                    metadata: [
                        "container-id": .stringConvertible(name),
                        "snapshot-key": .stringConvertible(snapshotKey),
                        "snapshotter": .stringConvertible(snapshotter),
                        "error-code": .stringConvertible(String(describing: error.code)),
                        "error-message": .stringConvertible(error.message),
                    ]
                )
            }
        } else {
            logger.debug(
                "Container has no snapshot key, skipping snapshot cleanup",
                metadata: ["container-id": .stringConvertible(name)]
            )
        }
    }

    /// Validates that a snapshot key is an ephemeral snapshot (UUID format).
    ///
    /// Ephemeral snapshots use UUID keys (e.g., "550e8400-e29b-41d4-a716-446655440000").
    /// Image layer snapshots use ChainID format (e.g., "sha256:abc123...").
    ///
    /// - Parameter key: The snapshot key to validate
    /// - Returns: true if the key is a valid UUID (ephemeral), false otherwise
    internal static func isEphemeralSnapshotKey(_ key: String) -> Bool {
        // Use Foundation's UUID parser for robust validation
        // This handles case-insensitivity and proper UUID format validation
        return UUID(uuidString: key) != nil
    }

    public func readJSONContent<D: Decodable & Sendable>(
        digest: String,
        as type: D.Type
    ) async throws -> D {
        let content = Containerd_Services_Content_V1_Content.Client(wrapping: client)
        return try await content.read(.with { $0.digest = digest }) { response in
            var data = Data()
            for try await message in response.messages {
                data.append(message.data)
            }
            return try JSONDecoder().decode(type, from: data)
        }
    }

    /// Compute ChainID for a layer based on parent ChainID and current DiffID.
    /// ChainID is a cryptographic identifier representing the cumulative state of all layers.
    /// - For the first layer: ChainID = DiffID
    /// - For subsequent layers: ChainID = SHA256(ChainID(parent) + " " + DiffID(current))
    private func computeChainID(parent: String?, diffID: String) -> String {
        // Normalize diffID to remove "sha256:" prefix if present
        let normalizedDiffID = diffID.replacingOccurrences(of: "sha256:", with: "")

        guard let parent = parent, !parent.isEmpty else {
            // Base case: first layer's ChainID equals its DiffID
            return "sha256:\(normalizedDiffID)"
        }

        // Normalize parent to remove "sha256:" prefix
        let normalizedParent = parent.replacingOccurrences(of: "sha256:", with: "")

        // Compute: SHA256(parent + " " + diffID)
        let combined = "sha256:\(normalizedParent) sha256:\(normalizedDiffID)"
        let hash = SHA256.hash(data: Data(combined.utf8))
        let chainID = hash.map { String(format: "%02x", $0) }.joined()

        return "sha256:\(chainID)"
    }

    /// Unpack an image from the content store into snapshots.
    /// This is required when images are pushed to the registry but not yet unpacked.
    public func unpackImage(
        named imageName: String
    ) async throws -> (snapshotKey: String?, mounts: [Containerd_Types_Mount]) {
        logger.info("Unpacking image", metadata: ["image": .stringConvertible(imageName)])

        // Get the image
        let image = try await imagesService.get(.with { $0.name = imageName }).image

        // Read the manifest
        let manifest = try await self.readJSONContent(
            digest: image.target.digest,
            as: ImageManifest.self
        )

        // Read the image config to get DiffIDs (uncompressed layer hashes)
        let config = try await self.readJSONContent(
            digest: manifest.config.digest,
            as: ImageConfiguration.self
        )

        // Verify we have matching layer counts
        guard manifest.layers.count == config.rootfs.diff_ids.count else {
            throw RPCError(
                code: .internalError,
                message:
                    "Manifest layer count (\(manifest.layers.count)) doesn't match config diff_ids count (\(config.rootfs.diff_ids.count))"
            )
        }

        // Unpack each layer, reusing existing snapshots when possible
        var previousChainID: String? = nil
        let totalLayers = manifest.layers.count
        var snapshotsReused = 0
        var snapshotsCreated = 0

        for (index, layer) in manifest.layers.enumerated() {
            // Get the DiffID for this layer from the config
            let diffID = config.rootfs.diff_ids[index]

            // Compute the ChainID for this layer
            let chainID = computeChainID(parent: previousChainID, diffID: diffID)
            let layerKey = chainID

            logger.info(
                "Processing layer",
                metadata: [
                    "layer-index": .stringConvertible("\(index + 1)/\(totalLayers)"),
                    "layer-digest": .stringConvertible(layer.digest),
                    "layer-diff-id": .stringConvertible(diffID),
                    "layer-chain-id": .stringConvertible(chainID),
                    "layer-size": .stringConvertible(layer.size),
                    "layer-size-mb": .stringConvertible(
                        String(format: "%.2f", Double(layer.size) / 1_000_000.0)
                    ),
                    "layer-media-type": .stringConvertible(layer.mediaType),
                ]
            )

            // Check if snapshot already exists
            let snapshotExists: Bool
            do {
                _ = try await snapshotsService.stat(
                    .with {
                        $0.key = layerKey
                        $0.snapshotter = "overlayfs"
                    }
                )
                snapshotExists = true
            } catch let error as RPCError where error.code == .notFound {
                snapshotExists = false
            } catch {
                // Log unexpected errors but propagate them
                logger.error(
                    "Unexpected error checking snapshot existence",
                    metadata: [
                        "layer-index": .stringConvertible("\(index + 1)/\(totalLayers)"),
                        "chain-id": .stringConvertible(chainID),
                        "error": .stringConvertible(String(describing: error)),
                    ]
                )
                throw error
            }

            if snapshotExists {
                snapshotsReused += 1
                logger.debug(
                    "Snapshot already exists, reusing",
                    metadata: [
                        "layer-index": .stringConvertible("\(index + 1)/\(totalLayers)"),
                        "chain-id": .stringConvertible(chainID),
                    ]
                )
                previousChainID = chainID
                continue
            }

            // Snapshot doesn't exist, we'll create it
            snapshotsCreated += 1

            // Snapshot doesn't exist, create it
            let tmpKey = UUID().uuidString

            // Prepare snapshot
            let snapshot = try await snapshotsService.prepare(
                .with {
                    $0.key = tmpKey
                    if let parent = previousChainID {
                        $0.parent = parent
                    }
                    $0.snapshotter = "overlayfs"
                }
            )

            // Apply diff - this is the most expensive operation
            _ = try await diffsService.apply(
                .with {
                    $0.diff = .with {
                        $0.digest = layer.digest
                        $0.size = layer.size
                        $0.mediaType = layer.mediaType
                    }
                    $0.mounts = snapshot.mounts
                }
            )
            logger.info(
                "Applied diff",
                metadata: [
                    "layer-index": .stringConvertible("\(index + 1)/\(totalLayers)"),
                    "layer-size-mb": .stringConvertible(
                        String(format: "%.2f", Double(layer.size) / 1_000_000.0)
                    ),
                ]
            )

            // Commit snapshot
            do {
                try await snapshotsService.commit(
                    .with {
                        $0.key = tmpKey
                        $0.name = layerKey
                        $0.snapshotter = "overlayfs"
                    }
                )
                logger.debug(
                    "Committed snapshot",
                    metadata: [
                        "chain-id": .stringConvertible(chainID)
                    ]
                )
            } catch let error as RPCError where error.code == .alreadyExists {
                // Race condition: snapshot was created by another process
                logger.debug(
                    "Snapshot was created concurrently, cleaning up temporary snapshot",
                    metadata: [
                        "chain-id": .stringConvertible(chainID)
                    ]
                )
                do {
                    let snapshotsClient = Containerd_Services_Snapshots_V1_Snapshots.Client(wrapping: client)
                    _ = try await snapshotsClient.remove(
                        .with {
                            $0.key = tmpKey
                            $0.snapshotter = "overlayfs"
                        }
                    )
                } catch let removeError as RPCError where removeError.code == .notFound {
                    // Already cleaned up, this is fine
                    logger.debug(
                        "Temporary snapshot already removed",
                        metadata: ["tmp-key": .stringConvertible(tmpKey)]
                    )
                }
            }

            previousChainID = chainID
        }

        logger.info(
            "Image unpacked successfully",
            metadata: [
                "image": .stringConvertible(imageName),
                "total-layers": .stringConvertible(totalLayers),
                "snapshots-reused": .stringConvertible(snapshotsReused),
                "snapshots-created": .stringConvertible(snapshotsCreated),
                "reuse-percentage": .stringConvertible(
                    String(
                        format: "%.1f%%",
                        totalLayers > 0
                            ? (Double(snapshotsReused) / Double(totalLayers)) * 100.0 : 0.0
                    )
                ),
            ]
        )

        guard let previousChainID else {
            throw RPCError(code: .internalError, message: "Failed to unpack image")
        }

        let ephemeralKey = UUID().uuidString
        let ephemeralSnapshot = try await snapshotsService.prepare(
            .with {
                $0.key = ephemeralKey
                $0.parent = previousChainID
                $0.snapshotter = "overlayfs"
            }
        )

        // Use mounts from the prepare response (optimization: avoid extra RPC call)
        return (snapshotKey: ephemeralKey, mounts: ephemeralSnapshot.mounts)
    }

    public func createContainer(
        imageName: String,
        appName: String,
        snapshotKey: String,
        ociSpec spec: Data,
        labels: [String: String],
        runtime: String = "io.containerd.runc.v2",
        options: Containerd_Runc_V1_Options? = nil
    ) async throws {
        let containers = Containerd_Services_Containers_V1_Containers.Client(wrapping: client)
        try await containers.create(
            .with {
                $0.container = try .with {
                    $0.id = appName
                    $0.runtime = try .with {
                        $0.name = runtime
                        if let options {
                            $0.options = try .init(message: options)
                        }
                    }
                    $0.spec = .with {
                        $0.typeURL = "types.containerd.io/opencontainers/runtime-spec/1/Spec"
                        $0.value = spec
                    }
                    $0.snapshotter = "overlayfs"
                    $0.snapshotKey = snapshotKey
                    $0.labels = labels
                    $0.image = imageName
                }
            }
        ) { res in
            if case .failure(let error) = res.accepted {
                logger.error(
                    "Failed to create container",
                    metadata: [
                        "app-name": .stringConvertible(appName),
                        "image-name": .stringConvertible(imageName),
                        "error": .stringConvertible(error.description),
                    ]
                )
                throw error
            }
        }
    }

    public func updateContainer(
        imageName: String,
        appName: String,
        snapshotKey: String,
        ociSpec: Data,
        labels: [String: String],
        runtime: String = "io.containerd.runc.v2",
        options: Containerd_Runc_V1_Options? = nil
    ) async throws {
        do {
            let containers = Containerd_Services_Containers_V1_Containers.Client(wrapping: client)
            _ = try await containers.update(
                .with {
                    $0.container = try .with {
                        $0.id = appName
                        $0.runtime = try .with {
                            $0.name = runtime
                            if let options {
                                $0.options = try .init(message: options)
                            }
                        }
                        $0.spec = .with {
                            $0.typeURL = "types.containerd.io/opencontainers/runtime-spec/1/Spec"
                            $0.value = ociSpec
                        }
                        $0.snapshotter = "overlayfs"
                        $0.snapshotKey = snapshotKey
                        $0.image = imageName
                        $0.labels = labels
                    }
                }
            )
        } catch let error as RPCError {
            logger.error(
                "Failed to update container",
                metadata: [
                    "app-name": .stringConvertible(appName),
                    "image-name": .stringConvertible(imageName),
                    "snapshot-key": .stringConvertible(snapshotKey),
                    "error": .stringConvertible(error.description),
                ]
            )
            throw error
        }
    }

    public func stopTask(
        containerID: String,
        signal: UInt32 = 9
    ) async throws {
        let tasks = Containerd_Services_Tasks_V1_Tasks.Client(wrapping: client)
        _ = try await tasks.kill(
            .with {
                $0.containerID = containerID
                $0.signal = signal
            }
        )
    }

    public func listContainers() async throws -> [Containerd_Services_Containers_V1_Container] {
        let containers = Containerd_Services_Containers_V1_Containers.Client(wrapping: client)
        let apps = try await containers.list(request: .init(message: .init()))
        return apps.containers
    }

    public func listTasks() async throws -> [Containerd_V1_Types_Process] {
        let tasks = Containerd_Services_Tasks_V1_Tasks.Client(wrapping: client)
        return try await tasks.list(.init()).tasks
    }

    public func getContainer(
        named: String
    ) async throws -> Containerd_Services_Containers_V1_Container {
        let containers = Containerd_Services_Containers_V1_Containers.Client(wrapping: client)
        return try await containers.get(
            .with {
                $0.id = named
            }
        ).container
    }

    public func mountsSnapshot(
        named: String
    ) async throws -> Containerd_Services_Snapshots_V1_MountsResponse {
        let snapshots = Containerd_Services_Snapshots_V1_Snapshots.Client(wrapping: client)
        return try await snapshots.mounts(
            .with {
                $0.key = named
                $0.snapshotter = "overlayfs"
            }
        )
    }

    public func createTask(
        containerID: String,
        appName: String,
        mounts: [Containerd_Types_Mount],
        stdout: String?,
        stderr: String?,
        runtime: String = "io.containerd.runc.v2"
    ) async throws {
        let tasks = Containerd_Services_Tasks_V1_Tasks.Client(wrapping: client)
        do {
            _ = try await tasks.create(
                .with {
                    $0.containerID = containerID
                    $0.runtimePath = runtime
                    $0.rootfs = mounts
                    $0.terminal = false
                    if let stdout {
                        $0.stdout = stdout
                    }
                    if let stderr {
                        $0.stderr = stderr
                    }
                }
            )
        } catch let error as RPCError {
            logger.error(
                "Failed to create task",
                metadata: [
                    "container-id": .stringConvertible(containerID),
                    "app-name": .stringConvertible(appName),
                    "error": .stringConvertible(error.description),
                ]
            )
            throw error
        }
    }

    public func withStdout<T: Sendable>(
        perform: (String, String) async throws -> T,
        onStdout: @Sendable @escaping (ByteBuffer) async throws -> Void,
        onStderr: @Sendable @escaping (ByteBuffer) async throws -> Void
    ) async throws -> T {
        let id = UUID().uuidString
        // Use /run instead of /tmp because systemd PrivateTmp=true isolates /tmp
        // /run is shared between wendy-agent and containerd
        let fifoDir = "/run/wendy-agent"
        // Ensure the directory exists
        try? FileManager.default.createDirectory(atPath: fifoDir, withIntermediateDirectories: true)
        let stdoutSocketPath = "\(fifoDir)/attach-\(id)-stdout.sock"
        let stderrSocketPath = "\(fifoDir)/attach-\(id)-stderr.sock"

        // Create FIFOs using the injected manager
        try fifoManager.createFIFO(path: stdoutSocketPath, permissions: 0o644)
        try fifoManager.createFIFO(path: stderrSocketPath, permissions: 0o644)

        defer {
            // Clean up FIFOs when done
            fifoManager.removeFIFO(path: stdoutSocketPath)
            fifoManager.removeFIFO(path: stderrSocketPath)
        }

        logger.info("Creating task group")

        // Use continuations to wait for both FIFOs to be ready
        let (stdoutReady, stdoutContinuation) = AsyncStream.makeStream(of: Void.self)
        let (stderrReady, stderrContinuation) = AsyncStream.makeStream(of: Void.self)

        return try await withThrowingTaskGroup(of: Void.self) { group in
            group.addTask { [fifoManager] in
                let stdoutFd = try fifoManager.openForReading(path: stdoutSocketPath)
                logger.info("Creating stdout pipe")
                let stdoutPipe = try await NIOPipeBootstrap(
                    group: .singletonMultiThreadedEventLoopGroup
                )
                .takingOwnershipOfDescriptor(input: stdoutFd)
                .flatMapThrowing { channel in
                    try NIOAsyncChannel<ByteBuffer, Never>(wrappingChannelSynchronously: channel)
                }
                .get()
                logger.info("Stdout pipe ready")
                stdoutContinuation.yield(())
                logger.info("Executing stdout pipe")
                try await stdoutPipe.executeThenClose { stdout in
                    for try await bytes in stdout {
                        try await onStdout(bytes)
                    }
                }
            }
            group.addTask { [fifoManager] in
                let stderrFd = try fifoManager.openForReading(path: stderrSocketPath)
                logger.info("Creating stderr pipe")
                let stderrPipe = try await NIOPipeBootstrap(
                    group: .singletonMultiThreadedEventLoopGroup
                )
                .takingOwnershipOfDescriptor(input: stderrFd)
                .flatMapThrowing { channel in
                    try NIOAsyncChannel<ByteBuffer, Never>(wrappingChannelSynchronously: channel)
                }
                .get()
                logger.info("Stderr pipe ready")
                stderrContinuation.yield(())
                logger.info("Executing stderr pipe")
                try await stderrPipe.executeThenClose { stderr in
                    for try await bytes in stderr {
                        try await onStderr(bytes)
                    }
                }
            }

            // Wait for both FIFOs to be opened before calling perform
            async let stdoutReadySignal: Void? = stdoutReady.first { _ in true }
            async let stderrReadySignal: Void? = stderrReady.first { _ in true }
            _ = await (stdoutReadySignal, stderrReadySignal)

            logger.info("Both FIFOs ready, performing task")
            stdoutContinuation.finish()
            stderrContinuation.finish()
            let result = try await perform(stdoutSocketPath, stderrSocketPath)

            try await group.waitForAll()
            return result
        }
    }

    /// Wait for a task to exit and then delete it.
    /// This function will wait up to the specified timeout for the task to exit.
    /// If the task is still running after the kill signal, it will wait for it to exit.
    public func deleteTask(containerID: String, waitTimeout: Duration = .seconds(5)) async throws {
        let tasks = tasksClient
        let runningTasks = try await tasks.list(.init())

        for runningTask in runningTasks.tasks {
            logger.info(
                "Found task",
                metadata: [
                    "container-id": .stringConvertible(runningTask.containerID),
                    "task-id": .stringConvertible(runningTask.id),
                    "has-exited": .stringConvertible(runningTask.hasExitedAt),
                ]
            )

            guard runningTask.containerID == containerID || runningTask.id == containerID else {
                logger.debug(
                    "Ignoring task due to containerID mismatch",
                    metadata: [
                        "expected-container-id": .stringConvertible(containerID),
                        "found-container-id": .stringConvertible(runningTask.containerID),
                        "found-task-id": .stringConvertible(runningTask.id),
                    ]
                )
                continue
            }

            // If task hasn't exited yet, wait for it to exit
            if !runningTask.hasExitedAt {
                logger.debug(
                    "Task is still running, waiting for it to exit",
                    metadata: [
                        "container-id": .stringConvertible(containerID),
                        "task-id": .stringConvertible(runningTask.id),
                    ]
                )

                // Wait for task to exit with a timeout
                let startTime = ContinuousClock.now
                var hasExited = false

                while !hasExited && (ContinuousClock.now - startTime) < waitTimeout {
                    try await Task.sleep(for: .milliseconds(100))

                    // Check if task has exited
                    let updatedTasks = try await tasks.list(.init())
                    if let updatedTask = updatedTasks.tasks.first(where: {
                        $0.containerID == runningTask.containerID || $0.id == runningTask.id
                    }) {
                        hasExited = updatedTask.hasExitedAt
                    } else {
                        // Task no longer in list, consider it exited
                        hasExited = true
                    }
                }

                if !hasExited {
                    logger.warning(
                        "Task did not exit within timeout, attempting delete anyway",
                        metadata: [
                            "container-id": .stringConvertible(containerID),
                            "task-id": .stringConvertible(runningTask.id),
                            "timeout": .stringConvertible(waitTimeout),
                        ]
                    )
                }
            }

            // Now delete the task
            logger.debug(
                "Deleting task",
                metadata: [
                    "container-id": .stringConvertible(containerID),
                    "task-id": .stringConvertible(runningTask.id),
                ]
            )

            do {
                _ = try await tasks.delete(
                    .with {
                        $0.containerID = runningTask.id
                    }
                )
                logger.debug(
                    "Task deleted successfully",
                    metadata: [
                        "container-id": .stringConvertible(containerID),
                        "task-id": .stringConvertible(runningTask.id),
                    ]
                )
            } catch let error as RPCError {
                logger.error(
                    "Failed to delete task",
                    metadata: [
                        "container-id": .stringConvertible(containerID),
                        "task-id": .stringConvertible(runningTask.id),
                        "error": .stringConvertible(error.message),
                        "error-code": .stringConvertible(String(describing: error.code)),
                    ]
                )
                throw error
            }
        }
    }

    public func runTask(containerID: String) async throws {
        let tasks = Containerd_Services_Tasks_V1_Tasks.Client(wrapping: client)
        try await tasks.start(
            .with {
                $0.containerID = containerID
            }
        ) { res in
            if case .failure(let error) = res.accepted {
                logger.error(
                    "Failed to run container",
                    metadata: [
                        "container-id": .stringConvertible(containerID),
                        "error": .stringConvertible(error.description),
                    ]
                )
                throw error
            }
        }
    }
}
