import Foundation
import XCTest
@testable import WorldTwin

final class TimelineRenderTests: XCTestCase {
    @MainActor
    func testExportFailureReceiptIsBoundedAndDistinguishesCancellation() throws {
        let output = TestOutput.directory("render-failure")
        try WorldTimelineRenderer.writeFailure(CancellationError(), output: output)
        let path = output.appendingPathComponent("render-error.json")
        let first = try JSONDecoder().decode([String: String].self, from: Data(contentsOf: path))
        XCTAssertEqual(first["status"], "cancelled")
        try WorldTimelineRenderer.writeFailure(PromoFailure(message: String(repeating: "x", count: 10_000)), output: output)
        let bytes = try Data(contentsOf: path)
        let second = try JSONDecoder().decode([String: String].self, from: bytes)
        XCTAssertEqual(second["status"], "failed")
        XCTAssertEqual(second["error"]?.count, 1024)
        XCTAssertLessThan(bytes.count, 4096)
    }

    func testRenderArgumentsAreBoundedAndSeparateFromAuthority() throws {
        let arguments = ["--world-timeline", "/tmp/recorded-timeline", "--world-render-output", "/tmp/native-render", "--world-render-duration", "1", "--world-render-fps", "2"]
        XCTAssertNotNil(try WorldTimelineRenderRequest.parse(arguments))
        XCTAssertThrowsError(try WorldTimelineRenderRequest.parse(arguments + ["--world-session", "/tmp/run"]))
        XCTAssertThrowsError(try WorldTimelineRenderRequest.parse(arguments + ["--world-render-fps", "30"]))
        XCTAssertThrowsError(try WorldTimelineRenderRequest.parse(["--world-timeline", "/tmp/a", "--world-render-output", "/tmp/a/output", "--world-render-duration", "1", "--world-render-fps", "1"]))
    }

    func testClosingReviewNeverOverwritesApproval() throws {
        let directory = TestOutput.directory("review-close")
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        let approval = directory.appendingPathComponent("approved.json")
        let data = Data("{\"approved\":true}".utf8)
        try data.write(to: approval)
        WorldReviewCancellation.write(to: approval)
        XCTAssertEqual(try Data(contentsOf: approval), data)
        let cancelled = directory.appendingPathComponent("cancelled.json")
        WorldReviewCancellation.write(to: cancelled)
        XCTAssertEqual(try String(contentsOf: cancelled, encoding: .utf8), "{\"action\":\"cancel\"}")
    }

    @MainActor
    func testRetainedTimelineRendersNativeFrames() async throws {
        guard let input = ProcessInfo.processInfo.environment["WENDY_WORLD_TIMELINE_INPUT"] else {
            throw XCTSkip("Supply an immutable recorded timeline for native rendering.")
        }
        let output = TestOutput.directory("timeline-render")
        let duration = ProcessInfo.processInfo.environment["WENDY_WORLD_RENDER_TEST_DURATION"] ?? "2"
        let fps = ProcessInfo.processInfo.environment["WENDY_WORLD_RENDER_TEST_FPS"] ?? "1"
        let request = try XCTUnwrap(WorldTimelineRenderRequest.parse(["--world-timeline", input, "--world-render-output", output.path, "--world-render-duration", duration, "--world-render-fps", fps]))
        try await WorldTimelineRenderer.render(request)
        XCTAssertTrue(FileManager.default.fileExists(atPath: output.appendingPathComponent("render.json").path))
        XCTAssertTrue(FileManager.default.fileExists(atPath: output.appendingPathComponent("frames/000001.png").path))
        XCTAssertFalse(FileManager.default.fileExists(atPath: output.appendingPathComponent("playback/command.json").path))
    }
}
