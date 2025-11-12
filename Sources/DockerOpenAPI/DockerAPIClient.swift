//
//  DockerAPIClient.swift
//  DockerOpenAPI
//
//  Wrapper for DockerOpenAPI proto-generated code
//

import Foundation
import AsyncHTTPClient
import OpenAPIAsyncHTTPClient
import OpenAPIRuntime
import NIOCore
import NIOHTTP1
import NIOPosix
import NIOTransportServices
import Logging

// MARK: - Data Models

public struct LogMessage: Sendable {
    public enum StreamType: Sendable {
        case stdout
        case stderr
    }

    public let type: StreamType
    public let data: Data

    public var text: String? {
        String(data: data, encoding: .utf8)
    }
}

// MARK: - Errors

public struct ClientError: Error {
    let description: String
}

public enum DockerAPIError: Error, LocalizedError {
    case httpError(status: HTTPResponseStatus)
    case decodingError(Error)
    case connectionError(Error)
    case invalidResponse

    public var errorDescription: String? {
        switch self {
        case .httpError(let status):
            return "Docker API HTTP error: \(status)"
        case .decodingError(let error):
            return "Failed to decode response: \(error)"
        case .connectionError(let error):
            return "Connection error: \(error)"
        case .invalidResponse:
            return "Invalid response from Docker API"
        }
    }
}

