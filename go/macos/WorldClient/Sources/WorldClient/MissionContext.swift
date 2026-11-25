import CryptoKit
import Foundation

/// Explicit actuator selection. The profile only narrows a verified signed
/// context; it grants no authority and never changes the supplied target.
public enum MissionNavigationProfile: Sendable, Equatable {
    case oneAxis
    case planar(floorY: Double)

    var journalSuffix: String {
        switch self {
        case .oneAxis: return ""
        case .planar(let floor): return ":planar:" + String(floor)
        }
    }
}

/// Explicit navigation context, separate from the unchanged observed trigger.
/// The executor checks this configuration digest against its commissioned map.
public struct MissionInspectionDestination: Codable, Sendable, Equatable {
    public let floorY: Double
    public let maxOffsetM: Double
    public let planarConfigSHA256: String
    public init(floorY: Double, maxOffsetM: Double, planarConfigSHA256: String) throws {
        guard floorY.isFinite, abs(floorY) <= 1_000_000, maxOffsetM.isFinite,
              maxOffsetM > 0, maxOffsetM <= 5, WorldClient.isDigest(planarConfigSHA256) else { throw MissionError.invalidDraft }
        self.floorY = floorY; self.maxOffsetM = maxOffsetM; self.planarConfigSHA256 = planarConfigSHA256
    }
    public init(json: JSONValue) throws {
        guard Set(json.object?.keys.map { $0 } ?? []) == ["version", "mode", "floor_y", "max_offset_m", "planar_config_sha256"],
              json["version"]?.int == 1, json["mode"]?.string == "planar_nearby",
              let floor = json["floor_y"]?.double, let offset = json["max_offset_m"]?.double,
              let digest = json["planar_config_sha256"]?.string else { throw MissionError.invalidDraft }
        try self.init(floorY: floor, maxOffsetM: offset, planarConfigSHA256: digest)
    }
    func canonical() throws -> Data {
        GoAuthorityJSON.object([("version", Data("1".utf8)), ("mode", GoAuthorityJSON.string("planar_nearby")),
            ("floor_y", try MissionWire.value(.number(floorY))), ("max_offset_m", try MissionWire.value(.number(maxOffsetM))),
            ("planar_config_sha256", GoAuthorityJSON.string(planarConfigSHA256))])
    }
    public init(from decoder: any Decoder) throws { try self.init(json: JSONValue(from: decoder)) }
    public func encode(to encoder: any Encoder) throws { try JSONValue.decode(canonical()).encode(to: encoder) }
}

public struct MissionContext: Sendable {
    public let signed: JSONValue
    public let canonical: Data
    public let digest: String
    public let runID: String
    public let assignee: String
    public let frameID: String
    public let frameRevision: String
    public let maxDurationMS: Int64
    public let speedMPS: Double
    public let requiredSensors: [String]
    public let navigationProfile: MissionNavigationProfile
    public var minimumEnergy: Double { policy["minimum_energy"]!.double! }
    public var areaMin: [Double] { (try? MissionWire.vector(policy["area"]?["min"])) ?? [] }
    public var areaMax: [Double] { (try? MissionWire.vector(policy["area"]?["max"])) ?? [] }
    public var packageSHA256: String { policy["package_sha256"]!.string! }
    let rootPublicKey: Data
    let policy: JSONValue
    let notBefore: Int64
    let notAfter: Int64
    static let keys = ["version", "tenant", "site", "audience", "run_id", "package_sha256", "assignee", "frame", "type", "class", "mode", "area", "required_sensors", "minimum_energy", "fallback", "speed_mps", "tolerance_m", "stop_speed_mps", "max_duration_ms", "not_before_unix_ms", "not_after_unix_ms"]

