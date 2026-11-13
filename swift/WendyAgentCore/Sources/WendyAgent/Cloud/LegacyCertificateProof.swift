import Crypto
import Foundation
import GRPCCore
import SwiftASN1
import X509

struct LegacyCertificateProofSigner: Sendable {
    private static let domain = "wendy-legacy-certificate-proof/v1"

    private let identityURI: String
    private let certificateSerial: String
    private let privateKey: P256.Signing.PrivateKey

    init(credentials: WendyCloudCredentials) throws {
        guard let userID = credentials.userID, !userID.isEmpty else {
            throw RPCError(
                code: .failedPrecondition,
                message: "Wendy cloud user identity is missing"
            )
        }
        let certificate = try Certificate(pemEncoded: credentials.pemCertificate)
        let privateKey = try P256.Signing.PrivateKey(
            pemRepresentation: credentials.pemPrivateKey
        )
        guard certificate.publicKey == Certificate.PublicKey(privateKey.publicKey) else {
            throw RPCError(
                code: .failedPrecondition,
                message: "Wendy cloud certificate and key do not match"
            )
        }
        var serialBytes = Array(certificate.serialNumber.bytes)
        while serialBytes.count > 1, serialBytes.first == 0 {
            serialBytes.removeFirst()
        }
        guard
            !serialBytes.isEmpty,
            serialBytes.count <= 20,
            serialBytes.contains(where: { $0 != 0 })
        else {
            throw RPCError(
                code: .failedPrecondition,
                message: "Wendy cloud certificate serial is invalid"
            )
        }
        self.identityURI = "urn:wendy:org:\(credentials.organizationID):user:\(userID)"
        self.certificateSerial = serialBytes.map(Self.hexByte).joined()
        self.privateKey = privateKey
    }

    func metadata(fullMethod: String) throws -> Metadata {
        let method = fullMethod.hasPrefix("/") ? String(fullMethod.dropFirst()) : fullMethod
        guard !method.isEmpty, method.utf8.count <= 256 else {
            throw RPCError(code: .invalidArgument, message: "Wendy cloud RPC method is invalid")
        }
        let timestamp = String(Int64(Date().timeIntervalSince1970))
        let nonce = Data((0..<16).map { _ in UInt8.random(in: .min ... .max) })
            .base64URLEncodedString()
        let canonical = try Self.canonicalBytes([
            Self.domain,
            method,
            identityURI,
            certificateSerial,
            timestamp,
            nonce,
        ])
        let signature = try privateKey.signature(for: canonical).derRepresentation
            .base64URLEncodedString()

        var metadata = Metadata()
        metadata.addString(identityURI, forKey: "x-wendy-certificate-uri")
        metadata.addString(certificateSerial, forKey: "x-wendy-certificate-serial")
        metadata.addString(timestamp, forKey: "x-wendy-certificate-timestamp")
        metadata.addString(nonce, forKey: "x-wendy-certificate-nonce")
        metadata.addString(signature, forKey: "x-wendy-certificate-signature")
        return metadata
    }

    private static func canonicalBytes(_ components: [String]) throws -> Data {
        var result = Data()
        for component in components {
            let bytes = Array(component.utf8)
            guard let length = UInt32(exactly: bytes.count) else {
                throw RPCError(
                    code: .invalidArgument,
                    message: "Wendy cloud proof component is too large"
                )
            }
            result.append(UInt8((length >> 24) & 0xff))
            result.append(UInt8((length >> 16) & 0xff))
            result.append(UInt8((length >> 8) & 0xff))
            result.append(UInt8(length & 0xff))
            result.append(contentsOf: bytes)
        }
        return result
    }

    private static func hexByte(_ byte: UInt8) -> String {
        let alphabet = Array("0123456789abcdef".utf8)
        return String(
            decoding: [alphabet[Int(byte >> 4)], alphabet[Int(byte & 0x0f)]],
            as: UTF8.self
        )
    }
}

extension Data {
    fileprivate func base64URLEncodedString() -> String {
        base64EncodedString()
            .replacingOccurrences(of: "+", with: "-")
            .replacingOccurrences(of: "/", with: "_")
            .replacingOccurrences(of: "=", with: "")
    }
}
