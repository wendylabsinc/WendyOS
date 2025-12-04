import ContainerdGRPC
import Foundation
import GRPCCore
import SwiftProtobuf

@testable import wendy_agent

/// Mock implementation of Containerd Containers GRPC client for testing
actor MockContainersClient: Containerd_Services_Containers_V1_Containers.ClientProtocol {
    // MARK: - Configuration

    private var getResponse: Containerd_Services_Containers_V1_GetContainerResponse?
    private var getError: Error?
    private var deleteError: Error?
    private var listResponse: Containerd_Services_Containers_V1_ListContainersResponse?

    // MARK: - Configuration Methods

    func setGetResponse(_ response: Containerd_Services_Containers_V1_GetContainerResponse) {
        self.getResponse = response
        self.getError = nil
    }

    func setGetError(_ error: Error) {
        self.getError = error
        self.getResponse = nil
    }

    func setDeleteError(_ error: Error) {
        self.deleteError = error
    }

    // MARK: - Convenience Methods (Used in Tests)

    func get<Result>(
        request: GRPCCore.ClientRequest<Containerd_Services_Containers_V1_GetContainerRequest>,
        options: GRPCCore.CallOptions = .defaults,
        onResponse handleResponse:
            @Sendable @escaping (
                GRPCCore.ClientResponse<Containerd_Services_Containers_V1_GetContainerResponse>
            ) async throws -> Result = { response in try response.message }
    ) async throws -> Result where Result: Sendable {
        if let error = getError {
            throw error
        }
        guard let response = getResponse else {
            fatalError("getResponse not configured in mock")
        }
        let clientResponse = GRPCCore.ClientResponse<
            Containerd_Services_Containers_V1_GetContainerResponse
        >(
            message: response,
            metadata: [:]
        )
        return try await handleResponse(clientResponse)
    }

    func get(
        _ message: Containerd_Services_Containers_V1_GetContainerRequest,
        metadata: GRPCCore.Metadata = [:],
        options: GRPCCore.CallOptions = .defaults
    ) async throws -> Containerd_Services_Containers_V1_GetContainerResponse {
        let request = GRPCCore.ClientRequest(message: message, metadata: metadata)
        return try await get(request: request, options: options)
    }

    func delete<Result>(
        request: GRPCCore.ClientRequest<Containerd_Services_Containers_V1_DeleteContainerRequest>,
        options: GRPCCore.CallOptions = .defaults,
        onResponse handleResponse:
            @Sendable @escaping (
                GRPCCore.ClientResponse<SwiftProtobuf.Google_Protobuf_Empty>
            ) async throws -> Result = { response in try response.message }
    ) async throws -> Result where Result: Sendable {
        if let error = deleteError {
            throw error
        }
        let clientResponse = GRPCCore.ClientResponse<SwiftProtobuf.Google_Protobuf_Empty>(
            message: .init(),
            metadata: [:]
        )
        return try await handleResponse(clientResponse)
    }

    func delete(
        _ message: Containerd_Services_Containers_V1_DeleteContainerRequest,
        metadata: GRPCCore.Metadata = [:],
        options: GRPCCore.CallOptions = .defaults
    ) async throws -> SwiftProtobuf.Google_Protobuf_Empty {
        let request = GRPCCore.ClientRequest(message: message, metadata: metadata)
        return try await delete(request: request, options: options)
    }

    func list(
        request: GRPCCore.ClientRequest<
            Containerd_Services_Containers_V1_ListContainersRequest
        >,
        options: GRPCCore.CallOptions = .defaults
    ) async throws -> Containerd_Services_Containers_V1_ListContainersResponse {
        guard let response = listResponse else {
            fatalError("listResponse not configured in mock")
        }
        return response
    }

    // MARK: - Low-Level Protocol Requirements (Not Used in Tests)

    func get<Result>(
        request: GRPCCore.ClientRequest<
            Containerd_Services_Containers_V1_GetContainerRequest
        >,
        serializer: some GRPCCore.MessageSerializer<
            Containerd_Services_Containers_V1_GetContainerRequest
        >,
        deserializer: some GRPCCore.MessageDeserializer<
            Containerd_Services_Containers_V1_GetContainerResponse
        >,
        options: GRPCCore.CallOptions,
        onResponse handleResponse:
            @Sendable @escaping (
                GRPCCore.ClientResponse<
                    Containerd_Services_Containers_V1_GetContainerResponse
                >
            ) async throws -> Result
    ) async throws -> Result where Result: Sendable {
        if let error = getError {
            throw error
        }
        guard let response = getResponse else {
            fatalError("getResponse not configured in mock")
        }
        let clientResponse = GRPCCore.ClientResponse<
            Containerd_Services_Containers_V1_GetContainerResponse
        >(
            message: response,
            metadata: [:]
        )
        return try await handleResponse(clientResponse)
    }

    func list<Result>(
        request: GRPCCore.ClientRequest<
            Containerd_Services_Containers_V1_ListContainersRequest
        >,
        serializer: some GRPCCore.MessageSerializer<
            Containerd_Services_Containers_V1_ListContainersRequest
        >,
        deserializer: some GRPCCore.MessageDeserializer<
            Containerd_Services_Containers_V1_ListContainersResponse
        >,
        options: GRPCCore.CallOptions,
        onResponse handleResponse:
            @Sendable @escaping (
                GRPCCore.ClientResponse<
                    Containerd_Services_Containers_V1_ListContainersResponse
                >
            ) async throws -> Result
    ) async throws -> Result where Result: Sendable {
        fatalError("Use convenience method list(request:options:) instead")
    }

    func listStream<Result>(
        request: GRPCCore.ClientRequest<
            Containerd_Services_Containers_V1_ListContainersRequest
        >,
        serializer: some GRPCCore.MessageSerializer<
            Containerd_Services_Containers_V1_ListContainersRequest
        >,
        deserializer: some GRPCCore.MessageDeserializer<
            Containerd_Services_Containers_V1_ListContainerMessage
        >,
        options: GRPCCore.CallOptions,
        onResponse handleResponse:
            @Sendable @escaping (
                GRPCCore.StreamingClientResponse<
                    Containerd_Services_Containers_V1_ListContainerMessage
                >
            ) async throws -> Result
    ) async throws -> Result where Result: Sendable {
        fatalError("Not implemented in mock")
    }

    func create<Result>(
        request: GRPCCore.ClientRequest<
            Containerd_Services_Containers_V1_CreateContainerRequest
        >,
        serializer: some GRPCCore.MessageSerializer<
            Containerd_Services_Containers_V1_CreateContainerRequest
        >,
        deserializer: some GRPCCore.MessageDeserializer<
            Containerd_Services_Containers_V1_CreateContainerResponse
        >,
        options: GRPCCore.CallOptions,
        onResponse handleResponse:
            @Sendable @escaping (
                GRPCCore.ClientResponse<
                    Containerd_Services_Containers_V1_CreateContainerResponse
                >
            ) async throws -> Result
    ) async throws -> Result where Result: Sendable {
        fatalError("Not implemented in mock")
    }

    func update<Result>(
        request: GRPCCore.ClientRequest<
            Containerd_Services_Containers_V1_UpdateContainerRequest
        >,
        serializer: some GRPCCore.MessageSerializer<
            Containerd_Services_Containers_V1_UpdateContainerRequest
        >,
        deserializer: some GRPCCore.MessageDeserializer<
            Containerd_Services_Containers_V1_UpdateContainerResponse
        >,
        options: GRPCCore.CallOptions,
        onResponse handleResponse:
            @Sendable @escaping (
                GRPCCore.ClientResponse<
                    Containerd_Services_Containers_V1_UpdateContainerResponse
                >
            ) async throws -> Result
    ) async throws -> Result where Result: Sendable {
        fatalError("Not implemented in mock")
    }

    func delete<Result>(
        request: GRPCCore.ClientRequest<
            Containerd_Services_Containers_V1_DeleteContainerRequest
        >,
        serializer: some GRPCCore.MessageSerializer<
            Containerd_Services_Containers_V1_DeleteContainerRequest
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
        if let error = deleteError {
            throw error
        }
        let clientResponse = GRPCCore.ClientResponse<SwiftProtobuf.Google_Protobuf_Empty>(
            message: .init(),
            metadata: [:]
        )
        return try await handleResponse(clientResponse)
    }
}
