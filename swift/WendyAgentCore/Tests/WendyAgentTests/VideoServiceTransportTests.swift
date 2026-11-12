import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import GRPCProtobuf
import Logging
import Testing
import WendyAgentGRPC

@testable import WendyAgentCore

/// Actual HTTP/2/mTLS and VideoService lifecycle; the capture source is a
/// controlled stream of opaque probe bytes, not a physical or decoded camera.
@Suite("VideoService production transport")
struct VideoServiceTransportTests {
    @Test(.timeLimit(.minutes(1)), arguments: [false, true])
    func streamingAdmissionCancellationAndReuse(enrolled: Bool) async throws {
        let ca = try TestPKI.makeCA()
        let source = try TestPKI.makeDeviceIdentity(org: 7, asset: 1, ca: ca)
        let operatorIdentity = try TestPKI.makeIdentity(
            commonName: "video operator",
            ca: ca,
            uris: ["urn:wendy:org:7:user:fixture"]
        )
        let foreign = try TestPKI.makeIdentity(
            commonName: "foreign operator",
            ca: ca,
            uris: ["urn:wendy:org:8:user:fixture"]
        )
        let peerDevice = try TestPKI.makeDeviceIdentity(org: 7, asset: 2, ca: ca)
        let camera = TransportCameraProbe()
        defer { camera.finishAll() }
        let service = VideoService(camera: camera, controls: TransportCameraControls())
        let server = try WendyAgent.makeMainServer(
            services: [service],
            certs: enrolled
                ? .init(
                    certPEM: source.certPEM,
                    chainPEM: ca.pem,
                    keyBacking: .softwarePEM(source.keyPEM),
                    seKey: nil
                ) : nil,
            host: "127.0.0.1",
            port: 0,
            environment: ["WENDY_MTLS_ORG_ENFORCEMENT": "strict"],
            logger: Logger(label: "test.video-transport")
        )
        try await withThrowingTaskGroup(of: Void.self) { group in
            group.addTask { try await server.serve() }
            defer {
                server.beginGracefulShutdown()
                group.cancelAll()
            }
            let address = try #require(try await server.listeningAddress)
            let port = try #require(address.ipv4?.port)
            var options = CallOptions.defaults
            options.timeout = .seconds(4)

            if enrolled {
                for (identity, expected) in [
                    (nil, RPCError.Code.unavailable), (foreign, .unavailable),
                    (peerDevice, .permissionDenied),
                ] as [(TestPKI.Identity?, RPCError.Code)] {
                    let security: HTTP2ClientTransport.Posix.TransportSecurity
                    if let identity {
                        security = .mTLS(
                            certificateChain: [.bytes(Array(identity.certPEM.utf8), format: .pem)],
                            privateKey: .bytes(Array(identity.keyPEM.utf8), format: .pem)
                        ) {
                            $0.trustRoots = .certificates([.bytes(Array(ca.pem.utf8), format: .pem)]
                            )
                            $0.serverCertificateVerification = .noHostnameVerification
                        }
                    } else {
                        security = .tls {
                            $0.trustRoots = .certificates([.bytes(Array(ca.pem.utf8), format: .pem)]
                            )
                            $0.serverCertificateVerification = .noHostnameVerification
                        }
                    }
                    let transport = try HTTP2ClientTransport.Posix(
                        target: .ipv4(address: "127.0.0.1", port: port),
                        transportSecurity: security
                    )
                    try await withGRPCClient(transport: transport) { client in
                        let video = TransportVideoClient(client: client)
                        do {
                            _ = try await video.listVideoDevices(.init(), options: options)
                            Issue.record("unapproved caller reached camera enumeration")
                        } catch let error as RPCError {
                            #expect(error.code == expected)
                        }
                        do {
                            try await video.streamVideo(.init(), options: options) { response in
                                for try await _ in response.messages {
                                    Issue.record("unapproved caller received camera bytes")
                                }
                            }
                            Issue.record("unapproved camera stream succeeded")
                        } catch let error as RPCError {
                            #expect(error.code == expected)
                        }
                    }
                    #expect(camera.snapshot().discoveries == 0)
                    #expect(camera.snapshot().started == 0)
                }
            }

            let security: HTTP2ClientTransport.Posix.TransportSecurity =
                enrolled
                ? .mTLS(
                    certificateChain: [.bytes(Array(operatorIdentity.certPEM.utf8), format: .pem)],
                    privateKey: .bytes(Array(operatorIdentity.keyPEM.utf8), format: .pem)
                ) {
                    $0.trustRoots = .certificates([.bytes(Array(ca.pem.utf8), format: .pem)])
                    $0.serverCertificateVerification = .noHostnameVerification
                } : .plaintext
            let transport = try HTTP2ClientTransport.Posix(
                target: .ipv4(address: "127.0.0.1", port: port),
                transportSecurity: security
            )
            try await withGRPCClient(transport: transport) { client in
                let video = TransportVideoClient(client: client)
                let devices = try await video.listVideoDevices(.init(), options: options)
                #expect(devices.devices.map(\.id) == [0])
                let received = TransportFramesProbe()
                let stream: @Sendable (Int) async throws -> Void = { index in
                    var streamingOptions = CallOptions.defaults
                    streamingOptions.timeout = .seconds(15)
                    try await video.streamVideo(.init(), options: streamingOptions) { response in
                        for try await frame in response.messages {
                            #expect(frame.data == TransportCameraProbe.payload)
                            #expect(frame.codec == .h264)
                            #expect(frame.timestampNs == 1924)
                            received.record(index)
                        }
                    }
                }
                let first = Task { try await stream(1) }
                let second = Task { try await stream(2) }
                defer {
                    first.cancel()
                    second.cancel()
                }
                try #require(
                    await transportEventually {
                        camera.snapshot().started == 2 && received.count(1) > 0
                            && received.count(2) > 0
                    }
                )
                do {
                    try await video.streamVideo(.init(), options: options) { response in
                        for try await _ in response.messages {
                            Issue.record("third camera stream received bytes")
                        }
                    }
                    Issue.record("third concurrent camera stream succeeded")
                } catch let error as RPCError {
                    #expect(error.code == .resourceExhausted)
                }
                #expect(camera.snapshot().started == 2)
                first.cancel()
                _ = await first.result
                try #require(await transportEventually { camera.snapshot().terminated == 1 })
                let before = received.count(2)
                camera.emit()
                try #require(await transportEventually { received.count(2) > before })
                #expect(camera.snapshot().active == 1)

                let replacement = Task { try await stream(3) }
                defer { replacement.cancel() }
                try #require(
                    await transportEventually {
                        camera.snapshot().started == 3 && received.count(3) > 0
                    }
                )
                second.cancel()
                replacement.cancel()
                _ = await second.result
                _ = await replacement.result
                try #require(
                    await transportEventually {
                        camera.snapshot().terminated == 3 && camera.snapshot().active == 0
                    }
                )
                // Cancel before the first frame, while the server is suspended
                // in the capture iterator rather than a response write.
                camera.setInitialFrame(false)
                let idle = Task { try await stream(4) }
                defer { idle.cancel() }
                try #require(await transportEventually { camera.snapshot().started == 4 })
                #expect(received.count(4) == 0)
                idle.cancel()
                _ = await idle.result
                try #require(await transportEventually { camera.snapshot().terminated == 4 })

