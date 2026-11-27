import Foundation
import XCTest
@testable import WorldClient

final class GeographicPlacementTests: XCTestCase {
    let at = ISO8601DateFormatter().date(from: "2026-11-10T00:00:00Z")!
    func document(v2: Bool = false) throws -> JSONValue {
        var fields = try JSONValue.decode(Data(#"{"schema_version":"wendy.world.georeference.v1","site":"office","revision":"geo-1","package_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","root":{"id":"root","revision":"1"},"anchor":{"latitude_degrees":52,"longitude_degrees":5,"altitude_metres":0,"altitude_datum":"WGS84-ellipsoid","uncertainty_metres":100},"axes":["east","up","south"],"placement":"illustrative","placement_source":"Synthetic test fixture","valid_from":"2026-01-01T00:00:00Z","valid_until":"2027-01-01T00:00:00Z","frames":[{"ref":{"id":"floor","revision":"1"},"parent":{"id":"root","revision":"1"},"translation":[0,0,0],"rotation_xyzw":[0,0,0,1],"uncertainty_metres":0.1,"valid_from":"2026-01-01T00:00:00Z","valid_until":"2027-01-01T00:00:00Z"}]}"#.utf8)).object!
        if v2 {
            fields["schema_version"] = .string("wendy.world.georeference.v2")
            fields["root_basis_error"] = .object(["known": .bool(false)])
            var frame = fields["frames"]!.array![0].object!
            frame["rotation_error"] = .object(["known": .bool(false)])
            fields["frames"] = .array([.object(frame)])
        }
        return .object(fields)
    }
    func replacing(_ document: JSONValue, _ key: String, _ value: JSONValue?) -> JSONValue {
        var fields = document.object!; fields[key] = value; return .object(fields)
    }
    func frameReplacing(_ document: JSONValue, _ key: String, _ value: JSONValue?) -> JSONValue {
        var frame = document["frames"]!.array![0].object!; frame[key] = value
        return replacing(document, "frames", .array([.object(frame)]))
    }

    func testLegacyAndExplicitUnknownNeverBecomeZero() throws {
        for v2 in [false, true] {
            let doc = try document(v2: v2), placement = GeographicPlacement(document: doc)
            XCTAssertEqual(placement.state(at: at), .usable)
            XCTAssertEqual(placement.rootOrientationError, .unknown)
            XCTAssertEqual(placement.frameOrientationErrors, [.unknown])
            XCTAssertEqual(placement.rootOrientationText, "Root orientation: unknown")
            XCTAssertTrue(placement.frameOrientationText.contains("1 unknown"))
            XCTAssertEqual(placement.document, doc)
        }
    }
    func testKnownBoundsIncludeExplicitZeroAndPi() throws {
        var doc = try document(v2: true)
        doc = replacing(doc, "root_basis_error", .object(["known": .bool(true), "max_angle_radians": .integer(0)]))
        doc = frameReplacing(doc, "rotation_error", .object(["known": .bool(true), "max_angle_radians": .number(.pi)]))
        let placement = GeographicPlacement(document: doc)
        XCTAssertEqual(placement.state(at: at), .usable)
        XCTAssertEqual(placement.rootOrientationError, .bounded(0))
        XCTAssertEqual(placement.frameOrientationErrors, [.bounded(.pi)])
        XCTAssertTrue(placement.rootOrientationText.contains("0° declared bound"))
    }
    func testMalformedAngularTagsRefuseSpatialUse() throws {
        let invalid: [JSONValue?] = [nil, .null, .integer(0), .array([]), .object([:]),
            .object(["known": .integer(1)]), .object(["known": .bool(true)]),
            .object(["known": .bool(false), "max_angle_radians": .integer(0)]),
            .object(["known": .bool(false), "max_angle_radians": .null]),
            .object(["known": .bool(true), "max_angle_radians": .null]),
            .object(["known": .bool(true), "max_angle_radians": .bool(false)]),
            .object(["known": .bool(true), "max_angle_radians": .string("0")]),
            .object(["known": .bool(true), "max_angle_radians": .number(-0.01)]),
            .object(["known": .bool(true), "max_angle_radians": .number(.pi.nextUp)]),
            .object(["known": .bool(true), "max_angle_radians": .number(.infinity)]),
            .object(["known": .bool(true), "max_angle_radians": .number(.nan)]),
            .object(["known": .bool(true), "max_angle_radians": .integer(0), "extra": .bool(true)])]
        let doc = try document(v2: true)
        for (index, value) in invalid.enumerated() {
            for changed in [replacing(doc, "root_basis_error", value), frameReplacing(doc, "rotation_error", value)] {
                let parsed = GeographicPlacement(document: changed)
                XCTAssertEqual(parsed.state(at: at), .invalid, "case \(index)")
                XCTAssertNil(parsed.latitude)
                XCTAssertNil(parsed.rootOrientationError)
            }
        }
    }
    func testLegacyRejectsNewFieldsAndUnknownVersionIsNotExpiry() throws {
        let legacy = try document()
        for changed in [replacing(legacy, "root_basis_error", .object(["known": .bool(false)])),
                        frameReplacing(legacy, "rotation_error", .object(["known": .bool(false)]))] {
            XCTAssertEqual(GeographicPlacement(document: changed).state(at: at), .invalid)
        }
        let future = GeographicPlacement(document: replacing(legacy, "schema_version", .string("wendy.world.georeference.v3")))
        XCTAssertEqual(future.state(at: at), .unsupported)
        XCTAssertNil(future.latitude)
        XCTAssertEqual(future.state(at: at).message, "Unsupported geographic placement version")
    }
    func testExactShapesGraphAndValidity() throws {
        let doc = try document(v2: true)
        for changed in [replacing(doc, "axes", .array([.string("east"), .null, .string("up"), .string("south")])),
                        replacing(doc, "frames", .null),
                        frameReplacing(doc, "rotation_xyzw", .array([.integer(0), .integer(0), .integer(1)])),
                        frameReplacing(doc, "parent", .object(["id": .string("absent"), "revision": .string("1")])),
                        frameReplacing(doc, "uncertainty_metres", .integer(-1)),
                        replacing(doc, "valid_from", .string("0001-01-01T00:00:00Z")),
                        frameReplacing(doc, "valid_from", .string("0001-01-01T00:00:00Z")),
                        replacing(doc, "extra", .bool(true))] {
            XCTAssertEqual(GeographicPlacement(document: changed).state(at: at), .invalid)
        }
        let p = GeographicPlacement(document: doc)
        XCTAssertEqual(p.state(at: ISO8601DateFormatter().date(from: "2025-12-31T23:59:59Z")!), .notYetValid)
        XCTAssertEqual(p.state(at: ISO8601DateFormatter().date(from: "2027-01-01T00:00:00Z")!), .expired)
        for v2 in [false, true] {
            let base = try document(v2: v2)
            var anchor = base["anchor"]!.object!
            anchor["uncertainty_metres"] = .number(1e9)
            XCTAssertEqual(GeographicPlacement(document: replacing(base, "anchor", .object(anchor))).state(at: at), .usable)
            anchor["uncertainty_metres"] = .number(1e9 + 1)
            XCTAssertEqual(GeographicPlacement(document: replacing(base, "anchor", .object(anchor))).state(at: at), .invalid)
        }
    }
    func testCacheRetainsLegacyAbsenceV2UnknownAndOriginalValidity() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: directory) }
        let cache = OfflineSnapshotCache(directory: directory), scope = String(repeating: "a", count: 64)
        for v2 in [false, true] {
            let doc = try document(v2: v2)
            let sites: JSONValue = .object(["publications": .array([.object(["component": .object(["payload": doc])])])])
            let model = CachedWorldReadModel(snapshot: try WorldSnapshot(data: Data(#"{"components":[],"cursor":""}"#.utf8)), sites: sites,
                tasks: .object(["entries": .array([]), "last_known": .bool(true)]), fetchedAt: Date(), scopeID: scope)
            try cache.save(model)
            let restored = try XCTUnwrap(cache.load(scopeID: scope))
            XCTAssertEqual(restored.sites, sites)
            let restoredDoc = try XCTUnwrap(restored.sites["publications"]?.array?[0]["component"]?["payload"])
            XCTAssertEqual(restoredDoc["root_basis_error"], doc["root_basis_error"])
            XCTAssertEqual(GeographicPlacement(document: restoredDoc).rootOrientationError, .unknown)
            XCTAssertEqual(GeographicPlacement(document: restoredDoc).state(at: ISO8601DateFormatter().date(from: "2027-01-01T00:00:00Z")!), .expired)
        }
    }
}
