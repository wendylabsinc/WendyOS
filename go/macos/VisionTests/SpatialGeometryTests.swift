import XCTest
import simd
@testable import WendySpatial

final class SpatialGeometryTests: XCTestCase {
    func testSummonStopsShortOnRobotSideAndProjectsToFloor() throws {
        let target = try SpatialGeometry.summon(robot: [4, 0, 0], wearer: [0, 1.7, 0], floor: 0)
        XCTAssertEqual(target.x, 1.5, accuracy: 0.0001)
        XCTAssertEqual(target.y, 0)
        XCTAssertEqual(target.z, 0)
        let diagonal = try SpatialGeometry.summon(robot: [4, 0, 4], wearer: [0, 1.7, 0], floor: 0)
        XCTAssertEqual(simd_length(SIMD2(diagonal.x, diagonal.z)), 1.5, accuracy: 0.0001)
        XCTAssertEqual(diagonal.x, diagonal.z)
    }
    func testSummonRejectsNearbyFarInvalidAndWrongFloor() {
        for robot: SIMD3<Float> in [[1, 0, 0], [11, 0, 0], [.nan, 0, 0], [.infinity, 0, 0]] {
            XCTAssertThrowsError(try SpatialGeometry.summon(robot: robot, wearer: [0, 1.7, 0], floor: 0))
        }
        XCTAssertThrowsError(try SpatialGeometry.summon(robot: [4, 0, 0], wearer: [0, 1.7, 0], floor: 1.5))
        XCTAssertThrowsError(try SpatialGeometry.summon(robot: [4, 0, 0], wearer: [0, 1.7, 0], floor: 0, clearance: 0))
    }
    func testHeadingMatchesControllerCoordinates() throws {
        XCTAssertEqual(try SpatialGeometry.heading(center: .zero, front: [1, 0, 0]), 0)
        XCTAssertEqual(try SpatialGeometry.heading(center: .zero, front: [0, 0, -1]), .pi / 2, accuracy: 0.0001)
        XCTAssertEqual(try SpatialGeometry.heading(center: .zero, front: [0, 0, 1]), -.pi / 2, accuracy: 0.0001)
        XCTAssertThrowsError(try SpatialGeometry.heading(center: .zero, front: [0.1, 0, 0]))
    }
    func testPreviewExpiresAndWearerMovementInvalidatesIt() {
        XCTAssertTrue(SpatialGeometry.previewValid(created: 10, now: 11, wearer: [0, 1.7, 0], originalWearer: [0, 1.7, 0]))
        XCTAssertFalse(SpatialGeometry.previewValid(created: 10, now: 16, wearer: [0, 1.7, 0], originalWearer: [0, 1.7, 0]))
        XCTAssertFalse(SpatialGeometry.previewValid(created: 10, now: 9, wearer: [0, 1.7, 0], originalWearer: [0, 1.7, 0]))
        XCTAssertFalse(SpatialGeometry.previewValid(created: 10, now: 11, wearer: [0.31, 1.7, 0], originalWearer: [0, 1.7, 0]))
    }
    func testRegistrationResolvesTranslationYawAndHeadHeight() throws {
        let registration = try SpatialRegistration.solve(siteA: [5, 0.2, 6], siteB: [5, 0.2, 4],
            frameA: [1, 0, 1], frameB: [3, 0, 1], frame: "session", model: URL(fileURLWithPath: "/site.usdz"))
        let mapped = registration.point([3, 1.7, 1])
        XCTAssertEqual(mapped.x, 5, accuracy: 0.0001)
        XCTAssertEqual(mapped.y, 1.9, accuracy: 0.0001)
        XCTAssertEqual(mapped.z, 4, accuracy: 0.0001)
        let facing = registration.direction([1, 0, 0])
        XCTAssertEqual(facing.x, 0, accuracy: 0.0001)
        XCTAssertEqual(facing.z, -1, accuracy: 0.0001)
    }
    func testRegistrationRejectsScaleMismatchShortBaselineAndDifferentFloors() {
        let model = URL(fileURLWithPath: "/site.usdz")
        XCTAssertThrowsError(try SpatialRegistration.solve(siteA: .zero, siteB: [3, 0, 0], frameA: .zero, frameB: [2, 0, 0], frame: "s", model: model))
        XCTAssertThrowsError(try SpatialRegistration.solve(siteA: .zero, siteB: [0.5, 0, 0], frameA: .zero, frameB: [0.5, 0, 0], frame: "s", model: model))
        XCTAssertThrowsError(try SpatialRegistration.solve(siteA: .zero, siteB: [2, 0.3, 0], frameA: .zero, frameB: [2, 0, 0], frame: "s", model: model))
    }
    func testRigidRegistrationRoundTripAcrossHeadPositions() throws {
        let registration = try SpatialRegistration.solve(siteA: [2, 1, 3], siteB: [4, 1, 3], frameA: [-1, 0, 1], frameB: [-1, 0, -1], frame: "s", model: URL(fileURLWithPath: "/site.usdz"))
        for point: SIMD3<Float> in [[0, 1.7, 0], [4, 1.5, -2], [-2, 0, 8]] {
            let mapped = registration.point(point)
            let back = simd_inverse(registration.siteFromFrame) * SIMD4(mapped, 1)
            XCTAssertLessThan(simd_distance(SIMD3(back.x, back.y, back.z), point), 0.0001)
        }
    }
    func testRecordingRejectsNonmonotonicSamplesAndInvalidGeometry() throws {
        let reading = SpatialReading(message: "Waiting", warning: "", pose: true, ready: false, aligned: true, moving: false, disconnected: false, position: .zero, yaw: 0, route: [], hazards: [])
        var record = SpatialRecording(robot: "Go2", samples: [.init(elapsed: 0, reading: reading), .init(elapsed: 1, reading: reading)])
        XCTAssertNoThrow(try record.validate())
        record.samples[1].elapsed = -1
        XCTAssertThrowsError(try record.validate())
        record.samples[1].elapsed = 1
        record.samples[1].reading.position.x = .nan
        XCTAssertThrowsError(try record.validate())
    }
    func testRecordingPreservesTimingAndReadsOlderSamples() throws {
        var reading = SpatialReading(message: "Live", warning: "", pose: true, ready: false, aligned: true,
            moving: false, disconnected: false, position: .zero, yaw: 0, route: [], hazards: [])
        let legacy = try JSONEncoder().encode(reading)
        XCTAssertNil(try JSONDecoder().decode(SpatialReading.self, from: legacy).timing)
        reading.timing = SiteRobotTiming(enabled: true, usingArrival: true, unavailable: true)
        let current = try JSONEncoder().encode(reading)
        XCTAssertEqual(try JSONDecoder().decode(SpatialReading.self, from: current).timing, reading.timing)
    }
}
