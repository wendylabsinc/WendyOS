import Foundation
import SwiftUI
import CryptoKit
import simd

struct SiteRobotGeometryPacket: Sendable { var architecture, furniture: [Double] }
struct RobotMapPoint: Codable, Sendable { var X, Y: Double }
struct RobotMapVertex: Codable, Sendable { var X, Y, Z: Double }
struct RobotObservedSurface: Codable, Sendable {
    var position: RobotMapVertex
    var direction: RobotMapPoint
    var horizontal: Bool
    var stable: Bool
}
struct RobotVisualHint: Encodable, Sendable {
    var position: RobotMapPoint
    var yaw, at: Double
    var inliers: Int
    var residual: Double
}
struct RobotFusionRequest: Encodable, Sendable {
    var frame: String
    var architecture, furniture: [Double]
    var visual: RobotVisualHint?
    var reference: [RobotObservedSurface]
}
struct RobotScanPoint: Decodable, Equatable, Sendable {
    var X, Y, Z: Double
    var Class: Int
}
struct RobotFusionReport: Decodable, Sendable {
    struct Candidate: Decodable, Sendable { var X, Z, Yaw, FloorY, RMS, Coverage: Double }
    struct Tracking: Decodable, Sendable {
        struct Fit: Decodable, Sendable { var architecture, furniture, reference, directions: Int; var cost: Double; var valid: Bool }
        var fits: [Fit]?
        var independentViews: Int
        var novelFraction: Double
        var selected: Int
        var message: String
    }
    var message: String
    var candidates: [Candidate]?
    var tracking: Tracking
    var overlay: [RobotScanPoint]?
    var reference: [RobotObservedSurface]?
    var Accepted, Rejected: Int
    var visualUsed: Bool
    var observationRevision: UInt64
}

struct RobotSuggestionFreshness {
    var observationRevision: UInt64
    var received: Date

    func shouldRefresh(observationRevision current: UInt64, now: Date) -> Bool {
        let age = now.timeIntervalSince(received)
        return age >= 30 || (age >= 5 && current != observationRevision)
    }
}

// Each robot's references are bound to the exact imported model and revision.
// These saved surfaces are localization targets, never navigation free space.
struct RobotSavedMap: Codable, Sendable {
    var version = 1
    var frame, modelDigest, robot: String
    var savedAt = Date()
    var surfaces: [RobotObservedSurface] = []
    var lens: SiteCameraLens?
}
enum RobotReferenceStore {
    static func url(model: URL, robot: String) -> URL {
        let digest = SHA256.hash(data: Data(robot.utf8)).prefix(8).map { String(format: "%02x", $0) }.joined()
        return model.deletingLastPathComponent().appendingPathComponent("robot-\(digest)-references.json")
    }
    static func read(model: URL, robot: String, frame: String) throws -> RobotSavedMap {
        let digest = SiteVisualMap.digest(try Data(contentsOf: model))
        let target = url(model: model, robot: robot)
        if FileManager.default.fileExists(atPath: target.path) {
            let info = try target.resourceValues(forKeys: [.fileSizeKey, .isSymbolicLinkKey])
            guard info.isSymbolicLink != true, (info.fileSize ?? Int.max) <= 3_000_000 else { throw SiteLocalizationError.invalidMap }
            let stored = try JSONDecoder().decode(RobotSavedMap.self, from: Data(contentsOf: target))
            if stored.version == 1, stored.frame == frame, stored.modelDigest == digest, stored.robot == robot,
               (0...7*24*3600).contains(Date().timeIntervalSince(stored.savedAt)), stored.surfaces.count <= 3000 {
                guard stored.surfaces.allSatisfy({
                    [$0.position.X,$0.position.Y,$0.position.Z,$0.direction.X,$0.direction.Y].allSatisfy { $0.isFinite && abs($0) < 1000 }
                }), stored.lens == nil || stored.lens?.isValid == true else { throw SiteLocalizationError.invalidMap }
                return stored
            }
        }
        return RobotSavedMap(frame: frame, modelDigest: digest, robot: robot)
    }
    static func save(_ value: RobotSavedMap, model: URL) throws {
        guard value.modelDigest == SiteVisualMap.digest(try Data(contentsOf: model)), value.surfaces.count <= 3000 else { throw SiteLocalizationError.invalidMap }
        try JSONEncoder().encode(value).write(to: url(model: model, robot: value.robot), options: .atomic)
    }
    static func merging(_ surfaces: [RobotObservedSurface], into map: RobotSavedMap) -> RobotSavedMap {
        var result = map
        var cells: [String:RobotObservedSurface] = [:]
        for p in map.surfaces + surfaces {
            let key = "\(Int((p.position.X*10).rounded()))/\(Int((p.position.Y*10).rounded()))/\(Int((p.position.Z*10).rounded()))"
            // A weaker re-observation must not erase a confirmed wall anchor.
            if let old = cells[key], old.stable, !p.stable { continue }
            cells[key] = p
        }
        let all = cells.sorted { $0.key < $1.key }.map(\.value)
        result.surfaces = all.count <= 3000 ? all : (0..<3000).map { all[$0*all.count/3000] }
        result.savedAt = Date()
        return result
    }
}

