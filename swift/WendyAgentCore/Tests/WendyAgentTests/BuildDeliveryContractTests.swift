import Crypto
import Foundation
import SwiftASN1
import Testing
import X509

@testable import WendyAgentCore

@Suite("Build delivery contract conformance")
struct BuildDeliveryContractTests {
    @Test(
        "Go ML-DSA JWS verifies with the same builder SPKI and session reference",
        arguments: ["build-delivery-go.json", "build-delivery-go-mldsa87.json"]
    )
    func goConformance(filename: String) async throws {
        guard #available(macOS 26.0, *) else { return }
        struct Vector: Decodable {
            var root_pem: String
            var target: String
            var builder_der: Data
            var authorization_jws: String
            var session: String
            var verify_at: Int64
        }
        let path = URL(fileURLWithPath: #filePath).deletingLastPathComponent()
            .appendingPathComponent(
                "../../../../go/internal/shared/builddelivery/testdata/" + filename
            )
            .standardizedFileURL
        let vector = try JSONDecoder().decode(Vector.self, from: Data(contentsOf: path))
        let verified = try await BuildDeliveryContract.verify(
            vector.authorization_jws,
            roots: ClientCertAuthorizer.parseCertificates(pem: vector.root_pem),
            target: vector.target,
            at: vector.verify_at
        )
        let builder = try Certificate(derEncoded: Array(vector.builder_der))
        let fingerprint = try BuildDeliveryContract.fingerprint(builder)
        #expect(verified.claims.builder_spki_sha256 == fingerprint)
        #expect(BuildDeliveryContract.sessionID(vector.authorization_jws) == vector.session)
        #expect(verified.claims.reference == "app:latest")
        await #expect(throws: (any Error).self) {
            try await BuildDeliveryContract.verify(
                vector.authorization_jws,
                roots: [],
                target: vector.target,
                at: vector.verify_at
            )
        }
        await #expect(throws: (any Error).self) {
            try await BuildDeliveryContract.verify(
                vector.authorization_jws,
                roots: ClientCertAuthorizer.parseCertificates(pem: vector.root_pem),
                target: vector.target,
                at: vector.verify_at + 1200
            )
        }
    }
    @Test("only 65 and 87 with matching operator keys are accepted")
    func signatureAlgorithms() async throws {
        guard #available(macOS 26.0, *) else { return }
        let keys: [(String, TestPKI.MLDSAKey)] = [
            ("ML-DSA-65", .init(variant: .mldsa65(try MLDSA65.PrivateKey()))),
            ("ML-DSA-87", .init(variant: .mldsa87(try MLDSA87.PrivateKey()))),
        ]
        for (keyAlgorithm, key) in keys {
            let fixture = try BuildDeliveryFixture(signingKey: key)
            for algorithm in ["ML-DSA-44", "ML-DSA-65", "ML-DSA-87", "ES256", "none", "ML-DSA-99"] {
                let compact = try fixture.sign(fixture.claims(), algorithm: algorithm)
                if algorithm == keyAlgorithm {
                    let verified = try await BuildDeliveryContract.verify(
                        compact,
                        roots: [fixture.root.certificate],
                        target: BuildDeliveryFixture.target,
                        at: fixture.time
                    )
                    #expect(verified.claims.iss == BuildDeliveryFixture.issuer)
                    if algorithm == "ML-DSA-87",
                        let path = ProcessInfo.processInfo.environment[
                            "WENDY_BUILD_DELIVERY_SWIFT_87_OUTPUT"
                        ]
                    {
                        let output: [String: Any] = [
                            "root_pem": fixture.root.pem, "target": BuildDeliveryFixture.target,
                            "builder_der": Data(fixture.builder.der).base64EncodedString(),
                            "authorization_jws": compact,
                            "session": BuildDeliveryContract.sessionID(compact),
                            "verify_at": fixture.time,
                        ]
                        try JSONSerialization.data(
                            withJSONObject: output,
                            options: [.prettyPrinted, .sortedKeys]
                        )
                        .write(to: URL(fileURLWithPath: path))
                    }
                } else {
                    await #expect(throws: (any Error).self) {
                        try await BuildDeliveryContract.verify(
                            compact,
                            roots: [fixture.root.certificate],
                            target: BuildDeliveryFixture.target,
                            at: fixture.time
                        )
                    }
                }
            }
        }
    }
    @Test("strict JSON refuses duplicate, aliased, fractional and unknown fields")
    func strictJSON() throws {
        struct Message: Decodable { var v: Int64 }
        for text in [
            #"{"v":1,"v":2}"#, #"{"v":1,"V":2}"#, #"{"v":1,"extra":0}"#, #"{"v":1.0}"#,
            #"{"v":1e0}"#, #"{"\u0076":1,"v":2}"#,
        ] {
            #expect(throws: (any Error).self) {
                try DeliveryStrictJSON.decode(Message.self, data: Data(text.utf8), keys: ["v"])
            }
        }
    }
    @Test(
        "existing operator certificates authorize delivery; wrong targets, signatures and device certificates fail"
    )
    func nativeSignatures() async throws {
        guard #available(macOS 26.0, *) else { return }
        let fixture = try BuildDeliveryFixture()
        let compact = try fixture.sign(fixture.claims())
        let roots = [fixture.root.certificate]
        let verified = try await BuildDeliveryContract.verify(
            compact,
            roots: roots,
            target: BuildDeliveryFixture.target,
            at: fixture.time
        )
        #expect(verified.claims.builder == BuildDeliveryFixture.builder)
        await #expect(throws: (any Error).self) {
            try await BuildDeliveryContract.verify(
                compact,
                roots: roots,
                target: BuildDeliveryFixture.builder,
                at: fixture.time
            )
        }
        let parts = compact.components(separatedBy: ".")
        var signature = try BuildDeliveryContract.decodeURL(parts[2])
        signature[0] ^= 1
        let changed = parts[0] + "." + parts[1] + "." + BuildDeliveryContract.base64URL(signature)
        await #expect(throws: (any Error).self) {
            try await BuildDeliveryContract.verify(
                changed,
                roots: roots,
                target: BuildDeliveryFixture.target,
                at: fixture.time
            )
        }
        // A trusted certificate with a device or signer identity cannot issue
        // an operator grant, even when the JWS is correctly signed by its key.
        for principal in [
            BuildDeliveryFixture.builder,
            "spiffe://wendy.sh/tenant/\(BuildDeliveryFixture.tenant)/signer/release",
        ] {
            let device = try BuildDeliveryFixture(signingPrincipal: principal)
            let forged = try device.sign(device.claims())
            await #expect(throws: (any Error).self) {
                try await BuildDeliveryContract.verify(
                    forged,
                    roots: [device.root.certificate],
                    target: BuildDeliveryFixture.target,
                    at: device.time
                )
            }
        }
        if let path = ProcessInfo.processInfo.environment["WENDY_BUILD_DELIVERY_SWIFT_OUTPUT"] {
            let output: [String: Any] = [
                "root_pem": fixture.root.pem, "target": BuildDeliveryFixture.target,
                "builder_der": Data(fixture.builder.der).base64EncodedString(),
                "authorization_jws": compact, "session": BuildDeliveryContract.sessionID(compact),
                "verify_at": fixture.time,
            ]
            try JSONSerialization.data(
                withJSONObject: output,
                options: [.prettyPrinted, .sortedKeys]
            ).write(to: URL(fileURLWithPath: path))
        }
    }
}
