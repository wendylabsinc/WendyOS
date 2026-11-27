import CryptoKit
import Foundation

public enum MissionError: Error, LocalizedError, Sendable {
    case invalidProfile, invalidContext, invalidProposal, invalidDraft, clockUnavailable, expired, unsupported, invalidJournal, journalFull, changedJournal, uncertain, mismatchedStatus
    public var errorDescription: String? {
        switch self {
        case .invalidProfile: "A separately commissioned simulation mission profile is required."
        case .invalidContext: "The commissioned mission context failed verification."
        case .invalidProposal: "This proposal cannot be reviewed as a typed inspection."
        case .invalidDraft: "The inspection target or duration is outside the commissioned limits."
        case .clockUnavailable: "Fresh verified gateway time is unavailable. The draft remains unsigned."
        case .expired: "The instruction or commissioning authority has expired."
        case .unsupported: "This client supports autonomous simulated inspections only."
        case .invalidJournal: "The saved mission journal is invalid or belongs to another profile."
        case .journalFull: "The mission journal is full."
        case .changedJournal: "Another process changed this mission journal. Reopen the profile."
        case .uncertain: "Delivery is uncertain. Reconcile with the assignee before deciding whether to retry."
        case .mismatchedStatus: "The assignee returned a different instruction. The outcome remains unknown."
        }
    }
}

/// Mission credentials are deliberately independent of the reader and annotation
/// credential type. Importing either of those profiles never grants motion access.
public struct MissionCredentials: Sendable {
    public let endpoint: URL
    public let audience: String
    public let tenant: String
    public let site: String
    public let principal: String
    public let scopeID: String
    let grant: JSONValue
    let canonicalGrant: Data
    let key: Curve25519.Signing.PrivateKey
    let notBefore: Int64
    let notAfter: Int64

    public init(data: Data) throws {
        let json = try JSONValue.decode(data, limit: 128 << 10)
        guard let object = json.object,
              Set(object.keys).isSubset(of: ["url", "audience", "grant", "private_key", "allow_insecure_simulation"]),
              let address = json["url"]?.string, let url = URL(string: address),
              url.host != nil, url.user == nil, url.password == nil, url.query == nil, url.fragment == nil,
              url.path.isEmpty || url.path == "/",
              url.scheme == "https" || (url.scheme == "http" && json["allow_insecure_simulation"]?.bool == true),
              let audience = json["audience"]?.string, !audience.isEmpty, audience.utf8.count <= 256,
              let grant = json["grant"], let body = grant["grant"],
              body["version"]?.int == 1, body["domain"]?.string == "simulation", body["kind"]?.string == "operator",
              let tenant = body["tenant"]?.string, !tenant.isEmpty,
              let site = body["site"]?.string, !site.isEmpty,
              let principal = body["principal"]?.string, !principal.isEmpty,
              let scopes = body["scopes"]?.array?.compactMap(\.string),
              scopes.count == body["scopes"]?.array?.count, Set(scopes).count == scopes.count,
              Set(scopes) == ["read", "mission", "stop"],
              let before = body["not_before_unix_ms"]?.int, let after = body["not_after_unix_ms"]?.int,
              before > 0, after > before, after - before <= 31_536_000_000,
              let raw = json["private_key"]?.string.flatMap({ Data(base64Encoded: $0) }), raw.count == 64,
              let publicKey = body["public_key"]?.string.flatMap({ Data(base64Encoded: $0) }), publicKey.count == 32,
              grant["signature"]?.string.flatMap({ Data(base64Encoded: $0) })?.count == 64 else { throw MissionError.invalidProfile }
        let key = try Curve25519.Signing.PrivateKey(rawRepresentation: raw.prefix(32))
        guard key.publicKey.rawRepresentation == publicKey, raw.suffix(32) == publicKey else { throw MissionError.invalidProfile }
        self.endpoint = URL(string: address.hasSuffix("/") ? String(address.dropLast()) : address)!
        self.audience = audience; self.tenant = tenant; self.site = site; self.principal = principal
        self.grant = grant; self.canonicalGrant = try GoAuthorityJSON.signedGrant(grant)
        self.key = key; notBefore = before; notAfter = after
        scopeID = WorldCredentials.digest(GoAuthorityJSON.object([
            ("endpoint", GoAuthorityJSON.string(self.endpoint.absoluteString)), ("audience", GoAuthorityJSON.string(audience)),
            ("grant", canonicalGrant)
        ]))
    }

    func verifyRoot(_ root: Data) throws {
        guard let signature = grant["signature"]?.string.flatMap({ Data(base64Encoded: $0) }),
              let marker = canonicalGrant.range(of: Data(",\"signature\":".utf8), options: .backwards) else { throw MissionError.invalidProfile }
        var message = Data("wendy.world.authority.v1/grant\0".utf8)
        message.append(canonicalGrant[canonicalGrant.index(canonicalGrant.startIndex, offsetBy: 9)..<marker.lowerBound])
        guard try Curve25519.Signing.PublicKey(rawRepresentation: root).isValidSignature(signature, for: message) else { throw MissionError.invalidProfile }
    }