    public init(json: JSONValue, credentials: MissionCredentials, pins: AnnotationClockPins,
                navigationProfile: MissionNavigationProfile = .oneAxis) throws {
        guard let fields = json.object, Set(fields.keys) == ["context", "signature"], let policy = json["context"],
              policy["version"]?.int == 1, policy["tenant"]?.string == credentials.tenant,
              policy["site"]?.string == credentials.site, policy["audience"]?.string == credentials.audience,
              let run = policy["run_id"]?.string, run == pins.runID,
              let package = policy["package_sha256"]?.string, WorldClient.isDigest(package),
              let assignee = policy["assignee"]?.string, !assignee.isEmpty, assignee.utf8.count <= 256,
              let frame = policy["frame"], Set(frame.object?.keys.map { $0 } ?? []) == ["id", "revision"],
              let frameID = frame["id"]?.string, !frameID.isEmpty,
              let revision = frame["revision"]?.string, !revision.isEmpty,
              policy["type"]?.string == "wendy.nav.inspect.v1", policy["class"]?.string == "MOVE",
              policy["mode"]?.string == "autonomous", policy["fallback"]?.string == "hold",
              let sensors = policy["required_sensors"]?.array?.compactMap(\.string), !sensors.isEmpty,
              sensors.count <= 32, sensors.count == policy["required_sensors"]?.array?.count,
              Set(sensors).count == sensors.count, sensors.allSatisfy({ !$0.isEmpty && $0.utf8.count <= 256 }),
              let minimum = policy["minimum_energy"]?.double, (0...1).contains(minimum),
              let speed = policy["speed_mps"]?.double, speed > 0, speed <= 0.5,
              let tolerance = policy["tolerance_m"]?.double, tolerance > 0, tolerance <= 1,
              let stopSpeed = policy["stop_speed_mps"]?.double, stopSpeed >= 0, stopSpeed <= speed,
              let duration = policy["max_duration_ms"]?.int, duration > 0, duration <= 86_400_000,
              let before = policy["not_before_unix_ms"]?.int, let after = policy["not_after_unix_ms"]?.int, before > 0, after > before,
              let area = policy["area"], Set(area.object?.keys.map { $0 } ?? []) == ["min", "max"],
              let signature = json["signature"]?.string.flatMap({ Data(base64Encoded: $0) }) else { throw MissionError.invalidContext }
        let min = try MissionWire.vector(area["min"]), max = try MissionWire.vector(area["max"])
        guard zip(min, max).allSatisfy({ $0 <= $1 }) else { throw MissionError.invalidContext }
        if case .planar(let floor) = navigationProfile {
            guard floor.isFinite, min[1] <= floor, floor <= max[1], min[0] < max[0], min[2] < max[2] else { throw MissionError.invalidContext }
        }
        let body = try Self.policyBytes(policy)
        var message = Data("wendy.world.authority.v1/mission-control-context\0".utf8); message.append(body)
        guard try Curve25519.Signing.PublicKey(rawRepresentation: pins.rootPublicKey).isValidSignature(signature, for: message) else { throw MissionError.invalidContext }
        try credentials.verifyRoot(pins.rootPublicKey)
        signed = json; canonical = GoAuthorityJSON.object([("context", body), ("signature", GoAuthorityJSON.string(signature.base64EncodedString()))])
        digest = WorldCredentials.digest(canonical); runID = run; self.assignee = assignee
        self.frameID = frameID; frameRevision = revision; maxDurationMS = duration; speedMPS = speed; requiredSensors = sensors
        rootPublicKey = pins.rootPublicKey
        self.policy = policy; notBefore = before; notAfter = after
        self.navigationProfile = navigationProfile
    }

