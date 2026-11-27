import CryptoKit
import Foundation

struct ZoneTrust: Sendable {
  let root: Data, rev: JSONValue, revision: UInt64, hash: String, until: Int64
  init(data: Data, credentials: ZoneCredentials, now: Int64) throws {
    let v = try ZoneWire.strict(data)
    guard Set(v.object?.keys.map { $0 } ?? []) == ["root_public_key", "revocations"],
      let root = v["root_public_key"]?.string.flatMap({ Data(base64Encoded: $0) }),
      root.count == 32, let signed = v["revocations"], let rev = signed["revocations"],
      let revision = rev["revision"]?.uint, revision > 0, rev["version"]?.int == 1,
      rev["tenant"]?.string == credentials.tenant, rev["site"]?.string == credentials.site,
      rev["domain"]?.string == "simulation", let issued = rev["issued_at_unix_ms"]?.int,
      let until = rev["expires_at_unix_ms"]?.int, issued > 0, issued <= now, now < until,
      until > issued, until - issued <= 3_600_000,
      let signature = signed["signature"]?.string.flatMap({ Data(base64Encoded: $0) }),
      Set(signed.object?.keys.map { $0 } ?? []) == ["revocations", "signature"]
    else { throw ZoneError.expired }
    let names = [
      "version", "tenant", "site", "domain", "revision", "issued_at_unix_ms", "expires_at_unix_ms",
      "grant_ids", "key_ids", "principals",
    ]
    guard Set(rev.object?.keys.map { $0 } ?? []) == Set(names) else { throw ZoneError.invalid }
    for key in ["grant_ids", "key_ids", "principals"] {
      guard rev[key] == .null || (rev[key]?.array?.count ?? 4097) <= 4096 else {
        throw ZoneError.invalid
      }
      if let a = rev[key]?.array {
        guard a.allSatisfy({ $0.string != nil }), Set(a.compactMap(\.string)).count == a.count
        else { throw ZoneError.invalid }
      }
    }
    let raw = try ZoneWire.o(names.map { ($0, try ZoneTrust.value(rev[$0]!)) })
    guard
      try Curve25519.Signing.PublicKey(rawRepresentation: root).isValidSignature(
        signature, for: Data("wendy.world.authority.v1/revocations\0".utf8) + raw)
    else { throw ZoneError.authority }
    self.root = root
    self.rev = rev
    self.revision = revision
    self.hash = ZoneWire.hash(raw)
    self.until = until
  }
  static func value(_ v: JSONValue) throws -> Data {
    switch v {
    case .string(let s): return ZoneWire.s(s)
    case .array(let a): return try ZoneWire.array(a.map(value))
    default: return try v.encoded()
    }
  }
  func verifyGrant(
    _ signed: JSONValue, credentials: ZoneCredentials, issued: Int64, now: Int64, write: Bool = true
  ) throws -> Curve25519.Signing.PublicKey {
    guard now < until, let g = signed["grant"], g["version"]?.int == 1,
      g["domain"]?.string == "simulation", g["tenant"]?.string == credentials.tenant,
      g["site"]?.string == credentials.site, g["kind"]?.string == "operator",
      let before = g["not_before_unix_ms"]?.int, let after = g["not_after_unix_ms"]?.int,
      before > 0, after > before, after - before <= 86_400_000, before <= issued, issued < after,
      let principal = g["principal"]?.string, let id = g["id"]?.string,
      let key = g["public_key"]?.string.flatMap({ Data(base64Encoded: $0) }),
      let sig = signed["signature"]?.string.flatMap({ Data(base64Encoded: $0) }),
      let scopes = g["scopes"]?.array?.compactMap(\.string),
      !write || Set(scopes).isSuperset(of: ["publish", "site-publish"])
    else { throw ZoneError.authority }
    let canonical = try GoAuthorityJSON.signedGrant(signed)
    let body = try ZoneWire.raw(canonical, ["grant"])
    guard
      try Curve25519.Signing.PublicKey(rawRepresentation: root).isValidSignature(
        sig, for: Data("wendy.world.authority.v1/grant\0".utf8) + body),
      !(rev["principals"]?.array?.contains(.string(principal)) ?? false),
      !(rev["grant_ids"]?.array?.contains(.string(id)) ?? false),
      !(rev["key_ids"]?.array?.contains(.string(ZoneWire.hash(key))) ?? false)
    else { throw ZoneError.authority }
    return try Curve25519.Signing.PublicKey(rawRepresentation: key)
  }
}

