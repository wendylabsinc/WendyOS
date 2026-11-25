import XCTest
@testable import WorldTwin

final class LaunchTests: XCTestCase {
    func testExplicitSessionAndReadOnlyReplay() throws {
        XCTAssertNil(try WorldTwinLaunchRequest.parse(["WendyDesktop"]))
        let live = try XCTUnwrap(WorldTwinLaunchRequest.parse(["WendyDesktop", "--world-session", "/tmp/session"]))
        XCTAssertEqual(live.mode, .session)
        XCTAssertFalse(live.readOnly)
        let replay = try XCTUnwrap(WorldTwinLaunchRequest.parse(["WendyDesktop", "--world-session", "/tmp/session", "--world-replay", "--mission-config", "/tmp/operator.json"]))
        XCTAssertTrue(replay.readOnly)
        XCTAssertNil(replay.mission)
        XCTAssertNil(replay.viewer)
    }

    func testReviewRequiresExplicitPathsAndSingleMode() throws {
        XCTAssertThrowsError(try WorldTwinLaunchRequest.parse(["--world-session", "relative"]))
        XCTAssertThrowsError(try WorldTwinLaunchRequest.parse(["--world-replay"]))
        XCTAssertThrowsError(try WorldTwinLaunchRequest.parse(["--world-session", "/tmp/a", "--incident", "/tmp/b"]))
        XCTAssertThrowsError(try WorldTwinLaunchRequest.parse(["--patrol-schedule-review", "/tmp/review.json"]))
        let review = try XCTUnwrap(WorldTwinLaunchRequest.parse(["--room-inspection-review", "/tmp/input.json", "--schedule-output", "/tmp/approval.json"]))
        XCTAssertEqual(review.mode, .tour)
        XCTAssertEqual(review.output?.path, "/tmp/approval.json")
        XCTAssertThrowsError(try WorldTwinLaunchRequest.parse(["--camera-profile", "/tmp/profile.json", "--world-replay"]))
    }

    @MainActor
    func testReplayRefusesDirectWritesAndCloseSignal() throws {
        let directory = TestOutput.directory("read-only")
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        let model = DemoModel(session: directory, capture: nil, readOnly: true)
        model.presentationReplay = false
        XCTAssertFalse(model.canCommand)
        XCTAssertEqual(model.statusBadge, "SIMULATION REPLAY")
        XCTAssertThrowsError(try model.exclusiveJSON(["action": "start"], path: directory.appendingPathComponent("command.json")))
        model.command("start")
        model.close()
        XCTAssertTrue(try FileManager.default.contentsOfDirectory(atPath: directory.path).isEmpty)
    }
}