    static func policyBytes(_ value: JSONValue) throws -> Data {
        guard Set(value.object?.keys.map { $0 } ?? []) == Set(keys) else { throw MissionError.invalidContext }
        return try GoAuthorityJSON.object(keys.map { key in
            guard let item = value[key] else { throw MissionError.invalidContext }
            if key == "frame" { return (key, try MissionWire.ordered(item, keys: ["id", "revision"])) }
            if key == "area" { return (key, try MissionWire.ordered(item, keys: ["min", "max"])) }
            return (key, try MissionWire.value(item))
        })
    }
    public func validate(target: [Double], durationMS: Int64) throws {
        guard target.count == 3, target.allSatisfy(\.isFinite), durationMS > 0, durationMS <= maxDurationMS else { throw MissionError.invalidDraft }
        switch navigationProfile {
        case .oneAxis:
            guard target[0] >= -2, target[0] <= 2, target[1] == 0.2, target[2] == 0 else { throw MissionError.invalidDraft }
        case .planar(let floor):
            guard target[1] == floor else { throw MissionError.invalidDraft }
        }
        let min = try MissionWire.vector(policy["area"]?["min"]), max = try MissionWire.vector(policy["area"]?["max"])
        guard target.indices.allSatisfy({ min[$0] <= target[$0] && target[$0] <= max[$0] }) else { throw MissionError.invalidDraft }
    }
    /// Serialized review bound using the largest allowed positive deadline.
    /// No signature or delivery is performed; signing still validates its clock.
    public func reviewedInstructionPayloadSize(draft:MissionDraft)throws->Int {
        try spec(draft:draft,deadline:notAfter).count
    }
    func spec(draft: MissionDraft, deadline: Int64) throws -> Data {
        try validate(draft: draft)
        guard draft.contextSHA256 == digest, deadline <= notAfter else { throw MissionError.invalidDraft }
        var fields: [(String, Data)] = []
        if let trigger = draft.trigger { fields.append(("trigger", trigger.canonical)) }
        if let destination = draft.inspectionDestination { fields.append(("inspection_destination", try destination.canonical())) }
        fields.append(("control_context", canonical))
        if let proposal = draft.proposal { fields.append(("proposal", try proposal.canonical())) }
        fields += [("id", GoAuthorityJSON.string(draft.id)), ("type", GoAuthorityJSON.string("wendy.nav.inspect.v1")),
            ("site", try MissionWire.value(policy["site"]!)), ("assignee", GoAuthorityJSON.string(assignee)),
            ("domain", GoAuthorityJSON.string("simulation")), ("class", GoAuthorityJSON.string("MOVE")),
            ("mode", GoAuthorityJSON.string("autonomous")), ("frame", try MissionWire.ordered(policy["frame"]!, keys: ["id", "revision"])),
            ("not_after_unix_ms", Data(String(deadline).utf8)), ("area", try MissionWire.ordered(policy["area"]!, keys: ["min", "max"]))]
        for key in ["required_sensors", "minimum_energy", "fallback"] { fields.append((key, try MissionWire.value(policy[key]!))) }
        fields.append(("target", try MissionWire.value(.array(draft.target.map(JSONValue.number)))))
        for key in ["speed_mps", "tolerance_m", "stop_speed_mps"] { fields.append((key, try MissionWire.value(policy[key]!))) }
        return GoAuthorityJSON.object(fields)
    }
    func validate(draft: MissionDraft) throws {
        try PreparedProposalHandoff.validateStored(draft)
        try validate(target: draft.target, durationMS: draft.durationMS)
        guard draft.contextSHA256 == digest else { throw MissionError.invalidDraft }
        if let destination = draft.inspectionDestination {
            guard draft.trigger != nil, case .planar(let floor) = navigationProfile,
                  floor == destination.floorY else { throw MissionError.invalidDraft }
        }
        if let trigger = draft.trigger {
            guard draft.proposal != nil else { throw MissionError.invalidDraft }
            try trigger.validate(target: draft.target, inspectionDestination: draft.inspectionDestination)
            try trigger.validate(context: self)
        }
    }
}