public actor ZoneClient {
  public nonisolated let credentials: ZoneCredentials
  private var trust: ZoneTrust
  private var lastTime: Int64
  private let nowSource: @Sendable () -> Int64
  public init(credentials: ZoneCredentials, trustData: Data) throws {
    try self.init(credentials: credentials, trustData: trustData,
                  nowSource: { Int64(Date().timeIntervalSince1970 * 1000) })
  }
  init(credentials: ZoneCredentials, trustData: Data, nowSource: @escaping @Sendable () -> Int64) throws {
    let now = nowSource()
    self.nowSource = nowSource
    self.credentials = credentials
    trust = try ZoneTrust(data: trustData, credentials: credentials, now: now)
    lastTime = now
    _ = try trust.verifyGrant(
      credentials.grant, credentials: credentials, issued: now, now: now, write: false)
  }
  public func updateTrust(_ data: Data) throws {
    let now = try clock(allowExpiredTrust: true)
    let candidate = try ZoneTrust(data: data, credentials: credentials, now: now)
    guard candidate.root == trust.root, candidate.revision >= trust.revision,
      candidate.revision != trust.revision || candidate.hash == trust.hash
    else { throw ZoneError.changedTrust }
    trust = candidate
  }
  private func clock(allowExpiredTrust: Bool = false) throws -> Int64 {
    let now = nowSource()
    guard now >= lastTime else { throw ZoneError.expired }
    lastTime = now
    guard allowExpiredTrust || now < trust.until else { throw ZoneError.expired }
    return now
  }
  private func request(_ path: String, operation: String = "read", body: Data = Data()) async throws
    -> Data
  {
    let now = try clock()
    _ = try trust.verifyGrant(
      credentials.grant, credentials: credentials, issued: now, now: now,
      write: operation == "publish")
    var r = URLRequest(
      url: credentials.endpoint.appendingPathComponent(path),
      cachePolicy: .reloadIgnoringLocalCacheData, timeoutInterval: 2)
    r.httpMethod = operation == "read" ? "GET" : "POST"
    if operation != "read" { r.httpBody = body }
    r.setValue("application/json", forHTTPHeaderField: "Content-Type")
    r.setValue(
      try credentials.authorization(operation: operation, body: body, now: now),
      forHTTPHeaderField: "X-World-Authorization")
    let raw = try await BoundedRequest(limit: 1 << 20, resourceTimeout: 2).run(r)
    let done = try clock()
    _ = try trust.verifyGrant(
      credentials.grant, credentials: credentials, issued: done, now: done,
      write: operation == "publish")
    return raw
  }
  public func list() async throws -> [ZoneRevision] {
    let raw = try await request("v1/zones")
    let v = try ZoneWire.strict(raw)
    guard Set(v.object?.keys.map { $0 } ?? []) == ["records"] else { throw ZoneError.invalid }
    let records = try ZoneWire.elements(ZoneWire.raw(raw, ["records"]))
    guard records.count <= 10000 else { throw ZoneError.invalid }
    var rows = [ZoneRevision]()
    var seen = Set<String>()
    for record in records {
      let r = try verify(record)
      guard seen.insert(r.id).inserted else { throw ZoneError.invalid }
      rows.append(r)
    }
    let now = try clock()
    guard rows.allSatisfy({ $0.deadline.timeIntervalSince1970 * 1000 > Double(now) }) else {
      throw ZoneError.expired
    }
    return rows
  }
  public func publish(context: ZoneContext, name: String, revision: String, zones: [ZoneRectangle])
    async throws -> ZoneRevision
  {
    let epochReply = try ZoneWire.strict(
      await request("v1/publishers/epoch", operation: "publish", body: Data("{}".utf8)))
    guard let epoch = epochReply["epoch"]?.uint, epoch > 0 else { throw ZoneError.invalid }
    let now = try clock()
    let body = try credentials.publication(
      context: context, name: name, revision: revision, zones: zones, epoch: epoch, nowMS: now)
    let selected = try verify(body)
    _ = try await request("v1/components", operation: "publish", body: body)
    return selected
  }
  public func delete(_ selected: ZoneRevision) async throws {
    guard selected.source == credentials.source, !selected.deleted else {
      throw ZoneError.authority
    }
    _ = try verify(selected.record)
    let epochReply = try ZoneWire.strict(
      await request("v1/publishers/epoch", operation: "publish", body: Data("{}".utf8)))
    guard let epoch = epochReply["epoch"]?.uint else { throw ZoneError.invalid }
    let body = try credentials.deletion(selected, epoch: epoch, nowMS: clock())
    _ = try await request("v1/components", operation: "publish", body: body)
  }
  public func verify(_ raw: Data) throws -> ZoneRevision {
    let r = try ZoneWire.strict(raw, limit: 64 << 10)
    let now = try clock()
    guard
      Set(r.object?.keys.map { $0 } ?? []) == [
        "version", "grant", "component", "signed_at_unix_ms", "expires_at_unix_ms", "signature",
      ], r["version"]?.int == 1, let grant = r["grant"], let issued = r["signed_at_unix_ms"]?.int,
      let expires = r["expires_at_unix_ms"]?.int, issued > 0, issued <= now, now < expires,
      expires > issued, expires - issued <= 86_400_000,
      let signature = r["signature"]?.string.flatMap({ Data(base64Encoded: $0) }),
      let c = r["component"], let source = c["key"]?["source"], source == grant["grant"]?["source"],
      c["key"]?["tenant"]?.string == credentials.tenant,
      c["key"]?["site"]?.string == credentials.site, c["key"]?["type"]?.string == ZoneWire.type,
      let entity = c["key"]?["entity"]?.string, entity.hasPrefix("zone-catalog/"),
      c["frame"] == .object(["id": .string("site-metadata"), "revision": .string("v1")]),
      let epoch = c["version"]?["epoch"]?.uint, epoch > 0,
      let sequence = c["version"]?["sequence"]?.uint, sequence > 0, let ttl = c["ttl_ns"]?.int,
      ttl >= 1_000_000, ttl <= 86_400_000_000_000, let deleted = c["tombstone"]?.bool,
      let timing = c["timing"], timing["error_known"]?.bool == true,
      let errorNS = timing["error_bound_ns"]?.int, errorNS >= 0, errorNS < ttl,
      let hint = timing["age_hint_ns"]?.int, hint >= 0, hint < ttl,
      let provenance = c["provenance"], provenance["origin"]?.string == "simulated",
      let sim = provenance["simulation"], let run = sim["run_id"]?.string, !run.isEmpty,
      let revision = sim["scenario_version"]?.string, !revision.isEmpty,
      sim["clock_domain"]?.string == "simulation:" + run,
      timing["clock_domain"]?.string == "simulation:" + run
    else { throw ZoneError.invalid }
    guard
      Set(c["key"]?.object?.keys.map { $0 } ?? []) == [
        "tenant", "site", "entity", "type", "source",
      ], Set(c["version"]?.object?.keys.map { $0 } ?? []) == ["epoch", "sequence"],
      Set(timing.object?.keys.map { $0 } ?? []) == [
        "sample_time", "clock_domain", "error_known", "error_bound_ns", "age_hint_ns",
      ],
      Set(sim.object?.keys.map { $0 } ?? []) == [
        "run_id", "scenario_version", "seed", "clock_domain",
      ], sim["seed"]?.int != nil,
      Set(provenance.object?.keys.map { $0 } ?? []).isSubset(of: [
        "origin", "app_version", "simulation", "model_id", "model_version",
      ]), let app = provenance["app_version"]?.string, !app.isEmpty
    else { throw ZoneError.invalid }
    _ = try GoAuthorityJSON.source(source)
    let key = try trust.verifyGrant(grant, credentials: credentials, issued: issued, now: now)
    let canonicalGrant = try GoAuthorityJSON.signedGrant(grant)
    let component = try ZoneWire.raw(raw, ["component"])
    let names =
      ["key", "version", "frame", "timing", "ttl_ns", "provenance"] + (deleted ? [] : ["payload"])
      + ["tombstone"]
    guard Set(c.object?.keys.map { $0 } ?? []) == Set(names) else { throw ZoneError.invalid }
    let unsignedTiming = ZoneWire.o([
      ("sample_time", ZoneWire.s(timing["sample_time"]?.string ?? "")),
      ("clock_domain", ZoneWire.s("simulation:" + run)), ("error_known", Data("true".utf8)),
      ("error_bound_ns", ZoneWire.n(errorNS)), ("age_hint_ns", ZoneWire.n(0)),
    ])
    let canonical = try ZoneWire.o(
      names.map { ($0, $0 == "timing" ? unsignedTiming : try ZoneWire.raw(component, [$0])) })
    let signed = ZoneWire.o([
      ("version", ZoneWire.n(1)), ("grant_digest", ZoneWire.s(ZoneWire.hash(canonicalGrant))),
      ("component", canonical), ("signed_at", ZoneWire.n(issued)),
      ("expires_at", ZoneWire.n(expires)),
    ])
    guard
      key.isValidSignature(signature, for: Data("wendy.world.authority.v1/record\0".utf8) + signed)
    else { throw ZoneError.authority }
    let sampled = try ZoneWire.date(timing["sample_time"]?.string)
    let date = Date(timeIntervalSince1970: Double(now) / 1000)
    var deadline = min(
      sampled.addingTimeInterval(Double(ttl - errorNS) / 1e9),
      date.addingTimeInterval(Double(ttl - hint) / 1e9),
      Date(timeIntervalSince1970: Double(min(expires, trust.until)) / 1000))
    guard sampled <= date, deadline > date else { throw ZoneError.expired }
    let digest = String(entity.dropFirst(13))
    guard digest.count == 64,
      digest.utf8.allSatisfy({ (48...57).contains($0) || (97...102).contains($0) })
    else { throw ZoneError.invalid }
    if deleted {
      guard deadline.timeIntervalSince1970 * 1000 > Double(try clock()) else { throw ZoneError.expired }
      return ZoneRevision(
        digest: digest, source: source, name: "Deleted revision", revision: revision,
        context: ZoneContext(
          tenant: credentials.tenant, site: credentials.site, runID: run, frameID: "unavailable",
          frameRevision: "unavailable"), zones: [], deleted: true, deadline: deadline, record: raw)
    }
    let payload = try ZoneWire.raw(component, ["payload"])
    let p = try ZoneWire.strict(payload, limit: 16 << 10)
    guard ZoneWire.hash(payload) == digest,
      Set(p.object?.keys.map { $0 } ?? []) == [
        "schema_version", "name", "revision", "site", "run_id", "frame", "valid_from",
        "valid_until", "zones",
      ], p["schema_version"]?.string == ZoneWire.type, p["site"]?.string == credentials.site,
      p["run_id"]?.string == run, p["revision"]?.string == revision, let name = p["name"]?.string,
      let frame = p["frame"], let fid = frame["id"]?.string, let frev = frame["revision"]?.string,
      Set(frame.object?.keys.map { $0 } ?? []) == ["id", "revision"]
    else { throw ZoneError.invalid }
    let context = ZoneContext(
      tenant: credentials.tenant, site: credentials.site, runID: run, frameID: fid,
      frameRevision: frev)
    try context.validate()
    let from = try ZoneWire.date(p["valid_from"]?.string)
    let until = try ZoneWire.date(p["valid_until"]?.string)
    guard from <= sampled, from < until, until.timeIntervalSince(from) <= 30 * 86400,
      sampled.addingTimeInterval(Double(ttl) / 1e9) <= until, !name.isEmpty, name.utf8.count <= 120,
      revision.utf8.count <= 120
    else { throw ZoneError.invalid }
    deadline = min(deadline, until)
    let zoneValues = p["zones"]?.array ?? []
    guard
      zoneValues.allSatisfy({
        Set($0.object?.keys.map { $0 } ?? []) == [
          "id", "label", "min_xz", "max_xz", "dwell_seconds",
        ]
      })
    else { throw ZoneError.invalid }
    let zones = try JSONDecoder().decode(
      [ZoneRectangle].self, from: ZoneWire.raw(payload, ["zones"]))
    guard (1...16).contains(zones.count), Set(zones.map(\.id)).count == zones.count else {
      throw ZoneError.invalid
    }
    for z in zones { try z.validate() }
    guard deadline.timeIntervalSince1970 * 1000 > Double(try clock()) else { throw ZoneError.expired }
    return ZoneRevision(
      digest: digest, source: source, name: name, revision: revision, context: context,
      zones: zones, deleted: false, deadline: deadline, record: raw)
  }
}
