import CryptoKit
import XCTest

@testable import WorldClient

final class ZoneTests: XCTestCase {
  struct Fixture {
    let root: Curve25519.Signing.PrivateKey, credentials: ZoneCredentials, data: Data, trust: Data,
      now: Int64
    func revoked(revision: Int64 = 2, principals: [String] = [], expires: Int64? = nil) throws -> Data {
      let value = try JSONValue.decode(trust)
      var r = value["revocations"]!["revocations"]!.object!
      r["revision"] = .integer(revision)
      if let expires { r["expires_at_unix_ms"] = .integer(expires) }
      r["principals"] = .array(principals.map(JSONValue.string))
      let raw = try ZoneWire.o(
        [
          "version", "tenant", "site", "domain", "revision", "issued_at_unix_ms",
          "expires_at_unix_ms", "grant_ids", "key_ids", "principals",
        ].map { ($0, try ZoneTrust.value(r[$0]!)) })
      let sig = try root.signature(for: Data("wendy.world.authority.v1/revocations\0".utf8) + raw)
      return ZoneWire.o([
        ("root_public_key", ZoneWire.s(root.publicKey.rawRepresentation.base64EncodedString())),
        (
          "revocations",
          ZoneWire.o([("revocations", raw), ("signature", ZoneWire.s(sig.base64EncodedString()))])
        ),
      ])
    }
  }
  func fixture() throws -> Fixture {
    let root = Curve25519.Signing.PrivateKey()
    let key = Curve25519.Signing.PrivateKey()
    let now = Int64(Date().timeIntervalSince1970 * 1000)
    let source: JSONValue = .object([
      "device": .string("operator"), "app": .string("world"), "service": .string("zones"),
      "stream": .string("catalog"),
    ])
    let grant: JSONValue = .object([
      "version": .integer(1), "id": .string("zone-test"), "tenant": .string("tenant"),
      "site": .string("office"), "domain": .string("simulation"), "principal": .string("operator"),
      "kind": .string("operator"), "source": source,
      "public_key": .string(key.publicKey.rawRepresentation.base64EncodedString()),
      "publish_audience": .string("node-1"),
      "scopes": .array(["read", "publish", "site-publish"].map(JSONValue.string)),
      "not_before_unix_ms": .integer(now - 1000), "not_after_unix_ms": .integer(now + 1_800_000),
    ])
    var signed: JSONValue = .object([
      "grant": grant, "signature": .string(Data(repeating: 0, count: 64).base64EncodedString()),
    ])
    let body = try ZoneWire.raw(GoAuthorityJSON.signedGrant(signed), ["grant"])
    signed = .object([
      "grant": grant,
      "signature": .string(
        try root.signature(for: Data("wendy.world.authority.v1/grant\0".utf8) + body)
          .base64EncodedString()),
    ])
    let config: JSONValue = .object([
      "url": .string("http://127.0.0.1:12345"), "audience": .string("node-1"), "grant": signed,
      "private_key": .string(
        (key.rawRepresentation + key.publicKey.rawRepresentation).base64EncodedString()),
      "allow_insecure_simulation": .bool(true),
    ])
    let rev = ZoneWire.o([
      ("version", ZoneWire.n(1)), ("tenant", ZoneWire.s("tenant")), ("site", ZoneWire.s("office")),
      ("domain", ZoneWire.s("simulation")), ("revision", ZoneWire.n(1)),
      ("issued_at_unix_ms", ZoneWire.n(now - 1000)),
      ("expires_at_unix_ms", ZoneWire.n(now + 1_800_000)), ("grant_ids", Data("null".utf8)),
      ("key_ids", Data("null".utf8)), ("principals", Data("null".utf8)),
    ])
    let sig = try root.signature(for: Data("wendy.world.authority.v1/revocations\0".utf8) + rev)
    let trust = ZoneWire.o([
      ("root_public_key", ZoneWire.s(root.publicKey.rawRepresentation.base64EncodedString())),
      (
        "revocations",
        ZoneWire.o([("revocations", rev), ("signature", ZoneWire.s(sig.base64EncodedString()))])
      ),
    ])
    let data = try config.encoded()
    return Fixture(
      root: root, credentials: try ZoneCredentials(data: data), data: data, trust: trust, now: now)
  }
  var context: ZoneContext {
    ZoneContext(
      tenant: "tenant", site: "office", runID: "run", frameID: "floor", frameRevision: "1")
  }
  var zones: [ZoneRectangle] {
    [
      ZoneRectangle(
        id: "west", label: "A <&> area", minXZ: [-1.125, 0], maxXZ: [0.125, 2], dwellSeconds: 1.001)
    ]
  }
  func testCanonicalNativeAuthoringAndViewerIsolation() throws {
    let f = try fixture()
    XCTAssertThrowsError(try WorldCredentials(data: f.data))
    let raw = try ZoneWire.catalog(
      context: context, name: "Office", revision: "1", zones: zones, from: 1_767_225_600_000,
      until: 1_767_225_601_000)
    let string = String(decoding: raw, as: UTF8.self)
    XCTAssertTrue(string.contains("2026-01-01T00:00:00Z"))
    XCTAssertTrue(string.contains("\\u003c\\u0026\\u003e"))
    XCTAssertTrue(string.contains("[-1.125,0]"))
    var invalid = zones
    invalid[0].minXZ = [0]
    XCTAssertThrowsError(try invalid[0].validate())
    invalid = zones
    invalid[0].maxXZ[0] = Double.infinity
    XCTAssertThrowsError(try invalid[0].validate())
    XCTAssertThrowsError(try ZoneWire.decimal(0.12345))
    XCTAssertThrowsError(
      try JSONDecoder().decode(
        ZoneRectangle.self,
        from: Data(
          #"{"id":"x","label":"x","min_xz":[null,0],"max_xz":[1,1],"dwell_seconds":1}"#.utf8)))
  }
  func testZoneOrderingUsesGoUTF8Bytes() throws {
    var first = zones[0]
    first.id = "A\u{030a}"
    var second = first
    second.id = "Z"
    let raw = try ZoneWire.catalog(context: context, name: "Office", revision: "1",
                                  zones: [second, first], from: 1_767_225_600_000, until: 1_767_225_601_000)
    let object = try JSONValue.decode(raw)
    XCTAssertEqual(object["zones"]?.array?.first?["id"]?.string, first.id)
  }
  func testSignedProofScopeTamperingAndRelayAge() async throws {
    let f = try fixture()
    let client = try ZoneClient(credentials: f.credentials, trustData: f.trust)
    let record = try f.credentials.publication(
      context: context, name: "Office", revision: "1", zones: zones, epoch: 1, nowMS: f.now)
    let first = try await client.verify(record)
    XCTAssertEqual(first.context, context)
    XCTAssertFalse(first.deleted)
    let modified = String(decoding: record, as: UTF8.self).replacingOccurrences(
      of: "\"age_hint_ns\":0", with: "\"age_hint_ns\":1000")
    let age = try await client.verify(Data(modified.utf8))
    XCTAssertLessThanOrEqual(age.deadline, first.deadline)
    for pair in [
      ("\"site\":\"office\"", "\"site\":\"other\""),
      ("\"max_xz\":[0.125,2]", "\"max_xz\":[0.125,3]"), ("\"epoch\":1", "\"epoch\":2"),
    ] {
      let changed = Data(
        String(decoding: record, as: UTF8.self).replacingOccurrences(of: pair.0, with: pair.1).utf8)
      do {
        _ = try await client.verify(changed)
        XCTFail("tamper accepted")
      } catch {}
    }
    let deletion = try f.credentials.deletion(first, epoch: 2, nowMS: f.now)
    let tombstone = try await client.verify(deletion)
    XCTAssertTrue(tombstone.deleted)
    XCTAssertEqual(tombstone.digest, first.digest)
  }
  func testRevocationRollbackAndOriginalDeadline() async throws {
    let f = try fixture()
    let client = try ZoneClient(credentials: f.credentials, trustData: f.trust)
    let record = try f.credentials.publication(
      context: context, name: "Office", revision: "1", zones: zones, epoch: 1, nowMS: f.now,
      lifetimeMS: 20)
    _ = try await client.verify(record)
    try await Task.sleep(for: .milliseconds(30))
    do {
      _ = try await client.verify(record)
      XCTFail("expired catalog accepted")
    } catch {}
    try await client.updateTrust(f.revoked(principals: ["operator"]))
    do {
      try await client.updateTrust(f.trust)
      XCTFail("revocation rollback accepted")
    } catch {}
    let live = try f.credentials.publication(
      context: context, name: "Office", revision: "1", zones: zones, epoch: 2,
      nowMS: Int64(Date().timeIntervalSince1970 * 1000))
    do {
      _ = try await client.verify(live)
      XCTFail("revoked origin accepted")
    } catch {}
  }
  func testTrustCanRefreshAfterExpiryWithoutRootOrRevisionRollback() async throws {
    let f = try fixture()
    let client = try ZoneClient(credentials: f.credentials,
                               trustData: f.revoked(revision: 2, expires: f.now + 80))
    let raw = try f.credentials.publication(context: context, name: "Office", revision: "1",
                                          zones: zones, epoch: 1, nowMS: f.now)
    _ = try await client.verify(raw)
    try await Task.sleep(for: .milliseconds(100))
    do { _ = try await client.verify(raw); XCTFail("expired trust accepted") } catch {}
    do { try await client.updateTrust(f.trust); XCTFail("expired trust reset revision floor") } catch {}
    let other = try fixture()
    do { try await client.updateTrust(other.revoked(revision: 3)); XCTFail("root replacement accepted") } catch {}
    try await client.updateTrust(f.revoked(revision: 3))
    _ = try await client.verify(raw)
  }
  func testExpiredTrustObservationCannotBeUndoneByClockRollback() async throws {
    final class Clock: @unchecked Sendable {
      private let lock = NSLock()
      private var value: Int64
      init(_ value: Int64) { self.value = value }
      func set(_ value: Int64) { lock.lock(); self.value = value; lock.unlock() }
      func read() -> Int64 { lock.lock(); defer { lock.unlock() }; return value }
    }
    let f = try fixture(), clock = Clock(0)
    clock.set(f.now)
    let client = try ZoneClient(credentials: f.credentials,
      trustData: f.revoked(revision: 2, expires: f.now + 80), nowSource: { clock.read() })
    let raw = try f.credentials.publication(context: context, name: "Office", revision: "1",
      zones: zones, epoch: 1, nowMS: f.now)
    _ = try await client.verify(raw)
    clock.set(f.now + 100)
    do { _ = try await client.verify(raw); XCTFail("expired trust accepted") } catch {}
    clock.set(f.now + 40)
    do { _ = try await client.verify(raw); XCTFail("rollback reopened expired trust") } catch {}
    do { try await client.updateTrust(f.revoked(revision: 3)); XCTFail("refresh accepted backward clock") } catch {}
    clock.set(f.now + 110)
    try await client.updateTrust(f.revoked(revision: 3))
    _ = try await client.verify(raw)
  }
  func testProofCrossingDeadlineDuringVerificationIsRejected() async throws {
    final class Clock: @unchecked Sendable {
      private let lock = NSLock()
      private var values: [Int64]
      init(_ values: [Int64]) { self.values = values }
      func read() -> Int64 {
        lock.lock(); defer { lock.unlock() }
        if values.count > 1 { return values.removeFirst() }
        return values[0]
      }
    }
    let f = try fixture()
    let clock = Clock([f.now, f.now, f.now + 100])
    let client = try ZoneClient(credentials: f.credentials, trustData: f.trust,
                               nowSource: { clock.read() })
    let raw = try f.credentials.publication(context: context, name: "Office", revision: "1",
                                           zones: zones, epoch: 1, nowMS: f.now, lifetimeMS: 80)
    do { _ = try await client.verify(raw); XCTFail("verification crossed original deadline") } catch {}
  }
  func testDuplicateJSONAndUnboundedTimesRejected() throws {
    XCTAssertThrowsError(try ZoneWire.strict(Data(#"{"x":1,"x":2}"#.utf8)))
    let f = try fixture()
    var trust = try JSONValue.decode(f.trust).object!
    var signed = trust["revocations"]!.object!
    var rev = signed["revocations"]!.object!
    rev["issued_at_unix_ms"] = .integer(Int64.min)
    signed["revocations"] = .object(rev)
    trust["revocations"] = .object(signed)
    XCTAssertThrowsError(
      try ZoneClient(credentials: f.credentials, trustData: JSONValue.object(trust).encoded()))
  }
}
