import Crypto
import Foundation
import SwiftASN1
import Testing
import X509

@testable import WendyAgentCore

@Suite("ClientCertAuthorizer")
struct ClientCertAuthorizerTests {
    /// A self-signed CA and its private key, for issuing leaf certificates.
    private struct TestCA {
        var certificate: Certificate
        var privateKey: Certificate.PrivateKey
        var pem: String
    }

    private static func makeCA(commonName: String = "Wendy Test CA") throws -> TestCA {
        let key = P256.Signing.PrivateKey()
        let name = try DistinguishedName { CommonName(commonName) }
        let cert = try Certificate(
            version: .v3,
            serialNumber: Certificate.SerialNumber(),
            publicKey: Certificate.PublicKey(key.publicKey),
            notValidBefore: Date().addingTimeInterval(-3600),
            notValidAfter: Date().addingTimeInterval(3600),
            issuer: name,
            subject: name,
            signatureAlgorithm: .ecdsaWithSHA256,
            extensions: try Certificate.Extensions {
                Critical(BasicConstraints.isCertificateAuthority(maxPathLength: nil))
                Critical(KeyUsage(keyCertSign: true))
            },
            issuerPrivateKey: Certificate.PrivateKey(key)
        )
        return TestCA(
            certificate: cert,
            privateKey: Certificate.PrivateKey(key),
            pem: try cert.serializeAsPEM().pemString
        )
    }

    private static func makeLeaf(org: Int32, asset: Int32, ca: TestCA) throws -> Certificate {
        let key = P256.Signing.PrivateKey()
        let subject = try DistinguishedName {
            CommonName(DeviceIdentity.commonName(organizationID: org, assetID: asset))
        }
        return try Certificate(
            version: .v3,
            serialNumber: Certificate.SerialNumber(),
            publicKey: Certificate.PublicKey(key.publicKey),
            notValidBefore: Date().addingTimeInterval(-3600),
            notValidAfter: Date().addingTimeInterval(3600),
            issuer: ca.certificate.subject,
            subject: subject,
            signatureAlgorithm: .ecdsaWithSHA256,
            extensions: try Certificate.Extensions {
                Critical(BasicConstraints.notCertificateAuthority)
                Critical(KeyUsage(digitalSignature: true))
                try ExtendedKeyUsage([.clientAuth, .serverAuth])
            },
            issuerPrivateKey: ca.privateKey
        )
    }

    /// A leaf with the `wendy` CLI's user-cert shape: CommonName
    /// `wendy/user/<uid>` and, optionally, an authoritative
    /// `urn:wendy:org:<org>:user:<uid>` SAN URI. When `org` is nil the cert
    /// carries no org identity at all (today's shipping CLI cert).
    private static func makeUserLeaf(
        uid: String,
        org: Int32?,
        ca: TestCA
    ) throws -> Certificate {
        let key = P256.Signing.PrivateKey()
        let subject = try DistinguishedName {
            CommonName("wendy/user/\(uid)")
        }
        return try Certificate(
            version: .v3,
            serialNumber: Certificate.SerialNumber(),
            publicKey: Certificate.PublicKey(key.publicKey),
            notValidBefore: Date().addingTimeInterval(-3600),
            notValidAfter: Date().addingTimeInterval(3600),
            issuer: ca.certificate.subject,
            subject: subject,
            signatureAlgorithm: .ecdsaWithSHA256,
            extensions: try Certificate.Extensions {
                Critical(BasicConstraints.notCertificateAuthority)
                Critical(KeyUsage(digitalSignature: true))
                try ExtendedKeyUsage([.clientAuth, .serverAuth])
                if let org {
                    SubjectAlternativeNames([
                        .uniformResourceIdentifier("urn:wendy:org:\(org):user:\(uid)")
                    ])
                }
            },
            issuerPrivateKey: ca.privateKey
        )
    }

    private static func der(_ certificate: Certificate) throws -> [UInt8] {
        var serializer = DER.Serializer()
        try serializer.serialize(certificate)
        return serializer.serializedBytes
    }

    // MARK: - Client (user) certificates

    @Test("grace (default) accepts a CA-signed user cert that carries no org claim")
    func graceAcceptsNoOrgUserCert() async throws {
        // Reproduces the field bug: the shipping `wendy` CLI cert has CN
        // `wendy/user/<uid>` and no org identity, yet must connect to a
        // provisioned device.
        let ca = try Self.makeCA()
        let client = try Self.makeUserLeaf(uid: "abc", org: nil, ca: ca)

        let authorized = await ClientCertAuthorizer.isAuthorized(
            peerCertificatesDER: [try Self.der(client)],
            trustRootsPEM: ca.pem,
            deviceScope: .init(tenantUUID: nil, orgID: 7)
        )
        #expect(authorized)
    }

