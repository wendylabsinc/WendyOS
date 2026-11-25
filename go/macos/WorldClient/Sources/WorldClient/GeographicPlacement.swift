import Foundation

/// A display interpretation of a gateway publication, not origin-signature or
/// survey verification. The original JSON remains unchanged in the read cache.
public struct GeographicPlacement: Sendable {
    public enum State: String, Sendable {
        case usable, unavailable, unsupported, invalid, notYetValid, expired
        public var message: String {
            switch self {
            case .usable: "Placement within its validity interval"
            case .unavailable: "Geographic placement unavailable"
            case .unsupported: "Unsupported geographic placement version"
            case .invalid: "Invalid geographic placement"
            case .notYetValid: "Geographic placement is not yet valid"
            case .expired: "Geographic placement has expired"
            }
        }
    }

    public enum AngularError: Equatable, Sendable {
        case unknown, bounded(Double)
        public var description: String {
            switch self {
            case .unknown: "unknown"
            case .bounded(let radians): String(format: "%.3g° declared bound", locale: Locale(identifier: "en_US_POSIX"), radians * 180 / .pi)
            }
        }
    }

    private struct Frame: Hashable, Sendable { let id: String; let revision: String }
    private struct Details: Sendable {
        let from: Date; let until: Date
        let latitude: Double; let longitude: Double; let uncertainty: Double
        let rootError: AngularError; let frameErrors: [AngularError]
    }
    private enum Invalid: Error { case document }
    public let document: JSONValue
    private let details: Details?
    private let failure: State

    public init(document: JSONValue) {
        self.document = document
        if document.object?.isEmpty == true {
            details = nil; failure = .unavailable
        } else if let schema = document["schema_version"]?.string,
                  !["wendy.world.georeference.v1", "wendy.world.georeference.v2"].contains(schema) {
            details = nil; failure = .unsupported
        } else {
            details = try? Self.parse(document)
            failure = .invalid
        }
    }

    public func state(at now: Date) -> State {
        guard let details, now.timeIntervalSince1970.isFinite else { return failure }
        if now < details.from { return .notYetValid }
        if now >= details.until { return .expired }
        return .usable
    }
    public var latitude: Double? { details?.latitude }
    public var longitude: Double? { details?.longitude }
    public var anchorUncertaintyMetres: Double? { details?.uncertainty }
    public var rootOrientationError: AngularError? { details?.rootError }
    public var frameOrientationErrors: [AngularError]? { details?.frameErrors }
    public var rootOrientationText: String {
        guard let details else { return "Root orientation: unavailable" }
        return "Root orientation: " + details.rootError.description
    }
    public var frameOrientationText: String {
        guard let details else { return "Frame orientation: unavailable" }
        guard !details.frameErrors.isEmpty else { return "No child-frame placements" }
        let unknown = details.frameErrors.filter { $0 == .unknown }.count
        return "Frame orientation: \(details.frameErrors.count - unknown) declared bounds · \(unknown) unknown"
    }
    public static let uncertaintyExplanation = "Anchor and frame translation bounds are not a total position error. Orientation error depends on distance from each frame origin."

