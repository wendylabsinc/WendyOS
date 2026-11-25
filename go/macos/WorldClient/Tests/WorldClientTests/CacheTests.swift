import Darwin
import Foundation
import XCTest
@testable import WorldClient

final class CacheTests: XCTestCase {
    let scope = String(repeating: "a", count: 64)

    func snapshot() throws -> WorldSnapshot {
        try WorldSnapshot(data: Data(#"{"components":[{"key":{"tenant":"lab","site":"synthetic-office","entity":"gas-1","type":"gas"},"timing":{"sample_time":"2026-11-10T12:00:00.123456789Z","clock_domain":"utc","error_known":true,"error_bound_ns":20000000,"age_hint_ns":0},"ttl_ns":2000000000,"provenance":{"origin":"inferred","inputs":[{"origin":"simulated","simulation":{"run_id":"run-1"}}]},"payload":{"unit":"ppm","concentration":12}}],"cursor":"native-test"}"#.utf8))
    }
    func model() throws -> CachedWorldReadModel {
        CachedWorldReadModel(snapshot: try snapshot(), sites: .object(["publications": .array([])]), tasks: .object(["entries": .array([]), "last_known": .bool(true)]), fetchedAt: Date().addingTimeInterval(-10), scopeID: scope)
    }
    func directory() -> URL { FileManager.default.temporaryDirectory.appendingPathComponent("world-cache-test-" + UUID().uuidString, isDirectory: true) }

    func testCachePreservesOriginalTimesAndSyntheticAncestry() throws {
        let directory = directory()
        defer { try? FileManager.default.removeItem(at: directory) }
        let cache = OfflineSnapshotCache(directory: directory)
        let model = try model()
        try cache.save(model)
        let read = try XCTUnwrap(cache.load(scopeID: scope))
        XCTAssertEqual(read.snapshot.originalJSON, model.snapshot.originalJSON)
        XCTAssertEqual(read.snapshot.components, model.snapshot.components)
        XCTAssertEqual(read.snapshot.components[0].raw["provenance"]?["inputs"]?.array?[0]["origin"]?.string, "simulated")
        XCTAssertEqual(read.fetchedAt.timeIntervalSince1970, model.fetchedAt.timeIntervalSince1970, accuracy: 0.001)
        XCTAssertTrue(read.isLastKnown)
        let file = directory.appendingPathComponent(scope + ".json")
        let attributes = try FileManager.default.attributesOfItem(atPath: file.path)
        XCTAssertEqual(attributes[.posixPermissions] as? NSNumber, NSNumber(value: 0o600))
        XCTAssertNil(try cache.load(scopeID: String(repeating: "b", count: 64)))
        XCTAssertThrowsError(try cache.load(scopeID: "../outside"))
    }

    func testCacheRejectsTamperingAndSymlinks() throws {
        let directory = directory()
        defer { try? FileManager.default.removeItem(at: directory) }
        let cache = OfflineSnapshotCache(directory: directory)
        try cache.save(model())
        let file = directory.appendingPathComponent(scope + ".json")
        var envelope = try JSONValue.decode(Data(contentsOf: file)).object!
        envelope["sha256"] = .string(String(repeating: "0", count: 64))
        try JSONValue.object(envelope).encoded().write(to: file)
        XCTAssertThrowsError(try cache.load(scopeID: scope))
        try FileManager.default.removeItem(at: file)
        try FileManager.default.createSymbolicLink(at: file, withDestinationURL: directory.appendingPathComponent("other.json"))
        XCTAssertThrowsError(try cache.load(scopeID: scope))
    }

    func testCacheRejectsWidePermissionsAndFutureFetchTime() throws {
        let directory = directory()
        defer { try? FileManager.default.removeItem(at: directory) }
        let cache = OfflineSnapshotCache(directory: directory)
        try cache.save(model())
        let file = directory.appendingPathComponent(scope + ".json")
        XCTAssertEqual(chmod(file.path, 0o644), 0)
        XCTAssertThrowsError(try cache.load(scopeID: scope))
        let original = try model()
        let future = CachedWorldReadModel(snapshot: original.snapshot, sites: original.sites, tasks: original.tasks, fetchedAt: Date().addingTimeInterval(3600), scopeID: scope)
        XCTAssertThrowsError(try cache.save(future))
        XCTAssertEqual(chmod(directory.path, 0o755), 0)
        XCTAssertThrowsError(try cache.save(original))
    }

    func testFreshnessDoesNotResetOnLoadOrAssumeUnknownClockIsZero() throws {
        let component = try snapshot().components[0]
        let sample = try XCTUnwrap(component.sampleTime)
        XCTAssertEqual(component.freshness(at: sample.addingTimeInterval(0.5)), .fresh)
        XCTAssertEqual(component.freshness(at: sample.addingTimeInterval(2)), .stale)
        XCTAssertEqual(component.freshness(at: sample.addingTimeInterval(-1)), .unknown)
        var raw = component.raw.object!
        var timing = raw["timing"]!.object!
        timing["error_known"] = .bool(false)
        raw["timing"] = .object(timing)
        XCTAssertEqual(try WorldComponent(raw: .object(raw)).freshness(at: sample), .unknown)
        timing["error_known"] = .bool(true); timing["clock_domain"] = .string("simulation:paused")
        raw["timing"] = .object(timing)
        XCTAssertEqual(try WorldComponent(raw: .object(raw)).freshness(at: sample), .unknown)
        timing["clock_domain"] = .string("utc"); timing["age_hint_ns"] = .integer(3_000_000_000)
        raw["timing"] = .object(timing)
        XCTAssertEqual(try WorldComponent(raw: .object(raw)).freshness(at: sample), .stale)
    }

    func testUnavailableSectionsNeverOverwritePreviousCompleteCache() throws {
        let directory = directory()
        defer { try? FileManager.default.removeItem(at: directory) }
        let cache = OfflineSnapshotCache(directory: directory)
        let previous = try model()
        try cache.save(previous)
        for sitesUnavailable in [true, false] {
            let partial = CachedWorldReadModel(
                snapshot: previous.snapshot,
                sites: sitesUnavailable ? .object([:]) : previous.sites,
                tasks: sitesUnavailable ? previous.tasks : .object([:]),
                fetchedAt: Date(), scopeID: previous.scopeID)
            XCTAssertThrowsError(try cache.save(partial))
            let retained = try XCTUnwrap(cache.load(scopeID: scope))
            XCTAssertEqual(retained.snapshot, previous.snapshot)
            XCTAssertEqual(retained.sites, previous.sites)
            XCTAssertEqual(retained.tasks, previous.tasks)
            XCTAssertEqual(retained.fetchedAt.timeIntervalSince1970, previous.fetchedAt.timeIntervalSince1970, accuracy: 0.001)
        }
    }
}
