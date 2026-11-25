import Darwin
import Foundation
import XCTest
@testable import WorldClient

@MainActor
final class InspectionMediaCacheTests: XCTestCase {
    private let scope = String(repeating: "a", count: 64)
    private let now = Date(timeIntervalSince1970: 1_794_528_000)

    private func directory() -> URL {
        let value = FileManager.default.temporaryDirectory.appendingPathComponent("inspection-media-" + UUID().uuidString)
        addTeardownBlock { try? FileManager.default.removeItem(at: value) }
        return value
    }

    private func media(_ byte: UInt8 = 1, size: Int = 500, until: Date? = nil) throws -> (VerifiedEvidence, MissionInspectionEvidence) {
        let bytes = Data(repeating: byte, count: size), hash = WorldCredentials.digest(bytes)
        return (VerifiedEvidence(data: bytes, sha256: hash, contentType: "image/png", retainUntil: until ?? now.addingTimeInterval(60)),
                try MissionInspectionEvidence(sha256: hash, contentType: "image/png", size: size))
    }

    func testReopenPreservesBytesDeadlineAndCredentialScope() async throws {
        let directory = directory(), (original, reference) = try media()
        let first = try InspectionMediaCache(directory: directory, scopeID: scope)
        try await first.store(original, reference: reference, now: now)
        let reopened = try InspectionMediaCache(directory: directory, scopeID: scope)
        let loaded = try await reopened.load(reference: reference, now: now.addingTimeInterval(10))
        let actual = try XCTUnwrap(loaded)
        XCTAssertEqual(actual.data, original.data)
        XCTAssertEqual(actual.sha256, original.sha256)
        XCTAssertEqual(actual.contentType, original.contentType)
        XCTAssertEqual(actual.retainUntil, original.retainUntil)
        let other = try InspectionMediaCache(directory: directory, scopeID: String(repeating: "b", count: 64))
        let absent = try await other.load(reference: reference, now: now)
        XCTAssertNil(absent)
        let folder = directory.appendingPathComponent(scope)
        let file = folder.appendingPathComponent(reference.sha256 + ".json")
        XCTAssertEqual(try FileManager.default.attributesOfItem(atPath: folder.path)[.posixPermissions] as? NSNumber, NSNumber(value: 0o700))
        XCTAssertEqual(try FileManager.default.attributesOfItem(atPath: file.path)[.posixPermissions] as? NSNumber, NSNumber(value: 0o600))
    }

    func testOriginalRetentionCannotBeRenewedAndExactExpiryIsExplicit() async throws {
        let cache = try InspectionMediaCache(directory: directory(), scopeID: scope)
        let (original, reference) = try media(until: now.addingTimeInterval(10))
        try await cache.store(original, reference: reference, now: now)
        let extended = VerifiedEvidence(data: original.data, sha256: original.sha256,
            contentType: original.contentType, retainUntil: now.addingTimeInterval(100))
        try await cache.store(extended, reference: reference, now: now.addingTimeInterval(5))
        let read = try await cache.load(reference: reference, now: now.addingTimeInterval(9))
        XCTAssertEqual(read?.retainUntil, original.retainUntil)
        do { _ = try await cache.load(reference: reference, now: original.retainUntil); XCTFail("expired media returned") }
        catch { XCTAssertEqual(error as? InspectionMediaCacheError, .expired) }
        do { try await cache.store(extended, reference: reference, now: original.retainUntil); XCTFail("known expiry renewed") }
        catch { XCTAssertEqual(error as? InspectionMediaCacheError, .expired) }
    }

    func testExpectedDigestSizeAndMIMEAreCheckedOnStoreAndLoad() async throws {
        let cache = try InspectionMediaCache(directory: directory(), scopeID: scope)
        let (original, reference) = try media()
        let wrongReferences = [
            try MissionInspectionEvidence(sha256: String(repeating: "0", count: 64), contentType: reference.contentType, size: reference.size),
            try MissionInspectionEvidence(sha256: reference.sha256, contentType: reference.contentType, size: reference.size + 1),
            try MissionInspectionEvidence(sha256: reference.sha256, contentType: "audio/wav", size: reference.size),
        ]
        for wrong in wrongReferences {
            do { try await cache.store(original, reference: wrong, now: now); XCTFail("mismatched evidence stored") }
            catch { XCTAssertEqual(error as? WorldClientError, .invalidEvidence) }
        }
        let damaged = VerifiedEvidence(data: Data(repeating: 2, count: original.data.count), sha256: original.sha256,
                                       contentType: original.contentType, retainUntil: original.retainUntil)
        do { try await cache.store(damaged, reference: reference, now: now); XCTFail("wrong bytes stored") }
        catch { XCTAssertEqual(error as? WorldClientError, .invalidEvidence) }
        try await cache.store(original, reference: reference, now: now)
        for wrong in wrongReferences.dropFirst() {
            do { _ = try await cache.load(reference: wrong, now: now); XCTFail("mismatched cached evidence returned") }
            catch { XCTAssertEqual(error as? WorldClientError, .invalidEvidence) }
        }
    }

