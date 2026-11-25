import Foundation
import XCTest
@testable import WorldClient

private actor InspectionFixtureTransport: MissionTransport {
    let entry: MissionEntry
    init(_ entry: MissionEntry) { self.entry = entry }
    func beginMissionOperation() async throws -> UInt64 { 1 }
    func prepareInstruction(draft: MissionDraft, operation: UInt64) async throws -> Data { entry.instruction! }
    func submit(instruction: Data, operation: UInt64) async throws -> JSONValue { entry.record! }
    func status(taskID: String) async throws -> JSONValue { entry.record! }
    func prepareStop(taskID: String) async throws -> Data { throw MissionError.invalidDraft }
    func submitStop(instruction: Data) async throws -> JSONValue { throw MissionError.invalidDraft }
}

@MainActor final class MissionInspectionTests: XCTestCase {
    func fixture() throws -> (MissionEntry, MissionContext, MissionCredentials) {
        let v = try JSONValue.decode(Data(contentsOf: Bundle.module.url(forResource: "native-mission-v1", withExtension: "json", subdirectory: "Fixtures")!))
        let credentials = try MissionCredentials(data: JSONValue.object([
            "url": .string("https://127.0.0.1:9999"), "audience": .string("node-1"),
            "grant": v["operator_grant"]!, "private_key": v["operator_private_key"]!
        ]).encoded())
        let pins = try MissionClockPins(json: v["mission_clock_trust"]!)
        let context = try MissionContext(json: v["control_context"]!, credentials: credentials, pins: pins.value)
        let spec = v["spec"]!
        let draft = try JSONDecoder().decode(MissionDraft.self, from: JSONValue.object([
            "id": spec["id"]!, "target": spec["target"]!, "durationMS": .integer(30_000),
            "contextSHA256": .string(context.digest), "proposal": spec["proposal"]!
        ]).encoded())
        var samples: [JSONValue] = []
        for (sensor, kind) in [("camera-a", "camera"), ("camera-b", "camera"), ("gas-1", "gas"), ("audio-1", "audio")] {
            var sample: [String: JSONValue] = [
                "schema_version": .string("wendy.sensor.sample.v1"), "id": .string("run-native/\(sensor)/4294967297"),
                "sensor_id": .string(sensor), "kind": .string(kind), "run_id": .string(context.runID),
                "site_id": spec["site"]!, "frame": spec["frame"]!, "sequence": .unsigned(4_294_967_297),
                "sample_ns": .integer(2_000_000_000), "scenario_version": .string("inspection-test-v1"), "seed": .integer(7),
                "package_sha256": .string(context.packageSHA256), "sensor_model": .string("synthetic-test-v1"),
                "calibration_revision": .string("calibration-v1"), "position_m": spec["target"]!, "uncertainty_m": .number(0.05)
            ]
            if kind == "camera" {
                sample["camera"] = .object([
                    "width": .integer(320), "height": .integer(240), "fx": .integer(240), "fy": .integer(240),
                    "cx": .integer(160), "cy": .integer(120), "calibration_valid_for_ns": .integer(60_000_000_000),
                    "camera_to_site": .array([1,0,0,1.25,0,1,0,0.2,0,0,1,0,0,0,0,1].map(JSONValue.number))
                ])
            } else if kind == "gas" {
                sample["gas"] = .object(["value_ppm": .number(12.5), "uncertainty_ppm": .number(0.8), "species": .string("test-gas")])
            } else {
                sample["audio"] = .object(["sample_rate_hz": .integer(16000), "channels": .integer(1), "duration_ns": .integer(250_000_000)])
            }
            if kind != "gas" {
                sample["evidence"] = .object(["sha256": .string(String(repeating: kind == "audio" ? "b" : "a", count: 64)),
                    "content_type": .string(kind == "audio" ? "audio/wav" : "image/png"), "size": .integer(128)])
            }
            samples.append(.object(sample))
        }
        let result: JSONValue = .object([
            "task_id": spec["id"]!, "run_id": .string(context.runID), "boot_id": .string("scene-boot"), "session": .string("inspector-session"),
            "sequence": .unsigned(4_294_967_297), "settled_since_simulation_ns": .integer(1_000_000_000),
            "settled_since_unix_ms": .integer(1_794_398_410_000), "captured_unix_ms": .integer(1_794_398_411_000),
            "simulation_time_ns": .integer(2_000_000_000), "frame": spec["frame"]!, "position_m": spec["target"]!,
            "velocity_mps": .array([0,0,0].map(JSONValue.integer)), "sensors": .object(["obstacles": .bool(true)]),
            "obstacle_clear": .bool(true), "energy": .number(0.9), "samples": .array(samples)
        ])
        let record: JSONValue = .object(["spec": spec, "proof": v["instruction"]!, "result": result,
            "status": .object(["Version": .integer(3), "Phase": .string("succeeded"), "Reason": .string("inspection captured")])])
        let wireRecord = try JSONValue.decode(record.encoded())
        return (MissionEntry(draft: draft, state: .terminal, instruction: Data(v["instruction_canonical"]!.string!.utf8), record: wireRecord), context, credentials)
    }

