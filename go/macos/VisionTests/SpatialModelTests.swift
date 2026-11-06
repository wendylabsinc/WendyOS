import XCTest
import QuartzCore
import RealityKit
@testable import WendySpatial

@MainActor
final class SpatialModelTests: XCTestCase {
    func testRepeatedPinUpdatesAndReplacementCardsDoNotLeaveCopies() {
        let renderer = SpatialLabelRenderer()
        let label = SpatialLabel(id: "device", name: "Gateway", lite: true, position: .zero)
        var previous: Entity?
        for _ in 0..<500 {
            let card = Entity()
            renderer.update([label], card: { _ in card }, face: { _ in })
            XCTAssertEqual(renderer.root.children.count, 2)
            XCTAssertNil(previous?.parent)
            previous = card
        }
        renderer.update([], card: { _ in nil }, face: { _ in })
        XCTAssertEqual(renderer.root.children.count, 0)
        XCTAssertNil(previous?.parent)
        renderer.update([label], card: { _ in Entity() }, face: { _ in })
        renderer.clear()
        XCTAssertEqual(renderer.root.children.count, 0)
    }

    func testTwinPickingUsesMeshOnTransformedChildAndRejectsMarkers() async throws {
        let root = Entity(), metric = Entity()
        metric.scale = [0.15, 0.15, 0.15]; metric.position = [-1, -2, -3]
        metric.addChild(root); root.scale = [2, 2, 2]
        let floor = ModelEntity(mesh: .generatePlane(width: 4, depth: 4))
        floor.position = [0, 0.7, 0]
        root.addChild(floor)
        let marker = ModelEntity(mesh: .generateSphere(radius: 0.1))
        metric.addChild(marker)
        try await SpatialTwinPicking.prepare(root)
        XCTAssertNil(root.components[CollisionComponent.self])
        XCTAssertNotNil(floor.components[CollisionComponent.self])
        XCTAssertNotNil(floor.components[InputTargetComponent.self])
        XCTAssertTrue(SpatialTwinPicking.contains(floor, in: root))
        XCTAssertFalse(SpatialTwinPicking.contains(marker, in: root))
        let selected = metric.convert(position: [1, 0, 1], from: floor)
        XCTAssertEqual(selected.x, 2, accuracy: 0.0001)
        XCTAssertEqual(selected.y, 1.4, accuracy: 0.0001)
        XCTAssertEqual(selected.z, 2, accuracy: 0.0001)
    }

    func testRealFloorSelectionAdvancesOnlyAtMatchingStepWithFinitePoint() {
        let model = SpatialModel()
        model.tracking.tracked = true
        model.registrationStep = 0
        model.selectSitePoint([1, 0, 2])
        XCTAssertEqual(model.registrationStep, 1)
        model.selectFloor([.nan, 0, 2])
        XCTAssertEqual(model.registrationStep, 1)
        model.selectFloor([3, 0, 4])
        XCTAssertEqual(model.registrationStep, 2)
        XCTAssertEqual(model.roomReferences, [[3, 0, 4]])
        model.selectFloor([5, 0, 6])
        XCTAssertEqual(model.roomReferences.count, 1)
        model.selectSitePoint([3, 0, 2])
        model.selectFloor([5, 0, 4])
        XCTAssertEqual(model.registrationStep, 4)
        XCTAssertEqual(model.roomReferences.count, 2)
    }

    func testFloorMeshFallbackKeepsTargetIdentityAndRejectsWalls() async throws {
        let chunk = SpatialMeshChunk(vertices: [[0, 0, 0], [2, 0, 0], [0, 0, 2], [0, 2, 0]],
                                     indices: [0, 1, 2, 0, 2, 3])
        let floor = try XCTUnwrap(SpatialMeshChunk.floorFacesOnly(chunk, classifiedFloor: [true, true]))
        XCTAssertEqual(floor.indices, [0, 1, 2])
        XCTAssertNil(SpatialMeshChunk.floorFacesOnly(chunk, classifiedFloor: [false, false]))
        let entity = try await SpatialFloorTarget.mesh(floor, updating: nil)
        let updated = try await SpatialFloorTarget.mesh(floor, updating: entity)
        XCTAssertTrue(updated === entity)
        XCTAssertNotNil(entity.components[SpatialFloorTarget.self])
        XCTAssertNotNil(entity.components[InputTargetComponent.self])
        XCTAssertNotNil(entity.components[CollisionComponent.self])
    }

