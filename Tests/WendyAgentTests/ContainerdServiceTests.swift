import ContainerdGRPC
import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import Testing

@testable import wendy_agent

@Suite("ContainerdService.unpackImage Tests")
struct UnpackImageTests {

    // MARK: - Mock Data Structures

    struct MockImageManifest: Codable {
        let config: ManifestDescriptor
        let layers: [ManifestDescriptor]

        struct ManifestDescriptor: Codable {
            let digest: String
            let size: Int64
            let mediaType: String
        }
    }

    struct MockImageConfig: Codable {
        let rootfs: RootFS

        struct RootFS: Codable {
            let diff_ids: [String]

            enum CodingKeys: String, CodingKey {
                case diff_ids
            }
        }
    }

    // MARK: - Mock Implementations

    /// Mock implementation of ContainerdImagesService
    actor MockImagesService: ContainerdImagesService {
        var images: [String: Containerd_Services_Images_V1_Image] = [:]

        func get(
            _ request: Containerd_Services_Images_V1_GetImageRequest
        ) async throws -> Containerd_Services_Images_V1_GetImageResponse {
            guard let image = images[request.name] else {
                throw RPCError(code: .notFound, message: "Image not found: \(request.name)")
            }
            return Containerd_Services_Images_V1_GetImageResponse.with { $0.image = image }
        }

        func addImage(name: String, manifestDigest: String, manifestSize: Int64) {
            images[name] = Containerd_Services_Images_V1_Image.with {
                $0.name = name
                $0.target = Containerd_Types_Descriptor.with {
                    $0.digest = manifestDigest
                    $0.size = manifestSize
                    $0.mediaType = "application/vnd.oci.image.manifest.v1+json"
                }
            }
        }
    }

    /// Mock implementation of ContainerdContentService
    actor MockContentService: ContainerdContentService {
        var blobs: [String: Data] = [:]

        func read<R: Sendable>(
            _ request: Containerd_Services_Content_V1_ReadContentRequest,
            handler: @Sendable @escaping (ContentReadStream) async throws -> R
        ) async throws -> R {
            guard let data = blobs[request.digest] else {
                throw RPCError(code: .notFound, message: "Blob not found: \(request.digest)")
            }

            // Create a streaming response that yields the data
            let stream = AsyncStream<Containerd_Services_Content_V1_ReadContentResponse> {
                (
                    continuation: AsyncStream<Containerd_Services_Content_V1_ReadContentResponse>
                        .Continuation
                ) in
                let response = Containerd_Services_Content_V1_ReadContentResponse.with {
                    $0.data = data
                }
                continuation.yield(response)
                continuation.finish()
            }

            return try await handler(ContentReadStream(stream))
        }

        func addBlob(digest: String, data: Data) {
            blobs[digest] = data
        }

        func addManifest(digest: String, layers: [(String, Int64)], configDigest: String) throws {
            let manifest = MockImageManifest(
                config: .init(digest: configDigest, size: 1000, mediaType: "application/json"),
                layers: layers.map { digest, size in
                    .init(
                        digest: digest,
                        size: size,
                        mediaType: "application/vnd.oci.image.layer.v1.tar+gzip"
                    )
                }
            )
            let data = try JSONEncoder().encode(manifest)
            addBlob(digest: digest, data: data)
        }

        func addConfig(digest: String, diffIDs: [String]) throws {
            let config = MockImageConfig(rootfs: .init(diff_ids: diffIDs))
            let data = try JSONEncoder().encode(config)
            addBlob(digest: digest, data: data)
        }
    }

