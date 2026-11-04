import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import GRPCProtobuf
import Logging
import Testing
import WendyAgentGRPC
import X509

@testable import WendyAgentCore

@Suite("RPC caller identity")
struct RPCIdentityTests {
    static let tenant = "11111111-1111-4111-8111-111111111111"
    static let sensorManifest = "wendy.agent.services.v2.WendySensorService/GetSensorManifest"
    static let sensorStream = "wendy.agent.services.v2.WendySensorService/StreamSensors"
    static let management = "wendy.agent.services.v1.WendyAgentService/GetAgentVersion"

    // Independent strings keep generated descriptor changes from silently
    // changing the reviewed set of device capabilities.
    @Test func exactDeviceMethods() throws {
        let allowed = [
            "wendy.agent.services.v1.WendyContainerService/QueryChunks",
            "wendy.agent.services.v1.WendyContainerService/QueryLayers",
            "wendy.agent.services.v1.WendyContainerService/WriteChunks",
            "wendy.agent.services.v1.WendyContainerService/PrepareImage",
            "wendy.agent.services.v2.WendyMeshService/MeshDial",
            Self.sensorManifest, Self.sensorStream,
        ]
        let denied = [
            Self.management,
            "wendy.agent.services.v1.WendyContainerService/CreateContainer",
            "wendy.agent.services.v1.WendyContainerService/RunContainer",
            "wendy.agent.services.v1.WendyContainerService/ExecContainer",
            "wendy.agent.services.v1.WendyAudioService/StreamAudio",
            "wendy.agent.services.v1.WendyVideoService/StreamVideo",
            "wendy.agent.services.v1.WendySensorService/StreamSensors",
            "wendy.agent.services.v2.WendySensorService/PairSensor",
            "wendy.agent.services.v2.WendySensorService/StreamSensors/Other",
            "wendy.agent.services.v1.WendyMeshService/MeshDial",
            "future.AdminService/NewMethod",
        ]
        for method in allowed { #expect(RPCIdentityInterceptor.deviceRPCAllowed(method)) }
        for method in denied { #expect(!RPCIdentityInterceptor.deviceRPCAllowed(method)) }
    }

    @Test func rolesAndTenantMatching() throws {
        let ca = try TestPKI.makeCA()
        let scope = OrgIdentity.Scope(tenantUUID: Self.tenant, orgID: 7)
        func leaf(_ role: String, tenant: String = Self.tenant) throws -> Certificate {
            try TestPKI.makeIdentity(
                commonName: "test",
                ca: ca,
                uris: ["spiffe://wendy.sh/tenant/\(tenant)/\(role)/peer"]
            ).certificate
        }
        let device = try leaf("device")
        try RPCIdentityInterceptor.check(
            leaf: device,
            method: Self.sensorStream,
            expectedScope: scope
        )
        try RPCIdentityInterceptor.check(
            leaf: leaf("operator"),
            method: Self.management,
            expectedScope: scope
        )
        let legacy = try TestPKI.makeDeviceIdentity(org: 7, asset: 2, ca: ca)
        try RPCIdentityInterceptor.check(
            leaf: legacy.certificate,
            method: Self.sensorManifest,
            expectedScope: scope
        )
        for (cert, expected, method) in [
            (device, Optional(scope), Self.management),
            (device, nil, Self.sensorManifest),
            (device, OrgIdentity.Scope(tenantUUID: nil, orgID: 7), Self.sensorManifest),
            (
                try leaf("device", tenant: "22222222-2222-4222-8222-222222222222"), scope,
                Self.sensorManifest
            ),
            (try leaf("signer"), scope, Self.sensorManifest),
            (
                try TestPKI.makeIdentity(commonName: "no identity", ca: ca).certificate, scope,
                Self.management
            ),
            (
                try TestPKI.makeIdentity(commonName: "sh/wendy/broken", ca: ca).certificate, scope,
                Self.sensorManifest
            ),
        ] {
            do {
                try RPCIdentityInterceptor.check(
                    leaf: cert,
                    method: method,
                    expectedScope: expected
                )
                Issue.record("unexpected permission for \(method)")
            } catch let error as RPCError {
                try Self.expectIdentityDenied(error)
            }
        }
        #expect(throws: RPCError.self) {
            try RPCIdentityInterceptor.check(
                leaf: nil,
                method: Self.sensorManifest,
                expectedScope: scope
            )
        }
    }

    @Test func legacyUserGraceIsNarrow() throws {
        let ca = try TestPKI.makeCA()
        let scope = OrgIdentity.Scope(tenantUUID: Self.tenant, orgID: 7)
        let principal = "spiffe://wendy.sh/tenant/\(Self.tenant)/"
        let cases: [(String, [String], Bool)] = [
            ("wendy/user/human", [], true),
            ("wendy/user/", [], false),
            ("wendy/user/human/other", [], false),
            ("wendy/user/human:other", [], false),
            ("wendy/user/human ", [], false),
            ("wendy/user/human\n", [], false),
            ("unknown", [], false),
            ("wendy/user/human", [principal + "device/peer"], false),
            ("wendy/user/human", [principal + "service/asset-12"], false),
            ("wendy/user/human", [principal + "signer/code"], false),
            ("wendy/user/human", [principal + "admin/root"], false),
            ("wendy/user/human", ["urn:wendy:org:0:user:human"], false),
            ("wendy/user/human", [principal + "operator/human", principal + "device/peer"], false),
        ]
        for mode in [ClientCertAuthorizer.OrgEnforcementMode.off, .grace, .strict] {
            for (cn, uris, legacyUser) in cases {
                let cert = try TestPKI.makeIdentity(commonName: cn, ca: ca, uris: uris)
                for method in [Self.management, "future.AdminService/NewMethod"] {
                    do {
                        try RPCIdentityInterceptor.check(
                            leaf: cert.certificate,
                            method: method,
                            expectedScope: scope,
                            orgMode: mode
                        )
                        #expect(mode == .grace && legacyUser)
                    } catch let error as RPCError {
                        #expect(mode != .grace || !legacyUser)
                        try Self.expectIdentityDenied(error)
                    }
                }
            }
        }
    }

    @Test(.timeLimit(.minutes(1)), arguments: ["default", "grace", "strict", "off"])
    func legacyUsersOverProductionMTLS(mode: String) async throws {
        let ca = try TestPKI.makeCA()
        let source = try TestPKI.makeDeviceIdentity(org: 7, asset: 1, ca: ca)
        let legacy = try TestPKI.makeIdentity(commonName: "wendy/user/human", ca: ca)
        let device = try TestPKI.makeIdentity(
            commonName: "wendy/user/human",
            ca: ca,
            uris: ["urn:wendy:org:7:asset:2"]
        )
        let unknown = try TestPKI.makeIdentity(commonName: "unknown", ca: ca)
        let calls = RPCManagementCalls()
        let server = try WendyAgent.makeMainServer(
            services: [RPCLegacyManagement(calls: calls)],
            certs: .init(
                certPEM: source.certPEM,
                chainPEM: ca.pem,
                keyBacking: .softwarePEM(source.keyPEM),
                seKey: nil
            ),
            host: "127.0.0.1",
            port: 0,
            environment: mode == "default" ? [:] : ["WENDY_MTLS_ORG_ENFORCEMENT": mode],
            logger: Logger(label: "test.legacy-rpc")
        )
        try await withThrowingTaskGroup(of: Void.self) { group in
            group.addTask { try await server.serve() }
            defer {
                server.beginGracefulShutdown()
                group.cancelAll()
            }
            let listeningAddress = try await server.listeningAddress
            let address = try #require(listeningAddress)
            let port = try #require(address.ipv4?.port)
            for (identity, isLegacy, isDevice) in [
                (legacy, true, false), (device, false, true), (unknown, false, false),
            ] {
                let transport = try HTTP2ClientTransport.Posix(
                    target: .ipv4(address: "127.0.0.1", port: port),
                    transportSecurity: .mTLS(
                        certificateChain: [.bytes(Array(identity.certPEM.utf8), format: .pem)],
                        privateKey: .bytes(Array(identity.keyPEM.utf8), format: .pem)
                    ) {
                        $0.trustRoots = .certificates([.bytes(Array(ca.pem.utf8), format: .pem)])
                        $0.serverCertificateVerification = .noHostnameVerification
                    }
                )
                let allowed = isLegacy && (mode == "default" || mode == "grace")
                let before = await calls.count
                try await withGRPCClient(transport: transport) { client in
                    let agent = Wendy_Agent_Services_V1_WendyAgentService.Client(wrapping: client)
                    var options = CallOptions.defaults
                    options.timeout = .seconds(3)
                    do {
                        let version = try await agent.getAgentVersion(
                            .init(),
                            options: options
                        )
                        #expect(allowed)
                        #expect(version.version == "legacy-user-test")
                    } catch let error as RPCError {
                        #expect(!allowed)
                        if mode == "strict" && !isDevice {
                            #expect(error.code == .unavailable)
                        } else {
                            try Self.expectIdentityDenied(error)
                        }
                    }
                    do {
                        let count = try await agent.runContainer(
                            request: .init(producer: { _ in }),
                            options: options
                        ) { response in
                            var count = 0
                            for try await _ in response.messages { count += 1 }
                            return count
                        }
                        #expect(allowed)
                        #expect(count == 1)
                    } catch let error as RPCError {
                        #expect(!allowed)
                        if mode == "strict" && !isDevice {
                            #expect(error.code == .unavailable)
                        } else {
                            try Self.expectIdentityDenied(error)
                        }
                    }
                }
                let after = await calls.count
                #expect(after == before + (allowed ? 2 : 0))
            }
        }
    }

    static func expectIdentityDenied(_ error: RPCError) throws {
        #expect(error.code == .permissionDenied)
        let unpacked = try error.unpackGoogleRPCStatus()
        let status = try #require(unpacked)
        #expect(status.code == .permissionDenied)
        let info = try #require(status.details.first?.errorInfo)
        #expect(info.reason == "RPC_IDENTITY_DENIED")
        #expect(info.domain == "wendy.dev")
    }

    @Test func requestMetadataCannotSupplyAnIdentity() async throws {
        let interceptor = RPCIdentityInterceptor(
            expectedScope: .init(tenantUUID: Self.tenant, orgID: 7),
            logger: Logger(label: "test.rpc-metadata"),
            orgMode: .grace
        )
        let context = ServerContext(
            descriptor: .init(
                fullyQualifiedService: "wendy.agent.services.v2.WendySensorService",
                method: "GetSensorManifest"
            ),
            remotePeer: "in-process:test",
            localPeer: "in-process:test",
            cancellation: .init()
        )
        do {
            let _: StreamingServerResponse<Int> = try await interceptor.intercept(
                request: .init(single: .init(metadata: ["x-wendy-role": "user"], message: 0)),
                context: context
            ) { _, _ in
                Issue.record("unverified request reached handler")
                throw RPCError(code: .internalError, message: "unexpected handler")
            }
            Issue.record("missing transport identity was accepted")
        } catch let error as RPCError { #expect(error.code == .unauthenticated) }
    }

    @Test(.timeLimit(.minutes(1)), arguments: ["strict", "grace", "off"])
    func sensorLinkOverProductionMTLS(mode: String) async throws {
        let ca = try TestPKI.makeCA()
        let sourceID = "33333333-3333-4333-8333-333333333333"
        let consumerID = "44444444-4444-4444-8444-444444444444"
        let source = try TestPKI.makeIdentity(
            commonName: "source",
            ca: ca,
            uris: [
                "spiffe://wendy.sh/tenant/\(Self.tenant)/device/\(sourceID)",
                "urn:wendy:org:7:asset:\(sourceID)",
            ]
        )
        let consumer = try TestPKI.makeIdentity(
            commonName: "consumer",
            ca: ca,
            uris: [
                "spiffe://wendy.sh/tenant/\(Self.tenant)/device/\(consumerID)",
                "urn:wendy:org:7:asset:\(consumerID)",
            ]
        )
        let legacyConsumer = try TestPKI.makeDeviceIdentity(org: 7, asset: 2, ca: ca)
        let operatorIdentity = try TestPKI.makeIdentity(
            commonName: "operator",
            ca: ca,
            uris: ["urn:wendy:org:7:user:123"]
        )
        let server = try WendyAgent.makeMainServer(
            services: [
                SensorService(
                    audio: RPCSensorAudio(),
                    camera: RPCSensorCamera(),
                    micAuthorized: { true }
                ),
                AgentService(reportedVersion: { "rpc-policy-test" }),
            ],
            certs: .init(
                certPEM: source.certPEM,
                chainPEM: ca.pem,
                keyBacking: .softwarePEM(source.keyPEM),
                seKey: nil
            ),
            host: "127.0.0.1",
            port: 0,
            environment: mode == "grace" ? [:] : ["WENDY_MTLS_ORG_ENFORCEMENT": mode],
            logger: Logger(label: "test.rpc-identity")
        )
        try await withThrowingTaskGroup(of: Void.self) { group in
            group.addTask { try await server.serve() }
            defer {
                server.beginGracefulShutdown()
                group.cancelAll()
            }
            let listeningAddress = try await server.listeningAddress
            let address = try #require(listeningAddress)
            let port = try #require(address.ipv4?.port)
            var callers = [
                (consumer, false, true), (legacyConsumer, false, true),
                (operatorIdentity, true, true),
            ]
            if mode == "off" {
                // Even when TLS org checks are disabled, the RPC policy must
                // reject foreign devices and certificates without a caller role.
                callers.append(
                    (try TestPKI.makeDeviceIdentity(org: 8, asset: 3, ca: ca), false, false)
                )
                callers.append(
                    (try TestPKI.makeIdentity(commonName: "no identity", ca: ca), false, false)
                )
            }
            for (identity, isOperator, mayStream) in callers {
                let transport = try HTTP2ClientTransport.Posix(
                    target: .ipv4(address: "127.0.0.1", port: port),
                    transportSecurity: .mTLS(
                        certificateChain: [.bytes(Array(identity.certPEM.utf8), format: .pem)],
                        privateKey: .bytes(Array(identity.keyPEM.utf8), format: .pem)
                    ) {
                        $0.trustRoots = .certificates([.bytes(Array(ca.pem.utf8), format: .pem)])
                        $0.serverCertificateVerification = .noHostnameVerification
                    }
                )
                try await withGRPCClient(transport: transport) { client in
                    let sensors = Wendy_Agent_Services_V2_WendySensorService.Client(
                        wrapping: client
                    )
                    if !mayStream {
                        do {
                            _ = try await sensors.getSensorManifest(.init())
                            Issue.record("unapproved caller reached SensorLink")
                        } catch let error as RPCError { try Self.expectIdentityDenied(error) }
                        return
                    }
                    let manifest = try await sensors.getSensorManifest(.init())
                    #expect(Set(manifest.sensors.map(\.channelID)) == [1, 2])
                    var request = Wendy_Agent_Services_V2_StreamSensorsRequest()
                    request.channelID = [1, 2]
                    let frames = try await sensors.streamSensors(request) { response in
                        var frames: [Wendy_Lite_Sensorlink_SensorData] = []
                        for try await frame in response.messages { frames.append(frame) }
                        return frames
                    }
                    #expect(frames.count == 2)
                    #expect(
                        frames.first(where: { $0.channelID == 1 })?.payload == Data([1, 2, 3, 4])
                    )
                    #expect(
                        frames.first(where: { $0.channelID == 2 })?.payload
                            == Data([0, 0, 0, 1, 0x67])
                    )
                    let agent = Wendy_Agent_Services_V1_WendyAgentService.Client(wrapping: client)
                    if !isOperator {
                        do {
                            _ = try await agent.getAgentVersion(.init())
                            Issue.record("device reached management RPC")
                        } catch let error as RPCError { try Self.expectIdentityDenied(error) }
                        do {
                            try await agent.runContainer(request: .init(producer: { _ in })) {
                                response in
                                for try await _ in response.messages {}
                            }
                            Issue.record("device reached streaming management RPC")
                        } catch let error as RPCError { try Self.expectIdentityDenied(error) }
                    } else {
                        let version = try await agent.getAgentVersion(.init())
                        #expect(version.version == "rpc-policy-test")
                    }
                }
            }
        }
    }
}

