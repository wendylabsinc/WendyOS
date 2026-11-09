import Crypto
import Foundation
import SwiftASN1
@_spi(FixedExpiryValidationTime) import X509

/// The same versioned, self-contained authorization consumed by the Go agent.
/// It authorizes one delivery; it is independent of image artifact signatures.
enum BuildDeliveryContract {
    static let maxBytes = 192 * 1024
    static let entitlementOID: ASN1ObjectIdentifier = [1, 3, 6, 1, 4, 1, 65441, 1]
    struct Failure: Error, CustomStringConvertible, Sendable {
        let description: String
        init(_ message: String) { self.description = message }
    }
    struct Claims: Codable, Sendable {
        var v: Int
        var jti: String
        var iss: String
        var aud: String
        var builder: String
        var builder_spki_sha256: String
        var reference: String
        var input_sha256: String
        var expected_digest: String
        var iat: Int64
        var admit_before: Int64
        var exp: Int64
        var operations: [String]
        var max_bytes: Int64
    }
    private struct Header: Decodable {
        var alg: String
        var typ: String
        var x5c: [String]
    }
    struct Verified: Sendable {
        var claims: Claims
        var compact: String
        var admittedAt: Int64
    }

    static func base64URL(_ bytes: some Sequence<UInt8>) -> String {
        Data(bytes).base64EncodedString().replacingOccurrences(of: "+", with: "-")
            .replacingOccurrences(of: "/", with: "_").replacingOccurrences(of: "=", with: "")
    }
    static func decodeURL(_ text: String) throws -> Data {
        guard !text.contains("="),
            text.utf8.allSatisfy({
                (65...90).contains($0) || (97...122).contains($0) || (48...57).contains($0)
                    || $0 == 45 || $0 == 95
            })
        else { throw Failure("invalid base64url") }
        let padded =
            text.replacingOccurrences(of: "-", with: "+").replacingOccurrences(of: "_", with: "/")
            + String(repeating: "=", count: (4 - text.utf8.count % 4) % 4)
        guard let bytes = Data(base64Encoded: padded), base64URL(bytes) == text else {
            throw Failure("noncanonical base64url")
        }
        return bytes
    }
    static func sessionID(_ compact: String) -> String {
        base64URL(SHA256.hash(data: Data(compact.utf8)))
    }
    static func fingerprint(_ leaf: Certificate) throws -> String {
        // X509.subjectPublicKeyInfoBytes is only the BIT STRING key payload.
        // Hash the complete, original DER SPKI (algorithm + key), as Go does.
        let tbs = try DER.parse(leaf.tbsCertificateBytes)
        guard case .constructed(let children) = tbs.content else {
            throw Failure("invalid certificate TBS")
        }
        let fields = Array(children)
        let hasVersion =
            fields.first?.identifier == ASN1Identifier(tagWithNumber: 0, tagClass: .contextSpecific)
        let index = hasVersion ? 6 : 5
        guard fields.count > index, fields[index].identifier == .sequence else {
            throw Failure("certificate has no SPKI")
        }
        return base64URL(SHA256.hash(data: Data(fields[index].encodedBytes)))
    }
    static func canonicalUUID(_ value: String) -> Bool {
        guard let uuid = UUID(uuidString: value), uuid.uuidString.lowercased() == value else {
            return false
        }
        return value != "00000000-0000-0000-0000-000000000000"
    }
    static func principal(_ text: String, kind: String) throws -> (tenant: String, subject: String)
    {
        let parts = text.components(separatedBy: "/")
        guard text.utf8.count <= 1024, parts.count == 7, parts[0] == "spiffe:", parts[1].isEmpty,
            parts[2] == "wendy.sh", parts[3] == "tenant", canonicalUUID(parts[4]), parts[5] == kind,
            !parts[6].isEmpty, !parts[6].contains(":"),
            text.unicodeScalars.allSatisfy({
                !CharacterSet.controlCharacters.contains($0)
                    && !CharacterSet.whitespacesAndNewlines.contains($0)
            }),
            let url = URLComponents(string: text), url.query == nil, url.fragment == nil,
            url.user == nil,
            url.string == text
        else { throw Failure("invalid exact tenant principal") }
        if kind == "device", !canonicalUUID(parts[6]) { throw Failure("invalid device UUID") }
        return (parts[4], parts[6])
    }
    static func reference(_ text: String) throws -> (repository: String, tag: String) {
        let parts = text.components(separatedBy: ":")
        guard parts.count == 2, parts[0].utf8.count <= 255, parts[0] == parts[0].lowercased(),
            BlobStore.isValidRepository(parts[0]), BlobStore.isValidReference(parts[1]),
            !parts[1].hasPrefix("."), !parts[1].hasPrefix("-"),
            parts[0].range(
                of: #"^[a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*$"#,
                options: .regularExpression
            ) != nil
        else { throw Failure("delivery requires a supported local repository and explicit tag") }
        return (parts[0], parts[1])
    }