public struct MissionProposalRef: Codable, Sendable, Equatable {
    public let audience: String
    public let id: String
    public let bodySHA256: String
    enum CodingKeys: String, CodingKey { case audience, id; case bodySHA256 = "body_sha256" }
    func canonical() throws -> Data { GoAuthorityJSON.object([("audience", GoAuthorityJSON.string(audience)), ("id", GoAuthorityJSON.string(id)), ("body_sha256", GoAuthorityJSON.string(bodySHA256))]) }
}
public struct MissionProposal: Sendable, Identifiable {
    public let id: String
    public let summary: String
    public let principal: String
    public let target: [Double]
    public let reference: MissionProposalRef
    public let expiresAtUnixMS: Int64
    public let triggerSHA256: String?
    public let inspectionDestination: MissionInspectionDestination?
    public init(view: JSONValue, context: MissionContext, credentials: MissionCredentials) throws {
        guard view["expired"]?.bool == false, let record = view["record"], let proposal = record["proposal"],
              let body = record["body"]?.string.flatMap({ Data(base64Encoded: $0) }), body.count <= 32 << 10,
              record["body_sha256"]?.string == WorldCredentials.digest(body), try JSONValue.decode(body) == proposal,
              proposal["version"]?.int == 1, proposal["tenant"]?.string == credentials.tenant, proposal["site"]?.string == credentials.site,
              proposal["run_id"]?.string == context.runID, proposal["assignee"]?.string == context.assignee,
              proposal["type"]?.string == "wendy.nav.inspect.v1", record["class"]?.string == "MOVE",
              let id = proposal["id"]?.string, MissionWire.identifier(id),
              let summary = proposal["summary"]?.string, summary.utf8.count <= 4096,
              let principal = record["principal"]?.string, !principal.isEmpty,
              let parameters = proposal["parameters"], let parameterKeys = parameters.object?.keys,
              Set(parameterKeys).isSubset(of: ["target", "trigger_sha256", "inspection_destination"]), parameterKeys.contains("target"),
              let expires = proposal["not_after_unix_ms"]?.int, expires > 0 else { throw MissionError.invalidProposal }
        try MissionProof.proposal(record: record, body: body, credentials: credentials, root: context.rootPublicKey)
        let target = try MissionWire.vector(parameters["target"])
        if let digest = parameters["trigger_sha256"] {
            guard let text = digest.string, WorldClient.isDigest(text) else { throw MissionError.invalidProposal }
            triggerSHA256 = text
        } else { triggerSHA256 = nil }
        if let raw = parameters["inspection_destination"] {
            guard triggerSHA256 != nil else { throw MissionError.invalidProposal }
            let destination = try MissionInspectionDestination(json: raw)
            guard case .planar(let floor) = context.navigationProfile, floor == destination.floorY else { throw MissionError.invalidProposal }
            inspectionDestination = destination
        } else { inspectionDestination = nil }
        try context.validate(target: target, durationMS: min(context.maxDurationMS, 30_000))
        self.id = id; self.summary = summary; self.principal = principal; self.target = target; expiresAtUnixMS = expires
        reference = MissionProposalRef(audience: credentials.audience, id: id, bodySHA256: WorldCredentials.digest(body))
    }
}
/// Explicit local origin of draft preparation. Journal v2 requires this field;
/// legacy decoding is reserved for backward-compatible trusted callers/v1 data.
public enum MissionPreparationMode: String, Codable, Sendable { case legacyTrusted, preparedReader }

public struct MissionDraft: Codable, Sendable, Equatable, Identifiable {
    public let id: String
    public let target: [Double]
    public let durationMS: Int64
    public let contextSHA256: String
    public let proposal: MissionProposalRef?
    public let trigger: MissionNativeTrigger?
    public let inspectionDestination: MissionInspectionDestination?
    public let preparedUntilUnixMS: Int64?
    public let preparedHandoff: Data?
    public let preparationMode: MissionPreparationMode
    public init(id: String = "native-" + UUID().uuidString.lowercased(), target: [Double], durationMS: Int64, context: MissionContext, proposal: MissionProposal? = nil, trigger: MissionNativeTrigger? = nil, inspectionDestination: MissionInspectionDestination? = nil, preparedUntilUnixMS: Int64? = nil, preparedHandoff: Data? = nil) throws {
        guard preparedUntilUnixMS == nil || (trigger != nil && preparedUntilUnixMS! > 0) else { throw MissionError.invalidDraft }
        guard MissionWire.identifier(id), proposal == nil || proposal?.target == target,
              proposal?.triggerSHA256 == trigger?.sha256, proposal?.inspectionDestination == inspectionDestination else { throw MissionError.invalidDraft }
        try context.validate(target: target, durationMS: durationMS)
        self.id = id; self.target = target; self.durationMS = durationMS; contextSHA256 = context.digest; self.proposal = proposal?.reference; self.trigger = trigger
        self.inspectionDestination = inspectionDestination; self.preparedUntilUnixMS = preparedUntilUnixMS; self.preparedHandoff = preparedHandoff; preparationMode = preparedHandoff == nil ? .legacyTrusted : .preparedReader
        try context.validate(draft: self)
    }
    private enum CodingKeys: String, CodingKey { case id, target, durationMS, contextSHA256, proposal, trigger, inspectionDestination, preparedUntilUnixMS, preparedHandoff, preparationMode }
    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id); target = try c.decode([Double].self, forKey: .target)
        durationMS = try c.decode(Int64.self, forKey: .durationMS); contextSHA256 = try c.decode(String.self, forKey: .contextSHA256)
        proposal = try c.decodeIfPresent(MissionProposalRef.self, forKey: .proposal); trigger = try c.decodeIfPresent(MissionNativeTrigger.self, forKey: .trigger)
        inspectionDestination = try c.decodeIfPresent(MissionInspectionDestination.self, forKey: .inspectionDestination)
        preparedUntilUnixMS = try c.decodeIfPresent(Int64.self, forKey: .preparedUntilUnixMS); preparedHandoff = try c.decodeIfPresent(Data.self, forKey: .preparedHandoff)
        preparationMode = try c.decodeIfPresent(MissionPreparationMode.self, forKey: .preparationMode) ?? .legacyTrusted
        try PreparedProposalHandoff.validateStored(self)
    }

}

