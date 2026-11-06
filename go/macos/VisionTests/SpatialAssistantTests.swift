import XCTest
import RealityKit
@testable import WendySpatial

@MainActor
final class SpatialAssistantTests: XCTestCase {
    private func model() -> SpatialModel {
        let model = SpatialModel()
        model.robot = SpatialRobot(id: "live", name: "Live Go2", connected: true)
        model.reading = SpatialReading(message: "Ready", warning: "", pose: true, ready: true,
            aligned: true, moving: false, disconnected: false, position: [4, 0, 0], yaw: 0,
            route: [[4, 0, 0], [2, 0, 0]], hazards: [])
        model.readingAt = 100; model.telemetryAt = 100; model.telemetry = "CPU 12%"
        return model
    }
    func testStaleAndFutureReadingsAreExcluded() {
        let model = model()
        for now in [99.0, 101.0] {
            let context = SpatialAIContext.capture(model, now: now)
            XCTAssertNil(context.ready)
            XCTAssertNil(context.routePoints)
            XCTAssertNil(context.status)
            XCTAssertEqual(context.mode, "live observations unavailable or stale")
        }
        XCTAssertNil(SpatialAIContext.capture(model, now: 109).telemetry)
    }
    func testReplayUsesRecordedRobotWithoutCurrentTelemetryOrRoom() {
        let model = model()
        model.site = SpatialSite(name: "Current room", modelURL: URL(fileURLWithPath: "/room.usdz"), metresPerUnit: 1, cameras: [])
        model.replay = SpatialRecording(robot: "Recorded Go2", samples: [.init(elapsed: 4, reading: model.reading!)])
        let context = SpatialAIContext.capture(model, now: 100)
        XCTAssertEqual(context.mode, "recorded observations")
        XCTAssertEqual(context.robot, "Recorded Go2")
        XCTAssertEqual(context.sampleTime, 4)
        XCTAssertNil(context.telemetry)
        XCTAssertNil(context.site)
        XCTAssertNil(context.userPositionInSiteMetres)
        XCTAssertFalse(context.tracking)
        XCTAssertFalse(context.roomAligned)
    }
    func testQuestionSnapshotDoesNotChangeWithTelemetry() throws {
        let model = model()
        let context = SpatialAIContext.capture(model, now: 100)
        let message = SpatialAIMessage(user: true, text: "What is happening?", context: context.json())
        model.reading?.message = "Moving"
        model.telemetry = "CPU 98%"
        XCTAssertEqual(context.status, "Ready")
        XCTAssertTrue(message.wireText.contains("CPU 12%"))
        XCTAssertFalse(message.wireText.contains("CPU 98%"))
        let object = try XCTUnwrap(JSONSerialization.jsonObject(with: Data(context.json().utf8)) as? [String: Any])
        XCTAssertEqual(object["status"] as? String, "Ready")
    }
    func testPositionOnlyIncludedForCurrentRegistration() throws {
        let model = model()
        let url = URL(fileURLWithPath: "/room.usdz")
        model.site = SpatialSite(name: "Room", modelURL: url, metresPerUnit: 1, cameras: [])
        model.tracking.tracked = true; model.tracking.wearer = [0.123, 1.746, 0.281]
        XCTAssertNil(SpatialAIContext.capture(model, now: 100).userPositionInSiteMetres)
        model.registration = try SpatialRegistration.solve(siteA: .zero, siteB: [2, 0, 0], frameA: .zero, frameB: [2, 0, 0], frame: model.tracking.frameID, model: url)
        XCTAssertEqual(SpatialAIContext.capture(model, now: 100).userPositionInSiteMetres, [0.1, 1.7, 0.3])
        model.tracking.epoch = UUID()
        XCTAssertNil(SpatialAIContext.capture(model, now: 100).userPositionInSiteMetres)
    }
    func testObservationRendererReusesEntitiesAndHidesOldRoute() {
        let renderer = SpatialObservationRenderer()
        var reading = model().reading!
        renderer.update(reading)
        let original = Array(renderer.root.children)
        reading.position = [3, 0, 0]
        renderer.update(reading)
        XCTAssertEqual(original.map(\.id), renderer.root.children.map(\.id))
        XCTAssertEqual(renderer.root.children.first?.position, [3, 0, 0])
        reading.route = []
        renderer.update(reading)
        XCTAssertFalse(renderer.root.children[1].isEnabled)
        reading.pose = false
        renderer.update(reading)
        XCTAssertFalse(renderer.root.isEnabled)
    }
    func testBundledGo2HasMetricGeometry() async throws {
        await SpatialAssets.load()
        let robot = try XCTUnwrap(SpatialAssets.robot)
        let bounds = robot.visualBounds(relativeTo: robot)
        XCTAssertGreaterThan(bounds.extents.x, 0.5)
        XCTAssertLessThan(bounds.extents.x, 1.2)
        XCTAssertGreaterThan(bounds.extents.y, 0.25)
        XCTAssertLessThan(bounds.extents.y, 0.8)
    }
}
