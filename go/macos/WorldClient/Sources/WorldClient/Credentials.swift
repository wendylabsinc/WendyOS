import CryptoKit
import Foundation
import Security

/// Imports the existing Go client configuration. This viewer intentionally has
/// no signing operation for missions, stops, sensor publication or execution.
public struct WorldCredentials: Sendable {
    public let endpoint: URL
    public let audience: String
    public let tenant: String
    public let site: String
    public let principal: String
    public let scopes: Set<String>
    public let scopeID: String
    public let clockPins: AnnotationClockPins?
    let grant: JSONValue
    let canonicalGrant: Data
    let key: Curve25519.Signing.PrivateKey
    let notBefore: Int64
    let notAfter: Int64

    public init(data: Data) throws {
        guard data.count <= 128 << 10,
              let fields = try JSONValue.decode(data, limit: 128 << 10).object,
              Set(fields.keys).isSubset(of: ["url", "audience", "grant", "private_key", "allow_insecure_simulation", "clock_trust"]),
              let address = fields["url"]?.string,
              let endpoint = URL(string: address),
              let parts = URLComponents(url: endpoint, resolvingAgainstBaseURL: false),
              let host = parts.host, !host.isEmpty,
              parts.user == nil, parts.password == nil, parts.query == nil, parts.fragment == nil,
              parts.path.isEmpty || parts.path == "/",
              let audience = fields["audience"]?.string, !audience.isEmpty, audience.utf8.count <= 256,
              let grant = fields["grant"], let signed = grant.object,
              Set(signed.keys) == ["grant", "signature"],
              let signature = signed["signature"]?.string.flatMap({ Data(base64Encoded: $0) }), signature.count == 64,
              let body = signed["grant"]?.object,
              let privateData = fields["private_key"]?.string.flatMap({ Data(base64Encoded: $0) }), privateData.count == 64,
              let publicData = body["public_key"]?.string.flatMap({ Data(base64Encoded: $0) }), publicData.count == 32,
              body["version"]?.int == 1, body["domain"]?.string == "simulation",
              let tenant = body["tenant"]?.string, !tenant.isEmpty,
              let site = body["site"]?.string, !site.isEmpty,
              let principal = body["principal"]?.string, !principal.isEmpty,
              let before = body["not_before_unix_ms"]?.int, before > 0,
              let after = body["not_after_unix_ms"]?.int, after > before,
              let values = body["scopes"]?.array else { throw WorldClientError.invalidCredentials }
        guard parts.scheme == "https" || (parts.scheme == "http" && fields["allow_insecure_simulation"]?.bool == true) else { throw WorldClientError.invalidEndpoint }
        let scopes = Set(values.compactMap(\.string))
        guard scopes.count == values.count, scopes.contains("read"), scopes.isSubset(of: ["read", "evidence-read", "annotation-write"]) else { throw WorldClientError.missingReadScope }
        if scopes.contains("annotation-write") {
            guard body["kind"]?.string == "operator", body["publish_audience"]?.string == audience else { throw WorldClientError.invalidCredentials }
        }
        let key = try Curve25519.Signing.PrivateKey(rawRepresentation: privateData.prefix(32))
        guard key.publicKey.rawRepresentation == publicData, privateData.suffix(32) == publicData else { throw WorldClientError.invalidCredentials }
        let canonical = try GoAuthorityJSON.signedGrant(grant)
        self.clockPins = try fields["clock_trust"].map(AnnotationClockPins.init(json:))
        self.endpoint = URL(string: address.hasSuffix("/") ? String(address.dropLast()) : address)!
        self.audience = audience
        self.tenant = tenant
        self.site = site
        self.principal = principal
        self.scopes = scopes
        self.grant = grant
        self.key = key
        self.notBefore = before
        self.notAfter = after
        self.canonicalGrant = canonical
        self.scopeID = Self.digest(GoAuthorityJSON.object([
            ("endpoint", GoAuthorityJSON.string(self.endpoint.absoluteString)),
            ("audience", GoAuthorityJSON.string(audience)),
            ("tenant", GoAuthorityJSON.string(tenant)),
            ("site", GoAuthorityJSON.string(site)),
            ("public_key", GoAuthorityJSON.string(publicData.base64EncodedString()))
        ]))
    }

    static func digest(_ data: Data) -> String { SHA256.hash(data: data).map { String(format: "%02x", $0) }.joined() }

    /// Exposed for cross-language contract tests. Only the fixed viewer read
    /// operations are permitted. This never returns private key bytes.
    public func signedReadAuthorization(operation: String, body: Data = Data(), nonce: String, issuedAtUnixMS: Int64, lifetimeMS: Int64 = 30_000) throws -> Data {
        guard operation == "read" || operation == "evidence-read" else { throw WorldClientError.missingReadScope }
        return try signedAuthorization(operation: operation, body: body, nonce: nonce, issuedAtUnixMS: issuedAtUnixMS, lifetimeMS: lifetimeMS)
    }