    @Test("strict rejects a CA-signed user cert that carries no org claim")
    func strictRejectsNoOrgUserCert() async throws {
        let ca = try Self.makeCA()
        let client = try Self.makeUserLeaf(uid: "abc", org: nil, ca: ca)

        let authorized = await ClientCertAuthorizer.isAuthorized(
            peerCertificatesDER: [try Self.der(client)],
            trustRootsPEM: ca.pem,
            deviceScope: .init(tenantUUID: nil, orgID: 7),
            mode: .strict
        )
        #expect(!authorized)
    }

    @Test("accepts a user cert whose org URN matches the device org")
    func acceptsMatchingOrgURN() async throws {
        let ca = try Self.makeCA()
        let client = try Self.makeUserLeaf(uid: "abc", org: 7, ca: ca)

        for mode in [ClientCertAuthorizer.OrgEnforcementMode.grace, .strict] {
            let authorized = await ClientCertAuthorizer.isAuthorized(
                peerCertificatesDER: [try Self.der(client)],
                trustRootsPEM: ca.pem,
                deviceScope: .init(tenantUUID: nil, orgID: 7),
                mode: mode
            )
            #expect(authorized, "mode \(mode.name) should accept a matching org URN")
        }
    }

    @Test("rejects a user cert whose org URN differs from the device org")
    func rejectsMismatchedOrgURN() async throws {
        let ca = try Self.makeCA()
        let client = try Self.makeUserLeaf(uid: "abc", org: 9, ca: ca)

        for mode in [ClientCertAuthorizer.OrgEnforcementMode.grace, .strict] {
            let authorized = await ClientCertAuthorizer.isAuthorized(
                peerCertificatesDER: [try Self.der(client)],
                trustRootsPEM: ca.pem,
                deviceScope: .init(tenantUUID: nil, orgID: 7),
                mode: mode
            )
            #expect(!authorized, "mode \(mode.name) must reject a mismatched org URN")
        }
    }

    @Test("off mode accepts any CA-signed client regardless of org")
    func offModeSkipsOrgCheck() async throws {
        let ca = try Self.makeCA()
        let noOrg = try Self.makeUserLeaf(uid: "abc", org: nil, ca: ca)
        let mismatched = try Self.makeUserLeaf(uid: "abc", org: 9, ca: ca)

        for client in [noOrg, mismatched] {
            let authorized = await ClientCertAuthorizer.isAuthorized(
                peerCertificatesDER: [try Self.der(client)],
                trustRootsPEM: ca.pem,
                deviceScope: .init(tenantUUID: nil, orgID: 7),
                mode: .off
            )
            #expect(authorized)
        }
    }

    @Test("off mode still rejects a client not signed by the device CA")
    func offModeStillRequiresValidChain() async throws {
        let deviceCA = try Self.makeCA(commonName: "Wendy Device CA")
        let rogueCA = try Self.makeCA(commonName: "Rogue CA")
        let client = try Self.makeUserLeaf(uid: "abc", org: nil, ca: rogueCA)

        let authorized = await ClientCertAuthorizer.isAuthorized(
            peerCertificatesDER: [try Self.der(client)],
            trustRootsPEM: deviceCA.pem,
            deviceScope: .init(tenantUUID: nil, orgID: 7),
            mode: .off
        )
        #expect(!authorized)
    }

    @Test("device org unknown rejects even a no-org user cert under grace")
    func graceStillFailsClosedWhenDeviceOrgUnknown() async throws {
        let ca = try Self.makeCA()
        let client = try Self.makeUserLeaf(uid: "abc", org: nil, ca: ca)

        let authorized = await ClientCertAuthorizer.isAuthorized(
            peerCertificatesDER: [try Self.der(client)],
            trustRootsPEM: ca.pem,
            deviceScope: nil
        )
        #expect(!authorized)
    }

    @Test("accepts a CA-signed client whose org matches the device org")
    func acceptsMatchingOrg() async throws {
        let ca = try Self.makeCA()
        let client = try Self.makeLeaf(org: 7, asset: 42, ca: ca)

        let authorized = await ClientCertAuthorizer.isAuthorized(
            peerCertificatesDER: [try Self.der(client)],
            trustRootsPEM: ca.pem,
            deviceScope: .init(tenantUUID: nil, orgID: 7)
        )
        #expect(authorized)
    }