    /// Mock implementation of ContainerdSnapshotsService
    actor MockSnapshotsService: ContainerdSnapshotsService {
        var snapshots: Set<String> = []
        var preparedCount = 0
        var committedCount = 0
        var statCalls: [String] = []
        var prepareCalls: [String] = []
        var commitCalls: [String] = []

        func stat(
            _ request: Containerd_Services_Snapshots_V1_StatSnapshotRequest
        ) async throws -> Containerd_Services_Snapshots_V1_StatSnapshotResponse {
            statCalls.append(request.key)
            if snapshots.contains(request.key) {
                return Containerd_Services_Snapshots_V1_StatSnapshotResponse.with {
                    $0.info = Containerd_Services_Snapshots_V1_Info.with {
                        $0.name = request.key
                        $0.kind = .committed
                    }
                }
            } else {
                throw RPCError(code: .notFound, message: "Snapshot not found: \(request.key)")
            }
        }

        func prepare(
            _ request: Containerd_Services_Snapshots_V1_PrepareSnapshotRequest
        ) async throws -> Containerd_Services_Snapshots_V1_PrepareSnapshotResponse {
            preparedCount += 1
            prepareCalls.append(request.key)
            return Containerd_Services_Snapshots_V1_PrepareSnapshotResponse.with {
                $0.mounts = [
                    Containerd_Types_Mount.with {
                        $0.type = "overlay"
                        $0.source = "overlay"
                    }
                ]
            }
        }

        func commit(
            _ request: Containerd_Services_Snapshots_V1_CommitSnapshotRequest
        ) async throws {
            commitCalls.append(request.name)
            if snapshots.contains(request.name) {
                throw RPCError(
                    code: .alreadyExists,
                    message: "Snapshot already exists: \(request.name)"
                )
            }
            snapshots.insert(request.name)
            committedCount += 1
        }

        func addSnapshot(key: String) {
            snapshots.insert(key)
        }
    }

    /// Mock implementation of ContainerdDiffsService
    actor MockDiffService: ContainerdDiffsService {
        var appliedCount = 0
        var applyCalls: [String] = []

        func apply(
            _ request: Containerd_Services_Diff_V1_ApplyRequest
        ) async throws -> Containerd_Services_Diff_V1_ApplyResponse {
            appliedCount += 1
            applyCalls.append(request.diff.digest)
            return Containerd_Services_Diff_V1_ApplyResponse.with {
                $0.applied = Containerd_Types_Descriptor.with {
                    $0.digest = request.diff.digest
                }
            }
        }
    }

    // MARK: - Tests

    @Test("Image with all layers already unpacked should skip unpacking")
    func allLayersAlreadyUnpacked() async throws {
        // Given: An image with 3 layers where all snapshots exist
        let images = MockImagesService()
        let content = MockContentService()
        let snapshots = MockSnapshotsService()
        let diffs = MockDiffService()

        let imageName = "testapp"
        let manifestDigest = "sha256:manifest123"
        let configDigest = "sha256:config456"
        let layers = [
            ("sha256:layer1", Int64(1000)),
            ("sha256:layer2", Int64(2000)),
            ("sha256:layer3", Int64(3000)),
        ]
        let diffIDs = [
            "sha256:diff1",
            "sha256:diff2",
            "sha256:diff3",
        ]

        // Setup image metadata
        await images.addImage(
            name: imageName,
            manifestDigest: manifestDigest,
            manifestSize: 5000
        )

        // Setup manifest and config blobs
        try await content.addManifest(
            digest: manifestDigest,
            layers: layers,
            configDigest: configDigest
        )
        try await content.addConfig(digest: configDigest, diffIDs: diffIDs)

        // Pre-populate all snapshots as already existing
        await snapshots.addSnapshot(key: "\(imageName)-diff1")
        await snapshots.addSnapshot(key: "\(imageName)-diff2")
        await snapshots.addSnapshot(key: "\(imageName)-diff3")

        // Create a mock gRPC client (not used but required for Containerd init)
        let transport = try HTTP2ClientTransport.Posix(
            target: .unixDomainSocket(path: "/run/containerd/containerd.sock"),
            transportSecurity: .plaintext
        )
        let client = GRPCClient(transport: transport)

        // When: unpackImage is called
        let containerd = Containerd(
            client: client,
            imagesService: images,
            contentService: content,
            snapshotsService: snapshots,
            diffsService: diffs
        )

        _ = try await containerd.unpackImage(named: imageName)

        // Then: All snapshots were checked
        let statCalls = await snapshots.statCalls
        #expect(statCalls.count == 3)
        #expect(statCalls.contains("\(imageName)-diff1"))
        #expect(statCalls.contains("\(imageName)-diff2"))
        #expect(statCalls.contains("\(imageName)-diff3"))

        // And no unpacking occurred
        let preparedCount = await snapshots.preparedCount
        let appliedCount = await diffs.appliedCount
        let committedCount = await snapshots.committedCount

        #expect(preparedCount == 0)
        #expect(appliedCount == 0)
        #expect(committedCount == 0)
    }