    static func verify(
        _ compact: String,
        roots: [Certificate],
        target: String,
        at time: Int64
    ) async throws -> Verified {
        guard compact.utf8.count <= maxBytes else { throw Failure("oversized delivery contract") }
        let parts = compact.components(separatedBy: ".")
        guard parts.count == 3 else { throw Failure("invalid compact JWS") }
        let header = try DeliveryStrictJSON.decode(
            Header.self,
            data: decodeURL(parts[0]),
            keys: ["alg", "typ", "x5c"]
        )
        let c = try DeliveryStrictJSON.decode(
            Claims.self,
            data: decodeURL(parts[1]),
            keys: [
                "v", "jti", "iss", "aud", "builder", "builder_spki_sha256", "reference",
                "input_sha256", "expected_digest", "iat", "admit_before", "exp", "operations",
                "max_bytes",
            ]
        )
        guard header.alg == "ML-DSA-65", header.typ == "wendy-build-delivery+jws",
            !header.x5c.isEmpty,
            header.x5c.count <= 16
        else { throw Failure("invalid delivery JWS header") }
        let op = try principal(c.iss, kind: "operator")
        let destination = try principal(c.aud, kind: "device")
        let builder = try principal(c.builder, kind: "device")
        guard c.v == 1, canonicalUUID(c.jti), op.tenant == destination.tenant,
            builder.tenant == destination.tenant,
            c.aud == target, c.iat > 0, c.admit_before > c.iat, c.exp > c.iat,
            c.exp - c.iat <= 86_400, c.admit_before <= c.exp, time >= c.iat, time < c.admit_before,
            time < c.exp,
            c.operations == ["stage", "publish"], c.max_bytes > 0, c.max_bytes <= 1 << 40,
            try decodeURL(c.builder_spki_sha256).count == 32,
            try decodeURL(c.input_sha256).count == 32,
            c.expected_digest.isEmpty || BlobStore.isValidSHA256Digest(c.expected_digest)
        else { throw Failure("invalid delivery claims or admission window") }
        _ = try reference(c.reference)
        var total = 0
        let certificates: [Certificate] = try header.x5c.map { encoded in
            guard let data = Data(base64Encoded: encoded), data.base64EncodedString() == encoded,
                !data.isEmpty, data.count <= 32768
            else { throw Failure("invalid x5c certificate") }
            total += data.count
            guard total <= 128 * 1024 else { throw Failure("oversized operator chain") }
            return try Certificate(derEncoded: Array(data))
        }
        let leaf = certificates[0]
        guard let identity = try OrgIdentity.identity(fromLeaf: leaf), identity.principal == c.iss,
            identity.entityType == "user", try leaf.extensions.keyUsage?.digitalSignature == true,
            try leaf.extensions.basicConstraints != .isCertificateAuthority(maxPathLength: nil),
            let usages = try leaf.extensions.extendedKeyUsage, usages.contains(.clientAuth)
        else { throw Failure("invalid operator credential profile") }
        if case .isCertificateAuthority = try leaf.extensions.basicConstraints {
            throw Failure("operator credential is a CA")
        }
        let date = Date(timeIntervalSince1970: Double(time))
        let anchors = roots.filter {
            if case .isCertificateAuthority = try? $0.extensions.basicConstraints { return true }
            return false
        }
        guard !anchors.isEmpty else { throw Failure("local PKI trust unavailable") }
        var verifier = Verifier(rootCertificates: CertificateStore(anchors)) {
            RFC5280Policy(fixedExpiryValidationTime: date)
            DeliveryClientAuthPolicy()
        }
        let result = await verifier.validate(
            leaf: leaf,
            intermediates: CertificateStore(Array(certificates.dropFirst()))
        )
        guard case .validCertificate(let path) = result,
            let expiry = path.map(\.notValidAfter).min(),
            Double(c.admit_before) <= expiry.timeIntervalSince1970
        else { throw Failure("operator PKI path or admission deadline invalid") }
        try entitlements(leaf, device: destination.subject)
        let signature = try decodeURL(parts[2])
        guard
            leaf.publicKey.isValidSignature(
                signature,
                for: Data((parts[0] + "." + parts[1]).utf8),
                signatureAlgorithm: .mldsa65
            )
        else { throw Failure("invalid delivery signature") }
        return Verified(claims: c, compact: compact, admittedAt: time)
    }

