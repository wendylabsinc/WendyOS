import Foundation
import simd

public struct SiteCameraLens: Codable, Sendable, Equatable {
    var width: Int, height: Int
    var fx: Double, fy: Double, cx: Double, cy: Double
    var distortionModel: String
    var distortion: [Double]
    var measured: Bool
    var note: String
    var captureMode: String

    var isValid: Bool {
        (1...8192).contains(width) && (1...8192).contains(height)
        && [fx, fy, cx, cy].allSatisfy(\.isFinite) && fx > 0 && fy > 0
        && fx <= 1e7 && fy <= 1e7 && (0...Double(width)).contains(cx) && (0...Double(height)).contains(cy)
        && !captureMode.isEmpty && !note.isEmpty
        && (distortionModel == "none" && distortion.isEmpty
            || distortionModel == "brown_conrady" && distortion.count == 5 && distortion.allSatisfy { $0.isFinite && abs($0) <= 100 })
    }

    // Returns optical right/down/forward rays. Invert Brown-Conrady only when
    // it converges and verifies by redistorting; never substitute a default FOV.
    func ray(x: Double, y: Double) -> SIMD3<Double>? {
        guard isValid else { return nil }
        let xd = (x - cx) / fx, yd = (y - cy) / fy
        var u = xd, v = yd
        if distortionModel == "brown_conrady" {
            let k1 = distortion[0], k2 = distortion[1], p1 = distortion[2], p2 = distortion[3], k3 = distortion[4]
            for _ in 0..<40 {
                let r2 = u*u + v*v
                let radial = 1 + r2 * (k1 + r2 * (k2 + r2 * k3))
                guard radial.isFinite, radial > 0.01 else { return nil }
                let du = 2*p1*u*v + p2*(r2 + 2*u*u)
                let dv = p1*(r2 + 2*v*v) + 2*p2*u*v
                u = (xd-du)/radial; v = (yd-dv)/radial
            }
            let r2 = u*u + v*v
            let radial = 1 + r2 * (k1 + r2 * (k2 + r2 * k3))
            let checkX = (u*radial + 2*p1*u*v + p2*(r2 + 2*u*u) - xd)*fx
            let checkY = (v*radial + p1*(r2 + 2*v*v) + 2*p2*u*v - yd)*fy
            guard checkX.isFinite, checkY.isFinite, hypot(checkX, checkY) < 0.1 else { return nil }
        }
        guard u.isFinite, v.isFinite else { return nil }
        return simd_normalize(SIMD3(u, v, 1))
    }

    var horizontalFOV: Double? { angle(ray(x: 0, y: cy), ray(x: Double(width), y: cy)) }
    var verticalFOV: Double? { angle(ray(x: cx, y: 0), ray(x: cx, y: Double(height))) }
    private func angle(_ a: SIMD3<Double>?, _ b: SIMD3<Double>?) -> Double? {
        guard let a, let b else { return nil }
        return acos(max(-1, min(1, simd_dot(a,b)))) * 180 / .pi
    }

    var boundaryRays: [SIMD3<Double>]? {
        var result: [SIMD3<Double>] = []
        let w = Double(width), h = Double(height)
        for edge in 0..<4 {
            for step in 0..<8 {
                let t = Double(step)/8
                let p: (Double, Double)
                switch edge {
                case 0: p = (t*w, 0)
                case 1: p = (w, t*h)
                case 2: p = ((1-t)*w, h)
                default: p = (0, (1-t)*h)
                }
                guard let ray = ray(x: p.0, y: p.1) else { return nil }
                result.append(ray)
            }
        }
        return result
    }
}

public struct SiteCameraPose: Sendable, Equatable {
    var modelRevision: UUID?
    var position: SitePoint
    var rotation: SIMD4<Double>
    var method: Int
    var provenance: String
    var observedAt: Date
    var validUntil: Date

    func isCurrent(revision: UUID, now: Date) -> Bool {
        modelRevision == revision && position.isValid && (1...2).contains(method)
        && !provenance.isEmpty && (0..<4).allSatisfy { rotation[$0].isFinite }
        && abs(simd_length_squared(rotation)-1) < 0.001
        && observedAt <= now.addingTimeInterval(30) && validUntil > now
        && validUntil > observedAt && validUntil.timeIntervalSince(observedAt) <= 7*24*60*60
    }
    var quaternion: simd_quatd { simd_quatd(vector: rotation) }
    var forward: SIMD3<Double> { quaternion.act(SIMD3(0, 0, 1)) }
    var yaw: Double { atan2(forward.x, -forward.z)*180 / .pi }
    var pitch: Double { asin(max(-1, min(1, forward.y)))*180 / .pi }
}

public struct SiteCameraMetadata: Sendable, Equatable {
    var sourceID: String
    var revision: UInt64
    var lens: SiteCameraLens?
    var poses: [SiteCameraPose]

    func pose(in site: Site, now: Date) -> SiteCameraPose? {
        let matching = poses.filter { $0.isCurrent(revision: site.modelRevision, now: now) }
        return matching.count == 1 ? matching.first : nil
    }
}

public struct SiteCameraGeometry: Sendable, Equatable {
    var pose: SiteCameraPose
    var lens: SiteCameraLens?
}

