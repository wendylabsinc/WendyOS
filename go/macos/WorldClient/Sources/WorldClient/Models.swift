import Foundation

public enum ObservationFreshness: String, Codable, Sendable {
    case fresh, stale, unknown
}

public enum WorldReadMode: String, Codable, Sendable {
    case full
    case nativeInference = "native_inference"
    /// Explicit profile capability, never inferred from a failed endpoint.
    public init(profile: JSONValue) throws {
        guard let value = profile["read_mode"] else { self = .full; return }
        guard let name = value.string, let mode = Self(rawValue: name) else { throw WorldClientError.invalidCredentials }
        self = mode
    }
}

/// A partial, versioned component. Unknown payloads and integer precision survive
/// round trips; views must not interpret a missing field as a zero measurement.
public struct WorldComponent: Codable, Sendable, Equatable {
    public let raw: JSONValue
    public init(raw: JSONValue) throws {
        guard raw.object != nil else { throw WorldClientError.invalidResponse }
        self.raw = raw
    }
    public var entity: String? { raw["key"]?["entity"]?.string }
    public var type: String? { raw["key"]?["type"]?.string }
    public var origin: String? { raw["provenance"]?["origin"]?.string }
    public var frameID: String? { raw["frame"]?["id"]?.string }
    public var frameRevision: String? { raw["frame"]?["revision"]?.string }
    public var sampleTime: Date? { raw["timing"]?["sample_time"]?.string.flatMap(Self.parseTime) }

    /// Freshness needs a comparable wall clock and known uncertainty. This is a
    /// display estimate, not an authorization or robot safety decision.
    public func freshness(at now: Date = Date()) -> ObservationFreshness {
        if raw["tombstone"]?.bool == true { return .stale }
        guard let ttl = raw["ttl_ns"]?.int else { return .unknown }
        guard ttl > 0 else { return .stale }
        guard let timing = raw["timing"],
              timing["clock_domain"]?.string == "utc",
              timing["error_known"]?.bool == true,
              let error = timing["error_bound_ns"]?.int, error >= 0,
              let hint = timing["age_hint_ns"]?.int, hint >= 0,
              let sample = sampleTime else { return .unknown }
        let estimate = now.timeIntervalSince(sample)
        let bound = Double(error) / 1_000_000_000
        guard estimate >= -bound else { return .unknown }
        let upperAge = max(Double(hint) / 1_000_000_000, max(0, estimate) + bound)
        return upperAge < Double(ttl) / 1_000_000_000 ? .fresh : .stale
    }

    static func parseTime(_ value: String) -> Date? {
        let fractional = ISO8601DateFormatter()
        fractional.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        if let date = fractional.date(from: value) { return date }
        let whole = ISO8601DateFormatter()
        whole.formatOptions = [.withInternetDateTime]
        return whole.date(from: value)
    }
}

public struct WorldSnapshot: Codable, Sendable, Equatable {
    public let components: [WorldComponent]
    public let cursor: String
    public let originalJSON: Data

    public init(data: Data) throws {
        let value = try JSONValue.decode(data)
        guard let components = value["components"]?.array, components.count <= 10_000,
              let cursor = value["cursor"]?.string, cursor.utf8.count <= 1024 else { throw WorldClientError.invalidResponse }
        self.components = try components.map(WorldComponent.init(raw:))
        self.cursor = cursor
        self.originalJSON = data
    }

    // Only the response is serialized. Derived fields cannot disagree with the
    // original response after a cache is loaded.
    enum CodingKeys: String, CodingKey { case originalJSON }
    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        try self.init(data: c.decode(Data.self, forKey: .originalJSON))
    }
    public func encode(to encoder: any Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(originalJSON, forKey: .originalJSON)
    }
}

/// Bytes are available only after the complete requested digest is verified.
public struct VerifiedEvidence: Sendable {
    public let data: Data
    public let sha256: String
    public let contentType: String
    public let retainUntil: Date
}

/// An offline read model is always last-known state. fetchedAt describes the
/// fetch, never the age of an observation or freshness of a task status.
public struct CachedWorldReadModel: Codable, Sendable, Equatable {
    public let snapshot: WorldSnapshot
    public let sites: JSONValue
    public let tasks: JSONValue
    public let fetchedAt: Date
    public let scopeID: String
    public let readMode: WorldReadMode
    public var isLastKnown: Bool { true }
    public init(snapshot: WorldSnapshot, sites: JSONValue, tasks: JSONValue, fetchedAt: Date, scopeID: String, readMode: WorldReadMode = .full) {
        self.snapshot = snapshot; self.sites = sites; self.tasks = tasks
        self.fetchedAt = fetchedAt; self.scopeID = scopeID
        self.readMode = readMode
    }
    enum CodingKeys: String, CodingKey { case snapshot, sites, tasks, fetchedAt, scopeID; case readMode = "read_mode" }
    public init(from decoder: any Decoder) throws {
        let values = try decoder.container(keyedBy: CodingKeys.self)
        snapshot = try values.decode(WorldSnapshot.self, forKey: .snapshot)
        sites = try values.decode(JSONValue.self, forKey: .sites)
        tasks = try values.decode(JSONValue.self, forKey: .tasks)
        fetchedAt = try values.decode(Date.self, forKey: .fetchedAt)
        scopeID = try values.decode(String.self, forKey: .scopeID)
        readMode = try values.decodeIfPresent(WorldReadMode.self, forKey: .readMode) ?? .full
    }
}
