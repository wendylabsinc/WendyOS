import Foundation
import Hummingbird
import NIOCore
import NIOPosix
import NIOSSL
import Testing

@testable import WendyAgentCore

/// Real-socket tests for the registry's TLS push listener: the server demands
/// a client certificate, accepts only chains that verify against the device's
/// CA (via `ClientCertAuthorizer`), and never answers plain HTTP.
@Suite("Registry TLS listener")
struct RegistryTLSListenerTests {
    private struct ListenerNeverBound: Error {}
    private struct ResponseTimeout: Error {}

    private static func makeStore() -> BlobStore {
        BlobStore(
            root: FileManager.default.temporaryDirectory
                .appendingPathComponent("registry-tls-\(UUID().uuidString)")
        )
    }

    /// Starts a registry listener in a child task and waits for it to bind,
    /// returning the bound port. Throws the listener's own error if it fails
    /// to bind (e.g. port in use).
    ///
    /// Bind success and task failure both feed the same event stream: waiting
    /// on the port alone would hang forever when the bind fails, because this
    /// frame's reference to the application keeps the port continuation alive.
    private static func startListener(
        _ registry: AgentImageRegistry
    ) async throws -> (port: Int, task: Task<Void, any Error>) {
        let (events, continuation) = AsyncStream.makeStream(of: Result<Int, any Error>.self)
        let app = try registry.buildApplication(onServerRunning: { channel in
            continuation.yield(.success(channel.localAddress?.port ?? -1))
        })
        let task = Task {
            do {
                try await app.runService()
                continuation.yield(.failure(ListenerNeverBound()))
            } catch {
                continuation.yield(.failure(error))
                throw error
            }
        }
        var iterator = events.makeAsyncIterator()
        switch await iterator.next() {
        case .success(let port):
            return (port, task)
        case .failure(let error):
            task.cancel()
            throw error
        case nil:
            task.cancel()
            throw ListenerNeverBound()
        }
    }

    private static func stop(_ task: Task<Void, any Error>) async {
        task.cancel()
        _ = try? await task.value
    }

    /// Starts a listener on a port this test just released. `runService()`
    /// can return before the old listening socket is closed: Hummingbird
    /// tears the server down via two racing paths (structured cancellation
    /// of the accept loop, which awaits the close, and an unstructured
    /// graceful-shutdown task, which nothing awaits), so when the second
    /// path wins the port frees up a beat after `stop(_:)` returns. Retry
    /// EADDRINUSE briefly instead of flaking.
    private static func startListenerOnReleasedPort(
        _ makeRegistry: () throws -> AgentImageRegistry
    ) async throws -> (port: Int, task: Task<Void, any Error>) {
        for _ in 0..<49 {
            do {
                return try await Self.startListener(try makeRegistry())
            } catch let error where Self.isAddressInUse(error) {
                try await Task.sleep(nanoseconds: 20_000_000)
            }
        }
        return try await Self.startListener(try makeRegistry())
    }

    private static func isAddressInUse(_ error: any Error) -> Bool {
        let description = String(describing: error).lowercased()
        return description.contains("address already in use")
            || description.contains("errno: 48")
            || description.contains("errno: 98")
    }

    private static func tlsListener(
        port: Int,
        deviceOrg: Int32,
        ca: TestPKI.CA
    ) throws
        -> AgentImageRegistry
    {
        let device = try TestPKI.makeDeviceIdentity(org: deviceOrg, asset: 1, ca: ca)
        return AgentImageRegistry(
            store: Self.makeStore(),
            configuration: .init(
                host: "127.0.0.1",
                port: port,
                tls: RegistryTLS.Configuration(
                    certPEM: device.certPEM,
                    chainPEM: ca.pem,
                    keyBacking: .softwarePEM(device.keyPEM),
                    seKey: nil,
                    deviceScope: .init(tenantUUID: nil, orgID: deviceOrg),
                    orgMode: .grace
                ),
                routes: .pushAndPull,
                label: "test-push"
            )
        )
    }

