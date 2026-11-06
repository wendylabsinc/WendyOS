import Foundation
import simd

struct SpatialFailure: LocalizedError {
    let message: String
    init(_ message: String) { self.message = message }
    var errorDescription: String? { message }
}

/// All controller coordinates are metres in a fixed, Y-up frame. Tabletop
/// display scaling never participates in motion or headset coordinate conversion.
enum SpatialGeometry {
    static func finite(_ point: SIMD3<Float>) -> Bool {
        (0..<3).allSatisfy { point[$0].isFinite && abs(point[$0]) < 1000 }
    }

    static func heading(center: SIMD3<Float>, front: SIMD3<Float>) throws -> Double {
        guard finite(center), finite(front) else { throw SpatialFailure("Invalid alignment points.") }
        let delta = SIMD2(front.x - center.x, front.z - center.z)
        guard simd_length(delta) >= 0.3, simd_length(delta) <= 2 else {
            throw SpatialFailure("Choose a point 30 cm to 2 m directly in front of the robot.")
        }
        // The Go controller's heading is +X toward -Z, not RealityKit's -Z zero.
        return atan2(Double(-delta.y), Double(delta.x))
    }

    static func summon(robot: SIMD3<Float>, wearer: SIMD3<Float>, floor: Float,
                       clearance: Float = 1.5) throws -> SIMD3<Float> {
        guard finite(robot), finite(wearer), floor.isFinite,
              clearance.isFinite, clearance >= 1, clearance <= 3,
              (0.6...2.5).contains(wearer.y - floor) else {
            throw SpatialFailure("Confirm the floor and a tracked headset position before calling Go2.")
        }
        let delta = SIMD2(robot.x - wearer.x, robot.z - wearer.z)
        let distance = simd_length(delta)
        guard distance > clearance + 0.3 else { throw SpatialFailure("Go2 is already close to you.") }
        guard distance <= 10 else { throw SpatialFailure("Come within 10 metres of Go2 first.") }
        let target = SIMD2(wearer.x, wearer.z) + delta / distance * clearance
        return SIMD3(target.x, floor, target.y)
    }

    static func previewValid(created: TimeInterval, now: TimeInterval,
                             wearer: SIMD3<Float>, originalWearer: SIMD3<Float>) -> Bool {
        finite(wearer) && finite(originalWearer) && now >= created && now - created <= 5
            && simd_distance(wearer, originalWearer) <= 0.3
    }
}

struct SpatialDevice: Identifiable, Sendable, Equatable {
    let id: String
    let name: String
    let selector: String
    let lite: Bool
}
struct SpatialRobot: Identifiable, Sendable, Equatable {
    let id: String
    let name: String
    let connected: Bool
}
struct SpatialReading: Codable, Sendable {
    var message: String
    var warning: String
    var pose: Bool
    var ready: Bool
    var aligned: Bool
    var moving: Bool
    var disconnected: Bool
    var position: SIMD3<Float>
    var yaw: Float
    var route: [SIMD3<Float>]
    var hazards: [SpatialHazard]
    // Optional so recordings made before timing was exposed remain readable.
    var timing: SiteRobotTiming? = nil
    var search: RobotSearchReading? = nil
    var floorPolicy: SiteRobotFloorPolicy? = nil
}
struct SpatialHazard: Codable, Sendable {
    var position: SIMD3<Float>
    var size: Float
    var drop: Bool
}
struct SpatialSample: Codable, Sendable {
    var elapsed: TimeInterval
    var reading: SpatialReading
}
struct SpatialRecording: Codable, Sendable {
    var version = 1
    var robot: String
    var samples: [SpatialSample]
    func validate() throws {
        guard version == 1, robot.count <= 200, !samples.isEmpty, samples.count <= 18000 else {
            throw SpatialFailure("Unsupported or oversized spatial recording.")
        }
        var previous = -Double.infinity
        for sample in samples {
            let reading = sample.reading
            guard sample.elapsed.isFinite, sample.elapsed >= 0, sample.elapsed >= previous,
                  sample.elapsed <= 3600, SpatialGeometry.finite(reading.position), reading.yaw.isFinite,
                  reading.message.count <= 10000, reading.warning.count <= 10000,
                  reading.route.count <= 10000, reading.hazards.count <= 10000,
                  reading.route.allSatisfy(SpatialGeometry.finite),
                  reading.hazards.allSatisfy({ SpatialGeometry.finite($0.position) && $0.size.isFinite && $0.size > 0 && $0.size <= 100 }) else {
                throw SpatialFailure("The recording contains invalid spatial samples.")
            }
            previous = sample.elapsed
        }
    }
}

/// Rigid registration between the session's anchored AR frame and the saved
/// twin. Two floor correspondences resolve translation and yaw. Scale is never
/// fitted: mismatched baselines indicate the wrong points or wrong model units.
struct SpatialRegistration: Sendable {
    let siteFromFrame: simd_float4x4
    let frame: String
    let model: URL

    static func solve(siteA: SIMD3<Float>, siteB: SIMD3<Float>, frameA: SIMD3<Float>, frameB: SIMD3<Float>,
                      frame: String, model: URL) throws -> Self {
        guard [siteA, siteB, frameA, frameB].allSatisfy(SpatialGeometry.finite),
              abs(siteA.y - siteB.y) <= 0.12, abs(frameA.y - frameB.y) <= 0.12 else {
            throw SpatialFailure("Choose two points on the same floor in the twin and in the room.")
        }
        let a = SIMD2(siteB.x - siteA.x, siteB.z - siteA.z)
        let b = SIMD2(frameB.x - frameA.x, frameB.z - frameA.z)
        let siteLength = simd_length(a), frameLength = simd_length(b)
        guard siteLength >= 1, frameLength >= 1 else { throw SpatialFailure("Choose reference points at least one metre apart.") }
        guard abs(siteLength - frameLength) <= max(0.1, siteLength * 0.05) else {
            throw SpatialFailure("The reference distances disagree. Check the selected points and the model's metre scale.")
        }
        let yaw = atan2(-a.y, a.x) - atan2(-b.y, b.x)
        let rotation = simd_quatf(angle: yaw, axis: SIMD3(0, 1, 0))
        var matrix = simd_float4x4(rotation)
        matrix.columns.3 = SIMD4(siteA - rotation.act(frameA), 1)
        return Self(siteFromFrame: matrix, frame: frame, model: model)
    }
    func point(_ point: SIMD3<Float>) -> SIMD3<Float> {
        let p = siteFromFrame * SIMD4(point, 1)
        return SIMD3(p.x, p.y, p.z)
    }
    func direction(_ direction: SIMD3<Float>) -> SIMD3<Float> {
        let p = siteFromFrame * SIMD4(direction, 0)
        return SIMD3(p.x, p.y, p.z)
    }
}
