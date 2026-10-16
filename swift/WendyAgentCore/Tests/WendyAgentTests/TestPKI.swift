import Crypto
import Foundation
import SwiftASN1
import X509

@testable import WendyAgentCore

/// Shared PKI-minting helpers for TLS/mTLS tests: a self-signed CA plus
/// CA-signed identities carrying their private keys (unlike the
/// ClientCertAuthorizer suite's cert-only helpers, TLS handshakes need the
/// keys too).
enum TestPKI {
    struct CA {
        var certificate: Certificate
        var privateKey: Certificate.PrivateKey
        var pem: String
    }

    /// A leaf identity with its key material in the shapes the registry TLS
    /// plumbing consumes.
    struct Identity {
        var certificate: Certificate
        var certPEM: String
        var keyPEM: String
        var der: [UInt8]
    }

    static func makeCA(commonName: String = "Wendy Test CA") throws -> CA {
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
        return CA(
            certificate: cert,
            privateKey: Certificate.PrivateKey(key),
            pem: try cert.serializeAsPEM().pemString
        )
    }

    /// A CA-signed identity whose CommonName is the Wendy device identity for
    /// (org, asset) — parseable by `ClientCertAuthorizer.organizationID` — with
    /// the clientAuth+serverAuth EKUs real device certs carry.
    static func makeDeviceIdentity(org: Int32, asset: Int32, ca: CA) throws -> Identity {
        try makeIdentity(
            commonName: DeviceIdentity.commonName(organizationID: org, assetID: asset),
            ca: ca
        )
    }

    /// Whether this platform can verify ML-DSA certificates: swift-certificates
    /// does so through CryptoKit on Darwin, which has ML-DSA from macOS 26.
    static var mldsaAvailable: Bool {
        if #available(macOS 26.0, iOS 26.0, watchOS 26.0, tvOS 26.0, macCatalyst 26.0,
        visionOS 26.0, *) {
            return true
        }
        return false
    }

    /// A pki-core-shaped chain: an ML-DSA-87 root signing an ML-DSA-65 tenant CA,
    /// which issues leaves. `pem` is the tenant CA followed by the root, the order
    /// pki-core returns below a leaf; `signatureAlgorithm` is what leaves carry.
    @available(macOS 26.0, iOS 26.0, watchOS 26.0, tvOS 26.0, macCatalyst 26.0, visionOS 26.0, *)
    static func makeMLDSAChain() throws -> CA {
        let rootKey = MLDSAKey(variant: .mldsa87(try MLDSA87.PrivateKey()))
        let tenantKey = MLDSAKey(variant: .mldsa65(try MLDSA65.PrivateKey()))
        let rootName = try DistinguishedName { CommonName("Wendy Test Root (ML-DSA-87)") }
        let tenantName = try DistinguishedName { CommonName("Wendy Test Tenant CA (ML-DSA-65)") }
        func ca(
            _ key: MLDSAKey,
            _ subject: DistinguishedName,
            by issuer: MLDSAKey,
            _ issuerName: DistinguishedName
        )
            throws -> Certificate
        {
            try Certificate(
                version: .v3,
                serialNumber: Certificate.SerialNumber(),
                publicKey: key.publicKey,
                notValidBefore: Date().addingTimeInterval(-3600),
                notValidAfter: Date().addingTimeInterval(3600),
                issuer: issuerName,
                subject: subject,
                signatureAlgorithm: issuer.defaultSignatureAlgorithm,
                extensions: try Certificate.Extensions {
                    Critical(BasicConstraints.isCertificateAuthority(maxPathLength: nil))
                    Critical(KeyUsage(keyCertSign: true))
                },
                issuerPrivateKey: Certificate.PrivateKey(issuer)
            )
        }
        let root = try ca(rootKey, rootName, by: rootKey, rootName)
        let tenant = try ca(tenantKey, tenantName, by: rootKey, rootName)
        return CA(
            certificate: tenant,
            privateKey: Certificate.PrivateKey(tenantKey),
            pem: try tenant.serializeAsPEM().pemString + "\n" + root.serializeAsPEM().pemString
        )
    }

    /// Test-only ML-DSA signer: swift-certificates verifies ML-DSA but has no
    /// ML-DSA private key type of its own.
    @available(macOS 26.0, iOS 26.0, watchOS 26.0, tvOS 26.0, macCatalyst 26.0, visionOS 26.0, *)
    struct MLDSAKey: CustomPrivateKey {
        enum Variant: @unchecked Sendable {
            case mldsa65(MLDSA65.PrivateKey)
            case mldsa87(MLDSA87.PrivateKey)
        }
        let variant: Variant
        private let id = UUID()

        var publicKey: Certificate.PublicKey {
            switch self.variant {
            case .mldsa65(let key): Certificate.PublicKey(key.publicKey)
            case .mldsa87(let key): Certificate.PublicKey(key.publicKey)
            }
        }

        var defaultSignatureAlgorithm: Certificate.SignatureAlgorithm {
            switch self.variant {
            case .mldsa65: .mldsa65
            case .mldsa87: .mldsa87
            }
        }

        var supportedSignatureAlgorithms: [Certificate.SignatureAlgorithm] {
            [self.defaultSignatureAlgorithm]
        }

        func signSynchronously(
            bytes: some DataProtocol,
            signatureAlgorithm: Certificate.SignatureAlgorithm
        ) throws -> Certificate.Signature {
            let signature: Data
            switch self.variant {
            case .mldsa65(let key): signature = try key.signature(for: bytes)
            case .mldsa87(let key): signature = try key.signature(for: bytes)
            }
            return try Certificate.Signature(
                signatureAlgorithm: signatureAlgorithm,
                signatureBytes: ASN1BitString(bytes: ArraySlice(signature))
            )
        }

        static func == (lhs: Self, rhs: Self) -> Bool { lhs.id == rhs.id }
        func hash(into hasher: inout Hasher) { hasher.combine(self.id) }
        static var defaultPEMDiscriminator: String { "PRIVATE KEY" }
        func serialize(into coder: inout DER.Serializer) throws {
            throw CertificateError.unsupportedPrivateKey(
                reason: "test-only ML-DSA key is not serializable"
            )
        }
    }

    static func makeIdentity(commonName: String, ca: CA) throws -> Identity {
        let key = P256.Signing.PrivateKey()
        let subject = try DistinguishedName { CommonName(commonName) }
        let cert = try Certificate(
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
        var serializer = DER.Serializer()
        try serializer.serialize(cert)
        return Identity(
            certificate: cert,
            certPEM: try cert.serializeAsPEM().pemString,
            keyPEM: key.pemRepresentation,
            der: serializer.serializedBytes
        )
    }
}
