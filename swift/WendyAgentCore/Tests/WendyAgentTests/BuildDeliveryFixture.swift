import Crypto
import Foundation
import SwiftASN1
import X509

@testable import WendyAgentCore

@available(macOS 26.0, *)
struct BuildDeliveryFixture: Sendable {
    static let tenant = "11111111-1111-4111-8111-111111111111"
    static let target =
        "spiffe://wendy.sh/tenant/\(tenant)/device/22222222-2222-4222-8222-222222222222"
    static let builder =
        "spiffe://wendy.sh/tenant/\(tenant)/device/33333333-3333-4333-8333-333333333333"
    static let issuer = "spiffe://wendy.sh/tenant/\(tenant)/operator/alice"
    let root: TestPKI.CA
    let target: TestPKI.Identity
    let builder: TestPKI.Identity
    let otherBuilder: TestPKI.Identity
    let primary: TestPKI.Identity
    let operatorCertificate: Certificate
    let operatorKey: TestPKI.MLDSAKey
    let time: Int64
    init(
        operatorLifetime: TimeInterval = 300,
        rights: [String]? = nil,
        signingKey: TestPKI.MLDSAKey? = nil
    ) throws {
        time = Int64(Date().timeIntervalSince1970)
        root = try TestPKI.makeCA()
        target = try TestPKI.makeIdentity(commonName: "target", ca: root, uris: [Self.target])
        builder = try TestPKI.makeIdentity(commonName: "builder", ca: root, uris: [Self.builder])
        otherBuilder = try TestPKI.makeIdentity(
            commonName: "rekeyed builder",
            ca: root,
            uris: [Self.builder]
        )
        primary = try TestPKI.makeIdentity(
            commonName: "primary operator",
            ca: root,
            uris: [Self.issuer]
        )
        operatorKey = try signingKey ?? TestPKI.MLDSAKey(variant: .mldsa65(MLDSA65.PrivateKey()))
        let name = try DistinguishedName { CommonName("synthetic entitled operator") }
        var serializer = DER.Serializer()
        try serializer.appendConstructedNode(identifier: .sequence) { coder in
            for right in rights ?? [
                "entitlement:deployment:create:device:22222222-2222-4222-8222-222222222222:allow"
            ] {
                try coder.serialize(ASN1UTF8String(contentBytes: ArraySlice(right.utf8)))
            }
        }
        operatorCertificate = try Certificate(
            version: .v3,
            serialNumber: Certificate.SerialNumber(),
            publicKey: operatorKey.publicKey,
            notValidBefore: Date(timeIntervalSince1970: Double(time - 60)),
            notValidAfter: Date(timeIntervalSince1970: Double(time) + operatorLifetime),
            issuer: root.certificate.subject,
            subject: name,
            signatureAlgorithm: .ecdsaWithSHA256,
            extensions: try Certificate.Extensions {
                Critical(BasicConstraints.notCertificateAuthority)
                Critical(KeyUsage(digitalSignature: true))
                try ExtendedKeyUsage([.clientAuth])
                SubjectAlternativeNames([.uniformResourceIdentifier(Self.issuer)])
                Certificate.Extension(
                    oid: BuildDeliveryContract.entitlementOID,
                    critical: false,
                    value: ArraySlice(serializer.serializedBytes)
                )
            },
            issuerPrivateKey: root.privateKey
        )
    }
    func claims(
        reference: String = "app:latest",
        expected: String = "",
        budget: Int64 = 1 << 28
    ) throws -> BuildDeliveryContract.Claims {
        BuildDeliveryContract.Claims(
            v: 1,
            jti: UUID().uuidString.lowercased(),
            iss: Self.issuer,
            aud: Self.target,
            builder: Self.builder,
            builder_spki_sha256: try BuildDeliveryContract.fingerprint(builder.certificate),
            reference: reference,
            input_sha256: BuildDeliveryContract.base64URL([UInt8](repeating: 0, count: 32)),
            expected_digest: expected,
            iat: time,
            admit_before: min(
                time + 60,
                Int64(operatorCertificate.notValidAfter.timeIntervalSince1970)
            ),
            exp: time + 3600,
            operations: ["stage", "publish"],
            max_bytes: budget
        )
    }
    func sign(_ claims: BuildDeliveryContract.Claims, algorithm: String? = nil) throws -> String {
        let keyAlgorithm: String
        switch operatorKey.variant {
        case .mldsa65: keyAlgorithm = "ML-DSA-65"
        case .mldsa87: keyAlgorithm = "ML-DSA-87"
        }
        var serializer = DER.Serializer()
        try serializer.serialize(operatorCertificate)
        let header = try JSONSerialization.data(withJSONObject: [
            "alg": algorithm ?? keyAlgorithm, "typ": "wendy-build-delivery+jws",
            "x5c": [Data(serializer.serializedBytes).base64EncodedString()],
        ])
        let input =
            BuildDeliveryContract.base64URL(header) + "."
            + BuildDeliveryContract.base64URL(try JSONEncoder().encode(claims))
        let signature: Data
        switch operatorKey.variant {
        case .mldsa65(let key): signature = try key.signature(for: Data(input.utf8))
        case .mldsa87(let key): signature = try key.signature(for: Data(input.utf8))
        }
        return input + "." + BuildDeliveryContract.base64URL(signature)
    }
    func authority(
        root directory: URL,
        images: BlobStore,
        now: (@Sendable () -> Int64)? = nil
    ) async throws -> BuildDeliveryAuthority {
        let authority = BuildDeliveryAuthority(
            directory: directory,
            images: images,
            now: now ?? { Int64(Date().timeIntervalSince1970) }
        )
        try await authority.configure(certPEM: target.certPEM, chainPEM: root.pem, orgMode: .strict)
        return authority
    }
}