    private func readyModel() -> SpatialModel {
        let model = SpatialModel()
        model.active = true; model.immersive = true
        model.tracking.tracked = true; model.tracking.wearer = [0, 1.7, 0]
        model.handle = 999
        model.alignedFrame = model.tracking.frameID
        model.readingAt = CACurrentMediaTime()
        model.reading = SpatialReading(message: "Ready", warning: "", pose: true, ready: true, aligned: true, moving: false, disconnected: false, position: [4, 0, 0], yaw: 0, route: [], hazards: [])
        model.preview = SummonPreview(point: [1.5, 0, 0], wearer: [0, 1.7, 0], created: CACurrentMediaTime(), frame: model.tracking.frameID, handle: 999)
        return model
    }
    func testMovementRequiresFreshPreviewAndSameRobot() {
        let model = readyModel()
        XCTAssertTrue(model.canMove)
        model.handle = 1000
        XCTAssertFalse(model.canMove)
    }
    func testMovementBlockedAfterTrackingLossBackgroundOrFrameChange() {
        let model = readyModel()
        model.tracking.tracked = false; XCTAssertFalse(model.canMove)
        model.tracking.tracked = true; model.active = false; XCTAssertFalse(model.canMove)
        model.active = true; model.tracking.epoch = UUID(); XCTAssertFalse(model.canMove)
    }
    func testStaleTelemetryAndReplayCannotCommandMotion() {
        let model = readyModel()
        model.readingAt -= 1
        XCTAssertFalse(model.canMove)
        model.readingAt = CACurrentMediaTime()
        model.replay = SpatialRecording(robot: "Go2", samples: [.init(elapsed: 0, reading: model.reading!)])
        XCTAssertFalse(model.canPreview)
        XCTAssertFalse(model.canMove)
    }
    func testRegistrationCannotBeReusedInAnotherModelOrARSession() throws {
        let model = readyModel()
        let url = URL(fileURLWithPath: "/one.usdz")
        model.site = SpatialSite(name: "Room", modelURL: url, metresPerUnit: 1, cameras: [])
        model.registration = try SpatialRegistration.solve(siteA: .zero, siteB: [2, 0, 0], frameA: .zero, frameB: [2, 0, 0], frame: model.tracking.frameID, model: url)
        XCTAssertNotNil(model.registeredWearer)
        model.tracking.tracked = false; XCTAssertNil(model.registeredWearer)
        model.tracking.tracked = true
        model.site = SpatialSite(name: "Other", modelURL: URL(fileURLWithPath: "/two.usdz"), metresPerUnit: 1, cameras: [])
        XCTAssertNil(model.registeredWearer)
    }
    func testStartupHasNoSelectedRobotOrMotionPreview() {
        let model = SpatialModel()
        XCTAssertEqual(model.handle, 0)
        XCTAssertNil(model.preview)
        XCTAssertNil(model.alignedFrame)
        XCTAssertFalse(model.canMove)
    }
    func testAlignmentAcceptsLivePoseBeforeNavigationIsReady() {
        let model = readyModel()
        model.reading?.ready = false
        model.reading?.aligned = false
        model.reading?.message = "LiDAR is arriving, but its capture timestamp is too coarse for navigation."
        XCTAssertNil(model.alignmentBlocker)
        XCTAssertFalse(model.canPreview)
    }
    func testAlignmentExplainsRobotFailureWhileHeadsetIsTracked() async {
        let model = readyModel()
        model.reading?.pose = false
        model.reading?.message = "Go2 must be standing on level ground."
        await model.beginAlignment()
        XCTAssertEqual(model.error, "Robot pose is unavailable. Go2 must be standing on level ground.")
        XCTAssertFalse(model.selectingAlignment)
        model.reading?.disconnected = true
        model.reading?.message = "Go2 observation stream disconnected."
        XCTAssertEqual(model.alignmentBlocker, "Reconnect the robot to resume alignment. Go2 observation stream disconnected.")
    }
    func testAlignmentRejectsStaleTelemetryAndExplainsTrackingLoss() {
        let model = readyModel()
        model.readingAt -= 1
        XCTAssertEqual(model.alignmentBlocker, "Robot telemetry is stale. Reconnect the robot to resume alignment.")
        model.tracking.tracked = false
        model.tracking.message = "World Sensing permission was removed."
        XCTAssertEqual(model.alignmentBlocker, "Headset tracking is unavailable. World Sensing permission was removed.")
    }
    func testSupervisedTimingStillRequiresFreshPoseAndReadyNavigation() {
        let model = readyModel()
        model.reading?.timing = SiteRobotTiming(enabled: true, usingArrival: true, unavailable: true)
        XCTAssertTrue(model.canPreview)
        model.reading?.ready = false
        XCTAssertFalse(model.canPreview)
        model.reading?.ready = true
        model.reading?.pose = false
        XCTAssertFalse(model.canPreview)
        model.reading?.pose = true
        model.readingAt -= 1
        XCTAssertFalse(model.canPreview)
    }
}
