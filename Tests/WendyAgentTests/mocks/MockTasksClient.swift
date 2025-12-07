import ContainerdGRPC
import Foundation
import GRPCCore
import SwiftProtobuf

@testable import wendy_agent

/// Mock implementation of Containerd Tasks GRPC client for testing
actor MockTasksClient: Containerd_Services_Tasks_V1_Tasks.ClientProtocol {
    // MARK: - Configuration

    private var deleteError: Error?
    private var listResponse: Containerd_Services_Tasks_V1_ListTasksResponse = .init()
    private var killError: Error?

    // MARK: - Configuration Methods

    func setDeleteError(_ error: Error) {
        self.deleteError = error
    }

    // MARK: - Convenience Methods (Used in Tests)

    func delete(
        _ message: Containerd_Services_Tasks_V1_DeleteTaskRequest,
        metadata: GRPCCore.Metadata = [:],
        options: GRPCCore.CallOptions = .defaults
    ) async throws -> Containerd_Services_Tasks_V1_DeleteResponse {
        if let error = deleteError {
            throw error
        }
        return .init()
    }

    func list(
        _ message: Containerd_Services_Tasks_V1_ListTasksRequest,
        metadata: GRPCCore.Metadata = [:],
        options: GRPCCore.CallOptions = .defaults
    ) async throws -> Containerd_Services_Tasks_V1_ListTasksResponse {
        return listResponse
    }

    func kill(
        _ message: Containerd_Services_Tasks_V1_KillRequest,
        metadata: GRPCCore.Metadata = [:],
        options: GRPCCore.CallOptions = .defaults
    ) async throws -> SwiftProtobuf.Google_Protobuf_Empty {
        if let error = killError {
            throw error
        }
        return .init()
    }

    // MARK: - Low-Level Protocol Requirements (Not Used in Tests)

    func create<Result>(
        request: GRPCCore.ClientRequest<Containerd_Services_Tasks_V1_CreateTaskRequest>,
        serializer: some GRPCCore.MessageSerializer<
            Containerd_Services_Tasks_V1_CreateTaskRequest
        >,
        deserializer: some GRPCCore.MessageDeserializer<
            Containerd_Services_Tasks_V1_CreateTaskResponse
        >,
        options: GRPCCore.CallOptions,
        onResponse handleResponse:
            @Sendable @escaping (
                GRPCCore.ClientResponse<Containerd_Services_Tasks_V1_CreateTaskResponse>
            ) async throws -> Result
    ) async throws -> Result where Result: Sendable {
        fatalError("Not implemented in mock")
    }

    func start<Result>(
        request: GRPCCore.ClientRequest<Containerd_Services_Tasks_V1_StartRequest>,
        serializer: some GRPCCore.MessageSerializer<
            Containerd_Services_Tasks_V1_StartRequest
        >,
        deserializer: some GRPCCore.MessageDeserializer<
            Containerd_Services_Tasks_V1_StartResponse
        >,
        options: GRPCCore.CallOptions,
        onResponse handleResponse:
            @Sendable @escaping (
                GRPCCore.ClientResponse<Containerd_Services_Tasks_V1_StartResponse>
            ) async throws -> Result
    ) async throws -> Result where Result: Sendable {
        fatalError("Not implemented in mock")
    }

    func delete<Result>(
        request: GRPCCore.ClientRequest<Containerd_Services_Tasks_V1_DeleteTaskRequest>,
        serializer: some GRPCCore.MessageSerializer<
            Containerd_Services_Tasks_V1_DeleteTaskRequest
        >,
        deserializer: some GRPCCore.MessageDeserializer<
            Containerd_Services_Tasks_V1_DeleteResponse
        >,
        options: GRPCCore.CallOptions,
        onResponse handleResponse:
            @Sendable @escaping (
                GRPCCore.ClientResponse<Containerd_Services_Tasks_V1_DeleteResponse>
            ) async throws -> Result
    ) async throws -> Result where Result: Sendable {
        fatalError("Use convenience method delete(_:metadata:options:) instead")
    }

    func deleteProcess<Result>(
        request: GRPCCore.ClientRequest<
            Containerd_Services_Tasks_V1_DeleteProcessRequest
        >,
        serializer: some GRPCCore.MessageSerializer<
            Containerd_Services_Tasks_V1_DeleteProcessRequest
        >,
        deserializer: some GRPCCore.MessageDeserializer<
            Containerd_Services_Tasks_V1_DeleteResponse
        >,
        options: GRPCCore.CallOptions,
        onResponse handleResponse:
            @Sendable @escaping (
                GRPCCore.ClientResponse<Containerd_Services_Tasks_V1_DeleteResponse>
            ) async throws -> Result
    ) async throws -> Result where Result: Sendable {
        fatalError("Not implemented in mock")
    }

    func get<Result>(
        request: GRPCCore.ClientRequest<Containerd_Services_Tasks_V1_GetRequest>,
        serializer: some GRPCCore.MessageSerializer<Containerd_Services_Tasks_V1_GetRequest>,
        deserializer: some GRPCCore.MessageDeserializer<
            Containerd_Services_Tasks_V1_GetResponse
        >,
        options: GRPCCore.CallOptions,
        onResponse handleResponse:
            @Sendable @escaping (
                GRPCCore.ClientResponse<Containerd_Services_Tasks_V1_GetResponse>
            ) async throws -> Result
    ) async throws -> Result where Result: Sendable {
        fatalError("Not implemented in mock")
    }

    func list<Result>(
        request: GRPCCore.ClientRequest<Containerd_Services_Tasks_V1_ListTasksRequest>,
        serializer: some GRPCCore.MessageSerializer<
            Containerd_Services_Tasks_V1_ListTasksRequest
        >,
        deserializer: some GRPCCore.MessageDeserializer<
            Containerd_Services_Tasks_V1_ListTasksResponse
        >,
        options: GRPCCore.CallOptions,
        onResponse handleResponse:
            @Sendable @escaping (
                GRPCCore.ClientResponse<Containerd_Services_Tasks_V1_ListTasksResponse>
            ) async throws -> Result
    ) async throws -> Result where Result: Sendable {
        let clientResponse = GRPCCore.ClientResponse<
            Containerd_Services_Tasks_V1_ListTasksResponse
        >(
            message: listResponse,
            metadata: [:]
        )
        return try await handleResponse(clientResponse)
    }

    func kill<Result>(
        request: GRPCCore.ClientRequest<Containerd_Services_Tasks_V1_KillRequest>,
        serializer: some GRPCCore.MessageSerializer<Containerd_Services_Tasks_V1_KillRequest>,
        deserializer: some GRPCCore.MessageDeserializer<SwiftProtobuf.Google_Protobuf_Empty>,
        options: GRPCCore.CallOptions,
        onResponse handleResponse:
            @Sendable @escaping (
                GRPCCore.ClientResponse<SwiftProtobuf.Google_Protobuf_Empty>
            ) async throws -> Result
    ) async throws -> Result where Result: Sendable {
        fatalError("Use convenience method kill(_:metadata:options:) instead")
    }

    func exec<Result>(
        request: GRPCCore.ClientRequest<Containerd_Services_Tasks_V1_ExecProcessRequest>,
        serializer: some GRPCCore.MessageSerializer<
            Containerd_Services_Tasks_V1_ExecProcessRequest
        >,
        deserializer: some GRPCCore.MessageDeserializer<SwiftProtobuf.Google_Protobuf_Empty>,
        options: GRPCCore.CallOptions,
        onResponse handleResponse:
            @Sendable @escaping (
                GRPCCore.ClientResponse<SwiftProtobuf.Google_Protobuf_Empty>
            ) async throws -> Result
    ) async throws -> Result where Result: Sendable {
        fatalError("Not implemented in mock")
    }

    func resizePty<Result>(
        request: GRPCCore.ClientRequest<Containerd_Services_Tasks_V1_ResizePtyRequest>,
        serializer: some GRPCCore.MessageSerializer<
            Containerd_Services_Tasks_V1_ResizePtyRequest
        >,
        deserializer: some GRPCCore.MessageDeserializer<SwiftProtobuf.Google_Protobuf_Empty>,
        options: GRPCCore.CallOptions,
        onResponse handleResponse:
            @Sendable @escaping (
                GRPCCore.ClientResponse<SwiftProtobuf.Google_Protobuf_Empty>
            ) async throws -> Result
    ) async throws -> Result where Result: Sendable {
        fatalError("Not implemented in mock")
    }

    func closeIO<Result>(
        request: GRPCCore.ClientRequest<Containerd_Services_Tasks_V1_CloseIORequest>,
        serializer: some GRPCCore.MessageSerializer<
            Containerd_Services_Tasks_V1_CloseIORequest
        >,
        deserializer: some GRPCCore.MessageDeserializer<SwiftProtobuf.Google_Protobuf_Empty>,
        options: GRPCCore.CallOptions,
        onResponse handleResponse:
            @Sendable @escaping (
                GRPCCore.ClientResponse<SwiftProtobuf.Google_Protobuf_Empty>
            ) async throws -> Result
    ) async throws -> Result where Result: Sendable {
        fatalError("Not implemented in mock")
    }

    func pause<Result>(
        request: GRPCCore.ClientRequest<Containerd_Services_Tasks_V1_PauseTaskRequest>,
        serializer: some GRPCCore.MessageSerializer<
            Containerd_Services_Tasks_V1_PauseTaskRequest
        >,
        deserializer: some GRPCCore.MessageDeserializer<SwiftProtobuf.Google_Protobuf_Empty>,
        options: GRPCCore.CallOptions,
        onResponse handleResponse:
            @Sendable @escaping (
                GRPCCore.ClientResponse<SwiftProtobuf.Google_Protobuf_Empty>
            ) async throws -> Result
    ) async throws -> Result where Result: Sendable {
        fatalError("Not implemented in mock")
    }

    func resume<Result>(
        request: GRPCCore.ClientRequest<Containerd_Services_Tasks_V1_ResumeTaskRequest>,
        serializer: some GRPCCore.MessageSerializer<
            Containerd_Services_Tasks_V1_ResumeTaskRequest
        >,
        deserializer: some GRPCCore.MessageDeserializer<SwiftProtobuf.Google_Protobuf_Empty>,
        options: GRPCCore.CallOptions,
        onResponse handleResponse:
            @Sendable @escaping (
                GRPCCore.ClientResponse<SwiftProtobuf.Google_Protobuf_Empty>
            ) async throws -> Result
    ) async throws -> Result where Result: Sendable {
        fatalError("Not implemented in mock")
    }

    func listPids<Result>(
        request: GRPCCore.ClientRequest<Containerd_Services_Tasks_V1_ListPidsRequest>,
        serializer: some GRPCCore.MessageSerializer<
            Containerd_Services_Tasks_V1_ListPidsRequest
        >,
        deserializer: some GRPCCore.MessageDeserializer<
            Containerd_Services_Tasks_V1_ListPidsResponse
        >,
        options: GRPCCore.CallOptions,
        onResponse handleResponse:
            @Sendable @escaping (
                GRPCCore.ClientResponse<Containerd_Services_Tasks_V1_ListPidsResponse>
            ) async throws -> Result
    ) async throws -> Result where Result: Sendable {
        fatalError("Not implemented in mock")
    }

    func checkpoint<Result>(
        request: GRPCCore.ClientRequest<Containerd_Services_Tasks_V1_CheckpointTaskRequest>,
        serializer: some GRPCCore.MessageSerializer<
            Containerd_Services_Tasks_V1_CheckpointTaskRequest
        >,
        deserializer: some GRPCCore.MessageDeserializer<
            Containerd_Services_Tasks_V1_CheckpointTaskResponse
        >,
        options: GRPCCore.CallOptions,
        onResponse handleResponse:
            @Sendable @escaping (
                GRPCCore.ClientResponse<Containerd_Services_Tasks_V1_CheckpointTaskResponse>
            ) async throws -> Result
    ) async throws -> Result where Result: Sendable {
        fatalError("Not implemented in mock")
    }

    func update<Result>(
        request: GRPCCore.ClientRequest<Containerd_Services_Tasks_V1_UpdateTaskRequest>,
        serializer: some GRPCCore.MessageSerializer<
            Containerd_Services_Tasks_V1_UpdateTaskRequest
        >,
        deserializer: some GRPCCore.MessageDeserializer<SwiftProtobuf.Google_Protobuf_Empty>,
        options: GRPCCore.CallOptions,
        onResponse handleResponse:
            @Sendable @escaping (
                GRPCCore.ClientResponse<SwiftProtobuf.Google_Protobuf_Empty>
            ) async throws -> Result
    ) async throws -> Result where Result: Sendable {
        fatalError("Not implemented in mock")
    }

    func metrics<Result>(
        request: GRPCCore.ClientRequest<Containerd_Services_Tasks_V1_MetricsRequest>,
        serializer: some GRPCCore.MessageSerializer<
            Containerd_Services_Tasks_V1_MetricsRequest
        >,
        deserializer: some GRPCCore.MessageDeserializer<
            Containerd_Services_Tasks_V1_MetricsResponse
        >,
        options: GRPCCore.CallOptions,
        onResponse handleResponse:
            @Sendable @escaping (
                GRPCCore.ClientResponse<Containerd_Services_Tasks_V1_MetricsResponse>
            ) async throws -> Result
    ) async throws -> Result where Result: Sendable {
        fatalError("Not implemented in mock")
    }

    func wait<Result>(
        request: GRPCCore.ClientRequest<Containerd_Services_Tasks_V1_WaitRequest>,
        serializer: some GRPCCore.MessageSerializer<Containerd_Services_Tasks_V1_WaitRequest>,
        deserializer: some GRPCCore.MessageDeserializer<
            Containerd_Services_Tasks_V1_WaitResponse
        >,
        options: GRPCCore.CallOptions,
        onResponse handleResponse:
            @Sendable @escaping (
                GRPCCore.ClientResponse<Containerd_Services_Tasks_V1_WaitResponse>
            ) async throws -> Result
    ) async throws -> Result where Result: Sendable {
        fatalError("Not implemented in mock")
    }
}
