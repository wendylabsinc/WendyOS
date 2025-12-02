import ContainerdGRPC
import Foundation
import GRPCCore

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
    private let client: GRPCClient<Transport>

    public init(client: GRPCClient<Transport>) {
        self.client = client
    }

    public func get(
        _ request: Containerd_Services_Images_V1_GetImageRequest
    ) async throws -> Containerd_Services_Images_V1_GetImageResponse {
        let images = Containerd_Services_Images_V1_Images.Client(wrapping: client)
        return try await images.get(request)
    }
}

/// Production implementation wrapping the real gRPC Content client
public struct GRPCContentService<Transport: ClientTransport>: ContainerdContentService {
    private let client: GRPCClient<Transport>

    public init(client: GRPCClient<Transport>) {
        self.client = client
    }

    public func read<R: Sendable>(
        _ request: Containerd_Services_Content_V1_ReadContentRequest,
        handler: @Sendable @escaping (ContentReadStream) async throws -> R
    ) async throws -> R {
        let content = Containerd_Services_Content_V1_Content.Client(wrapping: client)
        return try await content.read(request) { serverResponse in
            // Convert gRPC ServerResponse stream to our ContentReadStream
            let stream = AsyncStream<Containerd_Services_Content_V1_ReadContentResponse> {
                continuation in
                Task {
                    do {
                        for try await message in serverResponse.messages {
                            continuation.yield(message)
                        }
                        continuation.finish()
                    } catch {
                        continuation.finish()
                    }
                }
            }
            return try await handler(ContentReadStream(stream))
        }
    }
}

/// Production implementation wrapping the real gRPC Snapshots client
public struct GRPCSnapshotsService<Transport: ClientTransport>: ContainerdSnapshotsService {
    private let client: GRPCClient<Transport>

    public init(client: GRPCClient<Transport>) {
        self.client = client
    }

    public func stat(
        _ request: Containerd_Services_Snapshots_V1_StatSnapshotRequest
    ) async throws -> Containerd_Services_Snapshots_V1_StatSnapshotResponse {
        let snapshots = Containerd_Services_Snapshots_V1_Snapshots.Client(wrapping: client)
        return try await snapshots.stat(request)
    }

    public func prepare(
        _ request: Containerd_Services_Snapshots_V1_PrepareSnapshotRequest
    ) async throws -> Containerd_Services_Snapshots_V1_PrepareSnapshotResponse {
        let snapshots = Containerd_Services_Snapshots_V1_Snapshots.Client(wrapping: client)
        return try await snapshots.prepare(request)
    }

    public func commit(
        _ request: Containerd_Services_Snapshots_V1_CommitSnapshotRequest
    ) async throws {
        let snapshots = Containerd_Services_Snapshots_V1_Snapshots.Client(wrapping: client)
        _ = try await snapshots.commit(request)
    }
}

/// Production implementation wrapping the real gRPC Diffs client
public struct GRPCDiffsService<Transport: ClientTransport>: ContainerdDiffsService {
    private let client: GRPCClient<Transport>

    public init(client: GRPCClient<Transport>) {
        self.client = client
    }

    public func apply(
        _ request: Containerd_Services_Diff_V1_ApplyRequest
    ) async throws -> Containerd_Services_Diff_V1_ApplyResponse {
        let diffs = Containerd_Services_Diff_V1_Diff.Client(wrapping: client)
        return try await diffs.apply(request)
    }
}