    @Test("Image with no unpacked layers should unpack all")
    func noLayersUnpacked() async throws {
        // Given: An image with 3 layers where no snapshots exist
        let images = MockImagesService()
        let content = MockContentService()
        let snapshots = MockSnapshotsService()
        let diffs = MockDiffService()

        let imageName = "testapp"
        let manifestDigest = "sha256:manifest123"
        let configDigest = "sha256:config456"
        let layers = [
            ("sha256:layer1", Int64(1000)),
            ("sha256:layer2", Int64(2000)),
            ("sha256:layer3", Int64(3000)),
        ]
        let diffIDs = [
            "sha256:diff1",
            "sha256:diff2",
            "sha256:diff3",
        ]

        // Setup image metadata
        await images.addImage(
            name: imageName,
            manifestDigest: manifestDigest,
            manifestSize: 5000
        )

        // Setup manifest and config blobs
        try await content.addManifest(
            digest: manifestDigest,
            layers: layers,
            configDigest: configDigest
        )
        try await content.addConfig(digest: configDigest, diffIDs: diffIDs)

        // Do NOT pre-populate any snapshots - they should all be missing

        // Create a mock gRPC client (not used but required for Containerd init)
        let transport = try HTTP2ClientTransport.Posix(
            target: .unixDomainSocket(path: "/run/containerd/containerd.sock"),
            transportSecurity: .plaintext
        )
        let client = GRPCClient(transport: transport)

        // When: unpackImage is called
        let containerd = Containerd(
            client: client,
            imagesService: images,
            contentService: content,
            snapshotsService: snapshots,
            diffsService: diffs
        )

        _ = try await containerd.unpackImage(named: imageName)

        // Then: All snapshots were checked (stat should be called 3 times)
        let statCalls = await snapshots.statCalls
        #expect(statCalls.count == 3)
        #expect(statCalls.contains("\(imageName)-diff1"))
        #expect(statCalls.contains("\(imageName)-diff2"))
        #expect(statCalls.contains("\(imageName)-diff3"))

        // And all layers were unpacked
        let preparedCount = await snapshots.preparedCount
        let appliedCount = await diffs.appliedCount
        let committedCount = await snapshots.committedCount

        #expect(preparedCount == 3)
        #expect(appliedCount == 3)
        #expect(committedCount == 3)

        // Verify the order of operations (prepare -> apply -> commit for each layer)
        let applyCalls = await diffs.applyCalls
        let commitCalls = await snapshots.commitCalls

        // Note: prepare uses temporary UUID keys, so we only verify the count
        #expect(applyCalls == ["sha256:layer1", "sha256:layer2", "sha256:layer3"])
        #expect(commitCalls == ["\(imageName)-diff1", "\(imageName)-diff2", "\(imageName)-diff3"])
    }

    @Test("Image with partially unpacked layers should only unpack missing ones")
    func partiallyUnpacked() async throws {
        // Given: An image with 3 layers where layer 1 exists but layers 2-3 don't
        let images = MockImagesService()
        let content = MockContentService()
        let snapshots = MockSnapshotsService()
        let diffs = MockDiffService()

        let imageName = "testapp"
        let manifestDigest = "sha256:manifest123"
        let configDigest = "sha256:config456"
        let layers = [
            ("sha256:layer1", Int64(1000)),
            ("sha256:layer2", Int64(2000)),
            ("sha256:layer3", Int64(3000)),
        ]
        let diffIDs = [
            "sha256:diff1",
            "sha256:diff2",
            "sha256:diff3",
        ]

        // Setup image metadata
        await images.addImage(
            name: imageName,
            manifestDigest: manifestDigest,
            manifestSize: 5000
        )

        // Setup manifest and config blobs
        try await content.addManifest(
            digest: manifestDigest,
            layers: layers,
            configDigest: configDigest
        )
        try await content.addConfig(digest: configDigest, diffIDs: diffIDs)

        // Pre-populate only the first snapshot
        await snapshots.addSnapshot(key: "\(imageName)-diff1")

        // Create a mock gRPC client
        let transport = try HTTP2ClientTransport.Posix(
            target: .unixDomainSocket(path: "/run/containerd/containerd.sock"),
            transportSecurity: .plaintext
        )
        let client = GRPCClient(transport: transport)

        // When: unpackImage is called
        let containerd = Containerd(
            client: client,
            imagesService: images,
            contentService: content,
            snapshotsService: snapshots,
            diffsService: diffs
        )

        _ = try await containerd.unpackImage(named: imageName)

        // Then: All snapshots were checked (stat called 3 times)
        let statCalls = await snapshots.statCalls
        #expect(statCalls.count == 3)

        // But only 2 layers were unpacked (the missing ones)
        let preparedCount = await snapshots.preparedCount
        let appliedCount = await diffs.appliedCount
        let committedCount = await snapshots.committedCount

        #expect(preparedCount == 2)
        #expect(appliedCount == 2)
        #expect(committedCount == 2)

        // Verify only layers 2 and 3 were processed
        let applyCalls = await diffs.applyCalls
        let commitCalls = await snapshots.commitCalls

        // Note: prepare uses temporary UUID keys, so we only verify the count
        #expect(applyCalls == ["sha256:layer2", "sha256:layer3"])
        #expect(commitCalls == ["\(imageName)-diff2", "\(imageName)-diff3"])
    }