                // A producer error and client cancellation may race. Both must
                // join the producer and release exactly one reservation.
                let racing = Task { try await stream(5) }
                defer { racing.cancel() }
                try #require(await transportEventually { camera.snapshot().started == 5 })
                await withTaskGroup(of: Void.self) { race in
                    race.addTask { racing.cancel() }
                    race.addTask { camera.failAll() }
                }
                _ = await racing.result
                try #require(
                    await transportEventually {
                        camera.snapshot().terminated == 5 && camera.snapshot().active == 0
                    }
                )

                // Refill both slots and require the original limit again. This
                // also catches double-release after the error/cancel race.
                camera.setInitialFrame(true)
                let sixth = Task { try await stream(6) }
                let seventh = Task { try await stream(7) }
                defer {
                    sixth.cancel()
                    seventh.cancel()
                }
                try #require(
                    await transportEventually {
                        camera.snapshot().started == 7 && received.count(6) > 0
                            && received.count(7) > 0
                    }
                )
                do {
                    try await video.streamVideo(.init(), options: options) { response in
                        for try await _ in response.messages {
                            Issue.record("capacity grew after cancellation/error cleanup")
                        }
                    }
                    Issue.record("third stream succeeded after cancellation/error cleanup")
                } catch let error as RPCError { #expect(error.code == .resourceExhausted) }
                sixth.cancel()
                seventh.cancel()
                _ = await sixth.result
                _ = await seventh.result
                try #require(
                    await transportEventually {
                        camera.snapshot().terminated == 7 && camera.snapshot().active == 0
                    }
                )

            }
        }
    }
}

private func transportEventually(_ predicate: @escaping @Sendable () -> Bool) async -> Bool {
    for _ in 0..<300 {
        if predicate() { return true }
        if Task.isCancelled { return false }
        try? await Task.sleep(for: .milliseconds(10))
    }
    return false
}

