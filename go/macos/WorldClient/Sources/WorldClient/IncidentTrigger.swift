import Foundation

// A typed historical association selected for review. Source hashes attest the
// retained binding; these checks are not independent sensor authentication.
enum IncidentTriggerWire {
    static let schema = "wendy.incident.inspection.v1"
    static func payload(_ value: JSONValue) throws -> Data {
        try NativeTriggerWire.object(value, required: ["schema", "incident_sha256", "contract_sha256", "input_digest", "state", "position_m", "uncertainty_m", "supports", "valid_until"]) { name, v in
            if name == "supports" {
                guard let supports = v.array, supports.count == 2 else { throw MissionError.invalidDraft }
                return try NativeTriggerWire.array(supports) { support in
                    try NativeTriggerWire.object(support, required: ["kind", "sha256", "source", "version", "captured_at", "valid_until"]) { field, item in
                        if field == "source" { return try NativeTriggerWire.object(item, required: ["device", "app", "service", "stream"]) }
                        if field == "version" { return try NativeTriggerWire.version(item) }
                        return try MissionWire.value(item)
                    }
                }
            }
            return try MissionWire.value(v)
        }
    }
    static func review(_ c: JSONValue, fixedTarget: [Double], destination: MissionInspectionDestination?) throws -> (schema: String, target: [Double]) {
        guard let p = c["payload"], let provenance = c["provenance"], let timing = c["timing"],
              p["schema"]?.string == schema, p["state"]?.string == "consistent",
              provenance["app_version"]?.string == "world-incident-inspection-v1",
              provenance["model_id"]?.string == "incident-inspection-selection", provenance["model_version"]?.string == "1",
              timing["error_bound_ns"]?.int == 0, timing["age_hint_ns"]?.int == 0,
              let ttl = timing["ttl_ns"]?.int ?? c["ttl_ns"]?.int, ttl > 0, ttl <= 10_000_000_000,
              let sample = try? MissionTimestamp.nanos(timing["sample_time"]),
              let until = try? MissionTimestamp.nanos(p["valid_until"]),
              (try MissionTimestamp.span(until,sample)) == ttl,
              let uncertainty = p["uncertainty_m"]?.double, uncertainty.isFinite, uncertainty > 0, uncertainty <= 10,
              let supports = p["supports"]?.array, supports.count == 2,
              let leaves = provenance["inputs"]?.array, (4...16).contains(leaves.count) else { throw MissionError.invalidDraft }
        for key in ["incident_sha256", "contract_sha256", "input_digest"] { guard let hash = p[key]?.string, WorldClient.isDigest(hash) else { throw MissionError.invalidDraft } }
        let target = try MissionWire.vector(p["position_m"])
        guard target.allSatisfy({ abs($0) <= 10_000 }), fixedTarget.count == 3, fixedTarget.allSatisfy({ $0.isFinite }) else { throw MissionError.invalidDraft }
        var kinds = Set<String>(), hashes = Set<String>(), captures = [Int64](), deadlines = [Int64]()
        for support in supports {
            guard let kind = support["kind"]?.string, ["gas", "visual"].contains(kind), kinds.insert(kind).inserted,
                  let hash = support["sha256"]?.string, WorldClient.isDigest(hash), hashes.insert(hash).inserted,
                  let capture = try? MissionTimestamp.nanos(support["captured_at"]),
                  let end = try? MissionTimestamp.nanos(support["valid_until"]),
                  end > capture, (try MissionTimestamp.span(end,capture)) <= 10_000_000_000 else { throw MissionError.invalidDraft }
            guard let version=support["version"] else { throw MissionError.invalidDraft }; _ = try NativeTriggerWire.version(version)
            for field in ["device", "app", "service", "stream"] { guard let text = support["source"]?[field]?.string, !text.isEmpty else { throw MissionError.invalidDraft } }
            captures.append(capture); deadlines.append(end)
        }
        guard supports[0]["source"] != supports[1]["source"], captures.min() == sample, deadlines.min() == until,
              (try MissionTimestamp.span(captures.max()!,captures.min()!)) <= 350_000_000,
              let domain = timing["clock_domain"]?.string, domain.hasPrefix("simulation:") else { throw MissionError.invalidDraft }
        let run = String(domain.dropFirst("simulation:".count)); var ids = Set<String>()
        guard !run.isEmpty, c["key"]?["entity"]?.string == run + "/incident-inspection" else { throw MissionError.invalidDraft }
        for leaf in leaves {
            guard leaf["origin"]?.string == "simulated", leaf["simulation"]?["run_id"]?.string == run,
                  leaf["simulation"]?["clock_domain"]?.string == domain,
                  let scenario=leaf["simulation"]?["scenario_version"]?.string, !scenario.isEmpty, leaf["simulation"]?["seed"]?.uint != nil,
                  let id = leaf["id"]?.string, !id.isEmpty, ids.insert(id).inserted else { throw MissionError.invalidDraft }
        }
        if let d = destination {
            guard fixedTarget[1] == d.floorY, hypot(fixedTarget[0]-target[0],fixedTarget[2]-target[2]) <= d.maxOffsetM else { throw MissionError.invalidDraft }
        } else { guard target == fixedTarget else { throw MissionError.invalidDraft } }
        return (schema,target)
    }
}
