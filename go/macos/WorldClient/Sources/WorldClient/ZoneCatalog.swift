import CryptoKit
import Foundation

public enum ZoneError: Error, LocalizedError, Sendable {
  case invalid, authority, expired, changedTrust, unavailable
  public var errorDescription: String? {
    switch self {
    case .invalid: "Zone data or its signed binding is invalid."
    case .authority: "An existing operator publish and site-publish grant is required."
    case .expired: "Zone authority, trust or catalog validity has expired."
    case .changedTrust: "Zone trust changed or regressed."
    case .unavailable: "This revision is unavailable at the configured replica."
    }
  }
}
public struct ZoneRectangle: Sendable, Equatable, Codable, Identifiable {
  public var id: String
  public var label: String
  public var minXZ: [Double]
  public var maxXZ: [Double]
  public var dwellSeconds: Double
  public init(id: String, label: String, minXZ: [Double], maxXZ: [Double], dwellSeconds: Double) {
    self.id = id
    self.label = label
    self.minXZ = minXZ
    self.maxXZ = maxXZ
    self.dwellSeconds = dwellSeconds
  }
  enum CodingKeys: String, CodingKey {
    case id, label
    case minXZ = "min_xz"
    case maxXZ = "max_xz"
    case dwellSeconds = "dwell_seconds"
  }
  public func validate() throws {
    guard !id.isEmpty, id.utf8.count <= 80, label.utf8.count <= 120, minXZ.count == 2,
      maxXZ.count == 2,
      minXZ.allSatisfy({ $0.isFinite && abs($0) <= 10000 }),
      maxXZ.allSatisfy({ $0.isFinite && abs($0) <= 10000 }),
      minXZ[0] < maxXZ[0], minXZ[1] < maxXZ[1], dwellSeconds.isFinite, dwellSeconds > 0,
      dwellSeconds <= 3600
    else { throw ZoneError.invalid }
  }
}
public struct ZoneContext: Sendable, Codable, Equatable {
  public let tenant: String, site: String, runID: String, frameID: String, frameRevision: String
  public init(tenant: String, site: String, runID: String, frameID: String, frameRevision: String) {
    self.tenant = tenant
    self.site = site
    self.runID = runID
    self.frameID = frameID
    self.frameRevision = frameRevision
  }
  public func validate() throws {
    guard
      [tenant, site, runID, frameID, frameRevision].allSatisfy({
        !$0.isEmpty && $0.utf8.count <= 120
          && $0.trimmingCharacters(in: .whitespacesAndNewlines) == $0
      })
    else { throw ZoneError.invalid }
  }
}
public struct ZoneRevision: Sendable, Identifiable {
  public let digest: String, source: JSONValue, name: String, revision: String
  public let context: ZoneContext
  public let zones: [ZoneRectangle]
  public let deleted: Bool
  public let deadline: Date
  public let record: Data
  public var id: String {
    digest + ":" + ((try? source.encoded()).flatMap { String(data: $0, encoding: .utf8) } ?? "")
  }
  public var selection: JSONValue {
    .object([
      "sha256": .string(digest), "source": source, "run_id": .string(context.runID),
      "frame": .object(["id": .string(context.frameID), "revision": .string(context.frameRevision)]
      ),
    ])
  }
}