/// An explicit handoff from a caller that verified and prepared this exact native
/// inference original. This type checks immutable task context, not the origin
/// signature, current freshness, input evidence or permission to move.
public struct MissionNativeTrigger: Codable, Sendable, Equatable {
    public let canonical: Data
    public let fixedTarget: [Double]
    public let originalEnvelopeSHA256: String
    public let sha256: String
    public let source: JSONValue
    public let version: JSONValue
    public let resultSchema: String
    public let sampleTime: String
    private let component: JSONValue

    /// `canonical` is the Go-encoded TriggerRef returned by the prepared-trigger
    /// adapter. Accepted bytes are retained unchanged. The envelope digest is
    /// caller-supplied provenance and is not an independent verification claim.
    public init(canonical: Data, fixedTarget: [Double], originalEnvelopeSHA256: String) throws {
        guard canonical.count <= 64 << 10, WorldClient.isDigest(originalEnvelopeSHA256) else { throw MissionError.invalidDraft }
        let value = try JSONValue.decode(canonical, limit: 64 << 10)
        guard try NativeTriggerWire.trigger(canonical: canonical) == canonical,
              let c = value["component"], let digest = value["sha256"]?.string else { throw MissionError.invalidDraft }
        let decoded = try NativeTriggerWire.review(c, fixedTarget: fixedTarget)
        self.canonical = canonical; self.fixedTarget = fixedTarget; self.originalEnvelopeSHA256 = originalEnvelopeSHA256
        sha256 = digest; component = c; source = c["key"]!["source"]!; version = c["version"]!
        resultSchema = decoded.schema; sampleTime = c["timing"]!["sample_time"]!.string!
    }

    /// Checks this retained original's task context, without establishing
    /// producer signature validity, current freshness or movement authority.
    public func validate(context: MissionContext) throws {
        guard component["key"]?["tenant"] == context.policy["tenant"],
              component["key"]?["site"] == context.policy["site"], component["frame"] == context.policy["frame"],
              component["timing"]?["clock_domain"]?.string == "simulation:" + context.runID else { throw MissionError.invalidDraft }
        for input in component["provenance"]!["inputs"]!.array! {
            guard input["simulation"]?["run_id"]?.string == context.runID else { throw MissionError.invalidDraft }
        }
    }

    func validate(target: [Double], inspectionDestination: MissionInspectionDestination?) throws {
        if inspectionDestination == nil, fixedTarget != target { throw MissionError.invalidDraft }
        _ = try NativeTriggerWire.review(component, fixedTarget: target, inspectionDestination: inspectionDestination)
    }

    private enum CodingKeys: String, CodingKey { case canonical, fixedTarget, originalEnvelopeSHA256 }
    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        try self.init(canonical: c.decode(Data.self, forKey: .canonical), fixedTarget: c.decode([Double].self, forKey: .fixedTarget),
                      originalEnvelopeSHA256: c.decode(String.self, forKey: .originalEnvelopeSHA256))
    }
    public func encode(to encoder: any Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(canonical, forKey: .canonical); try c.encode(fixedTarget, forKey: .fixedTarget)
        try c.encode(originalEnvelopeSHA256, forKey: .originalEnvelopeSHA256)
    }
}