    /// Collects whatever the server sends for one `GET /v2` request and
    /// resolves when the connection closes (the request carries
    /// `Connection: close`). A rejected handshake resolves to "".
    private final class ResponseCollector: ChannelInboundHandler, @unchecked Sendable {
        typealias InboundIn = ByteBuffer
        typealias OutboundOut = ByteBuffer

        private let promise: EventLoopPromise<String>
        private var accumulated = ""
        // All completion paths (close, error, timeout) funnel through
        // complete(_:) exactly once — a second completion of an
        // EventLoopPromise is a fatal error.
        private var completed = false
        private var timeout: Scheduled<Void>?

        init(promise: EventLoopPromise<String>) {
            self.promise = promise
        }

        private func complete(_ result: Result<String, any Error>) {
            guard !self.completed else { return }
            self.completed = true
            self.timeout?.cancel()
            self.promise.completeWith(result)
        }

        func handlerAdded(context: ChannelHandlerContext) {
            // Backstop so a wedged handshake fails the test instead of hanging it.
            self.timeout = context.eventLoop.scheduleTask(in: .seconds(10)) {
                self.complete(.failure(ResponseTimeout()))
                context.close(promise: nil)
            }
        }

        func channelActive(context: ChannelHandlerContext) {
            let request = "GET /v2 HTTP/1.1\r\nHost: registry\r\nConnection: close\r\n\r\n"
            let buffer = context.channel.allocator.buffer(string: request)
            context.writeAndFlush(self.wrapOutboundOut(buffer), promise: nil)
            context.fireChannelActive()
        }

        func channelRead(context: ChannelHandlerContext, data: NIOAny) {
            var buffer = self.unwrapInboundIn(data)
            self.accumulated += buffer.readString(length: buffer.readableBytes) ?? ""
        }

        func channelInactive(context: ChannelHandlerContext) {
            self.complete(.success(self.accumulated))
            context.fireChannelInactive()
        }

        func errorCaught(context: ChannelHandlerContext, error: any Error) {
            // A refused handshake surfaces here; resolve with what we have ("").
            self.complete(.success(self.accumulated))
            context.close(promise: nil)
        }
    }

    /// Sends `GET /v2` over TLS (optionally with a client identity) and
    /// returns the raw response, or "" when the server rejected the handshake.
    private static func tlsGET(port: Int, clientIdentity: TestPKI.Identity?) async throws -> String
    {
        var tls = TLSConfiguration.makeClientConfiguration()
        // The device cert carries no SAN for 127.0.0.1; server trust is not
        // under test here (the CLI skips it too — buildkit `insecure = true`).
        tls.certificateVerification = .none
        if let clientIdentity {
            let certs = try NIOSSLCertificate.fromPEMBytes(Array(clientIdentity.certPEM.utf8))
            tls.certificateChain = certs.map { .certificate($0) }
            tls.privateKey = .privateKey(
                try NIOSSLPrivateKey(bytes: Array(clientIdentity.keyPEM.utf8), format: .pem)
            )
        }
        let context = try NIOSSLContext(configuration: tls)
        return try await Self.send(port: port) { channel in
            try channel.pipeline.syncOperations.addHandler(
                try NIOSSLClientHandler(context: context, serverHostname: nil)
            )
        }
    }

    /// Sends `GET /v2` as plain HTTP (no TLS handler).
    private static func plainGET(port: Int) async throws -> String {
        try await Self.send(port: port) { _ in }
    }

    private static func send(
        port: Int,
        configure: @escaping @Sendable (any Channel) throws -> Void
    ) async throws -> String {
        let group = MultiThreadedEventLoopGroup.singleton
        let promise = group.next().makePromise(of: String.self)
        let bootstrap = ClientBootstrap(group: group)
            .channelInitializer { channel in
                channel.eventLoop.makeCompletedFuture {
                    try configure(channel)
                    try channel.pipeline.syncOperations.addHandler(
                        ResponseCollector(promise: promise)
                    )
                }
            }
        do {
            _ = try await bootstrap.connect(host: "127.0.0.1", port: port).get()
        } catch {
            promise.fail(error)
        }
        return try await promise.futureResult.get()
    }