    @Test("Manifest and config layer count mismatch should use digest fallback")
    func manifestConfigLayerMismatch() async throws {
        // Given: Manifest with 3 layers, config with only 2 diff_ids
        let images = MockImagesService()
        let content = MockContentService()
        let snapshots = MockSnapshotsService()
        let diffs = MockDiffService()

        let imageName = "testapp"
        let manifestDigest = "sha256:manifest123"
        let configDigest = "sha256:config456"
        let layers = [
            ("sha256:layer1", Int64(1000)),
            ("sha256:layer2", Int64(2000)),
            ("sha256:layer3", Int64(3000)),  // This layer has no corresponding diff_id
        ]
        // Config only has 2 diff_ids (intentional mismatch)
        let diffIDs = [
            "sha256:diff1",
            "sha256:diff2",
            // Missing diff3 - layer3 should fall back to using its digest
        ]

        // Setup image metadata
        await images.addImage(
            name: imageName,
            manifestDigest: manifestDigest,
            manifestSize: 5000
        )

        // Setup manifest and config blobs
        try await content.addManifest(
            digest: manifestDigest,
            layers: layers,
            configDigest: configDigest
        )
        try await content.addConfig(digest: configDigest, diffIDs: diffIDs)

        // Create a mock gRPC client
        let transport = try HTTP2ClientTransport.Posix(
            target: .unixDomainSocket(path: "/run/containerd/containerd.sock"),
            transportSecurity: .plaintext
        )
        let client = GRPCClient(transport: transport)

        // When: unpackImage is called
        let containerd = Containerd(
            client: client,
            imagesService: images,
            contentService: content,
            snapshotsService: snapshots,
            diffsService: diffs
        )

        _ = try await containerd.unpackImage(named: imageName)

        // Then: Verify layer 3 used the digest as fallback
        let statCalls = await snapshots.statCalls
        #expect(statCalls.count == 3)

        // First two layers use diff_ids
        #expect(statCalls.contains("\(imageName)-diff1"))
        #expect(statCalls.contains("\(imageName)-diff2"))

        // Third layer should fall back to using layer digest (without sha256: prefix)
        #expect(statCalls.contains("\(imageName)-layer3"))

        // Verify all layers were unpacked
        let preparedCount = await snapshots.preparedCount
        let committedCount = await snapshots.committedCount
        #expect(preparedCount == 3)
        #expect(committedCount == 3)
    }

