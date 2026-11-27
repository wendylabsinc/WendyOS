import Foundation

public enum MissionInspectionError: Error, LocalizedError, Sendable {
    case invalidResult
    public var errorDescription: String? { "The saved inspection does not match this mission or contains incomplete sensor data." }
}

public struct MissionInspectionEvidence: Sendable, Equatable {
    public let sha256: String
    public let contentType: String
    public let size: Int
    public init(sha256: String, contentType: String, size: Int) throws {
        guard WorldClient.isDigest(sha256), ["image/png", "image/jpeg", "audio/wav"].contains(contentType),
              (1...(8 << 20)).contains(size) else { throw WorldClientError.invalidEvidence }
        self.sha256 = sha256; self.contentType = contentType; self.size = size
    }
    init(_ value: JSONValue) throws {
        guard let digest = value["sha256"]?.string, let type = value["content_type"]?.string,
              let count = value["size"]?.int.flatMap(Int.init(exactly:)) else { throw MissionInspectionError.invalidResult }
        try self.init(sha256: digest, contentType: type, size: count)
    }
}

public struct MissionInspectionGas: Sendable {
    public let valuePPM: Double
    public let uncertaintyPPM: Double
    public let species: String
}
public struct MissionInspectionCamera: Sendable {
    public let width: Int
    public let height: Int
}
public struct MissionInspectionAudio: Sendable {
    public let sampleRateHz: Int
    public let channels: Int
    public let durationNS: Int64
}
public struct MissionInspectionSample: Sendable, Identifiable {
    public let id: String
    public let sensorID: String
    public let kind: String
    public let sampleNS: Int64
    public let position: [Double]
    public let uncertaintyM: Double
    public let scenarioVersion: String
    public let packageSHA256: String
    public let calibrationRevision: String
    public let gas: MissionInspectionGas?
    public let camera: MissionInspectionCamera?
    public let audio: MissionInspectionAudio?
    public let evidence: MissionInspectionEvidence?
    /// Retains original calibration, model and lineage fields for inspection.
    public let raw: JSONValue

    fileprivate init(_ value: JSONValue, result: JSONValue, context: MissionContext) throws {
        guard value["schema_version"]?.string == "wendy.sensor.sample.v1",
              let sensor = value["sensor_id"]?.string, let kind = value["kind"]?.string,
              ["camera-a": "camera", "camera-b": "camera", "gas-1": "gas", "audio-1": "audio"][sensor] == kind,
              let sequence = value["sequence"]?.uint, sequence > 0, sequence == result["sequence"]?.uint,
              value["run_id"]?.string == context.runID,
              value["site_id"] == context.policy["site"], value["frame"] == context.policy["frame"],
              let id = value["id"]?.string, id == "\(context.runID)/\(sensor)/\(sequence)",
              let time = value["sample_ns"]?.int, (0...86_400_000_000_000).contains(time), time == result["simulation_time_ns"]?.int,
              let scenario = value["scenario_version"]?.string, !scenario.isEmpty, scenario.utf8.count <= 256,
              let seed = value["seed"]?.uint, seed < 1 << 32,
              value["package_sha256"]?.string == context.packageSHA256,
              let model = value["sensor_model"]?.string, !model.isEmpty, model.utf8.count <= 256,
              let calibration = value["calibration_revision"]?.string, !calibration.isEmpty, calibration.utf8.count <= 256,
              let uncertainty = value["uncertainty_m"]?.double, uncertainty.isFinite, uncertainty >= 0 else {
            throw MissionInspectionError.invalidResult
        }
        let position = try MissionWire.vector(value["position_m"])
        let evidence = try value["evidence"].map(MissionInspectionEvidence.init)
        var gas: MissionInspectionGas?, camera: MissionInspectionCamera?, audio: MissionInspectionAudio?
        switch kind {
        case "camera":
            guard value["gas"] == nil, value["audio"] == nil, value["lidar"] == nil,
                  let c = value["camera"], let width = c["width"]?.int, (1...1024).contains(width),
                  let height = c["height"]?.int, (1...1024).contains(height),
                  let fx = c["fx"]?.double, fx.isFinite, fx > 0, let fy = c["fy"]?.double, fy.isFinite, fy > 0,
                  let cx = c["cx"]?.double, cx.isFinite, (0...Double(width)).contains(cx),
                  let cy = c["cy"]?.double, cy.isFinite, (0...Double(height)).contains(cy),
                  let matrix = c["camera_to_site"]?.array, matrix.count == 16,
                  matrix.allSatisfy({ $0.double?.isFinite == true }),
                  let validity = c["calibration_valid_for_ns"]?.int, (1...86_400_000_000_000).contains(validity),
                  evidence?.contentType == "image/png" || evidence?.contentType == "image/jpeg" else {
                throw MissionInspectionError.invalidResult
            }
            camera = MissionInspectionCamera(width: Int(width), height: Int(height))
        case "gas":
            guard value["camera"] == nil, value["audio"] == nil, value["lidar"] == nil, evidence == nil,
                  let g = value["gas"], let ppm = g["value_ppm"]?.double, ppm.isFinite, ppm >= 0,
                  let error = g["uncertainty_ppm"]?.double, error.isFinite, error >= 0,
                  let species = g["species"]?.string, !species.isEmpty, species.utf8.count <= 128 else {
                throw MissionInspectionError.invalidResult
            }
            gas = MissionInspectionGas(valuePPM: ppm, uncertaintyPPM: error, species: species)
        case "audio":
            guard value["camera"] == nil, value["gas"] == nil, value["lidar"] == nil,
                  let a = value["audio"], let rate = a["sample_rate_hz"]?.int, (2000...48000).contains(rate),
                  a["channels"]?.int == 1, let duration = a["duration_ns"]?.int,
                  (1...10_000_000_000).contains(duration), duration <= time,
                  let settled = result["settled_since_simulation_ns"]?.int, time - duration >= settled,
                  evidence?.contentType == "audio/wav" else { throw MissionInspectionError.invalidResult }
            audio = MissionInspectionAudio(sampleRateHz: Int(rate), channels: 1, durationNS: duration)
        default: throw MissionInspectionError.invalidResult
        }
        if kind == "gas" || kind == "audio" {
            let capturedPosition = try MissionWire.vector(result["position_m"])
            guard zip(position, capturedPosition).allSatisfy({ abs($0 - $1) <= 0.002 }) else { throw MissionInspectionError.invalidResult }
        }
        self.id = id; sensorID = sensor; self.kind = kind; sampleNS = time; self.position = position
        uncertaintyM = uncertainty; scenarioVersion = scenario; packageSHA256 = context.packageSHA256
        calibrationRevision = calibration; self.gas = gas; self.camera = camera; self.audio = audio
        self.evidence = evidence; raw = value
    }
}

