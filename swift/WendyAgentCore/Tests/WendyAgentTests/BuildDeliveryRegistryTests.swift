import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import Hummingbird
import Logging
import NIOCore
import NIOPosix
import NIOSSL
import Testing
import WendyAgentGRPC

@testable import WendyAgentCore

@Suite("Native delivery over real mTLS", .timeLimit(.minutes(1)))
struct BuildDeliveryRegistryTests {
    private struct Timeout: Error {}
    // Event-loop-confined state, including all promise completion paths.
    private final class Collector: ChannelInboundHandler, @unchecked Sendable {
        typealias InboundIn = ByteBuffer
        typealias OutboundOut = ByteBuffer
        let promise: EventLoopPromise<String>
        let wire: String
        var response = ""
        var completed = false
        var timeout: Scheduled<Void>?
        init(_ promise: EventLoopPromise<String>, wire: String) {
            self.promise = promise
            self.wire = wire
        }
        func finish(_ result: Result<String, any Error>) {
            guard !completed else { return }
            completed = true
            timeout?.cancel()
            promise.completeWith(result)
        }
        func handlerAdded(context: ChannelHandlerContext) {
            let channel = context.channel
            timeout = context.eventLoop.scheduleTask(in: .seconds(10)) {
                self.finish(.failure(Timeout()))
                channel.close(promise: nil)
            }
        }
        func channelActive(context: ChannelHandlerContext) {
            context.writeAndFlush(
                wrapOutboundOut(context.channel.allocator.buffer(string: wire)),
                promise: nil
            )
            context.fireChannelActive()
        }
        func channelRead(context: ChannelHandlerContext, data: NIOAny) {
            var buffer = unwrapInboundIn(data)
            response += buffer.readString(length: buffer.readableBytes) ?? ""
        }
        func channelInactive(context: ChannelHandlerContext) {
            finish(.success(response))
            context.fireChannelInactive()
        }
        func errorCaught(context: ChannelHandlerContext, error: any Error) {
            finish(.success(response))
            context.close(promise: nil)
        }
    }
    @available(macOS 26.0, *)
    private static func request(
        port: Int,
        fixture: BuildDeliveryFixture,
        identity: TestPKI.Identity,
        method: String,
        path: String,
        session: String? = nil,
        body: String = "",
        declared: Int? = nil,
        extraHeaders: String = ""
    ) async throws -> String {
        var tls = TLSConfiguration.makeClientConfiguration()
        tls.certificateVerification = .noHostnameVerification
        tls.trustRoots = .certificates(
            try NIOSSLCertificate.fromPEMBytes(Array(fixture.root.pem.utf8))
        )
        tls.certificateChain = try NIOSSLCertificate.fromPEMBytes(Array(identity.certPEM.utf8)).map
        { .certificate($0) }
        tls.privateKey = .privateKey(
            try NIOSSLPrivateKey(bytes: Array(identity.keyPEM.utf8), format: .pem)
        )
        let ssl = try NIOSSLContext(configuration: tls)
        let group = MultiThreadedEventLoopGroup.singleton
        let promise = group.next().makePromise(of: String.self)
        let header = session.map { "Wendy-Build-Delivery: \($0)\r\n" } ?? ""
        let wire =
            "\(method) \(path) HTTP/1.1\r\nHost: registry\r\nConnection: close\r\nContent-Length: \(declared ?? body.utf8.count)\r\n\(header)\(extraHeaders)\r\n\(body)"
        let bootstrap = ClientBootstrap(group: group).channelInitializer { channel in
            channel.eventLoop.makeCompletedFuture {
                try channel.pipeline.syncOperations.addHandler(
                    NIOSSLClientHandler(context: ssl, serverHostname: nil)
                )
                try channel.pipeline.syncOperations.addHandler(Collector(promise, wire: wire))
            }
        }
        let channel = try await bootstrap.connect(host: "127.0.0.1", port: port).get()
        defer { channel.close(promise: nil) }
        return try await promise.futureResult.get()
    }
    @available(macOS 26.0, *)
    private static func withRegistry<T: Sendable>(
        fixture: BuildDeliveryFixture,
        store: BlobStore,
        authority: BuildDeliveryAuthority,
        body: @Sendable (Int) async throws -> T
    ) async throws -> T {
        let registry = AgentImageRegistry(
            store: store,
            configuration: .init(
                host: "127.0.0.1",
                port: 0,
                tls: .init(
                    certPEM: fixture.target.certPEM,
                    chainPEM: fixture.root.pem,
                    keyBacking: .softwarePEM(fixture.target.keyPEM),
                    seKey: nil,
                    deviceScope: .init(tenantUUID: BuildDeliveryFixture.tenant, orgID: nil),
                    orgMode: .strict
                ),
                routes: .pushAndPull,
                label: "delivery-test"
            ),
            authority: authority
        )
        let (events, continuation) = AsyncThrowingStream<Int, any Error>.makeStream()
        let task = Task {
            do {
                try await registry.run { channel in
                    continuation.yield(channel.localAddress?.port ?? 0)
                }
                continuation.finish()
            } catch { continuation.finish(throwing: error) }
        }
        do {
            var iterator = events.makeAsyncIterator()
            let port = try #require(try await iterator.next())
            let result = try await body(port)
            task.cancel()
            _ = await task.result
            return result
        } catch {
            task.cancel()
            _ = await task.result
            throw error
        }
    }
    private static func location(_ response: String) throws -> String {
        for line in response.components(separatedBy: "\r\n")
        where line.lowercased().hasPrefix("location:") {
            return String(line.dropFirst("location:".count)).trimmingCharacters(in: .whitespaces)
        }
        throw BuildDeliveryContract.Failure("missing registry upload location")
    }