    func signedAuthorization(operation: String, body: Data, nonce: String, issuedAtUnixMS: Int64, lifetimeMS: Int64 = 30_000) throws -> Data {
        guard scopes.contains(operation), ["read", "evidence-read", "annotation-write"].contains(operation),
              body.count <= 128 << 10, (16...128).contains(nonce.utf8.count),
              nonce.utf8.allSatisfy({ (48...57).contains($0) || (65...90).contains($0) || (97...122).contains($0) || $0 == 45 || $0 == 95 }),
              lifetimeMS >= 1, lifetimeMS <= 60_000,
              issuedAtUnixMS >= notBefore, issuedAtUnixMS < notAfter,
              issuedAtUnixMS <= Int64.max - lifetimeMS else { throw WorldClientError.expiredCredentials }
        let expiry = min(issuedAtUnixMS + lifetimeMS, notAfter)
        let digest = Self.digest(body)
        let payload = GoAuthorityJSON.object([
            ("version", Data("1".utf8)), ("grant_digest", GoAuthorityJSON.string(Self.digest(canonicalGrant))),
            ("audience", GoAuthorityJSON.string(audience)), ("operation", GoAuthorityJSON.string(operation)),
            ("body_digest", GoAuthorityJSON.string(digest)), ("nonce", GoAuthorityJSON.string(nonce)),
            ("issued", Data(String(issuedAtUnixMS).utf8)), ("expires", Data(String(expiry).utf8))
        ])
        var message = Data("wendy.world.authority.v1/request\0".utf8)
        message.append(payload)
        let signature = try key.signature(for: message)
        return GoAuthorityJSON.object([
            ("version", Data("1".utf8)), ("grant", canonicalGrant),
            ("audience", GoAuthorityJSON.string(audience)), ("operation", GoAuthorityJSON.string(operation)),
            ("body_sha256", GoAuthorityJSON.string(digest)), ("nonce", GoAuthorityJSON.string(nonce)),
            ("issued_at_unix_ms", Data(String(issuedAtUnixMS).utf8)), ("expires_at_unix_ms", Data(String(expiry).utf8)),
            ("signature", GoAuthorityJSON.string(signature.base64EncodedString()))
        ])
    }

    func authorization(operation: String, body: Data) throws -> String {
        var random = [UInt8](repeating: 0, count: 24)
        guard SecRandomCopyBytes(kSecRandomDefault, random.count, &random) == errSecSuccess else { throw WorldClientError.invalidCredentials }
        let nonce = random.map { String(format: "%02x", $0) }.joined()
        let now = Int64(Date().timeIntervalSince1970 * 1000)
        return try signedAuthorization(operation: operation, body: body, nonce: nonce, issuedAtUnixMS: now).base64EncodedString()
    }
}

/// Go's authority signature format uses declared struct field order rather than
/// sorted dictionary keys. Keep this versioned encoder separate from display JSON.
enum GoAuthorityJSON {
    static func string(_ value: String) -> Data {
        var text = "\""
        for scalar in value.unicodeScalars {
            switch scalar.value {
            case 0x22: text += "\\\""
            case 0x5c: text += "\\\\"
            case 0x08: text += "\\b"
            case 0x09: text += "\\t"
            case 0x0a: text += "\\n"
            case 0x0c: text += "\\f"
            case 0x0d: text += "\\r"
            case 0...0x1f, 0x3c, 0x3e, 0x26, 0x2028, 0x2029: text += String(format: "\\u%04x", scalar.value)
            default: text.unicodeScalars.append(scalar)
            }
        }
        text += "\""
        return Data(text.utf8)
    }
    static func object(_ fields: [(String, Data)]) -> Data {
        var data = Data("{".utf8)
        for (index, field) in fields.enumerated() {
            if index > 0 { data.append(0x2c) }
            data.append(string(field.0)); data.append(0x3a); data.append(field.1)
        }
        data.append(0x7d)
        return data
    }
    static func signedGrant(_ value: JSONValue) throws -> Data {
        guard let signed = value.object, let body = signed["grant"]?.object, let signature = signed["signature"]?.string else { throw WorldClientError.invalidCredentials }
        let optional = ["task_status_assignee", "task_status_audience", "execute_assignee", "publish_audience"]
        let required = ["version", "id", "tenant", "site", "domain", "principal", "kind", "source", "public_key", "scopes", "not_before_unix_ms", "not_after_unix_ms"]
        guard Set(body.keys).isSubset(of: Set(optional + required)), Set(required).isSubset(of: Set(body.keys)),
              let source = body["source"]?.object, Set(source.keys) == ["device", "app", "service", "stream"] else { throw WorldClientError.invalidCredentials }
        var fields: [(String, Data)] = []
        for name in optional {
            if let value = body[name] {
                guard let text = value.string else { throw WorldClientError.invalidCredentials }
                if !text.isEmpty { fields.append((name, string(text))) }
            }
        }
        for name in required {
            switch name {
            case "version", "not_before_unix_ms", "not_after_unix_ms":
                guard let number = body[name]?.int else { throw WorldClientError.invalidCredentials }
                fields.append((name, Data(String(number).utf8)))
            case "source":
                var nested: [(String, Data)] = []
                for key in ["device", "app", "service", "stream"] {
                    guard let text = source[key]?.string, !text.isEmpty else { throw WorldClientError.invalidCredentials }
                    nested.append((key, string(text)))
                }
                fields.append((name, object(nested)))
            case "scopes":
                guard let scopes = body[name]?.array else { throw WorldClientError.invalidCredentials }
                let strings = try scopes.map { value -> String in
                    guard let text = value.string else { throw WorldClientError.invalidCredentials }; return text
                }
                var encoded = Data("[".utf8)
                for (index, item) in strings.enumerated() { if index > 0 { encoded.append(0x2c) }; encoded.append(string(item)) }
                encoded.append(0x5d)
                fields.append((name, encoded))
            default:
                guard let text = body[name]?.string, !text.isEmpty else { throw WorldClientError.invalidCredentials }
                fields.append((name, string(text)))
            }
        }
        return object([("grant", object(fields)), ("signature", string(signature))])
    }
}