@MainActor
final class RobotVisualLocalization {
    private var engine = SiteLocalizationEngine()
    private var loadedFrame = ""
    private var loadedDigest = ""
    private var revision: UUID?

    func load(model: URL, frame: String, revision: UUID) async throws {
        let digest = try await BlockingWork.run(on: .global(qos: .userInitiated)) {
            SiteVisualMap.digest(try Data(contentsOf: model))
        }
        guard loadedFrame != frame || loadedDigest != digest else { return }
        let visualURL = model.deletingLastPathComponent().appendingPathComponent("visual-map.json")
        let (map, bytes) = try await BlockingWork.run(on: .global(qos: .userInitiated)) {
            let bytes = try Data(contentsOf: model)
            let original = try SiteVisualMap.read(visualURL, revision: revision, model: bytes)
            let exclusions = try SiteRobotGeometry.referenceExclusions(model, scale: 1)
            return (Self.removingMovableReferences(from: original, exclusions: exclusions), bytes)
        }
        try await engine.load(map, revision: revision, model: bytes)
        self.revision = revision; loadedFrame = frame; loadedDigest = digest
    }

    nonisolated static func removingChairReferences(from map: SiteVisualMap, chairs: [SiteRobotGeometry.ChairFootprint]) -> SiteVisualMap {
        removingMovableReferences(from: map, exclusions: .init(chairs: chairs, doors: []))
    }

    nonisolated static func removingMovableReferences(from map: SiteVisualMap, exclusions: SiteRobotGeometry.ReferenceExclusions) -> SiteVisualMap {
        var result = map
        var retained = Data()
        let stride = 131 * MemoryLayout<Float>.size
        map.landmarks.withUnsafeBytes { raw in
            for index in 0..<map.count {
                let offset = index * stride
                let x = Double(raw.loadUnaligned(fromByteOffset: offset, as: Float.self))
                let y = Double(raw.loadUnaligned(fromByteOffset: offset + 4, as: Float.self))
                let z = Double(raw.loadUnaligned(fromByteOffset: offset + 8, as: Float.self))
                if !exclusions.contains(x: x, y: y, z: z) {
                    retained.append(contentsOf: raw[offset..<(offset + stride)])
                }
            }
        }
        result.landmarks = retained
        return result
    }

