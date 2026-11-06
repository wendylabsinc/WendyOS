import XCTest
import QuartzCore
import simd
@testable import WendySpatial

private extension SpatialRoomPlane {
    init(floor: Bool, width: Float, depth: Float, worldFromPlane: simd_float4x4) {
        let points = [SIMD4<Float>(-width/2, 0, -depth/2, 1), SIMD4<Float>(width/2, 0, -depth/2, 1),
                      SIMD4<Float>(width/2, 0, depth/2, 1), SIMD4<Float>(-width/2, 0, depth/2, 1)].map { p in
            let world = worldFromPlane * p
            return SIMD3(world.x, world.y, world.z)
        }
        self.init(floor: floor, vertices: points, indices: [0, 2, 1, 0, 3, 2])
    }
}

private actor SearchClientStub: RobotSearching {
    var turns = 0
    var matches = 0
    var confirmations = 0
    let found: Bool
    init(found: Bool = false) { self.found = found }
    func look(_ handle: UInt64, sequence: UInt64, direction: Int32) async throws { turns += 1 }
    func fuse(_ handle: UInt64, sequence: UInt64, request: RobotFusionRequest) async throws -> RobotFusionReport {
        matches += 1
        return RobotFusionReport(message: "No match", candidates: found ? [.init(X: 2, Z: 3, Yaw: 0, FloorY: 0, RMS: 0.02, Coverage: 0.9)] : [],
            tracking: .init(fits: nil, independentViews: 1, novelFraction: 1, selected: found ? 0 : -1, message: "No match"),
            overlay: nil, reference: nil, Accepted: 100, Rejected: 0, waitingForObservations: false, visualUsed: false, observationRevision: 1)
    }
    func confirm(_ handle: UInt64, sequence: UInt64, matchSequence: UInt64, frame: String, candidate: Int) async throws { confirmations += 1 }
}