/// Canonical typed fields of the two native producers. The supplied Component
/// payload is a Go RawMessage and keeps its original bytes and member ordering.
/// The core still performs the complete typed trigger validation at admission.
enum NativeTriggerWire {
    static func object(_ value: JSONValue, required: [String], optional: [String] = [],
                       encode: (String, JSONValue) throws -> Data = { _, v in try MissionWire.value(v) }) throws -> Data {
        guard let fields = value.object, Set(required).isSubset(of: Set(fields.keys)),
              Set(fields.keys).isSubset(of: Set(required + optional)), fields.values.allSatisfy({ $0 != .null }) else { throw MissionError.invalidDraft }
        return try GoAuthorityJSON.object((required + optional).compactMap { name in
            guard let v = fields[name] else { return nil }
            return (name, try encode(name, v))
        })
    }
    static func array(_ values: [JSONValue], encode: (JSONValue) throws -> Data) throws -> Data {
        var result = Data("[".utf8)
        for (i, v) in values.enumerated() { if i > 0 { result.append(0x2c) }; result.append(try encode(v)) }
        result.append(0x5d); return result
    }
    static func key(_ value: JSONValue) throws -> Data {
        try object(value, required: ["tenant", "site", "entity", "type", "source"]) { name, v in
            if name == "source" {
                return try object(v, required: ["device", "app", "service", "stream"]) { _, field in
                    guard let text = field.string, !text.isEmpty, text.utf8.count <= 1024 else { throw MissionError.invalidDraft }
                    return GoAuthorityJSON.string(text)
                }
            }
            guard let text = v.string, !text.isEmpty, text.utf8.count <= 1024 else { throw MissionError.invalidDraft }
            return GoAuthorityJSON.string(text)
        }
    }
    static func version(_ value: JSONValue) throws -> Data {
        guard let epoch = value["epoch"]?.uint, epoch > 0, let sequence = value["sequence"]?.uint, sequence > 0 else { throw MissionError.invalidDraft }
        return try object(value, required: ["epoch", "sequence"])
    }
    static func reference(_ value: JSONValue) throws -> Data {
        try object(value, required: ["key", "version", "envelope_sha256", "record_sha256"]) { name, v in
            if name == "key" { return try key(v) }
            if name == "version" { return try version(v) }
            guard let digest = v.string, WorldClient.isDigest(digest) else { throw MissionError.invalidDraft }
            return GoAuthorityJSON.string(digest)
        }
    }
    static func provenance(_ value: JSONValue, maxInputs: Int = 16) throws -> Data {
        guard value["origin"]?.string == "inferred", let inputs = value["inputs"]?.array, !inputs.isEmpty, inputs.count <= maxInputs else { throw MissionError.invalidDraft }
        return try object(value, required: ["origin", "app_version", "model_id", "model_version", "inputs"]) { name, v in
            if name != "inputs" {
                guard let text = v.string, !text.isEmpty else { throw MissionError.invalidDraft }
                return GoAuthorityJSON.string(text)
            }
            return try array(inputs) { input in
                guard input["origin"]?.string == "simulated" else { throw MissionError.invalidDraft }
                return try object(input, required: ["id", "origin", "simulation"]) { field, item in
                    if field == "simulation" { return try object(item, required: ["run_id", "scenario_version", "seed", "clock_domain"]) }
                    return try MissionWire.value(item)
                }
            }
        }
    }
    static func payload(_ value: JSONValue) throws -> Data {
        guard value["version"]?.int == 1, let inputs = value["inputs"]?.array, !inputs.isEmpty, inputs.count <= 16,
              let retention = value["retain_until_unix_ms"]?.int, retention > 0,
              let raw = value["result_json"]?.string.flatMap({ Data(base64Encoded: $0) }), raw.count <= 24 << 10 else { throw MissionError.invalidDraft }
        let bytes = try object(value, required: ["version", "result_schema", "result_json", "inputs", "retain_until_unix_ms"]) { name, v in
            name == "inputs" ? try array(inputs, encode: reference) : try MissionWire.value(v)
        }
        guard bytes.count <= 32 << 10 else { throw MissionError.invalidDraft }; return bytes
    }
    static func component(_ value: JSONValue, rawPayload: Data? = nil) throws -> Data {
        guard ["wendy.inference.result.v1", "wendy.incident.inspection.v1", RisingIncidentTriggerWire.schema].contains(value["key"]?["type"]?.string ?? ""), value["tombstone"]?.bool == false,
              let timing = value["timing"], timing["error_known"]?.bool == true, timing["age_hint_ns"]?.int == 0,
              let bound = timing["error_bound_ns"]?.int, bound >= 0, let ttl = value["ttl_ns"]?.int, ttl > bound,
              let sample = timing["sample_time"]?.string, WorldComponent.parseTime(sample) != nil,
              let domain = timing["clock_domain"]?.string, domain.hasPrefix("simulation:"), domain.count > 11 else { throw MissionError.invalidDraft }
        return try object(value, required: ["key", "version", "frame", "timing", "ttl_ns", "provenance", "payload", "tombstone"]) { name, v in
            switch name {
            case "key": return try key(v)
            case "version": return try version(v)
            case "frame": return try object(v, required: ["id", "revision"])
            case "timing": return try object(v, required: ["sample_time", "clock_domain", "error_known", "error_bound_ns", "age_hint_ns"])
            case "provenance": return try provenance(v,maxInputs:value["key"]?["type"]?.string == RisingIncidentTriggerWire.schema ? 32 : 16)
            case "payload":
                let typed = value["key"]?["type"]?.string == RisingIncidentTriggerWire.schema ? try RisingIncidentTriggerWire.payload(v) : value["key"]?["type"]?.string == "wendy.incident.inspection.v1" ? try IncidentTriggerWire.payload(v) : try payload(v)
                guard let rawPayload else { return typed }
                guard rawPayload.count <= (value["key"]?["type"]?.string == RisingIncidentTriggerWire.schema ? 56 << 10 : 32 << 10), try JSONValue.decode(rawPayload) == v else { throw MissionError.invalidDraft }
                return rawPayload
            default: return try MissionWire.value(v)
            }
        }
    }
    static func trigger(_ value: JSONValue, rawPayload: Data? = nil) throws -> Data {
        guard let digest = value["sha256"]?.string, WorldClient.isDigest(digest), let c = value["component"] else { throw MissionError.invalidDraft }
        let bytes = try component(c, rawPayload: rawPayload)
        guard WorldCredentials.digest(bytes) == digest else { throw MissionError.invalidDraft }
        return try object(value, required: ["sha256", "component"]) { name, v in name == "component" ? bytes : try MissionWire.value(v) }
    }
    static func trigger(canonical: Data) throws -> Data {
        let value = try JSONValue.decode(canonical, limit: 64 << 10)
        let payload = try MissionRawJSON.field(canonical, path: ["component", "payload"], limit: 64 << 10, requireCompact: true)
        let encoded = try trigger(value, rawPayload: payload)
        guard encoded == canonical else { throw MissionError.invalidDraft }
        return encoded
    }
    static func review(_ c: JSONValue, fixedTarget: [Double], inspectionDestination: MissionInspectionDestination? = nil) throws -> (schema: String, target: [Double]) {
        if c["key"]?["type"]?.string == RisingIncidentTriggerWire.schema { return try RisingIncidentTriggerWire.review(c,fixedTarget:fixedTarget,destination:inspectionDestination) }
        if c["key"]?["type"]?.string == "wendy.incident.inspection.v1" { return try IncidentTriggerWire.review(c, fixedTarget: fixedTarget, destination: inspectionDestination) }
        guard fixedTarget.count == 3, fixedTarget.allSatisfy({ $0.isFinite && abs($0) <= 1_000_000 }),
              let result = c["payload"], let schema = result["result_schema"]?.string,
              let raw = result["result_json"]?.string.flatMap({ Data(base64Encoded: $0) }) else { throw MissionError.invalidDraft }
        let body = try JSONValue.decode(raw, limit: 24 << 10), provenance = c["provenance"]!
        let inputs = result["inputs"]!.array!, leaves = provenance["inputs"]!.array!
        guard inputs.count == leaves.count else { throw MissionError.invalidDraft }
        for (ref, leaf) in zip(inputs, leaves) {
            guard ref["key"]?["tenant"] == c["key"]?["tenant"], ref["key"]?["site"] == c["key"]?["site"],
                  ref["key"]?["type"]?.string == "wendy.sensor.capture.v1",
                  leaf["id"]?.string == "sha256:" + ref["envelope_sha256"]!.string!,
                  leaf["simulation"]?["clock_domain"] == c["timing"]?["clock_domain"] else { throw MissionError.invalidDraft }
        }
        let target: [Double]
        switch schema {
        case "wendy.hazard.v1":
            guard provenance["app_version"]?.string == "world-fusion-v2", provenance["model_id"]?.string == "synthetic-gas-warning-rule",
                  provenance["model_version"]?.string == "2", body["label"]?.string == "possible-gas-hazard" else { throw MissionError.invalidDraft }
            target = try MissionWire.vector(body["position_m"])
        case "wendy.person.track.v1":
            guard provenance["app_version"]?.string == "world-live-people-v1", provenance["model_id"]?.string == "camera-ankle-two-observation-tracking",
                  provenance["model_version"]?.string == "1", body["schema"]?.string == schema,
                  body["frame"] == c["frame"], body["clock_domain"] == c["timing"]?["clock_domain"],
                  body["run_id"] == leaves.first?["simulation"]?["run_id"],
                  let xz = body["position_xz"]?.array, xz.count == 2, let x = xz[0].double, let z = xz[1].double,
                  x.isFinite, z.isFinite else { throw MissionError.invalidDraft }
            target = [x, fixedTarget[1], z] // Explicit commissioned/reviewed floor; never velocity extrapolation.
        default: throw MissionError.unsupported
        }
        if let destination = inspectionDestination {
            guard fixedTarget[1] == destination.floorY,
                  hypot(fixedTarget[0]-target[0], fixedTarget[2]-target[2]) <= destination.maxOffsetM else { throw MissionError.invalidDraft }
        } else { guard target == fixedTarget else { throw MissionError.invalidDraft } }
        return (schema, target)
    }
}

