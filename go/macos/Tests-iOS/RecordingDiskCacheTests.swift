import XCTest
@testable import WendyMobile

final class RecordingDiskCacheTests: XCTestCase {
    func testPersistsAcrossInstancesAndSeparatesAccounts() async throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: root) }
        let first = RecordingDiskCache(directory: root)
        await first.write(Data("clip".utf8), key: "account-a/device/episode")
        let reopened = RecordingDiskCache(directory: root)
        let restored = await reopened.read("account-a/device/episode")
        let other = await reopened.read("account-b/device/episode")
        XCTAssertEqual(restored, Data("clip".utf8))
        XCTAssertNil(other)
    }
    func testBoundedCacheAndOversizedEntry() async throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: root) }
        let cache = RecordingDiskCache(directory: root, byteLimit: 8, entryLimit: 1)
        await cache.write(Data(repeating: 1, count: 4), key: "a")
        await cache.write(Data(repeating: 2, count: 4), key: "b")
        await cache.write(Data(repeating: 3, count: 9), key: "large")
        let entries = try FileManager.default.contentsOfDirectory(atPath: root.path)
        let large = await cache.read("large")
        XCTAssertEqual(entries.count, 1)
        XCTAssertNil(large)
    }
}
