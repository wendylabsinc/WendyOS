import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import Logging
import NIOCore
import NIOPosix
import NIOSSL
import Testing

@testable import WendyAgentCore

/// Real-socket regression for WDY-3013: a peer that drops the TCP connection
/// while the mTLS handshake is still in flight must cost the agent one
/// connection, not the process.
///
/// The server pipeline writes the HTTP/2 preface before the handshake
/// completes, so the TLS handler holds a buffered, flushed write when the
/// socket goes away. If the TLS handler lets a downstream handler flush that
/// buffer after it has already declared the connection closed, BoringSSL
/// cannot make progress and `NIOSSLHandler.doUnbufferActions` aborts the
/// process with "looped too many times". On an affected `swift-nio-ssl` this
/// test does not fail: it kills the test runner with that exact message.
@Suite("mTLS handshake abort", .serialized)
struct MTLSHandshakeAbortTests {
    private struct HandshakeTimeout: Error {}
    private struct NoListeningPort: Error {}

    private static func currentPOSIXError() -> POSIXError {
        POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO)
    }

    private typealias PosixGRPCServer = GRPCServer<HTTP2ServerTransport.Posix>

    private static func makeServer(
        ca: TestPKI.CA,
        deviceOrg: Int32
    ) throws -> PosixGRPCServer {
        let device = try TestPKI.makeDeviceIdentity(org: deviceOrg, asset: 1, ca: ca)
        let certs = ProvisioningService.ProvisioningCerts(
            certPEM: device.certPEM,
            chainPEM: ca.pem,
            keyBacking: .softwarePEM(device.keyPEM),
            seKey: nil
        )
        let security = try WendyAgent.makeMTLSSecurity(
            certs: certs,
            environment: [:],
            logger: Logger(label: "test.mtls-handshake-abort")
        )
        return PosixGRPCServer(
            transport: HTTP2ServerTransport.Posix(
                address: .ipv4(host: "127.0.0.1", port: 0),
                transportSecurity: security,
                config: WendyAgent.mainServerConfig
            ),
            services: []
        )
    }

    private static func listeningPort(of server: PosixGRPCServer) async throws -> Int {
        guard let address = try await server.listeningAddress, let port = address.ipv4?.port else {
            throw NoListeningPort()
        }
        return port
    }

    /// Opens a TCP connection and closes it without sending a byte, using raw
    /// sockets so the FIN reaches the server within the same event-loop tick
    /// as the accept. That is what a peer that connects and immediately gives
    /// up looks like: the server's TLS handler is still waiting for a
    /// ClientHello, the HTTP/2 preface is buffered behind it, and the
    /// transport's flush coalescing has not yet released its pending flush.
    private static func connectAndAbort(port: Int) throws {
        let fileDescriptor = socket(AF_INET, SOCK_STREAM, IPPROTO_TCP)
        guard fileDescriptor >= 0 else {
            throw currentPOSIXError()
        }
        defer { Darwin.close(fileDescriptor) }

        var address = sockaddr_in()
        address.sin_len = UInt8(MemoryLayout<sockaddr_in>.size)
        address.sin_family = sa_family_t(AF_INET)
        address.sin_port = in_port_t(port).bigEndian
        address.sin_addr = in_addr(s_addr: inet_addr("127.0.0.1"))

        try withUnsafePointer(to: &address) { pointer in
            try pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) { socketAddress in
                guard
                    connect(
                        fileDescriptor,
                        socketAddress,
                        socklen_t(MemoryLayout<sockaddr_in>.size)
                    ) == 0
                else {
                    throw currentPOSIXError()
                }
            }
        }
    }

    /// Resolves once decrypted application data arrives — the server only
    /// releases its buffered HTTP/2 preface after the handshake completes — or
    /// fails on error/close.
    private final class HandshakeObserver: ChannelInboundHandler, @unchecked Sendable {
        typealias InboundIn = ByteBuffer

        private let promise: EventLoopPromise<Void>
        private var completed = false
        private var timeout: Scheduled<Void>?

        init(promise: EventLoopPromise<Void>) {
            self.promise = promise
        }

        private func complete(_ result: Result<Void, any Error>) {
            guard !self.completed else { return }
            self.completed = true
            self.timeout?.cancel()
            self.promise.completeWith(result)
        }

        func handlerAdded(context: ChannelHandlerContext) {
            self.timeout = context.eventLoop.scheduleTask(in: .seconds(10)) {
                self.complete(.failure(HandshakeTimeout()))
                context.close(promise: nil)
            }
        }

        func channelRead(context: ChannelHandlerContext, data: NIOAny) {
            self.complete(.success(()))
        }

        func errorCaught(context: ChannelHandlerContext, error: any Error) {
            self.complete(.failure(error))
            context.close(promise: nil)
        }

        func channelInactive(context: ChannelHandlerContext) {
            self.complete(.failure(HandshakeTimeout()))
            context.fireChannelInactive()
        }
    }

    /// Completes a full mTLS handshake with a CA-issued client identity, which
    /// exercises the server's asynchronous `ClientCertAuthorizer` path, and
    /// waits for the server's HTTP/2 preface to arrive over the encrypted link.
    private static func completeHandshake(
        port: Int,
        clientIdentity: TestPKI.Identity,
        ca: TestPKI.CA,
        group: any EventLoopGroup
    ) async throws {
        var tls = TLSConfiguration.makeClientConfiguration()
        tls.certificateVerification = .noHostnameVerification
        tls.trustRoots = .certificates(try NIOSSLCertificate.fromPEMBytes(Array(ca.pem.utf8)))
        tls.certificateChain = try NIOSSLCertificate.fromPEMBytes(
            Array(clientIdentity.certPEM.utf8)
        )
        .map { .certificate($0) }
        tls.privateKey = .privateKey(
            try NIOSSLPrivateKey(bytes: Array(clientIdentity.keyPEM.utf8), format: .pem)
        )
        let context = try NIOSSLContext(configuration: tls)

        let eventLoop = group.next()
        let handshake = eventLoop.makePromise(of: Void.self)
        let channel = try await ClientBootstrap(group: eventLoop)
            .channelInitializer { channel in
                channel.eventLoop.makeCompletedFuture {
                    try channel.pipeline.syncOperations.addHandlers(
                        try NIOSSLClientHandler(context: context, serverHostname: nil),
                        HandshakeObserver(promise: handshake)
                    )
                }
            }
            .connect(host: "127.0.0.1", port: port)
            .get()
        defer { channel.close(promise: nil) }
        try await handshake.futureResult.get()
    }

    @Test("A peer closing mid-handshake does not take the mTLS listener down")
    func peerAbortMidHandshakeKeepsListenerAlive() async throws {
        let ca = try TestPKI.makeCA()
        let deviceOrg: Int32 = 7
        let server = try Self.makeServer(ca: ca, deviceOrg: deviceOrg)
        let serveTask = Task { try await server.serve() }
        defer {
            server.beginGracefulShutdown()
            serveTask.cancel()
        }
        let port = try await Self.listeningPort(of: server)

        let group = MultiThreadedEventLoopGroup(numberOfThreads: 1)
        defer { group.shutdownGracefully { _ in } }

        // A burst of aborted connections: several are already half-closed by
        // the time the accept loop reaches them, which is the ordering that
        // matters. Repeating removes any dependence on one lucky schedule.
        for _ in 0..<50 {
            try Self.connectAndAbort(port: port)
        }

        // Give the server loop a beat to process the closes: on an affected
        // TLS handler the process is already gone by the time this returns.
        try await Task.sleep(nanoseconds: 300_000_000)

        // The same listener must still complete a genuine mTLS handshake.
        let client = try TestPKI.makeDeviceIdentity(org: deviceOrg, asset: 2, ca: ca)
        try await Self.completeHandshake(port: port, clientIdentity: client, ca: ca, group: group)
    }
}
