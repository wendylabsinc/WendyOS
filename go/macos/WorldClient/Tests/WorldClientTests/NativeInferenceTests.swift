import CryptoKit
import Foundation
import XCTest
@testable import WorldClient

final class NativeInferenceTests: XCTestCase {
    static func fixture() throws -> JSONValue {
        let fixture = try XCTUnwrap(Bundle.module.url(forResource: "native-inference-viewer-v1", withExtension: "json", subdirectory: "Fixtures"))
        return try JSONValue.decode(Data(contentsOf: fixture))
    }
    func components() throws -> [JSONValue] { try Self.fixture()["snapshot"]!["components"]!.array! }
    func changedBody(_ component: JSONValue, _ mutate: (inout [String: JSONValue]) -> Void) throws -> WorldComponent {
        var c = component.object!, payload = c["payload"]!.object!
        var body = try JSONValue.decode(Data(base64Encoded: payload["result_json"]!.string!)!).object!
        mutate(&body)
        payload["result_json"] = .string(try JSONValue.object(body).encoded().base64EncodedString())
        c["payload"] = .object(payload)
        return try WorldComponent(raw: .object(c))
    }

    func testActualGoTypesPreserveOriginalBytesAndLineage() throws {
        let fixture = try Self.fixture(), components = try components()
        for entry in fixture["cases"]!.array! {
            let raw = components[Int(entry["component_index"]!.int!)]
            let view = try NativeInference(component: WorldComponent(raw: raw)), expected = entry["expected"]!
            XCTAssertEqual(view.component.raw, raw)
            XCTAssertEqual(SHA256.hash(data: view.resultJSON).map { String(format: "%02x", $0) }.joined(), entry["result_json_sha256"]?.string)
            XCTAssertEqual(view.kind.rawValue, expected["kind"]?.string)
            XCTAssertEqual(view.runID, expected["run_id"]?.string)
            XCTAssertEqual(view.source, expected["source"])
            XCTAssertEqual(view.modelID, expected["model_id"]?.string)
            XCTAssertEqual(view.modelVersion, expected["model_version"]?.string)
            XCTAssertEqual(view.clockDomain, expected["clock_domain"]?.string)
            XCTAssertEqual(view.component.raw["frame"], expected["frame"])
            XCTAssertEqual(view.component.raw["version"], expected["version"])
            XCTAssertEqual(view.component.raw["timing"]?["sample_time"], expected["sample_time"])
            XCTAssertEqual(view.inputs.map(\.raw), raw["payload"]?["inputs"]?.array)
            if let person = view.person {
                XCTAssertEqual(person.positionXZ, expected["position_xz"]?.array?.compactMap(\.double))
                XCTAssertEqual(person.covariance, expected["covariance"]?.array?.compactMap(\.double))
                XCTAssertEqual(person.ambiguous, expected["ambiguous"]?.bool)
                XCTAssertEqual(person.motionKnown, expected["motion_known"]?.bool)
                XCTAssertEqual(person.velocityXZ, expected["velocity_xz"]?.array?.compactMap(\.double))
            }
            if let hazard = view.hazard {
                XCTAssertEqual(hazard.position, expected["position_m"]?.array?.compactMap(\.double))
                XCTAssertEqual(hazard.confidence, expected["confidence"]?.double)
                XCTAssertEqual(hazard.confidenceMeaning, expected["confidence_meaning"]?.string)
                XCTAssertEqual(hazard.gasValuePPM, expected["gas_value_ppm"]?.double)
                XCTAssertEqual(hazard.gasUncertaintyPPM, expected["gas_uncertainty_ppm"]?.double)
                XCTAssertEqual(hazard.uncertaintyM, expected["uncertainty_m"]?.double)
            }
            XCTAssertEqual(view.sampleValidUntil, try NativeInference.time(expected["sample_valid_until"]))
            // Date is a floating display estimate; exact nanoseconds remain in
            // the original member objects and resultJSON bytes above.
            XCTAssertEqual(view.physicalValidUntil.timeIntervalSince1970,
                           try NativeInference.time(expected["physical_valid_until"]).timeIntervalSince1970, accuracy: 0.000001)
        }
    }

