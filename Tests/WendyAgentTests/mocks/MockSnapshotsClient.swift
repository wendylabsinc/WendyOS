import ContainerdGRPC
import Foundation
import GRPCCore
import SwiftProtobuf

@testable import wendy_agent

/// Mock implementation of Containerd Snapshots GRPC client for testing
actor MockSnapshotsClient: Containerd_Services_Snapshots_V1_Snapshots.ClientProtocol {
    // MARK: - Configuration

    private var removeError: Error?
    private var statResponse: Containerd_Services_Snapshots_V1_StatSnapshotResponse?
    private var mountsResponse: Containerd_Services_Snapshots_V1_MountsResponse?
    private var prepareResponse: Containerd_Services_Snapshots_V1_PrepareSnapshotResponse?

    // MARK: - Configuration Methods

    func setRemoveError(_ error: Error) {
        self.removeError = error
    }

    // MARK: - Convenience Methods (Used in Tests)

    func remove<Result>(
        request: GRPCCore.ClientRequest<Containerd_Services_Snapshots_V1_RemoveSnapshotRequest>,
        options: GRPCCore.CallOptions = .defaults,
        onResponse handleResponse:
            @Sendable @escaping (
                GRPCCore.ClientResponse<SwiftProtobuf.Google_Protobuf_Empty>
            ) async throws -> Result = { response in try response.message }
    ) async throws -> Result where Result: Sendable {
        if let error = removeError {
            throw error
        }
        let clientResponse = GRPCCore.ClientResponse<SwiftProtobuf.Google_Protobuf_Empty>(
            message: .init(),
            metadata: [:]
        )
        return try await handleResponse(clientResponse)
    }

    func remove(
        _ message: Containerd_Services_Snapshots_V1_RemoveSnapshotRequest,
        metadata: GRPCCore.Metadata = [:],
        options: GRPCCore.CallOptions = .defaults
    ) async throws -> SwiftProtobuf.Google_Protobuf_Empty {
        let request = GRPCCore.ClientRequest(message: message, metadata: metadata)
        return try await remove(request: request, options: options)
    }

    // MARK: - Low-Level Protocol Requirements (Not Used in Tests)

    func prepare<Result>(
        request: GRPCCore.ClientRequest<
            Containerd_Services_Snapshots_V1_PrepareSnapshotRequest
        >,
        serializer: some GRPCCore.MessageSerializer<
            Containerd_Services_Snapshots_V1_PrepareSnapshotRequest
        >,
        deserializer: some GRPCCore.MessageDeserializer<
            Containerd_Services_Snapshots_V1_PrepareSnapshotResponse
        >,
        options: GRPCCore.CallOptions,
        onResponse handleResponse:
            @Sendable @escaping (
                GRPCCore.ClientResponse<
                    Containerd_Services_Snapshots_V1_PrepareSnapshotResponse
                >
            ) async throws -> Result
    ) async throws -> Result where Result: Sendable {
        fatalError("Not implemented in mock")
    }

    func view<Result>(
        request: GRPCCore.ClientRequest<
            Containerd_Services_Snapshots_V1_ViewSnapshotRequest
        >,
        serializer: some GRPCCore.MessageSerializer<
            Containerd_Services_Snapshots_V1_ViewSnapshotRequest
        >,
        deserializer: some GRPCCore.MessageDeserializer<
            Containerd_Services_Snapshots_V1_ViewSnapshotResponse
        >,
        options: GRPCCore.CallOptions,
        onResponse handleResponse:
            @Sendable @escaping (
                GRPCCore.ClientResponse<
                    Containerd_Services_Snapshots_V1_ViewSnapshotResponse
                >
            ) async throws -> Result
    ) async throws -> Result where Result: Sendable {
        fatalError("Not implemented in mock")
    }

    func mounts<Result>(
        request: GRPCCore.ClientRequest<
            Containerd_Services_Snapshots_V1_MountsRequest
        >,
        serializer: some GRPCCore.MessageSerializer<
            Containerd_Services_Snapshots_V1_MountsRequest
        >,
        deserializer: some GRPCCore.MessageDeserializer<
            Containerd_Services_Snapshots_V1_MountsResponse
        >,
        options: GRPCCore.CallOptions,
        onResponse handleResponse:
            @Sendable @escaping (
                GRPCCore.ClientResponse<Containerd_Services_Snapshots_V1_MountsResponse>
            ) async throws -> Result
    ) async throws -> Result where Result: Sendable {
        fatalError("Not implemented in mock")
    }

    func commit<Result>(
        request: GRPCCore.ClientRequest<
            Containerd_Services_Snapshots_V1_CommitSnapshotRequest
        >,
        serializer: some GRPCCore.MessageSerializer<
            Containerd_Services_Snapshots_V1_CommitSnapshotRequest
        >,
        deserializer: some GRPCCore.MessageDeserializer<
            SwiftProtobuf.Google_Protobuf_Empty
        >,
        options: GRPCCore.CallOptions,
        onResponse handleResponse:
            @Sendable @escaping (
                GRPCCore.ClientResponse<
                    SwiftProtobuf.Google_Protobuf_Empty
                >
            ) async throws -> Result
    ) async throws -> Result where Result: Sendable {
        fatalError("Not implemented in mock")
    }

    func remove<Result>(
        request: GRPCCore.ClientRequest<
            Containerd_Services_Snapshots_V1_RemoveSnapshotRequest
        >,
        serializer: some GRPCCore.MessageSerializer<
            Containerd_Services_Snapshots_V1_RemoveSnapshotRequest
        >,
        deserializer: some GRPCCore.MessageDeserializer<
            SwiftProtobuf.Google_Protobuf_Empty
        >,
        options: GRPCCore.CallOptions,
        onResponse handleResponse:
            @Sendable @escaping (
                GRPCCore.ClientResponse<
                    SwiftProtobuf.Google_Protobuf_Empty
                >
            ) async throws -> Result
    ) async throws -> Result where Result: Sendable {
        if let error = removeError {
            throw error
        }
        let clientResponse = GRPCCore.ClientResponse<SwiftProtobuf.Google_Protobuf_Empty>(
            message: .init(),
            metadata: [:]
        )
        return try await handleResponse(clientResponse)
    }

    func stat<Result>(
        request: GRPCCore.ClientRequest<
            Containerd_Services_Snapshots_V1_StatSnapshotRequest
        >,
        serializer: some GRPCCore.MessageSerializer<
            Containerd_Services_Snapshots_V1_StatSnapshotRequest
        >,
        deserializer: some GRPCCore.MessageDeserializer<
            Containerd_Services_Snapshots_V1_StatSnapshotResponse
        >,
        options: GRPCCore.CallOptions,
        onResponse handleResponse:
            @Sendable @escaping (
                GRPCCore.ClientResponse<
                    Containerd_Services_Snapshots_V1_StatSnapshotResponse
                >
            ) async throws -> Result
    ) async throws -> Result where Result: Sendable {
        fatalError("Not implemented in mock")
    }

    func update<Result>(
        request: GRPCCore.ClientRequest<
            Containerd_Services_Snapshots_V1_UpdateSnapshotRequest
        >,
        serializer: some GRPCCore.MessageSerializer<
            Containerd_Services_Snapshots_V1_UpdateSnapshotRequest
        >,
        deserializer: some GRPCCore.MessageDeserializer<
            Containerd_Services_Snapshots_V1_UpdateSnapshotResponse
        >,
        options: GRPCCore.CallOptions,
        onResponse handleResponse:
            @Sendable @escaping (
                GRPCCore.ClientResponse<
                    Containerd_Services_Snapshots_V1_UpdateSnapshotResponse
                >
            ) async throws -> Result
    ) async throws -> Result where Result: Sendable {
        fatalError("Not implemented in mock")
    }

    func list<Result>(
        request: GRPCCore.ClientRequest<
            Containerd_Services_Snapshots_V1_ListSnapshotsRequest
        >,
        serializer: some GRPCCore.MessageSerializer<
            Containerd_Services_Snapshots_V1_ListSnapshotsRequest
        >,
        deserializer: some GRPCCore.MessageDeserializer<
            Containerd_Services_Snapshots_V1_ListSnapshotsResponse
        >,
        options: GRPCCore.CallOptions,
        onResponse handleResponse:
            @Sendable @escaping (
                GRPCCore.StreamingClientResponse<
                    Containerd_Services_Snapshots_V1_ListSnapshotsResponse
                >
            ) async throws -> Result
    ) async throws -> Result where Result: Sendable {
        fatalError("Not implemented in mock")
    }

    func usage<Result>(
        request: GRPCCore.ClientRequest<
            Containerd_Services_Snapshots_V1_UsageRequest
        >,
        serializer: some GRPCCore.MessageSerializer<
            Containerd_Services_Snapshots_V1_UsageRequest
        >,
        deserializer: some GRPCCore.MessageDeserializer<
            Containerd_Services_Snapshots_V1_UsageResponse
        >,
        options: GRPCCore.CallOptions,
        onResponse handleResponse:
            @Sendable @escaping (
                GRPCCore.ClientResponse<Containerd_Services_Snapshots_V1_UsageResponse>
            ) async throws -> Result
    ) async throws -> Result where Result: Sendable {
        fatalError("Not implemented in mock")
    }

    func cleanup<Result>(
        request: GRPCCore.ClientRequest<
            Containerd_Services_Snapshots_V1_CleanupRequest
        >,
        serializer: some GRPCCore.MessageSerializer<
            Containerd_Services_Snapshots_V1_CleanupRequest
        >,
        deserializer: some GRPCCore.MessageDeserializer<
            SwiftProtobuf.Google_Protobuf_Empty
        >,
        options: GRPCCore.CallOptions,
        onResponse handleResponse:
            @Sendable @escaping (
                GRPCCore.ClientResponse<SwiftProtobuf.Google_Protobuf_Empty>
            ) async throws -> Result
    ) async throws -> Result where Result: Sendable {
        fatalError("Not implemented in mock")
    }
}