@MainActor
final class SpatialRobotSearchTests: XCTestCase {
    private let geometry = SiteRobotGeometryPacket(architecture: [], furniture: [])
    private func model(_ client: SearchClientStub) -> SpatialModel {
        let model = SpatialModel(robotSearch: SpatialRobotSearch(client: client))
        model.active = true; model.immersive = true; model.handle = 999
        model.tracking.tracked = true; model.readingAt = CACurrentMediaTime()
        model.reading = SpatialReading(message: "Live", warning: "", pose: true, ready: false, aligned: false,
            moving: false, disconnected: false, position: .zero, yaw: 0, route: [], hazards: [],
            search: RobotSearchReading(scanReady: true, measurementsReady: true))
        return model
    }
    func testFailedPassiveMatchOffersRotationWithoutMoving() async {
        let client = SearchClientStub(), model = model(client)
        await model.robotSearch.step(model, geometry: geometry)
        XCTAssertTrue(model.robotSearch.showPrompt)
        let matches = await client.matches, turns = await client.turns
        XCTAssertEqual(matches, 1); XCTAssertEqual(turns, 0)
        model.robotSearch.start(model, geometry: geometry)
        await model.robotSearch.step(model, geometry: geometry)
        let startedTurns = await client.turns
        XCTAssertEqual(startedTurns, 1)
        XCTAssertEqual(model.robotSearch.state, .turning(1))
    }
    func testSparseScanOffersAfterDelayAndConsentWaitsForMeasurements() async {
        let client = SearchClientStub(), model = model(client), now = Date()
        model.reading?.search = RobotSearchReading(scanReady: false, measurementsReady: false)
        await model.robotSearch.step(model, geometry: geometry, now: now)
        XCTAssertFalse(model.robotSearch.showPrompt)
        await model.robotSearch.step(model, geometry: geometry, now: now.addingTimeInterval(6))
        XCTAssertTrue(model.robotSearch.showPrompt)
        model.robotSearch.start(model, geometry: geometry, now: now.addingTimeInterval(6))
        await model.robotSearch.step(model, geometry: geometry, now: now.addingTimeInterval(7))
        let turns = await client.turns
        XCTAssertEqual(turns, 0)
        model.reading?.search?.measurementsReady = true
        await model.robotSearch.step(model, geometry: geometry, now: now.addingTimeInterval(8))
        let resumed = await client.turns
        XCTAssertEqual(resumed, 1)
    }
    func testDismissAndMissingRoomNeverArmRotation() async {
        let client = SearchClientStub(), model = model(client)
        await model.robotSearch.step(model, geometry: geometry)
        model.robotSearch.dismiss()
        await model.robotSearch.step(model, geometry: geometry, now: .now.addingTimeInterval(10))
        XCTAssertFalse(model.robotSearch.showPrompt)
        model.robotSearch.start(model)
        let turns = await client.turns
        XCTAssertEqual(turns, 0)
        XCTAssertFalse(model.robotSearch.canStart(model))
    }
    func testSuggestionRequiresConfirmationAndCannotSurviveFrameChange() async {
        let client = SearchClientStub(found: true), model = model(client)
        await model.robotSearch.step(model, geometry: geometry)
        XCTAssertNotNil(model.robotSearch.candidate)
        XCTAssertNil(model.alignedFrame)
        model.tracking.epoch = UUID()
        await model.robotSearch.confirm(model)
        let confirmations = await client.confirmations
        XCTAssertEqual(confirmations, 0)
        XCTAssertNil(model.alignedFrame)
    }
    func testConfirmationAlignsOnlyCurrentFrame() async {
        let client = SearchClientStub(found: true), model = model(client)
        await model.robotSearch.step(model, geometry: geometry)
        await model.robotSearch.confirm(model)
        let confirmations = await client.confirmations
        XCTAssertEqual(confirmations, 1)
        XCTAssertEqual(model.alignedFrame, model.tracking.frameID)
        XCTAssertNil(model.robotSearch.candidate)
    }
    func testOldPollCannotFinishTurnAndFreshCompletionTriggersMatching() async {
        let client = SearchClientStub(), model = model(client), now = Date()
        model.robotSearch.start(model, geometry: geometry, now: now)
        await model.robotSearch.step(model, geometry: geometry, now: now)
        await model.robotSearch.step(model, geometry: geometry, now: now.addingTimeInterval(1))
        XCTAssertEqual(model.robotSearch.state, .turning(1))
        model.reading?.search?.lookState = 2
        model.reading?.search?.lookDirection = 1
        model.readingAt = CACurrentMediaTime()
        await model.robotSearch.step(model, geometry: geometry, now: now.addingTimeInterval(2))
        XCTAssertEqual(model.robotSearch.state, .matching(1))
        await model.robotSearch.step(model, geometry: geometry, now: now.addingTimeInterval(4))
        let matches = await client.matches, turns = await client.turns
        XCTAssertEqual(matches, 1); XCTAssertEqual(turns, 1)
    }
    func testStalePoseAndLostTrackingCannotStartRotation() async {
        let client = SearchClientStub(), model = model(client)
        model.readingAt -= 1
        model.robotSearch.start(model, geometry: geometry)
        await model.robotSearch.step(model, geometry: geometry)
        model.readingAt = CACurrentMediaTime(); model.tracking.tracked = false
        model.robotSearch.start(model, geometry: geometry)
        await model.robotSearch.step(model, geometry: geometry)
        let turns = await client.turns
        XCTAssertEqual(turns, 0)
    }
    func testFloorAndWallOverhang() throws {
        let floor = SpatialRoomPlane(floor: true, width: 4, depth: 4, worldFromPlane: matrix_identity_float4x4)
        var wallTransform = simd_float4x4(simd_quatf(angle: .pi / 2, axis: [1, 0, 0]))
        // The rectangle extends to -0.5 m even though the measured floor is y=0.
        wallTransform.columns.3 = [0, 1, -2, 1]
        let wall = SpatialRoomPlane(floor: false, width: 4, depth: 3, worldFromPlane: wallTransform)
        var frame = matrix_identity_float4x4
        frame.columns.3 = [10, -2, 3, 1]
        let packet = try XCTUnwrap(SpatialRoomGeometry.geometry([floor, wall, wall], frameFromWorld: frame))
        let heights = stride(from: 1, to: packet.architecture.count, by: 3).map { packet.architecture[$0] }
        XCTAssertEqual(try XCTUnwrap(heights.min()), -2.5, accuracy: 0.00001)
        XCTAssertEqual(try XCTUnwrap(heights.max()), 0.5, accuracy: 0.00001)
        // The matcher handles wall overhangs without modifying the measured floor.
        let expectedFloor: [Double] = [8, -2, 1, 12, -2, 5, 12, -2, 1,
                                       8, -2, 1, 8, -2, 5, 12, -2, 5]
        XCTAssertEqual(Array(packet.architecture.prefix(18)), expectedFloor)
        XCTAssertEqual(packet.architecture.count, 54)
    }

    func testMissingFloor() {
        let floor = SpatialRoomPlane(floor: true, width: 0.5, depth: 0.5, worldFromPlane: matrix_identity_float4x4)
        var wallTransform = simd_float4x4(simd_quatf(angle: .pi / 2, axis: [1, 0, 0]))
        wallTransform.columns.3 = [0, 1, -2, 1]
        let wall = SpatialRoomPlane(floor: false, width: 4, depth: 3, worldFromPlane: wallTransform)
        XCTAssertNil(SpatialRoomGeometry.geometry([floor, wall, wall], frameFromWorld: matrix_identity_float4x4))
        XCTAssertNil(SpatialRoomGeometry.geometry([wall, wall], frameFromWorld: matrix_identity_float4x4))
    }

