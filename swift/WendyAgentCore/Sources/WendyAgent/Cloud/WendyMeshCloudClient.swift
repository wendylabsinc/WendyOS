public import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import SwiftProtobuf
import WendyCloudGRPC

public struct WendyCloudDevice: Equatable, Identifiable, Sendable {
    public let id: Int32
    public let name: String
    public let organizationID: Int32

    public init(id: Int32, name: String, organizationID: Int32) {
        self.id = id
        self.name = name
        self.organizationID = organizationID
    }
}

public enum WendyCloudDirectory {
    public static func listOnlineDevices(
        cloudGRPC: String,
        credentials: WendyCloudCredentials
    ) async throws -> [WendyCloudDevice] {
        try await withClient(cloudGRPC: cloudGRPC, credentials: credentials) { grpc, metadata in
            let client = Wendycloud_V1_AssetService.Client(wrapping: grpc)
            var request = Wendycloud_V1_ListAssetsRequest()
            request.organizationID = credentials.organizationID
            request.isComputeDevice = true
            request.onlineOnly = true
            return try await client.listAssets(request, metadata: metadata) { response in
                var devices: [WendyCloudDevice] = []
                for try await message in response.messages {
                    devices.append(
                        WendyCloudDevice(
                            id: message.asset.id,
                            name: message.asset.name,
                            organizationID: message.asset.organizationID
                        )
                    )
                }
                return devices
            }
        }
    }
}

/// Relays one TCP connection through the Wendy cloud tunnel broker.
public enum WendyCloudTunnel {
    public static func relay(
        assetID: Int32,
        host: String,
        port: Int,
        cloudGRPC: String,
        credentials: WendyCloudCredentials,
        readFlow: @escaping @Sendable () async throws -> Data?,
        writeFlow: @escaping @Sendable (Data) async throws -> Void
    ) async throws {
        try await withClient(cloudGRPC: cloudGRPC, credentials: credentials) { grpc, metadata in
            let client = Wendycloud_V1_TunnelBrokerService.Client(wrapping: grpc)
            try await client.clientTunnel(metadata: metadata) { writer in
                try await writer.write(
                    .with {
                        $0.open = .with {
                            $0.assetID = assetID
                            $0.host = host
                            $0.port = UInt32(port)
                        }
                    }
                )
                while let payload = try await readFlow() {
                    guard !payload.isEmpty else { continue }
                    try await writer.write(.with { $0.data = .with { $0.payload = payload } })
                }
            } onResponse: { response in
                for try await message in response.messages {
                    try await writeFlow(message.payload)
                }
            }
        }
    }
}

