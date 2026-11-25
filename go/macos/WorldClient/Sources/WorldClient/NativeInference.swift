import Foundation

public enum NativeInferenceError: Error, Sendable {
    case unsupported, malformed
}

public struct NativeInferenceEvidence: Sendable, Equatable {
    public let raw: JSONValue
    public let sha256: String
    public let contentType: String
    public let size: Int
    public let reference: NativeEvidenceReference
    public let proof: NativeInferenceReference
    public let isOriginalImage: Bool
    init(_ value: JSONValue, proof: NativeInferenceReference, originalImage: Bool = false) throws {
        guard let digest = value["sha256"]?.string, WorldClient.isDigest(digest),
              let size = value["size"]?.int, (1...(8 << 20)).contains(size),
              let mime = value["content_type"]?.string,
              ["image/png", "image/jpeg", "audio/wav"].contains(mime) else { throw NativeInferenceError.malformed }
        raw = value; sha256 = digest; contentType = mime; self.size = Int(size)
        var owner = proof.source.object!
        owner["component_type"] = proof.raw["key"]?["type"]
        reference = try NativeEvidenceReference(source: .object(owner), sha256: digest, size: Int(size), contentType: mime,
                                              proofEnvelopeSHA256: proof.envelopeSHA256)
        self.proof = proof; isOriginalImage = originalImage
    }
}

public struct NativeInferenceReference: Sendable, Equatable {
    public let raw: JSONValue
    public let source: JSONValue
    public let epoch: UInt64
    public let sequence: UInt64
    public let envelopeSHA256: String
    public let recordSHA256: String
    init(_ value: JSONValue) throws {
        guard let key = value["key"], let source = key["source"],
              key["type"]?.string == "wendy.sensor.capture.v1",
              let epoch = value["version"]?["epoch"]?.uint, epoch > 0,
              let sequence = value["version"]?["sequence"]?.uint, sequence > 0,
              let envelope = value["envelope_sha256"]?.string, WorldClient.isDigest(envelope),
              let record = value["record_sha256"]?.string, WorldClient.isDigest(record) else { throw NativeInferenceError.malformed }
        for field in ["tenant", "site", "entity"] { _ = try NativeInference.string(key[field]) }
        try NativeInference.checkSource(source)
        raw = value; self.source = source; self.epoch = epoch; self.sequence = sequence
        envelopeSHA256 = envelope; recordSHA256 = record
    }
}

public struct NativePersonTrack: Sendable {
    public let id: String
    public let revision: UInt64
    public let positionXZ: [Double]
    public let covariance: [Double]
    public let ambiguous: Bool
    public let motionKnown: Bool
    public let velocityXZ: [Double]?
    /// Exact bounded member objects retain detector, calibration and capture lineage.
    public let current: [JSONValue]
    public let previous: [JSONValue]
}

public struct NativeHazard: Sendable {
    public let label: String
    public let position: [Double]
    public let uncertaintyM: Double
    public let confidence: Double
    public let confidenceMeaning: String
    public let locationBasis: String
    public let gasValuePPM: Double
    public let gasUncertaintyPPM: Double
    public let gasSpecies: String
    public let inputs: [JSONValue]
    public let cameras: [JSONValue]
    public let audio: JSONValue
}

/// A display projection of a gateway-delivered native result. Decoding is not
/// signature verification, current source admission or independent inference.
/// Original component, result bytes and references remain available to callers.
public struct NativeInference: Sendable {
    public static let componentType = "wendy.inference.result.v1"
    public enum Kind: String, Sendable { case person, hazard }
    public let component: WorldComponent
    public let resultJSON: Data
    public let result: JSONValue
    public let schema: String
    public let kind: Kind
    public let runID: String
    public let clockDomain: String
    public let source: JSONValue
    public let modelID: String
    public let modelVersion: String
    public let inputs: [NativeInferenceReference]
    public let evidence: [NativeInferenceEvidence]
    public let observedAt: Date
    public let sampleValidUntil: Date
    public let physicalValidUntil: Date
    public let person: NativePersonTrack?
    public let hazard: NativeHazard?
    public var positionXZ: [Double] {
        if let person { return person.positionXZ }
        return [hazard!.position[0], hazard!.position[2]]
    }