    func testTwinGeometryConvertedIntoRoomFrame() throws {
        let site = SiteRobotGeometryPacket(architecture: [10, 0, 20, 12, 0, 20, 10, 0, 22], furniture: [10, 1, 20, 11, 1, 20, 10, 1, 21])
        let registration = try SpatialRegistration.solve(siteA: [10, 0, 20], siteB: [12, 0, 20],
            frameA: [1, 0, 2], frameB: [1, 0, 0], frame: "room", model: URL(fileURLWithPath: "/room.usdz"))
        let frame = try XCTUnwrap(SpatialRoomGeometry.transformed(site, by: simd_inverse(registration.siteFromFrame)))
        let restored = try XCTUnwrap(SpatialRoomGeometry.transformed(frame, by: registration.siteFromFrame))
        for (a, b) in zip(restored.architecture, site.architecture) { XCTAssertEqual(a, b, accuracy: 0.0001) }
        for (a, b) in zip(restored.furniture, site.furniture) { XCTAssertEqual(a, b, accuracy: 0.0001) }
        XCTAssertEqual(frame.architecture[0], 1, accuracy: 0.0001)
        XCTAssertEqual(frame.architecture[2], 2, accuracy: 0.0001)
        XCTAssertNil(SpatialRoomGeometry.transformed(.init(architecture: [0, 1], furniture: []), by: matrix_identity_float4x4))
    }

    func testXYPlaneMeshOrientation() throws {
        let local: [SIMD4<Float>] = [[-2, -2, 0, 1], [2, -2, 0, 1], [2, 2, 0, 1], [-2, 2, 0, 1]]
        var worldFromAnchor = simd_float4x4(simd_quatf(angle: -.pi/2, axis: [1, 0, 0]))
        worldFromAnchor.columns.3 = [3, 0.1, 4, 1]
        let world = local.map { p in let v = worldFromAnchor * p; return SIMD3(v.x, v.y, v.z) }
        let floor = SpatialRoomPlane(floor: true, vertices: world, indices: [0, 1, 2, 0, 2, 3])
        var wallTransform = simd_float4x4(simd_quatf(angle: .pi/2, axis: [1, 0, 0]))
        wallTransform.columns.3 = [0, 1.5, -2, 1]
        let wall = SpatialRoomPlane(floor: false, width: 4, depth: 3, worldFromPlane: wallTransform)
        let result = try XCTUnwrap(SpatialRoomGeometry.geometry([floor, wall, wall], frameFromWorld: matrix_identity_float4x4))
        let heights = stride(from: 1, to: 18, by: 3).map { result.architecture[$0] }
        for height in heights { XCTAssertEqual(height, 0.1, accuracy: 0.00001) }
        let normal = simd_normalize(simd_cross(world[1] - world[0], world[2] - world[0]))
        XCTAssertEqual(abs(normal.y), 1, accuracy: 0.00001)
        let wrong = SpatialRoomPlane(floor: true, width: 4, depth: 4, worldFromPlane: worldFromAnchor)
        XCTAssertNil(SpatialRoomGeometry.geometry([wrong, wall, wall], frameFromWorld: matrix_identity_float4x4))
    }

    func testDetectedPlaneGeometryUsesAnchoredCoordinatesAndRequiresRoomStructure() throws {
        let floor = SpatialRoomPlane(floor: true, width: 4, depth: 4, worldFromPlane: matrix_identity_float4x4)
        var wallTransform = simd_float4x4(simd_quatf(angle: .pi/2, axis: [1, 0, 0]))
        wallTransform.columns.3 = [0, 1.5, -2, 1]
        let wall = SpatialRoomPlane(floor: false, width: 4, depth: 3, worldFromPlane: wallTransform)
        XCTAssertNil(SpatialRoomGeometry.geometry([floor, wall], frameFromWorld: matrix_identity_float4x4))
        var frame = matrix_identity_float4x4
        frame.columns.3 = [-10, 0, 0, 1]
        let result = try XCTUnwrap(SpatialRoomGeometry.geometry([floor, wall, wall], frameFromWorld: frame))
        XCTAssertEqual(result.architecture.count, 54)
        XCTAssertEqual(result.architecture[0], -12, accuracy: 0.001)
        XCTAssertTrue(result.furniture.isEmpty)
        XCTAssertNil(SpatialRoomGeometry.geometry([wall, wall], frameFromWorld: frame))
    }
}