/// Docker API client that wraps proto-generated code
public actor DockerAPIClient {
    private let client: Client?
    private let logger: Logger
    private let httpClient: HTTPClient?
    private let ownsHTTPClient: Bool
    private let eventLoopGroup: EventLoopGroup?
    private let socketPath: String?
    private let ownsEventLoopGroup: Bool

    /// Initialize with Unix socket (default for Docker)
    public init(
        socketPath: String = "/var/run/docker.sock",
        logger: Logger = Logger(label: "DockerAPIClient")
    ) throws {
        self.logger = logger
        self.ownsHTTPClient = false
        self.httpClient = nil
        self.socketPath = socketPath
        self.ownsEventLoopGroup = true
        self.eventLoopGroup = MultiThreadedEventLoopGroup(numberOfThreads: 1)
        self.client = nil
    }

    public init(
        baseURL: String,
        httpClient: HTTPClient,
        logger: Logger = Logger(label: "DockerAPIClient")
    ) throws {
        self.logger = logger
        self.httpClient = httpClient
        self.ownsHTTPClient = false
        self.socketPath = nil
        self.eventLoopGroup = nil
        self.ownsEventLoopGroup = false

        // Create transport using provided HTTP client
        let transport = AsyncHTTPClientTransport(
            configuration: .init(
                client: httpClient
            )
        )

        // Initialize the generated client
        self.client = Client(
            serverURL: URL(string: baseURL)!,
            transport: transport
        )
    }

    /// Clean up resources
    public func shutdown() async throws {
        if ownsHTTPClient, let httpClient = self.httpClient {
            try await httpClient.shutdown()
        }
        if ownsEventLoopGroup, let eventLoopGroup = self.eventLoopGroup {
            try await eventLoopGroup.shutdownGracefully()
        }
    }

    /// Stream logs from a container
    public func streamLogs(
        containerID: String,
        stdout: Bool = true,
        stderr: Bool = true,
        follow: Bool = true,
        tail: String? = nil
    ) async throws -> AsyncThrowingStream<LogMessage, Error> {

        logger.debug("Streaming logs from container", metadata: [
            "containerID": .string(containerID)
        ])

        if let socketPath = self.socketPath, let eventLoopGroup = self.eventLoopGroup {
            return try await streamLogsViaUnixSocket(
                socketPath: socketPath,
                eventLoopGroup: eventLoopGroup,
                containerID: containerID,
                stdout: stdout,
                stderr: stderr,
                follow: follow,
                tail: tail
            )
        }

        guard let client = self.client else {
            throw DockerAPIError.connectionError(ClientError(description: "No client available"))
        }

        let response = try await client.containerLogs(
            path: .init(id: containerID),
            query: .init(
                follow: follow,
                stdout: stdout,
                stderr: stderr,
                tail: tail
            )
        )

        // Handle the response
        switch response {
        case .ok(let ok):
            // Process the streaming body
            let httpBody: OpenAPIRuntime.HTTPBody

            switch ok.body {
            case .applicationVnd_docker_multiplexedStream(let body):
                // Docker multiplexed stream format (stdout/stderr separated)
                httpBody = body
            case .applicationVnd_docker_rawStream(let body):
                // Raw stream format (for TTY containers)
                httpBody = body
            }

            return AsyncThrowingStream { continuation in
                Task {
                    do {
                        var buffer = ByteBuffer()

                        // Process the streaming body
                        for try await chunk in httpBody {
                            buffer.writeBytes(chunk)

                            // Parse Docker's multiplexed format
                            while buffer.readableBytes >= 8 {
                                guard let header = buffer.getBytes(at: buffer.readerIndex, length: 8) else {
                                    break
                                }

                                // Header format:
                                // [0]: stream type (0x01 = stdout, 0x02 = stderr)
                                // [1-3]: reserved
                                // [4-7]: frame size (big-endian)
                                let streamType = header[0]
                                let frameSize = UInt32(header[4]) << 24 |
                                               UInt32(header[5]) << 16 |
                                               UInt32(header[6]) << 8 |
                                               UInt32(header[7])

                                // Check if we have the complete frame
                                if buffer.readableBytes < 8 + Int(frameSize) {
                                    break
                                }

                                // Skip the header
                                buffer.moveReaderIndex(forwardBy: 8)

                                // Read the frame data
                                guard let data = buffer.readBytes(length: Int(frameSize)) else {
                                    break
                                }

                                // Create and yield the log message
                                let logType: LogMessage.StreamType = streamType == 1 ? .stdout : .stderr
                                let message = LogMessage(
                                    type: logType,
                                    data: Data(data)
                                )

                                continuation.yield(message)
                            }
                        }

                        continuation.finish()
                    } catch {
                        logger.error("Error processing log stream", metadata: [
                            "error": .string("\(error)")
                        ])
                        continuation.finish(throwing: error)
                    }
                }
            }

        case .notFound:
            throw DockerAPIError.httpError(status: .notFound)

        case .internalServerError:
            throw DockerAPIError.httpError(status: .internalServerError)

        case .undocumented(let statusCode, _):
            throw DockerAPIError.httpError(status: HTTPResponseStatus(statusCode: statusCode))
        }
    }

    /// List containers using proto
    public func listContainers(all: Bool = false) async throws -> [Components.Schemas.ContainerSummary] {
        guard let client = self.client else {
            throw DockerAPIError.connectionError(ClientError(description: "No OpenAPI client available for listing containers"))
        }

        let response = try await client.containerList(
            query: .init(all: all)
        )

        switch response {
        case .ok(let ok):
            return try ok.body.json

        case .badRequest:
            throw DockerAPIError.httpError(status: .badRequest)

        case .internalServerError:
            throw DockerAPIError.httpError(status: .internalServerError)

        case .undocumented(let statusCode, _):
            throw DockerAPIError.httpError(status: HTTPResponseStatus(statusCode: statusCode))
        }
    }

    /// Inspect a container using proto
    public func inspectContainer(_ containerID: String) async throws -> Components.Schemas.ContainerInspectResponse {
        guard let client = self.client else {
            throw DockerAPIError.connectionError(ClientError(description: "No OpenAPI client available for inspecting containers"))
        }

        let response = try await client.containerInspect(
            path: .init(id: containerID)
        )

        switch response {
        case .ok(let ok):
            return try ok.body.json

        case .notFound:
            throw DockerAPIError.httpError(status: .notFound)

        case .internalServerError:
            throw DockerAPIError.httpError(status: .internalServerError)

        case .undocumented(let statusCode, _):
            throw DockerAPIError.httpError(status: HTTPResponseStatus(statusCode: statusCode))
        }
    }

    /// Stream logs via Unix socket
    private func streamLogsViaUnixSocket(
        socketPath: String,
        eventLoopGroup: EventLoopGroup,
        containerID: String,
        stdout: Bool,
        stderr: Bool,
        follow: Bool,
        tail: String?
    ) async throws -> AsyncThrowingStream<LogMessage, Error> {

        return AsyncThrowingStream { continuation in
            Task {
                do {
                    // Create a channel connected to the Unix socket
                    let bootstrap = ClientBootstrap(group: eventLoopGroup)
                        .channelOption(ChannelOptions.socketOption(.so_reuseaddr), value: 1)
                        .channelInitializer { channel in
                            channel.pipeline.addHTTPClientHandlers(position: .first)
                        }

                    let channel = try await bootstrap.connect(
                        unixDomainSocketPath: socketPath
                    ).get()

                    // Build query parameters
                    var queryParams: [String] = []
                    if stdout { queryParams.append("stdout=true") }
                    if stderr { queryParams.append("stderr=true") }
                    if follow { queryParams.append("follow=true") }
                    if let tail = tail { queryParams.append("tail=\(tail)") }
                    let query = queryParams.isEmpty ? "" : "?" + queryParams.joined(separator: "&")

                    let uri = "/v1.41/containers/\(containerID)/logs\(query)"

                    // Create HTTP request
                    var headers = HTTPHeaders()
                    headers.add(name: "Host", value: "localhost")
                    headers.add(name: "User-Agent", value: "DockerAPIClient/1.0")

                    let requestHead = HTTPRequestHead(
                        version: .http1_1,
                        method: .GET,
                        uri: uri,
                        headers: headers
                    )

                    // Set up response handler
                    let handler = DockerLogStreamHandler(
                        logger: self.logger,
                        continuation: continuation
                    )

                    // Add handler to pipeline
                    try await channel.pipeline.addHandler(handler).get()

                    // Send the request
                    channel.write(HTTPClientRequestPart.head(requestHead), promise: nil)
                    channel.write(HTTPClientRequestPart.end(nil), promise: nil)
                    channel.flush()

                    // Keep channel alive while streaming
                    try await channel.closeFuture.get()

                    continuation.finish()
                } catch {
                    self.logger.error("Failed to stream logs via Unix socket", metadata: [
                        "error": .string("\(error)")
                    ])
                    continuation.finish(throwing: error)
                }
            }
        }
    }
}