    public init(component: WorldComponent) throws {
        guard component.type == Self.componentType else { throw NativeInferenceError.unsupported }
        let c = component.raw
        guard let payload = c["payload"], try payload.encoded().count <= 32 << 10,
              payload["version"]?.int == 1,
              let schema = payload["result_schema"]?.string,
              ["wendy.person.track.v1", "wendy.hazard.v1"].contains(schema) else { throw NativeInferenceError.unsupported }
        guard let encoded = payload["result_json"]?.string, encoded.utf8.count <= 32 << 10,
              c["tombstone"]?.bool != true,
              let bytes = Data(base64Encoded: encoded), !bytes.isEmpty, bytes.count <= 24 << 10,
              let refs = payload["inputs"]?.array, (1...16).contains(refs.count),
              let retention = payload["retain_until_unix_ms"]?.int, retention > 0,
              c["provenance"]?["origin"]?.string == "inferred",
              let source = c["key"]?["source"], let version = c["version"],
              let epoch = version["epoch"]?.uint, epoch > 0,
              let sequence = version["sequence"]?.uint, sequence > 0,
              let timing = c["timing"], timing["error_known"]?.bool == true,
              let ttl = c["ttl_ns"]?.int, ttl > 0,
              let error = timing["error_bound_ns"]?.int, error >= 0,
              let hint = timing["age_hint_ns"]?.int, hint >= 0,
              max(error, hint) < ttl else { throw NativeInferenceError.malformed }
        try Self.only(payload, ["version", "result_schema", "result_json", "inputs", "retain_until_unix_ms"])
        try Self.checkSource(source)
        _ = try Self.string(c["frame"]?["id"]); _ = try Self.string(c["frame"]?["revision"])
        let sample = try Self.time(timing["sample_time"])
        let clock = try Self.string(timing["clock_domain"])
        let body = try JSONValue.decode(bytes, limit: 24 << 10)
        let references = try refs.map(NativeInferenceReference.init)
        guard Set(references.map(\.envelopeSHA256)).count == references.count else { throw NativeInferenceError.malformed }
        for reference in references {
            guard reference.raw["key"]?["tenant"] == c["key"]?["tenant"], reference.raw["key"]?["site"] == c["key"]?["site"] else { throw NativeInferenceError.malformed }
        }
        let model = try Self.string(c["provenance"]?["model_id"])
        let modelVersion = try Self.string(c["provenance"]?["model_version"])
        let until = try Self.time(body["valid_until"])
        guard until > sample else { throw NativeInferenceError.malformed }
        var media: [NativeInferenceEvidence] = []
        let run: String, observed: Date, physical: Date
        if schema == "wendy.person.track.v1" {
            try Self.only(body, ["schema","algorithm","algorithm_version","track_id","revision","run_id","clock_domain","frame","observed_at","valid_until","position_xz","covariance","ambiguous","motion_known","velocity_xz","velocity_covariance","previous_observed_at","current","previous"])
            run = try Self.string(body["run_id"])
            observed = try Self.time(body["observed_at"])
            guard body["schema"]?.string == schema, body["frame"] == c["frame"],
                  body["clock_domain"]?.string == clock, clock == "simulation:" + run,
                  body["algorithm"]?.string == model, body["algorithm_version"]?.string == modelVersion,
                  let revision = body["revision"]?.uint, revision > 0,
                  let ambiguous = body["ambiguous"]?.bool, let motion = body["motion_known"]?.bool,
                  let current = body["current"]?.array, (1...8).contains(current.count) else { throw NativeInferenceError.malformed }
            let previous = body["previous"]?.array ?? []
            guard previous.count <= 8, body["previous"] == nil || body["previous"]?.array != nil else { throw NativeInferenceError.malformed }
            let velocity: [Double]?
            if motion {
                velocity = try Self.vector(body["velocity_xz"], 2)
                _ = try Self.covariance(body["velocity_covariance"])
                guard !previous.isEmpty, try Self.time(body["previous_observed_at"]) < observed else { throw NativeInferenceError.malformed }
            } else {
                guard previous.isEmpty, body["velocity_xz"] == nil, body["velocity_covariance"] == nil, body["previous_observed_at"] == nil else { throw NativeInferenceError.malformed }
                velocity = nil
            }
            var used: [JSONValue] = [], caps: [Date] = [], deadlines: [Date] = []
            for member in current + previous {
                try Self.only(member, ["proof","sensor_id","sample_id","capture_sequence","member_index","sample_time","valid_until","physical_until","position_xz","covariance","image","original_image","model_id","model_version","model_sha256","calibration_revision","calibration_sha256","scene_boot_id","captured_unix_ns","simulation_anchor_unix_ns"])
                guard let rawProof = member["proof"] else { throw NativeInferenceError.malformed }
                let reference = try NativeInferenceReference(rawProof)
                guard refs.contains(rawProof), reference.source["stream"] == member["sensor_id"],
                      reference.raw["key"]?["entity"]?.string == "sensor/" + (try Self.string(member["sensor_id"])),
                      let captured = member["captured_unix_ns"]?.int, captured > 0,
                      let anchor = member["simulation_anchor_unix_ns"]?.int, anchor > 0,
                      let captureSequence = member["capture_sequence"]?.uint, captureSequence > 0,
                      let memberIndex = member["member_index"]?.uint, memberIndex < 64 else { throw NativeInferenceError.malformed }
                for key in ["sample_id","model_id","model_version","calibration_revision","scene_boot_id"] { _ = try Self.string(member[key]) }
                for key in ["model_sha256","calibration_sha256"] { guard let digest = member[key]?.string, WorldClient.isDigest(digest) else { throw NativeInferenceError.malformed } }
                _ = try Self.vector(member["position_xz"], 2); _ = try Self.covariance(member["covariance"])
                let memberTime = try Self.time(member["sample_time"]), deadline = try Self.time(member["valid_until"])
                guard deadline > memberTime, deadline.timeIntervalSince(memberTime) <= 1.250001 else { throw NativeInferenceError.malformed }
                deadlines.append(deadline)
                caps.append(min(try Self.time(member["physical_until"]), Date(timeIntervalSince1970: Double(captured) / 1e9 + 5)))
                let image = try NativeInferenceEvidence(Self.required(member["image"]), proof: reference)
                guard ["image/png", "image/jpeg"].contains(image.contentType) else { throw NativeInferenceError.malformed }
                media.append(image)
                if let original = member["original_image"] {
                    let decoded = try NativeInferenceEvidence(original, proof: reference, originalImage: true)
                    guard image.contentType == "image/jpeg", decoded.contentType == "image/png" else { throw NativeInferenceError.malformed }
                    media.append(decoded)
                }
                if !used.contains(rawProof) { used.append(rawProof) }
            }
            guard used == refs, deadlines.min() == until,
                  try current.map({ try Self.time($0["sample_time"]) }).max() == observed,
                  c["key"]?["entity"]?.string == run + "/track/" + (try Self.string(body["track_id"])),
                  until > observed else { throw NativeInferenceError.malformed }
            physical = caps.min()!
            person = NativePersonTrack(id: try Self.string(body["track_id"]), revision: revision,
                positionXZ: try Self.vector(body["position_xz"], 2), covariance: try Self.covariance(body["covariance"]),
                ambiguous: ambiguous, motionKnown: motion, velocityXZ: velocity, current: current, previous: previous)
            hazard = nil; kind = .person
        } else {
            guard let rawInputs = body["inputs"]?.array, rawInputs.count == references.count,
                  (4...16).contains(rawInputs.count), let cameras = body["cameras"]?.array, cameras.count == 2,
                  let evidence = body["evidence"]?.array, evidence.count == 3,
                  let sampleInput = rawInputs.first(where: { $0["sample"] != nil })?["sample"],
                  let audio = body["audio"], audio.object != nil else { throw NativeInferenceError.malformed }
            run = try Self.string(sampleInput["run_id"]); observed = sample
            guard clock == "simulation:" + run, body["label"]?.string == "possible-gas-hazard",
                  c["key"]?["entity"]?.string == run + "/possible-gas-hazard" else { throw NativeInferenceError.malformed }
            for (index, input) in rawInputs.enumerated() {
                guard input["source"] == references[index].source, input["version"] == refs[index]["version"],
                      input["capture"]?["proof_sha256"]?.string == references[index].envelopeSHA256,
                      let kind = input["kind"]?.string, ["camera","audio","gas","lidar"].contains(kind) else { throw NativeInferenceError.malformed }
                _ = try Self.string(input["sensor_id"]); _ = try Self.string(input["sample_id"])
                _ = try Self.time(input["sample_time"]); _ = try Self.time(input["valid_until"])
                if let sample = input["sample"] {
                    guard sample["run_id"]?.string == run, sample["frame"] == c["frame"], sample["site_id"] == c["key"]?["site"] else { throw NativeInferenceError.malformed }
                } else { guard kind == "lidar", input["lidar_summary"]?.object != nil else { throw NativeInferenceError.malformed } }
            }
            for camera in cameras { _ = try Self.string(camera["sensor_id"]); _ = try Self.scalar(camera["red_fraction"], 0...1) }
            _ = try Self.scalar(audio["rms"], 0...1); _ = try Self.scalar(audio["tone_fraction"], 0...1)
            var active: [JSONValue] = []
            for (index, input) in rawInputs.enumerated() {
                if let rawMedia = input["evidence"] {
                    let decoded = try NativeInferenceEvidence(rawMedia, proof: references[index])
                    guard (input["kind"]?.string == "camera" && ["image/png", "image/jpeg"].contains(decoded.contentType)) ||
                          (input["kind"]?.string == "audio" && decoded.contentType == "audio/wav"),
                          input["sample"]?["evidence"] == rawMedia else { throw NativeInferenceError.malformed }
                    active.append(rawMedia); media.append(decoded)
                    if let original = input["capture"]?["original_evidence"] {
                        let png = try NativeInferenceEvidence(original, proof: references[index], originalImage: true)
                        guard decoded.contentType == "image/jpeg", png.contentType == "image/png" else { throw NativeInferenceError.malformed }
                        media.append(png)
                    }
                } else {
                    guard !["camera", "audio"].contains(input["kind"]?.string ?? ""), input["capture"]?["original_evidence"] == nil else { throw NativeInferenceError.malformed }
                }
            }
            var unmatched = evidence
            for item in active {
                guard let index = unmatched.firstIndex(of: item) else { throw NativeInferenceError.malformed }
                unmatched.remove(at: index)
            }
            guard unmatched.isEmpty else { throw NativeInferenceError.malformed }
            physical = try Self.time(body["physical_valid_until"])
            hazard = NativeHazard(label: try Self.string(body["label"]), position: try Self.vector(body["position_m"], 3),
                uncertaintyM: try Self.scalar(body["uncertainty_m"], 0...1e6), confidence: try Self.scalar(body["confidence"], 0...1),
                confidenceMeaning: try Self.string(body["confidence_meaning"]), locationBasis: try Self.string(body["location_basis"]),
                gasValuePPM: try Self.scalar(body["gas_value_ppm"], 0...1e9), gasUncertaintyPPM: try Self.scalar(body["gas_uncertainty_ppm"], 0...1e9),
                gasSpecies: try Self.string(body["gas_species"]), inputs: rawInputs, cameras: cameras, audio: audio)
            person = nil; kind = .hazard
        }
        guard let leaves = c["provenance"]?["inputs"]?.array, leaves.count == refs.count else { throw NativeInferenceError.malformed }
        for (index, leaf) in leaves.enumerated() {
            guard leaf["id"]?.string == "sha256:" + references[index].envelopeSHA256,
                  leaf["origin"]?.string == "simulated", leaf["simulation"]?["run_id"]?.string == run,
                  leaf["simulation"]?["clock_domain"]?.string == clock else { throw NativeInferenceError.malformed }
        }
        self.component = component; resultJSON = bytes; result = body; self.schema = schema; runID = run
        clockDomain = clock; self.source = source; modelID = model; self.modelVersion = modelVersion
        inputs = references; self.evidence = media; observedAt = observed
        sampleValidUntil = min(until, sample.addingTimeInterval(Double(ttl - max(error, hint)) / 1e9))
        physicalValidUntil = physical
    }