    func testPersonGeometryAndMotionRemainDistinctFromHazardMeasurements() throws {
        let all = try components().map { try NativeInference(component: WorldComponent(raw: $0)) }
        let people = all.compactMap(\.person), hazards = all.compactMap(\.hazard)
        XCTAssertEqual(people.count, 2); XCTAssertEqual(hazards.count, 1)
        XCTAssertEqual(Set(people.map(\.motionKnown)), [true, false])
        for person in people {
            XCTAssertEqual(person.positionXZ.count, 2); XCTAssertEqual(person.covariance.count, 4)
            XCTAssertEqual(person.velocityXZ != nil, person.motionKnown)
            XCTAssertFalse(person.current.isEmpty)
        }
        XCTAssertEqual(hazards[0].position.count, 3)
        XCTAssertGreaterThan(hazards[0].gasValuePPM, 0)
        XCTAssertGreaterThanOrEqual(hazards[0].gasUncertaintyPPM, 0)
        XCTAssertFalse(hazards[0].confidenceMeaning.isEmpty)
        XCTAssertEqual(hazards[0].inputs.filter { $0["kind"]?.string == "camera" }.count, 2)
    }

    func testSyntheticClockNeverBecomesFreshFromHostFetchTime() throws {
        for raw in try components() {
            let view = try NativeInference(component: WorldComponent(raw: raw))
            XCTAssertEqual(view.freshness(at: view.physicalValidUntil.addingTimeInterval(-0.01)), .unknown)
            XCTAssertEqual(view.freshness(at: view.physicalValidUntil), .stale)
            XCTAssertEqual(view.freshness(at: view.physicalValidUntil.addingTimeInterval(100)), .stale)
        }
    }

    func testMalformedGeometryMotionAndPrivacyExtensionsAreNotProjected() throws {
        let person = try components()[0]
        for mutate: (inout [String: JSONValue]) -> Void in [
            { $0["position_xz"] = .array([.integer(1), .null, .integer(2)]) },
            { $0["covariance"] = .array([.integer(1), .integer(2), .integer(0), .integer(1)]) },
            { $0["velocity_xz"] = .null },
            { $0["appearance"] = .string("unsupported") },
            { $0["run_id"] = .string("another-run") }
        ] { XCTAssertThrowsError(try NativeInference(component: changedBody(person, mutate))) }
    }

    func testBoundsSchemaAndExactInputBinding() throws {
        let original = try components()[0]
        for mutate: (inout [String: JSONValue]) -> Void in [
            { $0["result_schema"] = .string("unknown.inference") },
            { $0["result_json"] = .string("not-base64") },
            { $0["result_json"] = .string(Data(repeating: 32, count: 24 * 1024 + 1).base64EncodedString()) },
            { $0["inputs"] = .array([]) }
        ] {
            var raw = original.object!, payload = raw["payload"]!.object!; mutate(&payload); raw["payload"] = .object(payload)
            XCTAssertThrowsError(try NativeInference(component: WorldComponent(raw: .object(raw))))
        }
        var raw = original.object!, key = raw["key"]!.object!
        var tombstone = raw; tombstone["tombstone"] = .bool(true)
        XCTAssertThrowsError(try NativeInference(component: WorldComponent(raw: .object(tombstone))))
        key["site"] = .string("another-site"); raw["key"] = .object(key)
        XCTAssertThrowsError(try NativeInference(component: WorldComponent(raw: .object(raw))))
        for entry in try Self.fixture()["rejection_cases"]!.array! {
            XCTAssertThrowsError(try NativeInference(component: WorldComponent(raw: entry["component"]!)), entry["name"]!.string!)
        }
    }

    func testEvidenceAndDetectorMetadataRemainReferences() throws {
        for raw in try components() {
            let view = try NativeInference(component: WorldComponent(raw: raw))
            XCTAssertFalse(view.evidence.isEmpty)
            XCTAssertTrue(view.evidence.allSatisfy { $0.size > 0 && $0.size <= 8 << 20 && $0.sha256.count == 64 })
            for member in view.person?.current ?? [] {
                XCTAssertNotNil(member["model_id"]?.string)
                XCTAssertNotNil(member["calibration_revision"]?.string)
                XCTAssertNotNil(member["sample_time"]?.string)
            }
        }
    }
}