/// Handler for Docker log streaming responses
private final class DockerLogStreamHandler: ChannelInboundHandler, @unchecked Sendable {
    typealias InboundIn = HTTPClientResponsePart

    private let logger: Logger
    private let continuation: AsyncThrowingStream<LogMessage, Error>.Continuation
    private var buffer = ByteBuffer()

    init(logger: Logger, continuation: AsyncThrowingStream<LogMessage, Error>.Continuation) {
        self.logger = logger
        self.continuation = continuation
    }

    func channelRead(context: ChannelHandlerContext, data: NIOAny) {
        let part = self.unwrapInboundIn(data)

        switch part {
        case .head(let responseHead):
            logger.debug("Received response", metadata: [
                "status": .string("\(responseHead.status.code)")
            ])

        case .body(let bodyPart):
            // Append to buffer
            var data = bodyPart
            buffer.writeBuffer(&data)

            // Parse Docker's multiplexed format
            while buffer.readableBytes >= 8 {
                guard let header = buffer.getBytes(at: buffer.readerIndex, length: 8) else {
                    break
                }

                // Header format:
                // [0]: stream type (0x01 = stdout, 0x02 = stderr)
                // [1-3]: reserved
                // [4-7]: frame size (big-endian)
                let streamType = header[0]
                let frameSize = UInt32(header[4]) << 24 |
                               UInt32(header[5]) << 16 |
                               UInt32(header[6]) << 8 |
                               UInt32(header[7])

                // Check if we have the complete frame
                if buffer.readableBytes < 8 + Int(frameSize) {
                    break
                }

                // Skip the header
                buffer.moveReaderIndex(forwardBy: 8)

                // Read the frame data
                guard let frameBytes = buffer.readBytes(length: Int(frameSize)) else {
                    break
                }

                // Create and send the log message
                let logType: LogMessage.StreamType = streamType == 1 ? .stdout : .stderr
                let message = LogMessage(
                    type: logType,
                    data: Data(frameBytes)
                )

                continuation.yield(message)

                logger.debug("Processed log frame", metadata: [
                    "type": .string(streamType == 1 ? "stdout" : "stderr"),
                    "size": .string("\(frameSize)")
                ])
            }

        case .end:
            logger.debug("Stream ended")
            context.close(promise: nil)
        }
    }

    func errorCaught(context: ChannelHandlerContext, error: Error) {
        logger.error("Channel error", metadata: ["error": .string("\(error)")])
        continuation.finish(throwing: error)
        context.close(promise: nil)
    }
}