private struct RPCSensorAudio: AudioManaging {
    func listDevices(typeFilter: AudioKind?) async throws -> [AudioDeviceInfo] { [] }
    func setDefault(deviceID: UInt32) async throws {}
    func levels(
        deviceID: UInt32,
        rateHz: UInt32
    ) -> AsyncThrowingStream<(peakDb: Float, rmsDb: Float), any Error> {
        AsyncThrowingStream { $0.finish() }
    }
    func audio(
        deviceID: UInt32,
        sampleRate: UInt32,
        channels: UInt32
    )
        -> AsyncThrowingStream<(pcm: Data, sampleRate: UInt32, channels: UInt32), any Error>
    {
        AsyncThrowingStream {
            $0.yield((Data([1, 2, 3, 4]), 48000, 1))
            $0.finish()
        }
    }
}

private struct RPCSensorCamera: CameraCapturing {
    func descriptor() -> Wendy_Lite_Sensorlink_SensorDescriptor? {
        var descriptor = Wendy_Lite_Sensorlink_SensorDescriptor()
        descriptor.channelID = 2
        descriptor.name = "cam0"
        descriptor.video.codec = .h264
        return descriptor
    }
    func frames() -> AsyncThrowingStream<CameraFrame, any Error> {
        AsyncThrowingStream {
            $0.yield(CameraFrame(annexB: Data([0, 0, 0, 1, 0x67]), isKeyframe: true))
            $0.finish()
        }
    }
}

