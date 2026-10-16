import Crypto
import Foundation
import GRPCCore
import SwiftASN1
import SwiftProtobuf
import Testing
import WendyAgentGRPC
import X509

@testable import WendyAgentCore

/// Replays Proto/conformance/agent-provisioning-v2/contract.json, which the Go
/// agent's provisioning_contract_test.go replays too: the CLI's enrollment
/// preflight and RPCs get the same answers from either agent.
@Suite("Provisioning v2 contract")
struct ProvisioningContractTests {
    private static let principal =
        "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/sim"

    @Test("Swift agent answers exactly as the Go agent")
    func replay() async throws {
        let url = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()  // WendyAgentTests
            .deletingLastPathComponent()  // Tests
            .deletingLastPathComponent()  // WendyAgentCore
            .deletingLastPathComponent()  // swift
            .deletingLastPathComponent()  // repository root
            .appendingPathComponent("Proto/conformance/agent-provisioning-v2/contract.json")
        let contract =
            try JSONSerialization.jsonObject(with: Data(contentsOf: url)) as? [String: Any]
        let steps = try #require(contract?["steps"] as? [[String: Any]])

        let ca = try TestPKI.makeCA()
        let leaf = try TestPKI.makeIdentity(commonName: "sim", ca: ca)
        let dir = Self.tempDir()
        defer { try? FileManager.default.removeItem(at: dir) }
        let v1 = ProvisioningService(
            configPath: dir,
            acmeEnroll: { _, _, _, _ in (leaf.certPEM, ca.pem) },
            isSecureEnclaveAvailable: { false },
            acmeEnrollmentUnavailableReason: { nil }
        )
        let v2 = ProvisioningServiceV2(v1: v1)
        let context = Self.context()

        for step in steps {
            switch step["method"] as? String {
            case "IsProvisioned":
                try await Self.check(step) { (r: Wendy_Agent_Services_V2_IsProvisionedRequest) in
                    try await v2.isProvisioned(request: r, context: context)
                }
            case "StartACMEProvisioning":
                try await Self.check(step) {
                    (r: Wendy_Agent_Services_V2_StartACMEProvisioningRequest) in
                    try await v2.startACMEProvisioning(request: r, context: context)
                }
            case "Unprovision":
                try await Self.check(step) { (r: Wendy_Agent_Services_V2_UnprovisionRequest) in
                    try await v2.unprovision(request: r, context: context)
                }
            case "V1Unprovision":
                try await Self.check(step) { (r: Wendy_Agent_Services_V1_UnprovisionRequest) in
                    try await v1.unprovision(request: r, context: context)
                }
            case "RevokeACMECertificate":
                try await Self.check(step) {
                    (r: Wendy_Agent_Services_V2_RevokeACMECertificateRequest) in
                    try await v2.revokeACMECertificate(request: r, context: context)
                }
            case "CheckACMERevocation":
                try await Self.check(step) {
                    (r: Wendy_Agent_Services_V2_RevokeACMECertificateRequest) in
                    try await v2.checkACMERevocation(request: r, context: context)
                }
            case let method:
                Issue.record("unknown contract method \(method ?? "nil")")
            }
        }

        // The identity survives a restart, and the one-time EAB never reaches disk.
        let state = try String(
            contentsOf: dir.appendingPathComponent("provisioning.json"),
            encoding: .utf8
        )
        #expect(!state.contains("eab-id") && !state.contains("abababab"))
        let reloaded = ProvisioningService(configPath: dir, isSecureEnclaveAvailable: { false })
        #expect(await reloaded.provisioningInfo().principalURI == Self.principal)
    }