enum MissionWire {
    static func identifier(_ value: String) -> Bool { !value.isEmpty && value.utf8.count <= 128 && value.utf8.allSatisfy({ $0 >= 48 && $0 <= 57 || $0 >= 65 && $0 <= 90 || $0 >= 97 && $0 <= 122 || $0 == 45 || $0 == 95 }) }
    static func vector(_ value: JSONValue?) throws -> [Double] {
        guard let array = value?.array, array.count == 3 else { throw MissionError.invalidContext }
        let vector = array.compactMap(\.double)
        guard vector.count == 3, vector.allSatisfy({ $0.isFinite && abs($0) <= 1_000_000 }) else { throw MissionError.invalidContext }
        return vector
    }
    static func value(_ value: JSONValue) throws -> Data {
        switch value {
        case .string(let v): return GoAuthorityJSON.string(v)
        case .array(let values):
            var data = Data("[".utf8)
            for (index, item) in values.enumerated() { if index > 0 { data.append(0x2c) }; data.append(try self.value(item)) }
            data.append(0x5d); return data
        case .number(let number):
            guard number.isFinite else { throw MissionError.invalidContext }
            if number == 0 { return Data("0".utf8) }
            if number.rounded() == number, abs(number) < 1e15 { return Data(String(Int64(number)).utf8) }
            return try value.encoded()
        case .integer, .unsigned, .bool, .null: return try value.encoded()
        case .object: throw MissionError.invalidContext
        }
    }
    static func ordered(_ value: JSONValue, keys: [String]) throws -> Data {
        guard Set(value.object?.keys.map { $0 } ?? []) == Set(keys) else { throw MissionError.invalidContext }
        return try GoAuthorityJSON.object(keys.map { ($0, try self.value(value[$0]!)) })
    }
}