private actor RPCManagementCalls {
    var count = 0
    func record() { count += 1 }
}

// Fake management handlers prove unary and streaming calls pass the real
// listener's policy without starting a container or touching hardware.
private struct RPCLegacyManagement: RegistrableRPCService {
    let calls: RPCManagementCalls
    func registerMethods<Transport>(with router: inout RPCRouter<Transport>)
    where Transport: ServerTransport {
        router.registerHandler(
            forMethod: Wendy_Agent_Services_V1_WendyAgentService.Method.GetAgentVersion.descriptor,
            deserializer: ProtobufDeserializer<Wendy_Agent_Services_V1_GetAgentVersionRequest>(),
            serializer: ProtobufSerializer<Wendy_Agent_Services_V1_GetAgentVersionResponse>(),
            handler: { _, _ in
                await calls.record()
                return .init { writer in
                    var response = Wendy_Agent_Services_V1_GetAgentVersionResponse()
                    response.version = "legacy-user-test"
                    try await writer.write(response)
                    return [:]
                }
            }
        )
        router.registerHandler(
            forMethod: Wendy_Agent_Services_V1_WendyAgentService.Method.RunContainer.descriptor,
            deserializer: ProtobufDeserializer<Wendy_Agent_Services_V1_RunContainerRequest>(),
            serializer: ProtobufSerializer<Wendy_Agent_Services_V1_RunContainerResponse>(),
            handler: { _, _ in
                await calls.record()
                return .init { writer in
                    try await writer.write(Wendy_Agent_Services_V1_RunContainerResponse())
                    return [:]
                }
            }
        )
    }
}
