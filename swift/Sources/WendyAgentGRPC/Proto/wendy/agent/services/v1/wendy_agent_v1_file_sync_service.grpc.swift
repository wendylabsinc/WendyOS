// DO NOT EDIT.
// swift-format-ignore-file
// swiftlint:disable all
//
// Hand-written Swift gRPC bindings.
// Source: wendy/agent/services/v1/wendy_agent_v1_file_sync_service.proto

import GRPCCore
import GRPCProtobuf

// MARK: - wendy.agent.services.v1.WendyFileSyncService

@available(macOS 15.0, iOS 18.0, watchOS 11.0, tvOS 18.0, visionOS 2.0, *)
public enum Wendy_Agent_Services_V1_WendyFileSyncService: Sendable {
  public static let descriptor = GRPCCore.ServiceDescriptor(
    fullyQualifiedService: "wendy.agent.services.v1.WendyFileSyncService"
  )
  public enum Method: Sendable {
    public enum SyncFiles: Sendable {
      public typealias Input = Wendy_Agent_Services_V1_FileSyncRequest
      public typealias Output = Wendy_Agent_Services_V1_FileSyncResponse
      public static let descriptor = GRPCCore.MethodDescriptor(
        service: GRPCCore.ServiceDescriptor(
          fullyQualifiedService: "wendy.agent.services.v1.WendyFileSyncService"
        ),
        method: "SyncFiles"
      )
    }
    public static let descriptors: [GRPCCore.MethodDescriptor] = [SyncFiles.descriptor]
  }
}

@available(macOS 15.0, iOS 18.0, watchOS 11.0, tvOS 18.0, visionOS 2.0, *)
extension GRPCCore.ServiceDescriptor {
  public static let wendy_agent_services_v1_WendyFileSyncService = GRPCCore.ServiceDescriptor(
    fullyQualifiedService: "wendy.agent.services.v1.WendyFileSyncService"
  )
}

// MARK: wendy.agent.services.v1.WendyFileSyncService (server)

@available(macOS 15.0, iOS 18.0, watchOS 11.0, tvOS 18.0, visionOS 2.0, *)
extension Wendy_Agent_Services_V1_WendyFileSyncService {
  /// Streaming service protocol — lowest level, most flexible.
  public protocol StreamingServiceProtocol: GRPCCore.RegistrableRPCService {
    func syncFiles(
      request: GRPCCore.StreamingServerRequest<Wendy_Agent_Services_V1_FileSyncRequest>,
      context: GRPCCore.ServerContext
    ) async throws -> GRPCCore.StreamingServerResponse<Wendy_Agent_Services_V1_FileSyncResponse>
  }

  /// Service protocol — convenience level over StreamingServiceProtocol.
  /// SyncFiles is bidi-streaming, so there is no simpler non-streaming variant.
  public protocol ServiceProtocol: Wendy_Agent_Services_V1_WendyFileSyncService.StreamingServiceProtocol {}
}

// Default implementation of 'registerMethods(with:)'.
@available(macOS 15.0, iOS 18.0, watchOS 11.0, tvOS 18.0, visionOS 2.0, *)
extension Wendy_Agent_Services_V1_WendyFileSyncService.StreamingServiceProtocol {
  public func registerMethods<Transport>(with router: inout GRPCCore.RPCRouter<Transport>)
  where Transport: GRPCCore.ServerTransport {
    router.registerHandler(
      forMethod: Wendy_Agent_Services_V1_WendyFileSyncService.Method.SyncFiles.descriptor,
      deserializer: GRPCProtobuf.ProtobufDeserializer<Wendy_Agent_Services_V1_FileSyncRequest>(),
      serializer: GRPCProtobuf.ProtobufSerializer<Wendy_Agent_Services_V1_FileSyncResponse>(),
      handler: { request, context in
        try await self.syncFiles(request: request, context: context)
      }
    )
  }
}

// MARK: wendy.agent.services.v1.WendyFileSyncService (client)

@available(macOS 15.0, iOS 18.0, watchOS 11.0, tvOS 18.0, visionOS 2.0, *)
extension Wendy_Agent_Services_V1_WendyFileSyncService {
  public protocol ClientProtocol: Sendable {
    func syncFiles<Result>(
      request: GRPCCore.StreamingClientRequest<Wendy_Agent_Services_V1_FileSyncRequest>,
      serializer: some GRPCCore.MessageSerializer<Wendy_Agent_Services_V1_FileSyncRequest>,
      deserializer: some GRPCCore.MessageDeserializer<Wendy_Agent_Services_V1_FileSyncResponse>,
      options: GRPCCore.CallOptions,
      onResponse handleResponse: @Sendable @escaping (
        GRPCCore.StreamingClientResponse<Wendy_Agent_Services_V1_FileSyncResponse>
      ) async throws -> Result
    ) async throws -> Result where Result: Sendable
  }

  public struct Client<Transport>: ClientProtocol where Transport: GRPCCore.ClientTransport {
    private let client: GRPCCore.GRPCClient<Transport>

    public init(wrapping client: GRPCCore.GRPCClient<Transport>) {
      self.client = client
    }

    public func syncFiles<Result>(
      request: GRPCCore.StreamingClientRequest<Wendy_Agent_Services_V1_FileSyncRequest>,
      serializer: some GRPCCore.MessageSerializer<Wendy_Agent_Services_V1_FileSyncRequest>,
      deserializer: some GRPCCore.MessageDeserializer<Wendy_Agent_Services_V1_FileSyncResponse>,
      options: GRPCCore.CallOptions = .defaults,
      onResponse handleResponse: @Sendable @escaping (
        GRPCCore.StreamingClientResponse<Wendy_Agent_Services_V1_FileSyncResponse>
      ) async throws -> Result
    ) async throws -> Result where Result: Sendable {
      try await self.client.bidirectionalStreaming(
        request: request,
        descriptor: Wendy_Agent_Services_V1_WendyFileSyncService.Method.SyncFiles.descriptor,
        serializer: serializer,
        deserializer: deserializer,
        options: options,
        onResponse: handleResponse
      )
    }
  }
}

// Helpers providing default serializer/deserializer arguments.
@available(macOS 15.0, iOS 18.0, watchOS 11.0, tvOS 18.0, visionOS 2.0, *)
extension Wendy_Agent_Services_V1_WendyFileSyncService.ClientProtocol {
  public func syncFiles<Result>(
    request: GRPCCore.StreamingClientRequest<Wendy_Agent_Services_V1_FileSyncRequest>,
    options: GRPCCore.CallOptions = .defaults,
    onResponse handleResponse: @Sendable @escaping (
      GRPCCore.StreamingClientResponse<Wendy_Agent_Services_V1_FileSyncResponse>
    ) async throws -> Result
  ) async throws -> Result where Result: Sendable {
    try await self.syncFiles(
      request: request,
      serializer: GRPCProtobuf.ProtobufSerializer<Wendy_Agent_Services_V1_FileSyncRequest>(),
      deserializer: GRPCProtobuf.ProtobufDeserializer<Wendy_Agent_Services_V1_FileSyncResponse>(),
      options: options,
      onResponse: handleResponse
    )
  }
}