    func testPersistentBudgetEvictsEarliestRetentionAndRejectsOversizedEntry() async throws {
        let directory = directory(), budget = 2500
        let cache = try InspectionMediaCache(directory: directory, scopeID: scope, maxBytes: budget)
        let (early, a) = try media(1, until: now.addingTimeInterval(10))
        let (later, b) = try media(2, until: now.addingTimeInterval(20))
        let (last, c) = try media(3, until: now.addingTimeInterval(30))
        for (value, reference) in [(early, a), (later, b), (last, c)] {
            try await cache.store(value, reference: reference, now: now)
        }
        let old = try await cache.load(reference: a, now: now)
        let remaining = try await cache.load(reference: c, now: now)
        XCTAssertNil(old)
        XCTAssertEqual(remaining?.data, last.data)
        let files = try FileManager.default.contentsOfDirectory(at: directory.appendingPathComponent(scope), includingPropertiesForKeys: nil)
        let total = try files.filter { $0.pathExtension == "json" }.reduce(0) { try $0 + Data(contentsOf: $1).count }
        XCTAssertLessThanOrEqual(total, budget)
        let (huge, ref) = try media(4, size: budget)
        do { try await cache.store(huge, reference: ref, now: now); XCTFail("oversized entry stored") }
        catch { XCTAssertEqual(error as? InspectionMediaCacheError, .capacityExceeded) }
        let stillThere = try await cache.load(reference: c, now: now)
        XCTAssertEqual(stillThere?.retainUntil, last.retainUntil)
    }

    func testDamagedAndNonPrivateCacheFilesAreNotReturned() async throws {
        let directory = directory(), (original, reference) = try media()
        let cache = try InspectionMediaCache(directory: directory, scopeID: scope)
        try await cache.store(original, reference: reference, now: now)
        let file = directory.appendingPathComponent(scope).appendingPathComponent(reference.sha256 + ".json")
        XCTAssertEqual(chmod(file.path, 0o644), 0)
        do { _ = try await cache.load(reference: reference, now: now); XCTFail("public cache file read") }
        catch { XCTAssertEqual(error as? WorldClientError, .insecureCache) }
        XCTAssertEqual(chmod(file.path, 0o600), 0)
        var raw = try Data(contentsOf: file)
        raw.append(0)
        try raw.write(to: file)
        do { _ = try await cache.load(reference: reference, now: now); XCTFail("damaged cache returned") }
        catch { XCTAssertEqual(error as? WorldClientError, .invalidCache) }
        try FileManager.default.removeItem(at: file)
        try FileManager.default.createSymbolicLink(at: file, withDestinationURL: directory.appendingPathComponent("elsewhere"))
        do { _ = try await cache.load(reference: reference, now: now); XCTFail("symlink cache read") }
        catch { XCTAssertEqual(error as? WorldClientError, .insecureCache) }
        XCTAssertThrowsError(try InspectionMediaCache(directory: directory, scopeID: "../invalid"))
    }

    func testExistingDownloaderFeedsCacheThenReopensWithoutServer() async throws {
        let bytes = Data((0..<(60 << 10)).map { UInt8($0 % 251) }), digest = WorldCredentials.digest(bytes)
        let until = Int64(Date().addingTimeInterval(3600).timeIntervalSince1970 * 1000)
        let server = try LoopbackServer { request in
            guard let value = try? JSONValue.decode(request.body), let offset = value["offset"]?.int,
                  offset >= 0, offset < bytes.count else { return .init(status: 400) }
            let end = min(Int(offset) + (48 << 10), bytes.count)
            let response: JSONValue = .object([
                "sha256": .string(digest), "size": .integer(Int64(bytes.count)), "offset": .integer(offset),
                "data": .string(Data(bytes[Int(offset)..<end]).base64EncodedString()),
                "content_type": .string("image/png"), "retain_until_unix_ms": .integer(until),
            ])
            return .init(body: (try? response.encoded()) ?? Data())
        }
        defer { server.stop() }
        // Same unsigned local profile fixture used by TransportTests; no real
        // gateway accepts this test grant. The actual downloader verifies bytes.
        var config = try ProtocolTests.vectors()[0]["credentials"]!.object!
        var signed = config["grant"]!.object!, grant = signed["grant"]!.object!
        grant["not_before_unix_ms"] = .integer(Int64(Date().addingTimeInterval(-3600).timeIntervalSince1970 * 1000))
        grant["not_after_unix_ms"] = .integer(until)
        signed["grant"] = .object(grant); config["grant"] = .object(signed)
        config["url"] = .string(server.url.absoluteString)
        let credentials = try WorldCredentials(data: JSONValue.object(config).encoded())
        let downloaded = try await WorldClient(credentials: credentials).downloadEvidence(digest: digest)
        let reference = try MissionInspectionEvidence(sha256: digest, contentType: "image/png", size: bytes.count)
        let directory = directory(), cache = try InspectionMediaCache(directory: directory, scopeID: credentials.scopeID)
        try await cache.store(downloaded, reference: reference)
        XCTAssertEqual(server.requests.count, 2)
        server.stop()
        let reopened = try InspectionMediaCache(directory: directory, scopeID: credentials.scopeID)
        let saved = try await reopened.load(reference: reference)
        XCTAssertEqual(saved?.data, bytes)
        XCTAssertEqual(saved?.retainUntil, downloaded.retainUntil)
        XCTAssertEqual(server.requests.count, 2)
    }
}