    func edit(_ entry: MissionEntry, _ body: (inout [String: JSONValue]) -> Void) -> MissionEntry {
        var edited = entry, record = entry.record!.object!, result = record["result"]!.object!
        body(&result); record["result"] = .object(result); edited.record = .object(record)
        return edited
    }
    func testHistoricalCaptureShowsAllModalitiesAndOriginalMetadata() throws {
        let (entry, context, _) = try fixture()
        let result = try MissionInspectionResult(entry: entry, context: context)
        XCTAssertEqual(result.capturedUnixMS, 1_794_398_411_000)
        XCTAssertEqual(result.simulationTimeNS, 2_000_000_000)
        XCTAssertEqual(result.sequence, 4_294_967_297)
        XCTAssertEqual(result.samples.compactMap(\.camera).count, 2)
        XCTAssertEqual(result.samples.compactMap(\.gas).first?.valuePPM, 12.5)
        XCTAssertEqual(result.samples.compactMap(\.gas).first?.uncertaintyPPM, 0.8)
        XCTAssertEqual(result.samples.compactMap(\.audio).first?.durationNS, 250_000_000)
        XCTAssertEqual(result.samples.compactMap(\.evidence).count, 3)
        XCTAssertEqual(result.raw, entry.record?["result"])
    }
    func testSavedResultReopensWithoutSelectionOrTransport() async throws {
        let (entry, context, credentials) = try fixture()
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("inspection-reopen-" + UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: dir) }
        let journal = try MissionJournal(directory: dir, credentials: credentials, context: context)
        try await journal.saveDraft(entry.draft)
        try await journal.signAndSend(id: entry.id, using: InspectionFixtureTransport(entry))
        let reopened = try MissionJournal(directory: dir, credentials: credentials, context: context)
        let entries = await reopened.entries()
        let result = try MissionInspectionResult(entry: XCTUnwrap(entries.first), context: context)
        XCTAssertEqual(result.raw, entry.record?["result"])
        XCTAssertEqual(entries.first?.instruction, entry.instruction)
    }
    func testWrongTaskRunFrameOrUnrequestedTriggerCannotBeDisplayed() throws {
        let (entry, context, _) = try fixture()
        for key in ["task_id", "run_id", "frame", "trigger"] {
            let changed = edit(entry) { $0[key] = .string("different") }
            XCTAssertThrowsError(try MissionInspectionResult(entry: changed, context: context), key)
        }
        var pending = entry, record = entry.record!.object!
        record["status"] = .object(["Phase": .string("running")]); pending.record = .object(record)
        XCTAssertThrowsError(try MissionInspectionResult(entry: pending, context: context))
    }
    func testMixedMissingOrDuplicateSamplesAreRejected() throws {
        let (entry, context, _) = try fixture()
        for key in ["sample_ns", "sequence", "run_id", "frame", "package_sha256", "scenario_version", "seed", "id"] {
            let changed = edit(entry) { result in
                var samples = result["samples"]!.array!, sample = samples[0].object!
                sample[key] = .string("different"); samples[0] = .object(sample); result["samples"] = .array(samples)
            }
            XCTAssertThrowsError(try MissionInspectionResult(entry: changed, context: context), key)
        }
        for duplicate in [false, true] {
            let changed = edit(entry) { result in
                var samples = result["samples"]!.array!
                if duplicate { samples[1] = samples[0] } else { samples.removeLast() }
                result["samples"] = .array(samples)
            }
            XCTAssertThrowsError(try MissionInspectionResult(entry: changed, context: context))
        }
    }
    func testAbsentGasDoesNotBecomeZeroAndAudioIntervalStaysOriginal() throws {
        let (entry, context, _) = try fixture()
        for sensorIndex in [2, 3] {
            let changed = edit(entry) { result in
                var samples = result["samples"]!.array!, sample = samples[sensorIndex].object!
                if sensorIndex == 2 {
                    var gas = sample["gas"]!.object!; gas.removeValue(forKey: "value_ppm"); sample["gas"] = .object(gas)
                } else {
                    var audio = sample["audio"]!.object!; audio["duration_ns"] = .integer(2_000_000_000); sample["audio"] = .object(audio)
                }
                samples[sensorIndex] = .object(sample); result["samples"] = .array(samples)
            }
            XCTAssertThrowsError(try MissionInspectionResult(entry: changed, context: context))
        }
    }
}