private final class TransportCameraProbe: CameraManaging, @unchecked Sendable {
    // Explicit synthetic transport bytes, not an H264 correctness fixture.
    static let payload = Data((0..<32768).map { UInt8($0 % 251) })
    private let lock = NSLock()
    private var discoveries = 0
    private var started = 0
    private var terminated = 0
    private var initialFrame = true
    private var continuations: [UUID: AsyncThrowingStream<CameraFrame, any Error>.Continuation] =
        [:]

    func devices() async -> [CameraDeviceInfo] {
        lock.withLock { discoveries += 1 }
        return [
            CameraDeviceInfo(
                id: 0,
                uniqueID: "synthetic-transport-camera",
                name: "Transport probe",
                isExternal: false
            )
        ]
    }

    func frames(for device: CameraDeviceInfo) -> AsyncThrowingStream<CameraFrame, any Error> {
        let id = UUID()
        return AsyncThrowingStream { continuation in
            continuation.onTermination = { [weak self] _ in
                self?.lock.withLock {
                    self?.terminated += 1
                    self?.continuations[id] = nil
                }
            }
            lock.withLock {
                started += 1
                continuations[id] = continuation
            }
            if lock.withLock({ initialFrame }) {
                continuation.yield(
                    CameraFrame(annexB: Self.payload, isKeyframe: true, timestampNanoseconds: 1924)
                )
            }
        }
    }

    func snapshot() -> (discoveries: Int, started: Int, terminated: Int, active: Int) {
        lock.withLock { (discoveries, started, terminated, continuations.count) }
    }

    func emit() {
        for continuation in lock.withLock({ Array(continuations.values) }) {
            continuation.yield(
                CameraFrame(annexB: Self.payload, isKeyframe: true, timestampNanoseconds: 1924)
            )
        }
    }

    func setInitialFrame(_ value: Bool) { lock.withLock { initialFrame = value } }

    func failAll() {
        for continuation in lock.withLock({ Array(continuations.values) }) {
            continuation.finish(throwing: CameraError.accessDenied)
        }
    }

    func finishAll() {
        for continuation in lock.withLock({ Array(continuations.values) }) { continuation.finish() }
    }
}

private final class TransportFramesProbe: @unchecked Sendable {
    private let lock = NSLock()
    private var counts: [Int: Int] = [:]
    func record(_ index: Int) { lock.withLock { counts[index, default: 0] += 1 } }
    func count(_ index: Int) -> Int { lock.withLock { counts[index, default: 0] } }
}

private struct TransportCameraControls: CameraControlManaging {
    func list(uniqueID: String) async -> [UVCCameraControl] { [] }
    func set(
        uniqueID: String,
        values: [(String, Int32)],
        persist: Bool
    ) async -> [CameraControlOutcome] { [] }
    func reset(uniqueID: String, names: [String]) async -> [CameraControlOutcome] { [] }
    func restore(uniqueID: String) async {}
}

// The Mac video service deliberately registers the existing v1 contract by
// method name; no generated Swift video client exists in this package.
private struct TransportVideoClient<Transport: ClientTransport>: Sendable {
    let client: GRPCClient<Transport>

    func listVideoDevices(
        _ message: Wendy_Agent_Services_V1_ListVideoDevicesRequest,
        options: CallOptions
    ) async throws -> Wendy_Agent_Services_V1_ListVideoDevicesResponse {
        try await client.unary(
            request: ClientRequest(message: message),
            descriptor: MethodDescriptor(
                fullyQualifiedService: "wendy.agent.services.v1.WendyVideoService",
                method: "ListVideoDevices",
                type: .unary
            ),
            serializer: ProtobufSerializer<Wendy_Agent_Services_V1_ListVideoDevicesRequest>(),
            deserializer: ProtobufDeserializer<Wendy_Agent_Services_V1_ListVideoDevicesResponse>(),
            options: options
        ) { response in try response.message }
    }

    func streamVideo(
        _ message: Wendy_Agent_Services_V1_StreamVideoRequest,
        options: CallOptions,
        onResponse:
            @Sendable @escaping (StreamingClientResponse<Wendy_Agent_Services_V1_VideoFrame>)
            async throws -> Void
    ) async throws {
        try await client.serverStreaming(
            request: ClientRequest(message: message),
            descriptor: MethodDescriptor(
                fullyQualifiedService: "wendy.agent.services.v1.WendyVideoService",
                method: "StreamVideo",
                type: .serverStreaming
            ),
            serializer: ProtobufSerializer<Wendy_Agent_Services_V1_StreamVideoRequest>(),
            deserializer: ProtobufDeserializer<Wendy_Agent_Services_V1_VideoFrame>(),
            options: options,
            onResponse: onResponse
        )
    }
}