    @Test("Missing blob in content store should throw error")
    func missingBlobThrows() async throws {
        // Given: Image metadata exists but manifest blob is missing
        let images = MockImagesService()
        let content = MockContentService()
        let snapshots = MockSnapshotsService()
        let diffs = MockDiffService()

        let imageName = "testapp"
        let manifestDigest = "sha256:manifest123"

        // Setup image metadata pointing to a manifest that doesn't exist
        await images.addImage(
            name: imageName,
            manifestDigest: manifestDigest,
            manifestSize: 5000
        )

        // Do NOT add the manifest blob to content store - it's missing

        // Create a mock gRPC client
        let transport = try HTTP2ClientTransport.Posix(
            target: .unixDomainSocket(path: "/run/containerd/containerd.sock"),
            transportSecurity: .plaintext
        )
        let client = GRPCClient(transport: transport)

        // When: unpackImage is called
        let containerd = Containerd(
            client: client,
            imagesService: images,
            contentService: content,
            snapshotsService: snapshots,
            diffsService: diffs
        )

        // Then: Should throw notFound error
        await #expect(throws: RPCError.self) {
            _ = try await containerd.unpackImage(named: imageName)
        }
    }

    // MARK: - Unit Tests for Data Structures

    @Test("ImageManifest decoding works correctly")
    func manifestDecoding() throws {
        let json = """
            {
                "config": {
                    "digest": "sha256:abc123",
                    "size": 1234,
                    "mediaType": "application/json"
                },
                "layers": [
                    {
                        "digest": "sha256:layer1",
                        "size": 5000,
                        "mediaType": "application/vnd.oci.image.layer.v1.tar+gzip"
                    },
                    {
                        "digest": "sha256:layer2",
                        "size": 6000,
                        "mediaType": "application/vnd.oci.image.layer.v1.tar+gzip"
                    }
                ]
            }
            """

        let data = json.data(using: .utf8)!
        let manifest = try JSONDecoder().decode(MockImageManifest.self, from: data)

        #expect(manifest.config.digest == "sha256:abc123")
        #expect(manifest.layers.count == 2)
        #expect(manifest.layers[0].digest == "sha256:layer1")
        #expect(manifest.layers[1].size == 6000)
    }

    @Test("ImageConfig decoding handles diff_ids correctly")
    func configDecoding() throws {
        let json = """
            {
                "rootfs": {
                    "diff_ids": [
                        "sha256:diff1",
                        "sha256:diff2",
                        "sha256:diff3"
                    ]
                }
            }
            """

        let data = json.data(using: .utf8)!
        let config = try JSONDecoder().decode(MockImageConfig.self, from: data)

        #expect(config.rootfs.diff_ids.count == 3)
        #expect(config.rootfs.diff_ids[0] == "sha256:diff1")
        #expect(config.rootfs.diff_ids[2] == "sha256:diff3")
    }

    @Test("Layer key generation strips sha256 prefix")
    func layerKeyGeneration() {
        let imageName = "myapp"
        let diffID = "sha256:abc123def456"
        let expectedKey = "myapp-abc123def456"

        let layerKey = "\(imageName)-\(diffID.replacingOccurrences(of: "sha256:", with: ""))"

        #expect(layerKey == expectedKey)
    }

    @Test("Layer key generation handles diffID without prefix")
    func layerKeyWithoutPrefix() {
        let imageName = "myapp"
        let diffID = "abc123def456"
        let expectedKey = "myapp-abc123def456"

        let layerKey = "\(imageName)-\(diffID.replacingOccurrences(of: "sha256:", with: ""))"

        #expect(layerKey == expectedKey)
    }
}

// MARK: - Documentation Suite

@Suite("ContainerdService.unpackImage Documentation")
struct UnpackImageDocumentationTests {

    @Test(
        "unpackImage is required because buildx --push only populates content store",
        .disabled("Documentation test")
    )
    func whyUnpackIsNeeded() {
        // When using `docker buildx build --push`, images are pushed directly to the
        // registry. On the device side, when pulling from the registry, containerd
        // stores the image blobs in the content store but does NOT automatically
        // unpack them into snapshots.
        //
        // Snapshots are required for actually running containers. They represent the
        // unpacked filesystem layers that can be mounted as overlayfs.
        //
        // Therefore, after pulling an image from the registry, we must explicitly
        // call unpackImage to:
        // 1. Read the manifest and config from the content store
        // 2. For each layer:
        //    a. Check if a snapshot already exists (skip if yes)
        //    b. Prepare a snapshot (creates temporary mount)
        //    c. Apply the diff (extracts layer tarball to filesystem)
        //    d. Commit the snapshot (makes it permanent)
    }

    @Test("unpackImage implements incremental unpacking", .disabled("Documentation test"))
    func incrementalUnpacking() {
        // The function checks if each layer's snapshot already exists before unpacking.
        // This is important for:
        // 1. Avoiding redundant work on repeated calls
        // 2. Resuming interrupted unpacking operations
        // 3. Handling images that share base layers
        //
        // Each layer is checked with snapshots.stat(). If it exists, the layer is
        // skipped. Only missing snapshots are prepared, applied, and committed.
    }

    @Test("unpackImage handles diff_id vs digest distinction", .disabled("Documentation test"))
    func diffIdVsDigest() {
        // Image manifests reference layers by their compressed digest (what's in the blob store).
        // Image configs reference layers by their diff_id (hash of uncompressed content).
        //
        // For creating snapshots, we need the diff_id because that's what identifies
        // the unpacked layer content. The function:
        // 1. Reads diff_ids from the config
        // 2. Uses diff_id for snapshot key naming
        // 3. Falls back to digest if diff_ids array is shorter than layers array
    }
}