    private static func entitlements(_ leaf: Certificate, device: String) throws {
        let extensions = leaf.extensions.filter { $0.oid == entitlementOID }
        guard extensions.count == 1, let value = extensions.first?.value, !value.isEmpty,
            value.count <= 2048
        else { throw Failure("operator requires bounded deployment entitlements") }
        let node = try DER.parse(value)
        guard node.identifier == .sequence, case .constructed(let nodes) = node.content else {
            throw Failure("malformed entitlements")
        }
        var allowed = false
        for node in nodes {
            let bytes = try ASN1UTF8String(derEncoded: node).bytes
            guard let text = String(bytes: bytes, encoding: .utf8) else {
                throw Failure("invalid entitlement UTF-8")
            }
            let fields = text.components(separatedBy: ":")
            guard fields.count >= 4, fields[0] == "entitlement", !fields[1].isEmpty,
                !fields[2].isEmpty,
                let effect = fields.last, effect == "allow" || effect == "deny"
            else { throw Failure("invalid entitlement") }
            let scope = fields.dropFirst(3).dropLast().joined(separator: ":")
            guard fields.count == 4 || !scope.isEmpty else {
                throw Failure("empty explicit entitlement scope")
            }
            if !scope.isEmpty && scope != "all" && scope != "*" {
                let split = scope.split(
                    separator: ":",
                    maxSplits: 1,
                    omittingEmptySubsequences: false
                )
                guard split.count == 2, ["device", "site", "tag"].contains(String(split[0])),
                    !split[1].isEmpty,
                    !split[1].contains("*"),
                    split[1].unicodeScalars.allSatisfy({
                        !CharacterSet.whitespacesAndNewlines.contains($0)
                            && !CharacterSet.controlCharacters.contains($0)
                    })
                else { throw Failure("invalid entitlement scope") }
            }
            guard fields[1] == "deployment", fields[2] == "create" || fields[2] == "*" else {
                continue
            }
            let matches =
                scope.isEmpty || scope == "all" || scope == "*" || scope == "device:" + device
            if effect == "deny" && (matches || scope.hasPrefix("site:") || scope.hasPrefix("tag:"))
            {
                throw Failure("operator deployment denied")
            }
            if effect == "allow" && matches { allowed = true }
        }
        guard allowed else { throw Failure("operator lacks deployment:create for target") }
    }
}

/// RFC5280Policy deliberately does not police key usage or EKU. Match the
/// primary Go verifier: explicit clientAuth on the leaf, no unknown usages,
/// and clientAuth-compatible certificate-signing issuers on the actual path.
private struct DeliveryClientAuthPolicy: VerifierPolicy, Sendable {
    var verifyingCriticalExtensions: [ASN1ObjectIdentifier] { [.X509ExtensionID.extendedKeyUsage] }
    func chainMeetsPolicyRequirements(
        chain: UnverifiedCertificateChain
    ) async -> PolicyEvaluationResult {
        do {
            for (index, certificate) in chain.enumerated() {
                if index > 0, let usage = try certificate.extensions.keyUsage, !usage.keyCertSign {
                    return .failsToMeetPolicy(reason: "issuer cannot sign certificates")
                }
                if let usages = try certificate.extensions.extendedKeyUsage {
                    let known: Set<ExtendedKeyUsage.Usage> = [
                        .serverAuth, .clientAuth, .codeSigning, .emailProtection, .timeStamping,
                        .ocspSigning, .any, .certificateTransparency,
                    ]
                    guard usages.allSatisfy({ known.contains($0) }),
                        usages.contains(.clientAuth) || usages.contains(.any)
                    else {
                        return .failsToMeetPolicy(reason: "unsupported client authentication usage")
                    }
                }
            }
            return .meetsPolicy
        } catch { return .failsToMeetPolicy(reason: "malformed client authentication extensions") }
    }
}
