import Foundation
import XCTest
@testable import WorldClient

final class ReadModeTests: XCTestCase {
    func testProfileModeIsExplicitAndOldProfileDefaultsToFull() throws {
        XCTAssertEqual(try WorldReadMode(profile: .object([:])), .full)
        XCTAssertEqual(try WorldReadMode(profile: .object(["read_mode": .string("full")])), .full)
        XCTAssertEqual(try WorldReadMode(profile: .object(["read_mode": .string("native_inference")])), .nativeInference)
        for value in [JSONValue.null, .bool(true), .string("native"), .integer(1)] {
            XCTAssertThrowsError(try WorldReadMode(profile: .object(["read_mode": value])))
        }
    }

    func testNativeCacheReopensExactQueryWithUnavailableSectionsOnlyInMatchingMode() throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: dir) }
        let snapshot = try WorldSnapshot(data: NativeInferenceTests.fixture()["snapshot"]!.encoded())
        let cache = OfflineSnapshotCache(directory: dir), scope = String(repeating: "a", count: 64)
        let model = CachedWorldReadModel(snapshot: snapshot, sites: .object([:]), tasks: .object([:]), fetchedAt: Date(), scopeID: scope, readMode: .nativeInference)
        try cache.save(model)
        let read = try XCTUnwrap(cache.load(scopeID: scope, readMode: .nativeInference))
        XCTAssertEqual(read.snapshot.originalJSON, snapshot.originalJSON)
        XCTAssertEqual(read.readMode, .nativeInference); XCTAssertTrue(read.isLastKnown)
        XCTAssertEqual(read.sites, .object([:])); XCTAssertEqual(read.tasks, .object([:]))
        XCTAssertNil(try cache.load(scopeID: scope))
        let fakeEmpty = CachedWorldReadModel(snapshot: snapshot, sites: .object(["publications": .array([])]), tasks: .object(["entries": .array([])]), fetchedAt: Date(), scopeID: scope, readMode: .nativeInference)
        XCTAssertThrowsError(try cache.save(fakeEmpty))
        XCTAssertEqual(try cache.load(scopeID: scope, readMode: .nativeInference)?.snapshot.originalJSON, snapshot.originalJSON)
    }

    func testOldCachePayloadDefaultsToFullAndFullStillRequiresCompleteSections() throws {
        let old = CachedWorldReadModel(snapshot: try WorldSnapshot(data: Data(#"{"components":[],"cursor":"old"}"#.utf8)),
            sites: .object(["publications": .array([])]), tasks: .object(["entries": .array([]), "last_known": .bool(true)]),
            fetchedAt: Date().addingTimeInterval(-1), scopeID: String(repeating: "b", count: 64))
        let encoder = JSONEncoder(), decoder = JSONDecoder()
        encoder.dateEncodingStrategy = .millisecondsSince1970; decoder.dateDecodingStrategy = .millisecondsSince1970
        var raw = try JSONValue.decode(encoder.encode(old)).object!
        raw.removeValue(forKey: "read_mode")
        let restored = try decoder.decode(CachedWorldReadModel.self, from: JSONValue.object(raw).encoded())
        XCTAssertEqual(restored.readMode, .full); XCTAssertEqual(restored.snapshot, old.snapshot)
        raw["read_mode"] = .string("unknown")
        XCTAssertThrowsError(try decoder.decode(CachedWorldReadModel.self, from: JSONValue.object(raw).encoded()))
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: dir) }
        let cache = OfflineSnapshotCache(directory: dir)
        try cache.save(restored)
        XCTAssertNil(try cache.load(scopeID: restored.scopeID, readMode: .nativeInference))
        let partial = CachedWorldReadModel(snapshot: old.snapshot, sites: .object([:]), tasks: .object([:]), fetchedAt: Date(), scopeID: old.scopeID)
        XCTAssertThrowsError(try cache.save(partial))
    }
}