extension GoClient {
    func cameraMetadata(session: UInt64) async throws -> [SiteCameraMetadata] {
        try await perform {
            guard let p = WDCameraMetadataList(session) else { throw WendyError(message: "Missing camera metadata result.") }
            defer { WDCameraMetadataFree(p) }
            try Self.check(p.pointee.status)
            return UnsafeBufferPointer(start: p.pointee.items, count: p.pointee.count).map { m in
                let distortion = Self.string(m.distortion_model)
                let lens = m.has_intrinsics == 0 ? nil : SiteCameraLens(width: Int(m.width), height: Int(m.height),
                    fx: m.fx, fy: m.fy, cx: m.cx, cy: m.cy, distortionModel: distortion,
                    distortion: distortion == "brown_conrady" ? [m.k1, m.k2, m.p1, m.p2, m.k3] : [],
                    measured: m.measured != 0, note: Self.string(m.note), captureMode: Self.string(m.capture_mode))
                let poses = UnsafeBufferPointer(start: m.poses, count: m.pose_count).map { pose in
                    SiteCameraPose(modelRevision: UUID(uuidString: Self.string(pose.model_revision)),
                        position: SitePoint(x: pose.x, y: pose.y, z: pose.z), rotation: SIMD4(pose.qx, pose.qy, pose.qz, pose.qw),
                        method: Int(pose.method), provenance: Self.string(pose.provenance),
                        observedAt: Date(timeIntervalSince1970: Double(pose.observed_ms)/1000),
                        validUntil: Date(timeIntervalSince1970: Double(pose.valid_until_ms)/1000))
                }
                return SiteCameraMetadata(sourceID: Self.string(m.stable_id), revision: m.revision, lens: lens, poses: poses)
            }
        }
    }
}

extension GoClient {
    func saveCameraAlignment(session: UInt64, sourceID: String, expectedRevision: UInt64, result: SiteLocalizationResult) async throws {
        guard let revision = result.pose.modelRevision, result.lens.isValid,
              result.pose.isCurrent(revision: revision, now: Date()), result.inliers >= Int((result.fittedLens && result.lens.measured) ? WS_MIN_LENS_INLIERS : WS_MIN_POSE_INLIERS), result.residual.isFinite, result.residual <= (result.lens.measured ? 2.5 : 5) else {
            throw SiteLocalizationError.invalidResult
        }
        try await perform {
            try Self.withCString(sourceID) { source in
                try Self.withCString(revision.uuidString) { model in
                    try Self.withCString(result.lens.note) { note in
                        try Self.withCString(result.lens.captureMode) { mode in
                            try Self.withCString(result.pose.provenance) { provenance in
                                let lens = result.lens, pose = result.pose
                                let distortion = lens.distortionModel == "none" ? [Double](repeating: 0, count: 5) : lens.distortion
                                var input = WDCameraAlignmentInput()
                                input.stable_id = source; input.model_revision = model; input.note = note
                                input.capture_mode = mode; input.pose_provenance = provenance
                                input.expected_revision = expectedRevision
                                input.lens_measured = result.lens.measured ? 1 : 0
                                input.lens_only = 0
                                input.width = UInt32(lens.width); input.height = UInt32(lens.height); input.inliers = UInt32(result.inliers)
                                input.fx = lens.fx; input.fy = lens.fy; input.cx = lens.cx; input.cy = lens.cy
                                input.k1 = distortion[0]; input.k2 = distortion[1]; input.p1 = distortion[2]; input.p2 = distortion[3]; input.k3 = distortion[4]
                                input.x = pose.position.x; input.y = pose.position.y; input.z = pose.position.z
                                input.qx = pose.rotation.x; input.qy = pose.rotation.y; input.qz = pose.rotation.z; input.qw = pose.rotation.w
                                input.rms = result.residual
                                input.observed_ms = Int64(pose.observedAt.timeIntervalSince1970*1000)
                                input.valid_until_ms = Int64(pose.validUntil.timeIntervalSince1970*1000)
                                guard let response = WDCameraAlignmentSave(session, &input) else { throw WendyError(message: "No camera calibration response.") }
                                defer { WDStatusFree(response) }
                                try Self.check(response.pointee)
                            }
                        }
                    }
                }
            }
        }
    }
}


extension GoClient {
    func saveCameraLens(session: UInt64, sourceID: String, expectedRevision: UInt64, lens: SiteCameraLens) async throws {
        guard lens.isValid, lens.measured, lens.boundaryRays != nil else { throw SiteLocalizationError.invalidResult }
        try await perform {
            try Self.withCString(sourceID) { source in
                try Self.withCString(lens.note) { note in
                    try Self.withCString(lens.captureMode) { mode in
                        var input = WDCameraAlignmentInput()
                        input.stable_id = source; input.note = note; input.capture_mode = mode
                        input.expected_revision = expectedRevision; input.lens_measured = 1; input.lens_only = 1
                        input.width = UInt32(lens.width); input.height = UInt32(lens.height)
                        input.fx = lens.fx; input.fy = lens.fy; input.cx = lens.cx; input.cy = lens.cy
                        let d = lens.distortionModel == "none" ? [Double](repeating: 0, count: 5) : lens.distortion
                        input.k1 = d[0]; input.k2 = d[1]; input.p1 = d[2]; input.p2 = d[3]; input.k3 = d[4]
                        input.observed_ms = Int64(Date().timeIntervalSince1970*1000)
                        guard let response = WDCameraAlignmentSave(session, &input) else { throw WendyError(message: "No lens calibration response.") }
                        defer { WDStatusFree(response) }
                        try Self.check(response.pointee)
                    }
                }
            }
        }
    }
}
