//
//  DockerAPIClient.swift
//  DockerOpenAPI
//
//  Wrapper for DockerOpenAPI proto-generated code
//

import AsyncHTTPClient
import Foundation
import Logging
import NIOCore
import NIOHTTP1
import NIOPosix
import NIOTransportServices
import OpenAPIAsyncHTTPClient
import OpenAPIRuntime

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
    case invalidURL(String)

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
        case .invalidURL(let url):
            return "Invalid Docker API URL: \(url)"
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
    private let apiVersion: String

    /// Initialize with Unix socket (default for Docker)
    public init(
        socketPath: String = "/var/run/docker.sock",
        apiVersion: String = "v1.50",
        logger: Logger = Logger(label: "DockerAPIClient")
    ) throws {
        self.logger = logger
        self.ownsHTTPClient = false
        self.httpClient = nil
        self.socketPath = socketPath
        self.ownsEventLoopGroup = true
        self.eventLoopGroup = MultiThreadedEventLoopGroup(numberOfThreads: 1)
        self.client = nil
        self.apiVersion = apiVersion
    }

    public init(
        baseURL: String,
        httpClient: HTTPClient,
        apiVersion: String = "v1.50",
        logger: Logger = Logger(label: "DockerAPIClient")
    ) throws {
        self.logger = logger
        self.httpClient = httpClient
        self.ownsHTTPClient = false
        self.socketPath = nil
        self.eventLoopGroup = nil
        self.ownsEventLoopGroup = false
        self.apiVersion = apiVersion

        // Create transport using provided HTTP client
        let transport = AsyncHTTPClientTransport(
            configuration: .init(
                client: httpClient
            )
        )

        // Initialize the generated client
        guard let serverURL = URL(string: baseURL) else {
            throw DockerAPIError.invalidURL(baseURL)
        }
        self.client = Client(
            serverURL: serverURL,
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

        logger.debug(
            "Streaming logs from container",
            metadata: [
                "containerID": .string(containerID)
            ]
        )

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

                        // Determine if stream is multiplexed based on content type
                        let isMultiplexed =
                            switch ok.body {
                            case .applicationVnd_docker_multiplexedStream:
                                true
                            case .applicationVnd_docker_rawStream:
                                false
                            }

                        // Process the streaming body
                        for try await chunk in httpBody {
                            buffer.writeBytes(chunk)

                            if isMultiplexed {
                                // Parse Docker's multiplexed format
                                while buffer.readableBytes >= 8 {
                                    guard
                                        let header = buffer.getBytes(
                                            at: buffer.readerIndex,
                                            length: 8
                                        )
                                    else {
                                        break
                                    }

                                    // Header format:
                                    // [0]: stream type (0x01 = stdout, 0x02 = stderr)
                                    // [1-3]: reserved
                                    // [4-7]: frame size (big-endian)
                                    let streamType = header[0]
                                    let frameSize =
                                        UInt32(header[4]) << 24 | UInt32(header[5]) << 16 | UInt32(
                                            header[6]
                                        ) << 8 | UInt32(header[7])

                                    let frameSizeInt = Int(frameSize)

                                    // Check if we have the complete frame
                                    if buffer.readableBytes < 8 + frameSizeInt {
                                        break
                                    }

                                    // Skip the header
                                    buffer.moveReaderIndex(forwardBy: 8)

                                    // Read the frame data
                                    guard let data = buffer.readBytes(length: frameSizeInt) else {
                                        break
                                    }

                                    // Create and yield the log message
                                    let logType: LogMessage.StreamType =
                                        streamType == 1 ? .stdout : .stderr
                                    let message = LogMessage(
                                        type: logType,
                                        data: Data(data)
                                    )

                                    continuation.yield(message)
                                }
                            } else {
                                // For raw stream (TTY containers), all output is treated as stdout
                                // Process available data immediately
                                if let rawBytes = buffer.readBytes(length: buffer.readableBytes) {
                                    let message = LogMessage(
                                        type: .stdout,
                                        data: Data(rawBytes)
                                    )
                                    continuation.yield(message)
                                }
                            }
                        }

                        continuation.finish()
                    } catch {
                        logger.error(
                            "Error processing log stream",
                            metadata: [
                                "error": .string("\(error)")
                            ]
                        )
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
    public func listContainers(
        all: Bool = false
    ) async throws -> [Components.Schemas.ContainerSummary] {
        guard let client = self.client else {
            throw DockerAPIError.connectionError(
                ClientError(description: "No OpenAPI client available for listing containers")
            )
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
    public func inspectContainer(
        _ containerID: String
    ) async throws -> Components.Schemas.ContainerInspectResponse {
        guard let client = self.client else {
            throw DockerAPIError.connectionError(
                ClientError(description: "No OpenAPI client available for inspecting containers")
            )
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

                    let uri = "/\(self.apiVersion)/containers/\(containerID)/logs\(query)"

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
                    logger.debug(
                        "Sending HTTP request to Docker",
                        metadata: [
                            "uri": .string(uri),
                            "follow": .string(follow ? "true" : "false"),
                        ]
                    )
                    channel.write(HTTPClientRequestPart.head(requestHead), promise: nil)
                    channel.write(HTTPClientRequestPart.end(nil), promise: nil)
                    channel.flush()

                    // Store the channel for cleanup later if needed
                    // Don't wait for it to close if follow=true, as it will stay open
                    if !follow {
                        // For non-follow mode, wait for the channel to close
                        try await channel.closeFuture.get()
                        continuation.finish()
                    } else {
                        // For follow mode, keep the connection alive until cancelled
                        await withTaskCancellationHandler {
                            while !Task.isCancelled {
                                do {
                                    try await Task.sleep(for: .seconds(60))
                                } catch {
                                    break
                                }
                            }
                        } onCancel: {
                            channel.close(mode: .all, promise: nil)
                            continuation.finish()
                        }
                    }
                } catch {
                    self.logger.error(
                        "Failed to stream logs via Unix socket",
                        metadata: [
                            "error": .string("\(error)")
                        ]
                    )
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
    private var isMultiplexed = true

    init(logger: Logger, continuation: AsyncThrowingStream<LogMessage, Error>.Continuation) {
        self.logger = logger
        self.continuation = continuation
    }

    func channelRead(context: ChannelHandlerContext, data: NIOAny) {
        let part = self.unwrapInboundIn(data)

        switch part {
        case .head(let responseHead):
            logger.info(
                "Received Docker response",
                metadata: [
                    "status": .string("\(responseHead.status.code)"),
                    "headers": .string("\(responseHead.headers)"),
                ]
            )

            // Validate response status
            guard responseHead.status == .ok || responseHead.status == .switchingProtocols else {
                let error = DockerAPIError.httpError(status: responseHead.status)
                logger.error(
                    "Docker API returned error",
                    metadata: [
                        "status": .string("\(responseHead.status.code)"),
                        "reason": .string(responseHead.status.reasonPhrase),
                    ]
                )
                continuation.finish(throwing: error)
                context.close(promise: nil)
                return
            }

            // Check Content-Type to determine if stream is multiplexed or raw
            if let contentType = responseHead.headers["content-type"].first {
                // Docker returns "application/vnd.docker.raw-stream" for TTY containers
                // and "application/vnd.docker.multiplexed-stream" for non-TTY
                isMultiplexed = !contentType.contains("raw-stream")
                logger.info(
                    "Stream format detected",
                    metadata: [
                        "contentType": .string(contentType),
                        "isMultiplexed": .string(isMultiplexed ? "true" : "false"),
                        "transferEncoding": .string(
                            responseHead.headers["transfer-encoding"].first ?? "none"
                        ),
                    ]
                )
            }

        case .body(let bodyPart):
            // Append to buffer
            var data = bodyPart
            buffer.writeBuffer(&data)

            if isMultiplexed {
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
                    let frameSize =
                        UInt32(header[4]) << 24 | UInt32(header[5]) << 16 | UInt32(header[6]) << 8
                        | UInt32(header[7])

                    let frameSizeInt = Int(frameSize)

                    // Check if we have the complete frame
                    if buffer.readableBytes < 8 + frameSizeInt {
                        break
                    }

                    // Skip the header
                    buffer.moveReaderIndex(forwardBy: 8)

                    // Read the frame data
                    guard let frameBytes = buffer.readBytes(length: frameSizeInt) else {
                        break
                    }

                    // Create and send the log message
                    let logType: LogMessage.StreamType = streamType == 1 ? .stdout : .stderr
                    let message = LogMessage(
                        type: logType,
                        data: Data(frameBytes)
                    )

                    continuation.yield(message)

                    logger.debug(
                        "Processed log frame",
                        metadata: [
                            "type": .string(streamType == 1 ? "stdout" : "stderr"),
                            "size": .string("\(frameSize)"),
                        ]
                    )
                }
            } else {
                // For raw stream (TTY containers), all output is treated as stdout
                // Process available data immediately
                if let rawBytes = buffer.readBytes(length: buffer.readableBytes) {
                    let message = LogMessage(
                        type: .stdout,
                        data: Data(rawBytes)
                    )
                    continuation.yield(message)

                    logger.debug(
                        "Processed raw stream data",
                        metadata: [
                            "size": .string("\(rawBytes.count)")
                        ]
                    )
                }
            }

        case .end:
            logger.info("HTTP response ended - Docker closed the connection")
            continuation.finish()
            context.close(promise: nil)
        }
    }

    func errorCaught(context: ChannelHandlerContext, error: Error) {
        logger.error("Channel error", metadata: ["error": .string("\(error)")])
        continuation.finish(throwing: error)
        context.close(promise: nil)
    }
}