/// One long-lived multiplexed UDP/ICMP broker session for a mesh device.
public actor WendyCloudDatagramSession {
    public struct Configuration: Sendable {
        public let assetID: Int32
        public let cloudGRPC: String
        public let credentials: WendyCloudCredentials

        public init(assetID: Int32, cloudGRPC: String, credentials: WendyCloudCredentials) {
            self.assetID = assetID
            self.cloudGRPC = cloudGRPC
            self.credentials = credentials
        }
    }

    public struct EchoReply: Sendable {
        public let identifier: UInt32
        public let sequence: UInt32
        public let payload: Data
        public let originateUnixNanoseconds: UInt64
        public let agentUnixNanoseconds: UInt64
    }

    private let outbox: AsyncStream<Wendycloud_V1_ClientTunnelMessage>.Continuation
    private var datagramHandlers: [UInt32: @Sendable (Data) -> Void] = [:]
    private var echoHandlers: [UInt32: @Sendable (EchoReply) -> Void] = [:]
    private var closeHandler: (@Sendable () -> Void)?
    private var runTask: Task<Void, Never>?
    private var closed = false

    private init(outbox: AsyncStream<Wendycloud_V1_ClientTunnelMessage>.Continuation) {
        self.outbox = outbox
    }

    public static func open(
        _ configuration: Configuration
    ) async throws
        -> WendyCloudDatagramSession
    {
        let (stream, continuation) = AsyncStream<Wendycloud_V1_ClientTunnelMessage>.makeStream()
        let session = WendyCloudDatagramSession(outbox: continuation)
        let task = Task {
            do {
                try await withClient(
                    cloudGRPC: configuration.cloudGRPC,
                    credentials: configuration.credentials
                ) { grpc, metadata in
                    let client = Wendycloud_V1_TunnelBrokerService.Client(wrapping: grpc)
                    try await client.clientTunnel(metadata: metadata) { writer in
                        try await writer.write(
                            .with {
                                $0.open = .with {
                                    $0.assetID = configuration.assetID
                                    $0.host = "localhost"
                                    $0.`protocol` = .datagram
                                }
                            }
                        )
                        for await message in stream {
                            try await writer.write(message)
                        }
                    } onResponse: { response in
                        for try await frame in response.messages {
                            await session.deliver(frame)
                        }
                    }
                }
            } catch {
                // All session exits are reported through the close callback. A
                // future datagram causes the extension cache to open a new one.
            }
            await session.didClose()
        }
        await session.attach(task)
        return session
    }

    public func sendDatagram(flowID: UInt32, port: UInt32, payload: Data) {
        outbox.yield(
            .with {
                $0.data = .with {
                    $0.datagram = .with {
                        $0.flowID = flowID
                        $0.port = port
                        $0.payload = payload
                    }
                }
            }
        )
    }

    public func sendEcho(identifier: UInt32, sequence: UInt32, payload: Data) {
        outbox.yield(
            .with {
                $0.data = .with {
                    $0.icmpRequest = .with {
                        $0.identifier = identifier
                        $0.sequence = sequence
                        $0.payload = payload
                        $0.originateUnixNs = UInt64(
                            Date().timeIntervalSince1970 * 1_000_000_000
                        )
                    }
                }
            }
        )
    }

    public func setDatagramHandler(
        flowID: UInt32,
        _ handler: (@Sendable (Data) -> Void)?
    ) {
        datagramHandlers[flowID] = handler
    }

    public func setEchoHandler(
        identifier: UInt32,
        _ handler: (@Sendable (EchoReply) -> Void)?
    ) {
        echoHandlers[identifier] = handler
    }

    public func onClose(_ handler: @escaping @Sendable () -> Void) {
        if closed {
            handler()
        } else {
            closeHandler = handler
        }
    }

    public func close() {
        didClose()
        runTask?.cancel()
    }

    private func attach(_ task: Task<Void, Never>) {
        runTask = task
    }

    private func deliver(_ frame: Wendycloud_V1_TunnelData) {
        if frame.hasDatagram {
            datagramHandlers[frame.datagram.flowID]?(frame.datagram.payload)
        } else if frame.hasIcmpReply {
            let reply = frame.icmpReply
            echoHandlers[reply.identifier]?(
                EchoReply(
                    identifier: reply.identifier,
                    sequence: reply.sequence,
                    payload: reply.payload,
                    originateUnixNanoseconds: reply.originateUnixNs,
                    agentUnixNanoseconds: reply.agentUnixNs
                )
            )
        }
    }

    private func didClose() {
        guard !closed else { return }
        closed = true
        outbox.finish()
        closeHandler?()
    }
}

private func parseCloudEndpoint(_ endpoint: String) throws -> (host: String, port: Int) {
    if let colon = endpoint.lastIndex(of: ":"),
        let port = Int(endpoint[endpoint.index(after: colon)...]),
        port > 0
    {
        return (String(endpoint[..<colon]), port)
    }
    guard !endpoint.isEmpty else {
        throw RPCError(code: .invalidArgument, message: "Wendy cloud endpoint is empty")
    }
    return (endpoint, 443)
}

private func clientMetadata(for credentials: WendyCloudCredentials) -> Metadata {
    guard let userID = credentials.userID, !userID.isEmpty else { return [:] }
    let identity = "URI=urn:wendy:org:\(credentials.organizationID):user:\(userID)"
    var metadata = Metadata()
    metadata.addString(identity, forKey: "x-wendy-client-cert")
    metadata.addString(identity, forKey: "x-forwarded-client-cert")
    return metadata
}

private func makeCloudTransport(
    endpoint: String,
    credentials: WendyCloudCredentials
) throws -> HTTP2ClientTransport.Posix {
    let (host, port) = try parseCloudEndpoint(endpoint)
    let certificateChain: [TLSConfig.CertificateSource] = [
        credentials.pemCertificate,
        credentials.pemCertificateChain,
    ].filter { !$0.isEmpty }.map { .bytes(Array($0.utf8), format: .pem) }
    let tls = HTTP2ClientTransport.Posix.TransportSecurity.TLS(
        certificateChain: certificateChain,
        privateKey: .bytes(Array(credentials.pemPrivateKey.utf8), format: .pem),
        serverCertificateVerification: .fullVerification,
        trustRoots: .systemDefault
    )
    return try HTTP2ClientTransport.Posix(
        target: .dns(host: host, port: port),
        transportSecurity: .tls(tls)
    )
}

private func withClient<Result: Sendable>(
    cloudGRPC: String,
    credentials: WendyCloudCredentials,
    _ body:
        @Sendable @escaping (
            GRPCClient<HTTP2ClientTransport.Posix>, Metadata
        ) async throws -> Result
) async throws -> Result {
    let transport = try makeCloudTransport(endpoint: cloudGRPC, credentials: credentials)
    let metadata = clientMetadata(for: credentials)
    return try await withGRPCClient(transport: transport) { client in
        try await body(client, metadata)
    }
}
