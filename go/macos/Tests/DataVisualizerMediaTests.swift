import XCTest
@testable import WendyDesktop

final class DataVisualizerMediaTests: XCTestCase {
    func testAdoptionMovesVerifiedMediaAndReleasesItWithOwner() throws {
        let source = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        let content = Data([1, 2, 3, 4])
        try content.write(to: source)
        defer { try? FileManager.default.removeItem(at: source) }
        var owner: DataVisualMediaFiles? = DataVisualMediaFiles()
        let adopted = try XCTUnwrap(owner).adopt(source)
        XCTAssertFalse(FileManager.default.fileExists(atPath: source.path))
        XCTAssertEqual(try Data(contentsOf: adopted), content)
        // Freeing the bridge's original pathname must not remove adopted media.
        try? FileManager.default.removeItem(at: source)
        XCTAssertTrue(FileManager.default.fileExists(atPath: adopted.path))
        owner = nil
        XCTAssertFalse(FileManager.default.fileExists(atPath: adopted.path))
    }

    func testFailedAdoptionDoesNotCreateAFile() {
        let missing = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        let owner = DataVisualMediaFiles()
        XCTAssertThrowsError(try owner.adopt(missing))
        XCTAssertFalse(FileManager.default.fileExists(atPath: missing.path))
    }
}