    @Test(
        "RPC admission binds the actual builder; OCI resumes and publishes after operator disconnect"
    )
    func rpcAndRegistry() async throws {
        guard #available(macOS 26.0, *) else { return }
        let fixture = try BuildDeliveryFixture()
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(
            "delivery-tls-\(UUID().uuidString)"
        )
        defer { try? FileManager.default.removeItem(at: root) }
        let store = BlobStore(root: root)
        let path = root.appendingPathComponent("authorizations")
        let authority = try await fixture.authority(root: path, images: store)
        let server = try WendyAgent.makeMainServer(
            services: [
                BuildDeliveryService(
                    authority: authority,
                    expectedScope: .init(tenantUUID: BuildDeliveryFixture.tenant, orgID: nil),
                    orgMode: .strict
                )
            ],
            certs: .init(
                certPEM: fixture.target.certPEM,
                chainPEM: fixture.root.pem,
                keyBacking: .softwarePEM(fixture.target.keyPEM),
                seKey: nil
            ),
            host: "127.0.0.1",
            port: 0,
            environment: ["WENDY_MTLS_ORG_ENFORCEMENT": "strict"],
            logger: Logger(label: "test.delivery-rpc")
        )
        let compact = try fixture.sign(fixture.claims())
        let compact2 = try fixture.sign(fixture.claims())
        let sessions: (String, String) = try await withThrowingTaskGroup(of: Void.self) { group in
            group.addTask { try await server.serve() }
            defer {
                server.beginGracefulShutdown()
                group.cancelAll()
            }
            let address = try #require(try await server.listeningAddress)
            let port = try #require(address.ipv4?.port)
            func transport(_ identity: TestPKI.Identity) throws -> HTTP2ClientTransport.Posix {
                try HTTP2ClientTransport.Posix(
                    target: .ipv4(address: "127.0.0.1", port: port),
                    transportSecurity: .mTLS(
                        certificateChain: [.bytes(Array(identity.certPEM.utf8), format: .pem)],
                        privateKey: .bytes(Array(identity.keyPEM.utf8), format: .pem)
                    ) {
                        $0.trustRoots = .certificates([
                            .bytes(Array(fixture.root.pem.utf8), format: .pem)
                        ])
                        $0.serverCertificateVerification = .noHostnameVerification
                    }
                )
            }
            let admitted = try await withGRPCClient(transport: transport(fixture.primary)) {
                client in
                let delivery = Wendy_Agent_Services_V2_WendyBuildDeliveryService.Client(
                    wrapping: client
                )
                var options = CallOptions.defaults
                options.timeout = .seconds(5)
                let capabilities = try await delivery.getDeliveryCapabilities(
                    .init(),
                    options: options
                )
                #expect(
                    capabilities.contractVersion == 1
                        && capabilities.targetPrincipal == BuildDeliveryFixture.target
                )
                var request = Wendy_Agent_Services_V2_AdmitDeliveryRequest()
                request.authorizationJws = compact
                let first = try await delivery.admitDelivery(request, options: options)
                request.authorizationJws = compact2
                let second = try await delivery.admitDelivery(request, options: options)
                return (first.session, second.session)
            }
            // The primary operator transport is now closed. The worker uses its
            // own authenticated device key, not an ambient operator credential.
            try await withGRPCClient(transport: transport(fixture.builder)) { client in
                let delivery = Wendy_Agent_Services_V2_WendyBuildDeliveryService.Client(
                    wrapping: client
                )
                var options = CallOptions.defaults
                options.timeout = .seconds(5)
                var status = Wendy_Agent_Services_V2_GetDeliveryStatusRequest()
                status.session = admitted.0
                let statusResult = try await delivery.getDeliveryStatus(status, options: options)
                #expect(!statusResult.completed)
                var reference = Wendy_Agent_Services_V2_GetDeliveryReferenceRequest()
                reference.reference = "app:latest"
                await #expect(throws: (any Error).self) {
                    try await delivery.getDeliveryReference(reference, options: options)
                }
                var request = Wendy_Agent_Services_V2_AdmitDeliveryRequest()
                request.authorizationJws = compact
                await #expect(throws: (any Error).self) {
                    try await delivery.admitDelivery(request, options: options)
                }
            }
            return admitted
        }
        let upload: String = try await Self.withRegistry(
            fixture: fixture,
            store: store,
            authority: authority
        ) { port in
            for route in ["/v2/app/blobs/uploads", "/v2/app/manifests/latest"] {
                let result = try await Self.request(
                    port: port,
                    fixture: fixture,
                    identity: fixture.builder,
                    method: route.contains("uploads") ? "POST" : "PUT",
                    path: route,
                    body: "unauthorized"
                )
                #expect(result.contains("403 Forbidden"))
            }
            let wrong = try await Self.request(
                port: port,
                fixture: fixture,
                identity: fixture.otherBuilder,
                method: "POST",
                path: "/v2/app/blobs/uploads",
                session: sessions.0
            )
            #expect(wrong.contains("403 Forbidden"))
            let started = try await Self.request(
                port: port,
                fixture: fixture,
                identity: fixture.builder,
                method: "POST",
                path: "/v2/app/blobs/uploads",
                session: sessions.0
            )
            #expect(started.contains("202 Accepted"))
            let upload = try Self.location(started)
            let crossJob = try await Self.request(
                port: port,
                fixture: fixture,
                identity: fixture.builder,
                method: "PATCH",
                path: upload,
                session: sessions.1,
                body: "wrong job"
            )
            #expect(crossJob.contains("403 Forbidden"))
            let first = try await Self.request(
                port: port,
                fixture: fixture,
                identity: fixture.builder,
                method: "PATCH",
                path: upload,
                session: sessions.0,
                body: "hello "
            )
            #expect(first.contains("202 Accepted"))
            return upload
        }
        await authority.close()
        let recovered = try await fixture.authority(root: path, images: store)
        try await Self.withRegistry(fixture: fixture, store: store, authority: recovered) { port in
            let status = try await Self.request(
                port: port,
                fixture: fixture,
                identity: fixture.builder,
                method: "GET",
                path: upload,
                session: sessions.0
            )
            #expect(status.contains("204 No Content"))
            let second = try await Self.request(
                port: port,
                fixture: fixture,
                identity: fixture.builder,
                method: "PATCH",
                path: upload,
                session: sessions.0,
                body: "world"
            )
            #expect(second.contains("202 Accepted"))
            let digest = BlobStore.contentDigest(Data("hello world".utf8))
            let commit = try await Self.request(
                port: port,
                fixture: fixture,
                identity: fixture.builder,
                method: "PUT",
                path: upload + "?digest=" + digest,
                session: sessions.0
            )
            #expect(commit.contains("201 Created"))
            let read = try await Self.request(
                port: port,
                fixture: fixture,
                identity: fixture.builder,
                method: "GET",
                path: "/v2/app/blobs/" + digest,
                session: sessions.0
            )
            #expect(read.contains("403 Forbidden"))
            let manifest =
                #"{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}"#
            let publication = try await Self.request(
                port: port,
                fixture: fixture,
                identity: fixture.builder,
                method: "PUT",
                path: "/v2/app/manifests/latest",
                session: sessions.0,
                body: manifest
            )
            #expect(publication.contains("201 Created"))
            let receipt = try await recovered.status(sessions.0, leaf: fixture.builder.certificate)
            #expect(receipt.completed)
            let replay = try await Self.request(
                port: port,
                fixture: fixture,
                identity: fixture.builder,
                method: "PUT",
                path: "/v2/app/manifests/latest",
                session: sessions.0,
                body: manifest
            )
            #expect(replay.contains("201 Created"))
            let newer = manifest + " "
            let operatorWrite = try await Self.request(
                port: port,
                fixture: fixture,
                identity: fixture.primary,
                method: "PUT",
                path: "/v2/app/manifests/latest",
                body: newer
            )
            #expect(operatorWrite.contains("201 Created"))
            let stale = try await Self.request(
                port: port,
                fixture: fixture,
                identity: fixture.builder,
                method: "PUT",
                path: "/v2/app/manifests/latest",
                session: sessions.0,
                body: manifest
            )
            #expect(stale.contains("403 Forbidden"))
            #expect(
                try store.manifestDigest(repository: "app", reference: "latest")
                    == BlobStore.contentDigest(Data(newer.utf8))
            )
            for route in ["/v2/_catalog", "/v2/app/tags/list", "/v2/app/referrers/" + digest] {
                let denied = try await Self.request(
                    port: port,
                    fixture: fixture,
                    identity: fixture.builder,
                    method: "GET",
                    path: route,
                    session: sessions.0
                )
                #expect(!denied.contains("200 OK"))
            }
            let deletion = try await Self.request(
                port: port,
                fixture: fixture,
                identity: fixture.builder,
                method: "DELETE",
                path: "/v2/app/manifests/latest",
                session: sessions.0
            )
            #expect(!deletion.contains("200 OK") && !deletion.contains("202 Accepted"))
        }
        try await recovered.closeAndRemove()
    }

    @Test("an idle delegated HTTP body ends at the job deadline")
    func idleExpiry() async throws {
        guard #available(macOS 26.0, *) else { return }
        let fixture = try BuildDeliveryFixture()
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(
            "delivery-idle-\(UUID().uuidString)"
        )
        defer { try? FileManager.default.removeItem(at: root) }
        let store = BlobStore(root: root)
        let authority = try await fixture.authority(
            root: root.appendingPathComponent("authorizations"),
            images: store
        )
        var claims = try fixture.claims()
        claims.exp = fixture.time + 6
        claims.admit_before = fixture.time + 5
        let session = try await authority.admit(
            fixture.sign(claims),
            operator: fixture.primary.certificate
        )
        try await Self.withRegistry(fixture: fixture, store: store, authority: authority) { port in
            let started = try await Self.request(
                port: port,
                fixture: fixture,
                identity: fixture.builder,
                method: "POST",
                path: "/v2/app/blobs/uploads",
                session: session
            )
            let upload = try Self.location(started)
            let clock = ContinuousClock()
            let began = clock.now
            _ = try await Self.request(
                port: port,
                fixture: fixture,
                identity: fixture.builder,
                method: "PATCH",
                path: upload,
                session: session,
                declared: 100
            )
            #expect(began.duration(to: clock.now) < .seconds(8))
        }
        try await authority.closeAndRemove()
    }
}