enum ZoneWire {
  static let type = "wendy.zone.catalog.v1"
  static let o = GoAuthorityJSON.object, s = GoAuthorityJSON.string
  static func n(_ x: Int64) -> Data { Data(String(x).utf8) }
  static func hash(_ d: Data) -> String {
    SHA256.hash(data: d).map { String(format: "%02x", $0) }.joined()
  }
  static func time(_ ms: Int64) -> String {
    let f = ISO8601DateFormatter()
    f.formatOptions = [.withInternetDateTime]
    let seconds = ms / 1000
    let base = f.string(from: Date(timeIntervalSince1970: Double(seconds))).dropLast()
    let fraction = String(format: "%03lld", ms % 1000).replacingOccurrences(
      of: "0+$", with: "", options: .regularExpression)
    return String(base) + (fraction.isEmpty ? "" : "." + fraction) + "Z"
  }
  static func date(_ text: String?) throws -> Date {
    guard let text, text.utf8.count <= 64 else { throw ZoneError.invalid }
    let f = ISO8601DateFormatter()
    f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    if let d = f.date(from: text) { return d }
    f.formatOptions = [.withInternetDateTime]
    guard let d = f.date(from: text) else { throw ZoneError.invalid }
    return d
  }
  // Native authoring uses millimetres and milliseconds. Decimal spelling is
  // independent of Foundation floating-point formatting and matches Go JSON.
  static func decimal(_ value: Double) throws -> Data {
    guard value.isFinite, abs(value) <= 10000 else { throw ZoneError.invalid }
    let scaled = (value * 1000).rounded()
    guard abs(value * 1000 - scaled) < 0.000001 else { throw ZoneError.invalid }
    let x = Int64(scaled)
    let v = abs(x)
    let tail = String(format: "%03lld", v % 1000).replacingOccurrences(
      of: "0+$", with: "", options: .regularExpression)
    return Data(((x < 0 ? "-" : "") + String(v / 1000) + (tail.isEmpty ? "" : "." + tail)).utf8)
  }
  static func array(_ values: [Data]) -> Data {
    Data("[".utf8)
      + values.enumerated().reduce(into: Data()) { r, v in
        if v.offset > 0 { r.append(44) }
        r.append(v.element)
      } + Data("]".utf8)
  }
  static func catalog(
    context: ZoneContext, name: String, revision: String, zones: [ZoneRectangle], from: Int64,
    until: Int64
  ) throws -> Data {
    try context.validate()
    guard
      [name, revision].allSatisfy({
        !$0.isEmpty && $0.utf8.count <= 120
          && $0.trimmingCharacters(in: .whitespacesAndNewlines) == $0
      }), from > 0, until > from, until - from <= 30 * 86_400_000, (1...16).contains(zones.count),
      Set(zones.map(\.id)).count == zones.count
    else { throw ZoneError.invalid }
    let entries = try zones.sorted { $0.id.utf8.lexicographicallyPrecedes($1.id.utf8) }.map { z -> Data in
      try z.validate()
      return o([
        ("id", s(z.id)), ("label", s(z.label)), ("min_xz", try array(z.minXZ.map(decimal))),
        ("max_xz", try array(z.maxXZ.map(decimal))), ("dwell_seconds", try decimal(z.dwellSeconds)),
      ])
    }
    let data = o([
      ("schema_version", s(type)), ("name", s(name)), ("revision", s(revision)),
      ("site", s(context.site)), ("run_id", s(context.runID)),
      ("frame", o([("id", s(context.frameID)), ("revision", s(context.frameRevision))])),
      ("valid_from", s(time(from))), ("valid_until", s(time(until))), ("zones", array(entries)),
    ])
    guard data.count <= 16 << 10 else { throw ZoneError.invalid }
    return data
  }
  static func strict(_ raw: Data, limit: Int = 1 << 20) throws -> JSONValue {
    _ = try MissionRawJSON.field(raw, path: [], limit: limit)
    return try JSONValue.decode(raw, limit: limit)
  }
  static func fields(_ value: JSONValue, _ names: [String]) throws -> Data {
    guard let obj = value.object, Set(obj.keys) == Set(names) else { throw ZoneError.invalid }
    return try o(names.map { ($0, try value[$0]!.encoded()) })
  }
  static func raw(_ raw: Data, _ path: [String]) throws -> Data {
    try MissionRawJSON.field(raw, path: path, limit: 1 << 20)
  }
  static func elements(_ data: Data) throws -> [Data] {
    guard case .array = try strict(data) else { throw ZoneError.invalid }
    let b = Array(data)
    var result = [Data]()
    var depth = 0
    var quoted = false
    var escape = false
    var start = 1
    for i in 1..<(b.count - 1) {
      let c = b[i]
      if quoted {
        if escape {
          escape = false
        } else if c == 92 {
          escape = true
        } else if c == 34 {
          quoted = false
        }
        continue
      }
      if c == 34 { quoted = true }
      if c == 123 || c == 91 { depth += 1 }
      if c == 125 || c == 93 { depth -= 1 }
      if c == 44 && depth == 0 {
        result.append(Data(b[start..<i]))
        start = i + 1
      }
    }
    if start < b.count - 1 {
      let last = Data(b[start..<(b.count - 1)])
      if !String(decoding: last, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
        .isEmpty
      {
        result.append(last)
      }
    }
    return result
  }
}
