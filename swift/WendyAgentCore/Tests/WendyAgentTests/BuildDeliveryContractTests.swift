import Foundation
import SwiftASN1
import Testing
import X509

@testable import WendyAgentCore

@Suite("Build delivery contract conformance")
struct BuildDeliveryContractTests {
    @Test("Go ML-DSA JWS verifies with the same builder SPKI and session reference")
    func goConformance() async throws {
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
                "../../../../go/internal/shared/builddelivery/testdata/build-delivery-go.json"
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
    @Test("native signed artifacts reject wrong targets, signatures and deployment denies")
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
        let denied = try BuildDeliveryFixture(rights: [
            "entitlement:deployment:*:allow",
            "entitlement:deployment:create:tag:unknown-membership:deny",
        ])
        let denial = try denied.sign(denied.claims())
        await #expect(throws: (any Error).self) {
            try await BuildDeliveryContract.verify(
                denial,
                roots: [denied.root.certificate],
                target: BuildDeliveryFixture.target,
                at: denied.time
            )
        }
        let malformed = try BuildDeliveryFixture(rights: ["entitlement:deployment:create::allow"])
        let malformedGrant = try malformed.sign(malformed.claims())
        await #expect(throws: (any Error).self) {
            try await BuildDeliveryContract.verify(
                malformedGrant,
                roots: [malformed.root.certificate],
                target: BuildDeliveryFixture.target,
                at: malformed.time
            )
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