    /// A new nonce protects each delivery attempt. The instruction body is never
    /// changed by a retry. Stop is valid with an expired, unrevoked operator grant.
    public func signedAuthorization(operation: String, body: Data, nonce: String, issuedAtUnixMS: Int64, lifetimeMS: Int64 = 5_000) throws -> Data {
        guard ["read", "mission", "stop"].contains(operation), body.count <= 128 << 10,
              (16...128).contains(nonce.utf8.count), nonce.utf8.allSatisfy({ $0 >= 48 && $0 <= 57 || $0 >= 65 && $0 <= 90 || $0 >= 97 && $0 <= 122 || $0 == 45 || $0 == 95 }),
              lifetimeMS > 0, lifetimeMS <= 60_000, issuedAtUnixMS >= notBefore,
              operation == "stop" || issuedAtUnixMS < notAfter,
              issuedAtUnixMS <= Int64.max - lifetimeMS else { throw MissionError.expired }
        let expires = operation == "stop" ? issuedAtUnixMS + lifetimeMS : min(issuedAtUnixMS + lifetimeMS, notAfter)
        let digest = WorldCredentials.digest(body)
        let payload = GoAuthorityJSON.object([
            ("version", Data("1".utf8)), ("grant_digest", GoAuthorityJSON.string(WorldCredentials.digest(canonicalGrant))),
            ("audience", GoAuthorityJSON.string(audience)), ("operation", GoAuthorityJSON.string(operation)),
            ("body_digest", GoAuthorityJSON.string(digest)), ("nonce", GoAuthorityJSON.string(nonce)),
            ("issued", Data(String(issuedAtUnixMS).utf8)), ("expires", Data(String(expires).utf8))
        ])
        var message = Data("wendy.world.authority.v1/request\0".utf8); message.append(payload)
        return GoAuthorityJSON.object([
            ("version", Data("1".utf8)), ("grant", canonicalGrant), ("audience", GoAuthorityJSON.string(audience)),
            ("operation", GoAuthorityJSON.string(operation)), ("body_sha256", GoAuthorityJSON.string(digest)),
            ("nonce", GoAuthorityJSON.string(nonce)), ("issued_at_unix_ms", Data(String(issuedAtUnixMS).utf8)),
            ("expires_at_unix_ms", Data(String(expires).utf8)), ("signature", GoAuthorityJSON.string(try key.signature(for: message).base64EncodedString()))
        ])
    }

    public func signInstruction(operation: String, payload: Data, issuedAtUnixMS: Int64, expiresAtUnixMS: Int64) throws -> Data {
        guard ["mission", "stop"].contains(operation), payload.count <= 64 << 10,
              issuedAtUnixMS >= notBefore, expiresAtUnixMS > issuedAtUnixMS,
              expiresAtUnixMS - issuedAtUnixMS <= 86_400_000,
              operation == "stop" || (issuedAtUnixMS < notAfter && expiresAtUnixMS <= notAfter) else { throw MissionError.expired }
        _ = try JSONValue.decode(payload, limit: 64 << 10)
        let fields: [(String, Data)] = [("version", Data("1".utf8)), ("grant_digest", GoAuthorityJSON.string(WorldCredentials.digest(canonicalGrant))),
            ("audience", GoAuthorityJSON.string(audience)), ("operation", GoAuthorityJSON.string(operation)), ("payload", payload),
            ("issued", Data(String(issuedAtUnixMS).utf8)), ("expires", Data(String(expiresAtUnixMS).utf8))]
        var message = Data("wendy.world.authority.v1/instruction\0".utf8); message.append(GoAuthorityJSON.object(fields))
        return GoAuthorityJSON.object([("version", Data("1".utf8)), ("grant", canonicalGrant),
            ("audience", GoAuthorityJSON.string(audience)), ("operation", GoAuthorityJSON.string(operation)), ("payload", payload),
            ("issued_at_unix_ms", Data(String(issuedAtUnixMS).utf8)), ("expires_at_unix_ms", Data(String(expiresAtUnixMS).utf8)),
            ("signature", GoAuthorityJSON.string(try key.signature(for: message).base64EncodedString()))])
    }
    func verifyInstruction(_ bytes: Data, operation: String, payload: Data, issued: Int64, expires: Int64) throws {
        let value = try JSONValue.decode(bytes, limit: 128 << 10)
        guard let signature = value["signature"]?.string.flatMap({ Data(base64Encoded: $0) }), signature.count == 64 else { throw MissionError.invalidJournal }
        var message = Data("wendy.world.authority.v1/instruction\0".utf8)
        message.append(GoAuthorityJSON.object([("version", Data("1".utf8)), ("grant_digest", GoAuthorityJSON.string(WorldCredentials.digest(canonicalGrant))),
            ("audience", GoAuthorityJSON.string(audience)), ("operation", GoAuthorityJSON.string(operation)), ("payload", payload),
            ("issued", Data(String(issued).utf8)), ("expires", Data(String(expires).utf8))]))
        let expected = GoAuthorityJSON.object([("version", Data("1".utf8)), ("grant", canonicalGrant),
            ("audience", GoAuthorityJSON.string(audience)), ("operation", GoAuthorityJSON.string(operation)), ("payload", payload),
            ("issued_at_unix_ms", Data(String(issued).utf8)), ("expires_at_unix_ms", Data(String(expires).utf8)),
            ("signature", GoAuthorityJSON.string(signature.base64EncodedString()))])
        guard expected == bytes, key.publicKey.isValidSignature(signature, for: message),
              issued >= notBefore, expires > issued, expires - issued <= 86_400_000,
              operation == "stop" || (issued < notAfter && expires <= notAfter) else { throw MissionError.invalidJournal }
    }

}
