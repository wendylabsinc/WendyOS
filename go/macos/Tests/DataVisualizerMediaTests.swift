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
    func testAudioAdoptionPreservesWAVExtensionAndLifetime() throws {
        let source = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try Data([1, 2]).write(to: source)
        var owner: DataVisualMediaFiles? = DataVisualMediaFiles()
        let audio = try XCTUnwrap(owner).adopt(source, extension: "wav")
        XCTAssertEqual(audio.pathExtension, "wav")
        XCTAssertTrue(FileManager.default.fileExists(atPath: audio.path))
        owner = nil
        XCTAssertFalse(FileManager.default.fileExists(atPath: audio.path))
    }

    func testPreviewExportsPreserveNanosecondTimeAndCoordinates() {
        let sample = DataVisualSample(id: 1, track: "topic,\"quoted\"", label: "", detail: "", unit: "m/s", timestamp: -123456789, value: 1.25, image: nil, points: [SIMD3<Float>(1, 2, 3)])
        let csv = DataVisualExport.csv([sample])
        XCTAssertTrue(csv.contains("-123456789,\"topic,\"\"quoted\"\"\",1.25,\"m/s\""))
        let ply = DataVisualExport.ply(sample)
        XCTAssertTrue(ply.contains("element vertex 1\n"))
        XCTAssertTrue(ply.contains("episode_nanoseconds -123456789"))
        XCTAssertTrue(ply.hasSuffix("1.0 2.0 3.0\n"))
    }

}