    @Test("direct PKI device key is P-256: pki-core's audited tier A/B fallback until WDY-3620")
    func deviceKeyAlgorithm() async throws {
        let ca = try TestPKI.makeCA()
        let leaf = try TestPKI.makeIdentity(commonName: "sim", ca: ca)
        let dir = Self.tempDir()
        defer { try? FileManager.default.removeItem(at: dir) }
        let seen = Captured()
        let service = ProvisioningService(
            configPath: dir,
            acmeEnroll: { _, _, csrDER, publicKey in
                await seen.set(csrDER, publicKey)
                return (leaf.certPEM, ca.pem)
            },
            isSecureEnclaveAvailable: { false },
            acmeEnrollmentUnavailableReason: { nil }
        )
        _ = try await service.startACMEProvisioning(
            cloudHost: "api.dev.wendy.sh:443",
            config: Self.config
        )

        let (csrDER, publicKey) = try #require(await seen.value)
        let csr = try CertificateSigningRequest(derEncoded: csrDER)
        #expect(csr.publicKey == publicKey)
        #expect(P256.Signing.PublicKey(publicKey) != nil)
        #expect(csr.signatureAlgorithm == .ecdsaWithSHA256)
    }

    // Runs on every host, deliberately unconditional: on the macos-15 CI runner it
    // exercises the refusal, on macOS 26+ (and Linux) the supported branch.
    @Test("this host advertises direct PKI enrollment only from macOS 26, and says why otherwise")
    func hostEnrollmentFloor() async throws {
        let dir = Self.tempDir()
        defer { try? FileManager.default.removeItem(at: dir) }
        let v2 = ProvisioningServiceV2(
            v1: ProvisioningService(configPath: dir, isSecureEnclaveAvailable: { false })
        )

        let status = try await v2.isProvisioned(
            request: Wendy_Agent_Services_V2_IsProvisionedRequest(),
            context: Self.context()
        )

        let notProvisioned = try #require(status.notProvisioned)
        #expect(notProvisioned.hasAcmeEnrollmentSupported)
        #expect(notProvisioned.acmeEnrollmentSupported == TestPKI.mldsaAvailable)
        if TestPKI.mldsaAvailable {
            #expect(notProvisioned.acmeEnrollmentUnavailableReason.isEmpty)
        } else {
            #expect(
                notProvisioned.acmeEnrollmentUnavailableReason.contains("needs macOS 26 or later")
            )
        }
    }

    @Test("a host below the floor refuses enrollment before PKI is contacted")
    func belowFloorRefusesBeforePKI() async throws {
        let dir = Self.tempDir()
        defer { try? FileManager.default.removeItem(at: dir) }
        let reason =
            "direct PKI enrollment needs macOS 26 or later to verify ML-DSA certificate chains; this Mac runs macOS 15.5"
        let service = ProvisioningService(
            configPath: dir,
            acmeEnroll: { _, _, _, _ in
                Issue.record("PKI must not be contacted below the floor")
                return ("", "")
            },
            isSecureEnclaveAvailable: { false },
            acmeEnrollmentUnavailableReason: { reason }
        )
        let v2 = ProvisioningServiceV2(v1: service)

        let status = try await v2.isProvisioned(
            request: Wendy_Agent_Services_V2_IsProvisionedRequest(),
            context: Self.context()
        )
        #expect(status.notProvisioned.acmeEnrollmentSupported == false)
        #expect(status.notProvisioned.acmeEnrollmentUnavailableReason == reason)

        do {
            _ = try await service.startACMEProvisioning(
                cloudHost: "api.dev.wendy.sh:443",
                config: Self.config
            )
            Issue.record("enrollment below the floor must be refused")
        } catch let error as RPCError {
            #expect(error.code == .failedPrecondition)
            #expect(error.message == reason)
        }
        #expect(
            !FileManager.default.fileExists(
                atPath: dir.appendingPathComponent("acme-account-key.pem.d").path
            )
        )
    }

    @Test(
        "a pki-core ML-DSA chain loads into the mTLS stack and the enrollment persists",
        .enabled(if: TestPKI.mldsaAvailable, "ML-DSA verification needs macOS 26 on Darwin")
    )
    func mldsaChainPersisted() async throws {
        guard
            #available(macOS 26.0, iOS 26.0, watchOS 26.0, tvOS 26.0, macCatalyst 26.0,
            visionOS 26.0, *)
        else {
            return
        }
        let chain = try TestPKI.makeMLDSAChain()
        let leaf = try TestPKI.makeIdentity(commonName: "sim", ca: TestPKI.makeCA())
        let dir = Self.tempDir()
        defer { try? FileManager.default.removeItem(at: dir) }
        let service = ProvisioningService(
            configPath: dir,
            acmeEnroll: { _, _, _, _ in (leaf.certPEM, chain.pem) },
            isSecureEnclaveAvailable: { false },
            acmeEnrollmentUnavailableReason: { nil }
        )

        _ = try await service.startACMEProvisioning(
            cloudHost: "api.dev.wendy.sh:443",
            config: Self.config
        )

        #expect(await service.provisioningCerts()?.chainPEM == chain.pem)
    }

    @Test("a chain the mTLS stack cannot load is refused and nothing is persisted")
    func unloadableChainRefused() async throws {
        let leaf = try TestPKI.makeIdentity(commonName: "sim", ca: TestPKI.makeCA())
        let dir = Self.tempDir()
        defer { try? FileManager.default.removeItem(at: dir) }
        let service = ProvisioningService(
            configPath: dir,
            acmeEnroll: { _, _, _, _ in
                (leaf.certPEM, "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n")
            },
            isSecureEnclaveAvailable: { false },
            acmeEnrollmentUnavailableReason: { nil }
        )
        await #expect(throws: RPCError.self) {
            _ = try await service.startACMEProvisioning(
                cloudHost: "api.dev.wendy.sh:443",
                config: Self.config
            )
        }
        #expect(await service.provisioningInfo().enrolled == false)
        #expect(
            !FileManager.default.fileExists(
                atPath: dir.appendingPathComponent("provisioning.json").path
            )
        )
    }

    // MARK: - Helpers

    private static let config = ACMEEnrollmentConfig(
        directoryURL:
            "https://acme.dev.pki.wendy.sh/11111111-1111-4111-8111-111111111111/acme/directory",
        deviceID: "sim",
        eabKeyID: "eab-id",
        eabHMACKey: String(repeating: "ab", count: 32)
    )

    private static let codes: [String: RPCError.Code] = [
        "INVALID_ARGUMENT": .invalidArgument,
        "FAILED_PRECONDITION": .failedPrecondition,
        "UNIMPLEMENTED": .unimplemented,
    ]

    private static func check<Request: SwiftProtobuf.Message, Response: SwiftProtobuf.Message>(
        _ step: [String: Any],
        _ call: (Request) async throws -> Response
    ) async throws {
        let name = step["name"] as? String ?? "?"
        let code = step["code"] as? String ?? ""
        let request = try Request(
            jsonUTF8Data: JSONSerialization.data(withJSONObject: step["request"] ?? [String: Any]())
        )
        do {
            let got = try await call(request)
            #expect(code == "OK", "\(name): succeeded, want \(code)")
            if let response = step["response"] {
                let want = try Response(
                    jsonUTF8Data: JSONSerialization.data(withJSONObject: response)
                )
                #expect(got.isEqualTo(message: want), "\(name): got \(got)")
            }
        } catch let error as RPCError {
            #expect(Self.codes[code] == error.code, "\(name): got \(error.code) \(error.message)")
            if let message = step["message"] as? String {
                #expect(error.message == message, "\(name)")
            }
        }
    }

    private static func tempDir() -> URL {
        FileManager.default.temporaryDirectory
            .appendingPathComponent("wendy-provcontract-\(UUID().uuidString)", isDirectory: true)
    }

    private static func context() -> ServerContext {
        ServerContext(
            descriptor: MethodDescriptor(
                fullyQualifiedService: "wendy.agent.services.v2.WendyProvisioningService",
                method: "Contract"
            ),
            remotePeer: "in-process:test",
            localPeer: "in-process:test",
            cancellation: .init()
        )
    }
}

private actor Captured {
    var value: ([UInt8], Certificate.PublicKey)?
    func set(_ csr: [UInt8], _ key: Certificate.PublicKey) { self.value = (csr, key) }
}