/// A historical projection of a gateway result already retained by MissionJournal.
/// The journal verifies the signed instruction. This decoder binds the returned
/// samples to that instruction; it does not independently authenticate the result.
/// Capture time remains historical and is never refreshed to the time of viewing.
public struct MissionInspectionResult: Sendable {
    public let taskID: String
    public let runID: String
    public let frameID: String
    public let frameRevision: String
    public let capturedUnixMS: Int64
    public let simulationTimeNS: Int64
    public let sequence: UInt64
    public let position: [Double]
    public let samples: [MissionInspectionSample]
    public let raw: JSONValue

    public init(entry: MissionEntry, context: MissionContext) throws {
        try context.validate(draft: entry.draft)
        guard entry.phase == "succeeded", let record = entry.record, let result = record["result"],
              let instruction = entry.instruction, let spec = record["spec"],
              let deadline = spec["not_after_unix_ms"]?.int,
              try JSONValue.decode(context.spec(draft: entry.draft, deadline: deadline)) == spec,
              try JSONValue.decode(instruction)["payload"] == spec,
              result["task_id"]?.string == entry.id, result["run_id"]?.string == context.runID,
              result["frame"] == context.policy["frame"],
              (result["trigger"] ?? .null) == (spec["trigger"] ?? .null),
              let boot = result["boot_id"]?.string, !boot.isEmpty,
              let session = result["session"]?.string, !session.isEmpty,
              let captured = result["captured_unix_ms"]?.int, captured > 0,
              let settledWall = result["settled_since_unix_ms"]?.int, settledWall > 0, settledWall <= captured,
              let time = result["simulation_time_ns"]?.int, (0...86_400_000_000_000).contains(time),
              let settledSim = result["settled_since_simulation_ns"]?.int, settledSim >= 0, settledSim <= time,
              let sequence = result["sequence"]?.uint, sequence > 0,
              let values = result["samples"]?.array, values.count == 4 else { throw MissionInspectionError.invalidResult }
        let position = try MissionWire.vector(result["position_m"])
        let samples = try values.map { try MissionInspectionSample($0, result: result, context: context) }
        guard Set(samples.map(\.sensorID)) == ["camera-a", "camera-b", "gas-1", "audio-1"],
              Set(samples.map(\.scenarioVersion)).count == 1,
              Set(samples.compactMap { $0.raw["seed"]?.uint }).count == 1 else { throw MissionInspectionError.invalidResult }
        taskID = entry.id; runID = context.runID; frameID = context.frameID; frameRevision = context.frameRevision
        capturedUnixMS = captured; simulationTimeNS = time; self.sequence = sequence; self.position = position
        self.samples = samples; raw = result
    }
}
