import CryptoKit
import Foundation

/// A separate simulation zone signer. Viewer and mission capabilities are not
/// widened. No method signs arbitrary records, tasks or enrollment requests.
public struct ZoneCredentials: Sendable {
  public let endpoint: URL, audience: String, tenant: String, site: String, source: JSONValue
  let grant: JSONValue, canonicalGrant: Data, key: Curve25519.Signing.PrivateKey
  let before: Int64, after: Int64
  public let canPublish: Bool
  public init(data: Data) throws {
    let value = try ZoneWire.strict(data, limit: 128 << 10)
    guard let fields = value.object,
      Set(fields.keys).isSubset(of: [
        "url", "audience", "grant", "private_key", "allow_insecure_simulation",
      ]),
      let address = value["url"]?.string, let endpoint = URL(string: address),
      let parts = URLComponents(url: endpoint, resolvingAgainstBaseURL: false),
      let host = parts.host, !host.isEmpty, parts.user == nil, parts.password == nil,
      parts.query == nil, parts.fragment == nil, parts.path.isEmpty || parts.path == "/",
      parts.scheme == "https"
        || (parts.scheme == "http" && value["allow_insecure_simulation"]?.bool == true),
      let audience = value["audience"]?.string, !audience.isEmpty, let grant = value["grant"],
      let g = grant["grant"], g["version"]?.int == 1, g["domain"]?.string == "simulation",
      g["kind"]?.string == "operator",
      let scopes = g["scopes"]?.array?.compactMap(\.string), Set(scopes).isSuperset(of: ["read"]),
      Set(scopes).isSubset(of: [
        "read", "publish", "site-publish", "evidence-read", "evidence-write",
      ]), Set(scopes).count == scopes.count,
      let tenant = g["tenant"]?.string, let site = g["site"]?.string, let source = g["source"],
      let before = g["not_before_unix_ms"]?.int, let after = g["not_after_unix_ms"]?.int,
      before > 0, after > before, after - before <= 86_400_000,
      let secret = value["private_key"]?.string.flatMap({ Data(base64Encoded: $0) }),
      secret.count == 64,
      let publicKey = g["public_key"]?.string.flatMap({ Data(base64Encoded: $0) }),
      publicKey.count == 32
    else { throw ZoneError.authority }
    _ = try GoAuthorityJSON.source(source)
    let key = try Curve25519.Signing.PrivateKey(rawRepresentation: secret.prefix(32))
    guard key.publicKey.rawRepresentation == publicKey, secret.suffix(32) == publicKey else {
      throw ZoneError.authority
    }
    self.endpoint = endpoint
    self.audience = audience
    self.tenant = tenant
    self.site = site
    self.source = source
    self.grant = grant
    self.canonicalGrant = try GoAuthorityJSON.signedGrant(grant)
    self.key = key
    self.before = before
    self.after = after
    canPublish =
      Set(scopes).isSuperset(of: ["publish", "site-publish"])
      && g["publish_audience"]?.string == audience
  }
  func authorization(operation: String, body: Data, now: Int64) throws -> String {
    guard operation == "read" || (operation == "publish" && canPublish), now >= before, now < after
    else { throw ZoneError.authority }
    let nonce = UUID().uuidString.replacingOccurrences(of: "-", with: "")
    let expires = min(after, now + 30000)
    let signed = ZoneWire.o([
      ("version", ZoneWire.n(1)), ("grant_digest", ZoneWire.s(ZoneWire.hash(canonicalGrant))),
      ("audience", ZoneWire.s(audience)), ("operation", ZoneWire.s(operation)),
      ("body_digest", ZoneWire.s(ZoneWire.hash(body))), ("nonce", ZoneWire.s(nonce)),
      ("issued", ZoneWire.n(now)), ("expires", ZoneWire.n(expires)),
    ])
    let signature = try key.signature(for: Data("wendy.world.authority.v1/request\0".utf8) + signed)
    return ZoneWire.o([
      ("version", ZoneWire.n(1)), ("grant", canonicalGrant), ("audience", ZoneWire.s(audience)),
      ("operation", ZoneWire.s(operation)), ("body_sha256", ZoneWire.s(ZoneWire.hash(body))),
      ("nonce", ZoneWire.s(nonce)), ("issued_at_unix_ms", ZoneWire.n(now)),
      ("expires_at_unix_ms", ZoneWire.n(expires)),
      ("signature", ZoneWire.s(signature.base64EncodedString())),
    ]).base64EncodedString()
  }
  public func publication(
    context: ZoneContext, name: String, revision: String, zones: [ZoneRectangle], epoch: UInt64,
    nowMS: Int64, lifetimeMS: Int64 = 1_800_000
  ) throws -> Data {
    guard canPublish, context.tenant == tenant, context.site == site, epoch > 0, nowMS >= before,
      nowMS < after, lifetimeMS >= 1, lifetimeMS <= 86_400_000
    else { throw ZoneError.authority }
    let payload = try ZoneWire.catalog(
      context: context, name: name, revision: revision, zones: zones, from: nowMS,
      until: nowMS + lifetimeMS)
    return try record(
      context: context, digest: ZoneWire.hash(payload), revision: revision, payload: payload,
      epoch: epoch, nowMS: nowMS, lifetimeMS: lifetimeMS)
  }
  func deletion(_ selected: ZoneRevision, epoch: UInt64, nowMS: Int64) throws -> Data {
    guard selected.source == source, selected.context.tenant == tenant,
      selected.context.site == site, canPublish
    else { throw ZoneError.authority }
    return try record(
      context: selected.context, digest: selected.digest, revision: selected.revision, payload: nil,
      epoch: epoch, nowMS: nowMS, lifetimeMS: 1_800_000)
  }
  func record(
    context: ZoneContext, digest: String, revision: String, payload: Data?, epoch: UInt64,
    nowMS: Int64, lifetimeMS: Int64
  ) throws -> Data {
    guard canPublish, nowMS >= before, nowMS < after, epoch > 0 else { throw ZoneError.authority }
    let o = ZoneWire.o
    let s = ZoneWire.s
    let n = ZoneWire.n
    var fields: [(String, Data)] = [
      (
        "key",
        o([
          ("tenant", s(tenant)), ("site", s(site)), ("entity", s("zone-catalog/" + digest)),
          ("type", s(ZoneWire.type)), ("source", try GoAuthorityJSON.source(source)),
        ])
      ), ("version", o([("epoch", Data(String(epoch).utf8)), ("sequence", n(1))])),
      ("frame", o([("id", s("site-metadata")), ("revision", s("v1"))])),
      (
        "timing",
        o([
          ("sample_time", s(ZoneWire.time(nowMS))),
          ("clock_domain", s("simulation:" + context.runID)), ("error_known", Data("true".utf8)),
          ("error_bound_ns", n(0)), ("age_hint_ns", n(0)),
        ])
      ), ("ttl_ns", n(lifetimeMS * 1_000_000)),
      (
        "provenance",
        o([
          ("origin", s("simulated")), ("app_version", s("world-zone-v1")),
          (
            "simulation",
            o([
              ("run_id", s(context.runID)), ("scenario_version", s(revision)), ("seed", n(0)),
              ("clock_domain", s("simulation:" + context.runID)),
            ])
          ),
        ])
      ),
    ]
    if let payload { fields.append(("payload", payload)) }
    fields.append(("tombstone", Data((payload == nil ? "true" : "false").utf8)))
    let component = o(fields)
    let expires = nowMS + 86_400_000
    let signed = o([
      ("version", n(1)), ("grant_digest", s(ZoneWire.hash(canonicalGrant))),
      ("component", component), ("signed_at", n(nowMS)), ("expires_at", n(expires)),
    ])
    let signature = try key.signature(for: Data("wendy.world.authority.v1/record\0".utf8) + signed)
    return o([
      ("version", n(1)), ("grant", canonicalGrant), ("component", component),
      ("signed_at_unix_ms", n(nowMS)), ("expires_at_unix_ms", n(expires)),
      ("signature", s(signature.base64EncodedString())),
    ])
  }
}