    /// Local-wall display estimate only. An unrelated synthetic sample clock is
    /// never advanced from fetch time. A physical cap can only expire a display.
    public func freshness(at now: Date = Date()) -> ObservationFreshness {
        if component.raw["tombstone"]?.bool == true || now >= physicalValidUntil { return .stale }
        guard clockDomain == "utc" else { return .unknown }
        if now >= sampleValidUntil { return .stale }
        return component.freshness(at: now)
    }

    static func required(_ value: JSONValue?) throws -> JSONValue {
        guard let value else { throw NativeInferenceError.malformed }; return value
    }
    static func string(_ value: JSONValue?) throws -> String {
        guard let value = value?.string, !value.isEmpty, value.utf8.count <= 512,
              value == value.trimmingCharacters(in: .whitespacesAndNewlines),
              value.unicodeScalars.allSatisfy({ !CharacterSet.controlCharacters.contains($0) }) else { throw NativeInferenceError.malformed }
        return value
    }
    static func checkSource(_ source: JSONValue) throws {
        for key in ["device", "app", "service", "stream"] { _ = try string(source[key]) }
    }
    static func time(_ value: JSONValue?) throws -> Date {
        guard let date = WorldComponent.parseTime(try string(value)), date.timeIntervalSince1970 > 0 else { throw NativeInferenceError.malformed }; return date
    }
    static func scalar(_ value: JSONValue?, _ range: ClosedRange<Double>) throws -> Double {
        guard let value = value?.double, value.isFinite, range.contains(value) else { throw NativeInferenceError.malformed }; return value
    }
    static func vector(_ value: JSONValue?, _ count: Int) throws -> [Double] {
        guard let items = value?.array, items.count == count else { throw NativeInferenceError.malformed }
        return try items.map { try scalar($0, -1e6...1e6) }
    }
    static func covariance(_ value: JSONValue?) throws -> [Double] {
        let c = try vector(value, 4)
        guard c[0] > 0, c[3] > 0, abs(c[1]-c[2]) <= 1e-10, c[0]*c[3] > c[1]*c[2] else { throw NativeInferenceError.malformed }; return c
    }
    static func only(_ value: JSONValue, _ keys: Set<String>) throws {
        guard let fields = value.object, Set(fields.keys).isSubset(of: keys) else { throw NativeInferenceError.malformed }
    }
}