    private static func object(_ raw: JSONValue?, keys: Set<String>) throws -> [String: JSONValue] {
        guard let object = raw?.object, Set(object.keys) == keys else { throw Invalid.document }
        return object
    }
    private static func string(_ raw: JSONValue?, bound: Int? = nil) throws -> String {
        guard let text = raw?.string, !text.isEmpty else { throw Invalid.document }
        if let bound, text.utf8.count > bound { throw Invalid.document }
        return text
    }
    private static func number(_ raw: JSONValue?, lower: Double = -.greatestFiniteMagnitude, upper: Double = .greatestFiniteMagnitude) throws -> Double {
        guard let value = raw?.double, value.isFinite, lower <= value, value <= upper else { throw Invalid.document }
        return value
    }
    private static func vector(_ raw: JSONValue?, count: Int, bound: Double) throws -> [Double] {
        guard let values = raw?.array, values.count == count else { throw Invalid.document }
        return try values.map { try number($0, lower: -bound, upper: bound) }
    }
    private static func frame(_ raw: JSONValue?) throws -> Frame {
        let fields = try object(raw, keys: ["id", "revision"])
        return try Frame(id: string(fields["id"], bound: 256), revision: string(fields["revision"], bound: 256))
    }
    private static func interval(_ fields: [String: JSONValue]) throws -> (Date, Date) {
        guard let from = fields["valid_from"]?.string.flatMap(WorldComponent.parseTime),
              let until = fields["valid_until"]?.string.flatMap(WorldComponent.parseTime),
              from != WorldComponent.parseTime("0001-01-01T00:00:00Z"), until > from else { throw Invalid.document }
        return (from, until)
    }
    private static func angular(_ raw: JSONValue?) throws -> AngularError {
        guard let fields = raw?.object, let known = fields["known"]?.bool else { throw Invalid.document }
        if !known {
            guard Set(fields.keys) == ["known"] else { throw Invalid.document }
            return .unknown
        }
        guard Set(fields.keys) == ["known", "max_angle_radians"] else { throw Invalid.document }
        return try .bounded(number(fields["max_angle_radians"], lower: 0, upper: .pi))
    }
    private static func parse(_ raw: JSONValue) throws -> Details {
        let schema = try string(raw["schema_version"])
        guard schema == "wendy.world.georeference.v1" || schema == "wendy.world.georeference.v2" else { throw Invalid.document }
        let v2 = schema.hasSuffix(".v2")
        var keys: Set<String> = ["schema_version", "site", "revision", "package_sha256", "root", "anchor", "axes", "placement", "placement_source", "valid_from", "valid_until", "frames"]
        if v2 { keys.insert("root_basis_error") }
        let fields = try object(raw, keys: keys)
        _ = try string(fields["site"]); _ = try string(fields["revision"]); _ = try string(fields["placement_source"])
        let digest = try string(fields["package_sha256"])
        guard digest.range(of: "^[0-9a-f]{64}$", options: .regularExpression) != nil,
              ["illustrative", "estimated", "surveyed"].contains(try string(fields["placement"])),
              fields["axes"] == .array([.string("east"), .string("up"), .string("south")]) else { throw Invalid.document }
        let root = try frame(fields["root"]), validity = try interval(fields)
        let anchor = try object(fields["anchor"], keys: ["latitude_degrees", "longitude_degrees", "altitude_metres", "altitude_datum", "uncertainty_metres"])
        let latitude = try number(anchor["latitude_degrees"], lower: -90, upper: 90)
        let longitude = try number(anchor["longitude_degrees"], lower: -180, upper: 180)
        _ = try number(anchor["altitude_metres"], lower: -10_000, upper: 100_000)
        guard anchor["altitude_datum"]?.string == "WGS84-ellipsoid" else { throw Invalid.document }
        let uncertainty = try number(anchor["uncertainty_metres"], lower: 0, upper: 1e9)
        let rootError: AngularError = try v2 ? angular(fields["root_basis_error"]) : .unknown
        guard let frames = fields["frames"]?.array, frames.count <= 4096 else { throw Invalid.document }
        var parents: [Frame: Frame] = [:], errors: [AngularError] = []
        var frameKeys: Set<String> = ["ref", "parent", "translation", "rotation_xyzw", "valid_from", "valid_until", "uncertainty_metres"]
        if v2 { frameKeys.insert("rotation_error") }
        for rawFrame in frames {
            let f = try object(rawFrame, keys: frameKeys), ref = try frame(f["ref"]), parent = try frame(f["parent"])
            guard ref != root, ref.id != parent.id, parents[ref] == nil else { throw Invalid.document }
            _ = try vector(f["translation"], count: 3, bound: 1e9)
            let q = try vector(f["rotation_xyzw"], count: 4, bound: 2)
            guard abs(q.reduce(0) { $0 + $1 * $1 } - 1) <= 1e-6 else { throw Invalid.document }
            _ = try interval(f); _ = try number(f["uncertainty_metres"], lower: 0, upper: 1e9)
            errors.append(try v2 ? angular(f["rotation_error"]) : .unknown)
            parents[ref] = parent
        }
        for start in parents.keys {
            var current = start, seen: Set<Frame> = []
            while current != root {
                guard seen.count < 128, seen.insert(current).inserted, let parent = parents[current] else { throw Invalid.document }
                current = parent
            }
        }
        return Details(from: validity.0, until: validity.1, latitude: latitude, longitude: longitude,
                       uncertainty: uncertainty, rootError: rootError, frameErrors: errors)
    }
}
