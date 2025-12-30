import ContainerdGRPC
import Foundation
import GRPCCore
import Logging

// MARK: - Containerd Service Protocols

/// Protocol for containerd Images service operations
public protocol ContainerdImagesService: Sendable {
    func get(
        _ request: Containerd_Services_Images_V1_GetImageRequest
    ) async throws -> Containerd_Services_Images_V1_GetImageResponse
}

/// Protocol for containerd Content service operations
public protocol ContainerdContentService: Sendable {
    /// Read content with streaming handler
    /// The handler receives an async sequence of response chunks
    func read<R: Sendable>(
        _ request: Containerd_Services_Content_V1_ReadContentRequest,
        handler: @Sendable @escaping (ContentReadStream) async throws -> R
    ) async throws -> R
}

/// Async sequence of content read responses
public struct ContentReadStream: AsyncSequence, Sendable {
    public typealias Element = Containerd_Services_Content_V1_ReadContentResponse

    private let base: AsyncStream<Element>

    public init(_ base: AsyncStream<Element>) {
        self.base = base
    }

    public struct AsyncIterator: AsyncIteratorProtocol {
        private var iterator: AsyncStream<Element>.AsyncIterator

        init(_ iterator: AsyncStream<Element>.AsyncIterator) {
            self.iterator = iterator
        }

        public mutating func next() async throws -> Element? {
            return await iterator.next()
        }
    }

    public func makeAsyncIterator() -> AsyncIterator {
        AsyncIterator(base.makeAsyncIterator())
    }
}

/// Protocol for containerd Snapshots service operations
public protocol ContainerdSnapshotsService: Sendable {
    func stat(
        _ request: Containerd_Services_Snapshots_V1_StatSnapshotRequest
    ) async throws -> Containerd_Services_Snapshots_V1_StatSnapshotResponse

    func prepare(
        _ request: Containerd_Services_Snapshots_V1_PrepareSnapshotRequest
    ) async throws -> Containerd_Services_Snapshots_V1_PrepareSnapshotResponse

    func commit(
        _ request: Containerd_Services_Snapshots_V1_CommitSnapshotRequest
    ) async throws

    func remove(
        _ request: Containerd_Services_Snapshots_V1_RemoveSnapshotRequest
    ) async throws
}

/// Protocol for containerd Diffs service operations
public protocol ContainerdDiffsService: Sendable {
    func apply(
        _ request: Containerd_Services_Diff_V1_ApplyRequest
    ) async throws -> Containerd_Services_Diff_V1_ApplyResponse
}

// MARK: - Production Implementations

/// Production implementation wrapping the real gRPC Images client
public struct GRPCImagesService<Transport: ClientTransport>: ContainerdImagesService {
    private let imagesClient: Containerd_Services_Images_V1_Images.Client<Transport>

    public init(client: GRPCClient<Transport>) {
        self.imagesClient = Containerd_Services_Images_V1_Images.Client(wrapping: client)
    }

    public func get(
        _ request: Containerd_Services_Images_V1_GetImageRequest
    ) async throws -> Containerd_Services_Images_V1_GetImageResponse {
        return try await imagesClient.get(request)
    }
}

/// Production implementation wrapping the real gRPC Content client
public struct GRPCContentService<Transport: ClientTransport>: ContainerdContentService {
    private let contentClient: Containerd_Services_Content_V1_Content.Client<Transport>
    private let logger = Logger(label: "com.wendylabs.containerd.content")

    public init(client: GRPCClient<Transport>) {
        self.contentClient = Containerd_Services_Content_V1_Content.Client(wrapping: client)
    }

    public func read<R: Sendable>(
        _ request: Containerd_Services_Content_V1_ReadContentRequest,
        handler: @Sendable @escaping (ContentReadStream) async throws -> R
    ) async throws -> R {
        return try await contentClient.read(request) { serverResponse in
            // Convert gRPC ServerResponse stream to our ContentReadStream using structured concurrency
            return try await withThrowingTaskGroup(of: Void.self) { group in
                let stream = AsyncStream<Containerd_Services_Content_V1_ReadContentResponse> {
                    continuation in
                    group.addTask {
                        do {
                            for try await message in serverResponse.messages {
                                continuation.yield(message)
                            }
                            continuation.finish()
                        } catch {
                            // Log the error for debugging production issues
                            self.logger.error(
                                "Content stream error",
                                metadata: [
                                    "digest": .stringConvertible(request.digest),
                                    "error": .stringConvertible(String(describing: error)),
                                ]
                            )
                            continuation.finish()
                            throw error
                        }
                    }
                }

                // Execute handler with the stream
                let result = try await handler(ContentReadStream(stream))

                // Wait for stream processing to complete
                try await group.waitForAll()

                return result
            }
        }
    }
}

/// Production implementation wrapping the real gRPC Snapshots client
public struct GRPCSnapshotsService<Transport: ClientTransport>: ContainerdSnapshotsService {
    private let snapshotsClient: Containerd_Services_Snapshots_V1_Snapshots.Client<Transport>

    public init(client: GRPCClient<Transport>) {
        self.snapshotsClient = Containerd_Services_Snapshots_V1_Snapshots.Client(
            wrapping: client
        )
    }

    public func stat(
        _ request: Containerd_Services_Snapshots_V1_StatSnapshotRequest
    ) async throws -> Containerd_Services_Snapshots_V1_StatSnapshotResponse {
        return try await snapshotsClient.stat(request)
    }

    public func prepare(
        _ request: Containerd_Services_Snapshots_V1_PrepareSnapshotRequest
    ) async throws -> Containerd_Services_Snapshots_V1_PrepareSnapshotResponse {
        return try await snapshotsClient.prepare(request)
    }

    public func commit(
        _ request: Containerd_Services_Snapshots_V1_CommitSnapshotRequest
    ) async throws {
        _ = try await snapshotsClient.commit(request)
    }

    public func remove(
        _ request: Containerd_Services_Snapshots_V1_RemoveSnapshotRequest
    ) async throws {
        _ = try await snapshotsClient.remove(request)
    }
}

/// Production implementation wrapping the real gRPC Diffs client
public struct GRPCDiffsService<Transport: ClientTransport>: ContainerdDiffsService {
    private let diffsClient: Containerd_Services_Diff_V1_Diff.Client<Transport>

    public init(client: GRPCClient<Transport>) {
        self.diffsClient = Containerd_Services_Diff_V1_Diff.Client(wrapping: client)
    }

    public func apply(
        _ request: Containerd_Services_Diff_V1_ApplyRequest
    ) async throws -> Containerd_Services_Diff_V1_ApplyResponse {
        return try await diffsClient.apply(request)
    }
}