    @Test("rejects a CA-signed client whose org differs from the device org")
    func rejectsMismatchedOrg() async throws {
        let ca = try Self.makeCA()
        let client = try Self.makeLeaf(org: 99, asset: 1, ca: ca)

        let authorized = await ClientCertAuthorizer.isAuthorized(
            peerCertificatesDER: [try Self.der(client)],
            trustRootsPEM: ca.pem,
            deviceScope: .init(tenantUUID: nil, orgID: 7)
        )
        #expect(!authorized)
    }

    @Test("rejects a client not signed by the device CA even if the org matches")
    func rejectsUntrustedChain() async throws {
        let deviceCA = try Self.makeCA(commonName: "Wendy Device CA")
        let rogueCA = try Self.makeCA(commonName: "Rogue CA")
        // Same org number, but signed by a CA the device does not trust.
        let client = try Self.makeLeaf(org: 7, asset: 42, ca: rogueCA)

        let authorized = await ClientCertAuthorizer.isAuthorized(
            peerCertificatesDER: [try Self.der(client)],
            trustRootsPEM: deviceCA.pem,
            deviceScope: .init(tenantUUID: nil, orgID: 7)
        )
        #expect(!authorized)
    }

    @Test("rejects a client presenting a rogue CA as its own intermediate")
    func rejectsRogueIntermediateAsTrustAnchor() async throws {
        let deviceCA = try Self.makeCA(commonName: "Wendy Device CA")
        let rogueCA = try Self.makeCA(commonName: "Rogue CA")
        // Same org number as the device would expect, but the chain is rooted at
        // a CA the device does not trust — presented as a peer intermediate
        // rather than only a leaf, to prove the rogue CA can't smuggle itself in
        // as a terminal trust anchor.
        let client = try Self.makeLeaf(org: 7, asset: 42, ca: rogueCA)

        let authorized = await ClientCertAuthorizer.isAuthorized(
            peerCertificatesDER: [try Self.der(client), try Self.der(rogueCA.certificate)],
            trustRootsPEM: deviceCA.pem,
            deviceScope: .init(tenantUUID: nil, orgID: 7)
        )
        #expect(!authorized)
    }

    @Test("with device org unknown, rejects every client (fail closed)")
    func orgUnknownFailsClosed() async throws {
        let ca = try Self.makeCA()
        let rogueCA = try Self.makeCA(commonName: "Rogue CA")
        let trusted = try Self.makeLeaf(org: 3, asset: 9, ca: ca)
        let untrusted = try Self.makeLeaf(org: 3, asset: 9, ca: rogueCA)

        // Even a client with a chain that verifies to a trusted root is
        // rejected when the device cannot determine its own org: org-equality
        // is the sole cross-org barrier and is never silently dropped.
        let rejectsTrusted = await ClientCertAuthorizer.isAuthorized(
            peerCertificatesDER: [try Self.der(trusted)],
            trustRootsPEM: ca.pem,
            deviceScope: nil
        )
        #expect(!rejectsTrusted)

        let rejectsUntrusted = await ClientCertAuthorizer.isAuthorized(
            peerCertificatesDER: [try Self.der(untrusted)],
            trustRootsPEM: ca.pem,
            deviceScope: nil
        )
        #expect(!rejectsUntrusted)
    }

    @Test("rejects an empty peer chain")
    func rejectsEmptyPeerChain() async throws {
        let ca = try Self.makeCA()
        let authorized = await ClientCertAuthorizer.isAuthorized(
            peerCertificatesDER: [],
            trustRootsPEM: ca.pem,
            deviceScope: .init(tenantUUID: nil, orgID: 7)
        )
        #expect(!authorized)
    }

    @Test("rejects when there are no trust roots")
    func rejectsNoTrustRoots() async throws {
        let ca = try Self.makeCA()
        let client = try Self.makeLeaf(org: 7, asset: 42, ca: ca)
        let authorized = await ClientCertAuthorizer.isAuthorized(
            peerCertificatesDER: [try Self.der(client)],
            trustRootsPEM: "",
            deviceScope: .init(tenantUUID: nil, orgID: 7)
        )
        #expect(!authorized)
    }

    @Test("derives the device org from its own leaf PEM")
    func derivesDeviceOrgFromLeafPEM() throws {
        let ca = try Self.makeCA()
        let leaf = try Self.makeLeaf(org: 12, asset: 34, ca: ca)
        let pem = try leaf.serializeAsPEM().pemString
        #expect(ClientCertAuthorizer.scope(fromLeafPEM: pem) == .init(tenantUUID: nil, orgID: 12))
    }
}