    @Test("a client certificate from the device CA is accepted")
    func acceptsTrustedClientCert() async throws {
        let ca = try TestPKI.makeCA()
        let (port, task) = try await Self.startListener(
            try Self.tlsListener(port: 0, deviceOrg: 1, ca: ca)
        )

        let client = try TestPKI.makeIdentity(commonName: "wendy/user/tester", ca: ca)
        let response = try await Self.tlsGET(port: port, clientIdentity: client)
        #expect(response.contains("200 OK"))
        await Self.stop(task)
    }

    @Test("a handshake without a client certificate is rejected")
    func rejectsMissingClientCert() async throws {
        let ca = try TestPKI.makeCA()
        let (port, task) = try await Self.startListener(
            try Self.tlsListener(port: 0, deviceOrg: 1, ca: ca)
        )

        let response = try await Self.tlsGET(port: port, clientIdentity: nil)
        #expect(!response.contains("200 OK"))
        await Self.stop(task)
    }

    @Test("a client certificate from a different CA is rejected")
    func rejectsUntrustedClientCert() async throws {
        let ca = try TestPKI.makeCA()
        let otherCA = try TestPKI.makeCA(commonName: "Impostor CA")
        let (port, task) = try await Self.startListener(
            try Self.tlsListener(port: 0, deviceOrg: 1, ca: ca)
        )

        let impostor = try TestPKI.makeIdentity(commonName: "wendy/user/impostor", ca: otherCA)
        let response = try await Self.tlsGET(port: port, clientIdentity: impostor)
        #expect(!response.contains("200 OK"))
        await Self.stop(task)
    }

    @Test("plain HTTP against the TLS listener fails fast")
    func rejectsPlainHTTP() async throws {
        let ca = try TestPKI.makeCA()
        let (port, task) = try await Self.startListener(
            try Self.tlsListener(port: 0, deviceOrg: 1, ca: ca)
        )

        let response = try await Self.plainGET(port: port)
        #expect(!response.contains("200 OK"))
        await Self.stop(task)
    }

    @Test("stop-then-start swaps a plain listener for a TLS one on the same port")
    func restartSwapsScheme() async throws {
        let plain = AgentImageRegistry(
            store: Self.makeStore(),
            configuration: .init(
                host: "127.0.0.1",
                port: 0,
                tls: nil,
                routes: .pushAndPull,
                label: "test-push"
            )
        )
        let (port, plainTask) = try await Self.startListener(plain)
        #expect(try await Self.plainGET(port: port).contains("200 OK"))

        // Provisioning transition: tear down, rebind the SAME port with TLS.
        await Self.stop(plainTask)
        let ca = try TestPKI.makeCA()
        let (tlsPort, tlsTask) = try await Self.startListenerOnReleasedPort {
            try Self.tlsListener(port: port, deviceOrg: 1, ca: ca)
        }
        #expect(tlsPort == port)

        let client = try TestPKI.makeIdentity(commonName: "wendy/user/tester", ca: ca)
        #expect(try await Self.tlsGET(port: port, clientIdentity: client).contains("200 OK"))
        #expect(!(try await Self.plainGET(port: port).contains("200 OK")))
        await Self.stop(tlsTask)
    }

    @Test("binding an occupied port surfaces an address-in-use error")
    func occupiedPortThrows() async throws {
        let first = AgentImageRegistry(
            store: Self.makeStore(),
            configuration: .init(
                host: "127.0.0.1",
                port: 0,
                tls: nil,
                routes: .pullOnly,
                label: "first"
            )
        )
        let (port, firstTask) = try await Self.startListener(first)

        let second = AgentImageRegistry(
            store: Self.makeStore(),
            configuration: .init(
                host: "127.0.0.1",
                port: port,
                tls: nil,
                routes: .pullOnly,
                label: "second"
            )
        )
        await #expect(throws: (any Error).self) {
            _ = try await Self.startListener(second)
        }
        await Self.stop(firstTask)
    }
}