    func locate(first: CameraCalibrationFrame, lens: SiteCameraLens?, next: @MainActor () async throws -> CameraCalibrationFrame) async throws -> (RobotVisualHint, SiteCameraLens) {
        guard let revision, first.isFresh(at: Date()) else { throw SiteLocalizationError.invalidImage }
        var solution: SiteLocalizationResult
        do { solution = try await engine.localize(gray: first.gray, width: first.width, height: first.height, revision: revision, lens: lens) }
        catch let error as SiteLocalizationError {
            guard case .noSolution = error, lens?.measured != true else { throw error }
            solution = try await engine.localize(gray: first.gray, width: first.width, height: first.height, revision: revision, lens: lens, estimated: true)
        }
        let second = try await next()
        guard second.received > first.received, second.sequence != first.sequence,
              first.width == second.width, first.height == second.height, first.codec == second.codec,
              second.isFresh(at: Date()) else { throw SiteLocalizationError.invalidImage }
        let verification = try await engine.localize(gray: second.gray, width: second.width, height: second.height, revision: revision, lens: solution.lens, estimated: !solution.lens.measured)
        let a = solution.pose.position, b = verification.pose.position
        let distance = simd_distance(SIMD3(a.x,a.y,a.z), SIMD3(b.x,b.y,b.z))
        let angle = 2*acos(min(1,abs(simd_dot(solution.pose.rotation,verification.pose.rotation))))
        guard distance <= (solution.lens.measured ? 0.08 : 0.25), angle <= (solution.lens.measured ? 3 : 8)*Double.pi/180 else {
            throw WendyError(message: "Camera position did not agree between fresh frames.")
        }
        let forward = simd_quatd(vector: verification.pose.rotation).act(SIMD3<Double>(0,0,1))
        guard hypot(forward.x,forward.z) > 0.7 else { throw SiteLocalizationError.invalidResult }
        // The front camera is a soft position/heading cue. The backend retains
        // uncertainty for its offset from the body; this is not a body transform.
        let hint = RobotVisualHint(position: RobotMapPoint(X:b.x,Y:-b.z), yaw:atan2(-forward.z,forward.x),
            at:second.received.timeIntervalSince1970, inliers:min(solution.inliers,verification.inliers), residual:max(solution.residual,verification.residual))
        solution.lens.captureMode = "\(second.codec) \(second.width)x\(second.height)"
        return (hint, solution.lens)
    }
}

struct RobotLocalizationDiagnostics: View {
    @ObservedObject var model: SiteRobotMapModel
    var body: some View {
        DisclosureGroup("Localization evidence") {
            VStack(alignment: .leading, spacing: 6) {
                if let report = model.fusionReport {
                    Text("\(report.Accepted) surface samples · \(report.tracking.independentViews) distinct viewpoints")
                    Text(report.message)
                    if let fits = report.tracking.fits, !fits.isEmpty {
                        let best = fits[max(0, report.tracking.selected)]
                        Text("Best fit: \(best.architecture) wall · \(best.furniture) furniture · \(best.reference) saved matches")
                        Text("\(best.directions) supported surface directions · Camera constraint: \(report.visualUsed ? "verified" : "unavailable")")
                    }
                    Toggle("Show provisional scan fit", isOn: $model.showScan)
                    Text("Green: walls · Orange: furniture · Blue: saved scan · Gray: rejected or unmatched")
                    if report.tracking.selected < 0 { Text("This overlay is a hypothesis. Go2's position is not confirmed.").foregroundStyle(.orange) }
                }
                Text(model.visualStatus)
                Text("Saved robot map: \(model.referenceCount) surfaces")
                if let lens = model.measuredLens { Text("Measured lens · \(lens.captureMode)") }
                Text("For better visual references, include the room from Go2's camera height during the iPhone scan. Keep fixed desks and cabinets visible across several views.")
            }.font(.caption2).foregroundStyle(.secondary).padding(.top, 4)
        }.font(.caption)
    }
}

// Only an explicit Locate action arms movement. Passive observations never do.
struct RobotPositionSearch {
    private(set) var active = false
    private(set) var remainingTurns = 0
    private(set) var direction: Int32 = 0
    private var deadline = Date.distantPast
    mutating func start(now: Date) { active = true; remainingTurns = 12; direction = 0; deadline = now.addingTimeInterval(300) }
    mutating func resumeAfterCalibration(now: Date) { if active { deadline = now.addingTimeInterval(300) } }
    mutating func shouldRetryLocalization(pose: Bool, scanReady: Bool, aligned: Bool,
                                         rematchAfterLook: Bool, attempts: Int, now: Date,
                                         calibrating: Bool = false) -> Bool {
        guard !calibrating else { return false }
        if active && now >= deadline { cancel() }
        // An explicit search must be able to acquire another view when room
        // landmarks are sparse. The backend still checks sensor freshness and
        // the entire turning footprint before allowing any motion.
        if active { return pose }
        return scanReady && (!aligned || rematchAfterLook) && attempts < 3
    }
    mutating func takeTurn(now: Date) -> Bool {
        guard active, remainingTurns > 0, now < deadline else { cancel(); return false }
        remainingTurns -= 1
        return true
    }
    mutating func recordDirection(_ value: Int32) { if active && direction == 0 && (value == 1 || value == -1) { direction = value } }
    mutating func cancel() { active = false; remainingTurns = 0; direction = 0 }
}
