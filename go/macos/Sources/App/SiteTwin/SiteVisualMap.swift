import Foundation
import CryptoKit
import CoreGraphics
import simd

public struct SiteVisualMap: Codable, Sendable {
    public static let maximumBytes = 12 * 1024 * 1024
    var version = 1
    var modelRevision: UUID
    var modelSHA256: String
    var capturedAt: Date
    var frameCount: Int
    var landmarks: Data
    var count: Int { landmarks.count / (131 * MemoryLayout<Float>.size) }

    func validate(modelRevision: UUID, model: Data? = nil) throws {
        guard version == 1, self.modelRevision == modelRevision, (1...200).contains(frameCount),
              (120...12000).contains(count), landmarks.count == count*131*4,
              modelSHA256.count == 64, modelSHA256.allSatisfy({ $0.isHexDigit }),
              capturedAt.timeIntervalSince1970.isFinite else { throw SiteLocalizationError.invalidMap }
        if let model, Self.digest(model) != modelSHA256 { throw SiteLocalizationError.invalidMap }
        let valid = landmarks.withUnsafeBytes { raw in
            for index in 0..<(landmarks.count/4) {
                let value = raw.loadUnaligned(fromByteOffset: index*4, as: Float.self)
                if !value.isFinite || (index % 131 < 3 ? abs(value)>1000 : value<0 || value>512) { return false }
            }
            return true
        }
        guard valid else { throw SiteLocalizationError.invalidMap }
    }
    static func digest(_ data: Data) -> String { SHA256.hash(data: data).map { String(format: "%02x", $0) }.joined() }
    static func read(_ url: URL, revision: UUID, model: Data? = nil) throws -> SiteVisualMap {
        let attributes = try url.resourceValues(forKeys: [.fileSizeKey, .isRegularFileKey, .isSymbolicLinkKey])
        guard attributes.isRegularFile == true, attributes.isSymbolicLink != true,
              let size = attributes.fileSize, size <= maximumBytes else { throw SiteLocalizationError.invalidMap }
        let map = try JSONDecoder().decode(Self.self, from: Data(contentsOf: url))
        try map.validate(modelRevision: revision, model: model)
        return map
    }
}

struct SiteReferenceFrame: Sendable {
    var gray: Data
    var width: Int, height: Int
    var depth: [Float]
    var confidence: [UInt8]
    var depthWidth: Int, depthHeight: Int
    var intrinsics: [Double]
    var opticalToWorld: [Double]
}

struct SiteLocalizationResult: Sendable {
    var lens: SiteCameraLens
    var pose: SiteCameraPose
    var inliers: Int
    var residual: Double
    var fittedLens: Bool
}

private final class SiteMapHandle: @unchecked Sendable {
    let pointer = WSMapCreate()
    deinit { WSMapDestroy(pointer) }
}

actor SiteLocalizationEngine {
    private let storage = SiteLocalizationStorage()
    private let queue = DispatchQueue(label: "dev.wendy.site-localization", qos: .userInitiated)
    func add(_ frame: SiteReferenceFrame) async throws -> Int {
        try await BlockingWork.run(on: queue) { [storage] in try storage.add(frame) }
    }
    func snapshot(model: Data, revision: UUID) async throws -> SiteVisualMap {
        try await BlockingWork.run(on: queue) { [storage] in try storage.snapshot(model: model, revision: revision) }
    }
    func load(_ map: SiteVisualMap, revision: UUID, model: Data) async throws {
        try await BlockingWork.run(on: queue) { [storage] in try storage.load(map, revision: revision, model: model) }
    }
    func localize(gray: Data, width: Int, height: Int, revision: UUID, lens: SiteCameraLens?, estimated: Bool = false) async throws -> SiteLocalizationResult {
        try await BlockingWork.run(on: queue) { [storage] in try storage.localize(gray: gray, width: width, height: height, revision: revision, lens: lens, estimated: estimated) }
    }
}

// Accessed only by the engine's serial worker queue. Native image work never
// blocks the main actor or Swift's cooperative executor.
private final class SiteLocalizationStorage: @unchecked Sendable {
    private var handle = SiteMapHandle()
    private var frames = 0

    func add(_ frame: SiteReferenceFrame) throws -> Int {
        guard (32...1920).contains(frame.width), (32...1920).contains(frame.height),
              frame.gray.count == frame.width*frame.height,
              (8...1024).contains(frame.depthWidth), (8...1024).contains(frame.depthHeight),
              frame.depth.count == frame.depthWidth*frame.depthHeight, frame.confidence.count == frame.depth.count,
              frame.intrinsics.count == 4, frame.opticalToWorld.count == 16, frames < 200 else { throw SiteLocalizationError.invalidMap }
        let count = frame.gray.withUnsafeBytes { gray in
            frame.depth.withUnsafeBufferPointer { depth in
                frame.confidence.withUnsafeBufferPointer { confidence in
                    frame.intrinsics.withUnsafeBufferPointer { intrinsics in
                        frame.opticalToWorld.withUnsafeBufferPointer { transform in
                            WSMapAdd(handle.pointer, gray.bindMemory(to: UInt8.self).baseAddress, Int32(frame.width), Int32(frame.height),
                                depth.baseAddress, confidence.baseAddress, Int32(frame.depthWidth), Int32(frame.depthHeight), intrinsics.baseAddress, transform.baseAddress)
                        }
                    }
                }
            }
        }
        guard count >= 0 else { throw SiteLocalizationError.invalidMap }
        frames += 1
        return Int(count)
    }

    func snapshot(model: Data, revision: UUID) throws -> SiteVisualMap {
        let count = Int(WSMapCount(handle.pointer))
        guard count >= 120 else { throw SiteLocalizationError.insufficientMap }
        var values = [Float](repeating: 0, count: count*131)
        let written = values.withUnsafeMutableBufferPointer { WSMapExport(handle.pointer, $0.baseAddress, Int32(count)) }
        guard written == count else { throw SiteLocalizationError.invalidMap }
        let map = SiteVisualMap(modelRevision: revision, modelSHA256: SiteVisualMap.digest(model), capturedAt: Date(),
            frameCount: frames, landmarks: values.withUnsafeBytes { Data($0) })
        try map.validate(modelRevision: revision, model: model)
        return map
    }

    func load(_ map: SiteVisualMap, revision: UUID, model: Data) throws {
        try map.validate(modelRevision: revision, model: model)
        let next = SiteMapHandle()
        var values = [Float](repeating: 0, count: map.landmarks.count/4)
        _ = values.withUnsafeMutableBytes { map.landmarks.copyBytes(to: $0) }
        let count = values.withUnsafeBufferPointer { WSMapImport(next.pointer, $0.baseAddress, Int32(map.count)) }
        guard count == map.count else { throw SiteLocalizationError.invalidMap }
        handle = next; frames = map.frameCount
    }

    func localize(gray: Data, width: Int, height: Int, revision: UUID, lens: SiteCameraLens?, estimated: Bool = false) throws -> SiteLocalizationResult {
        guard (32...8192).contains(width), (32...8192).contains(height), width*height <= 32_000_000,
              gray.count == width*height else { throw SiteLocalizationError.invalidImage }
        let existing = lens.flatMap { $0.isValid && ($0.measured || estimated) && $0.width == width && $0.height == height ? $0 : nil }
        let known = existing.map { [$0.fx,$0.fy,$0.cx,$0.cy] + ($0.distortionModel == "none" ? [0,0,0,0,0] : $0.distortion) }
        let output = gray.withUnsafeBytes { pixels in
            if let known {
                return known.withUnsafeBufferPointer {
                    if existing?.measured == false { return WSLocalizeWithAssumedLens(handle.pointer, pixels.bindMemory(to: UInt8.self).baseAddress, Int32(width), Int32(height), $0.baseAddress) }
                    return WSLocalize(handle.pointer, pixels.bindMemory(to: UInt8.self).baseAddress, Int32(width), Int32(height), $0.baseAddress)
                }
            }
            if estimated { return WSLocalizeEstimated(handle.pointer, pixels.bindMemory(to: UInt8.self).baseAddress, Int32(width), Int32(height)) }
            return WSLocalize(handle.pointer, pixels.bindMemory(to: UInt8.self).baseAddress, Int32(width), Int32(height), nil)
        }
        guard output.success != 0 else { throw SiteLocalizationError.noSolution(Int(output.reason), Int(output.matches), Int(output.inliers)) }
        var rawTransform = output.world_from_camera, rawDistortion = output.distortion
        let t = withUnsafeBytes(of: &rawTransform) { Array($0.bindMemory(to: Double.self)) }
        let distortion = withUnsafeBytes(of: &rawDistortion) { Array($0.bindMemory(to: Double.self)) }
        let rotation = simd_double3x3(columns: (SIMD3(t[0],t[1],t[2]),SIMD3(t[4],t[5],t[6]),SIMD3(t[8],t[9],t[10])))
        let approximate = output.fitted_lens == 2 || existing?.measured == false
        let now = Date()
        let pose = SiteCameraPose(modelRevision: revision, position: SitePoint(x:t[12],y:t[13],z:t[14]),
            rotation: simd_quatd(rotation).normalized.vector, method: 2,
            provenance: (approximate ? "Approximate pose using an assumed pinhole lens. " : "") + "Matched camera image to iPhone LiDAR room references; held-out RMS \(String(format: "%.2f", output.rms)) px", observedAt: now,
            validUntil: now.addingTimeInterval(7*24*60*60))
        let fitted = SiteCameraLens(width: width, height: height, fx: output.fx, fy: output.fy, cx: output.cx, cy: output.cy,
            distortionModel: "brown_conrady", distortion: distortion, measured: !approximate,
            note: approximate ? "Estimated pinhole lens from scene matches; centred principal point and zero distortion assumed. FOV and camera position are approximate." : "Joint fit against iPhone LiDAR scene points; square pixels, radial distortion; held-out RMS \(String(format: "%.2f", output.rms)) px",
            captureMode: "JPEG \(width)x\(height)")
        let resultLens = existing ?? fitted
        guard pose.isCurrent(revision: revision, now: now), resultLens.isValid, resultLens.boundaryRays != nil else { throw SiteLocalizationError.invalidResult }
        return SiteLocalizationResult(lens: resultLens, pose: pose, inliers: Int(output.inliers), residual: output.rms, fittedLens: output.fitted_lens != 0)
    }
}

enum SiteLocalizationError: LocalizedError {
    case invalidMap, insufficientMap, invalidImage, invalidResult, coordinateMismatch, noSolution(Int, Int, Int)
    var errorDescription: String? {
        switch self {
        case .invalidMap: "The room’s visual reference map is invalid or belongs to a different model."
        case .insufficientMap: "Not enough visual references. Scan textured objects at different depths in the area the camera sees."
        case .invalidImage: "A fresh camera image is needed to calibrate and align this camera."
        case .invalidResult: "The estimated camera geometry did not pass validation. No calibration was saved."
        case .coordinateMismatch: "The exported room coordinates could not be verified against the scan. No alignment map was saved."
        case .noSolution(let reason, let matches, let inliers):
            switch reason {
            case 1: "Found \(matches) possible matches. Lens calibration needs at least \(WS_MIN_LENS_INLIERS) consistent points; alignment with a measured lens needs \(WS_MIN_POSE_INLIERS), or \(WS_MIN_ESTIMATED_INLIERS) for an approximate position."
            case 2: "Found \(matches) possible matches, but only \(inliers) agree on the camera geometry. Include more distinctive objects in the scan and camera view."
            case 3: "The matches are concentrated in a small part of the image. Try Estimated Alignment, or measure the lens with your phone screen."
            case 7: "These matches lie on a flat surface, so the lens cannot be measured from this view alone. Use Estimated Alignment or Phone Screen Calibration; the room does not need to change."
            case 8: "The viewing direction is plausible, but the camera’s distance is ambiguous. Different lens estimates fit this view at different positions. No position was saved."
            case 4, 5: "The \(inliers) geometric matches did not give a stable calibration across validation checks. Include clearer views of the camera’s scene and try again."
            default: "Automatic calibration could not solve this view. No calibration was saved."
            }
        }
    }
}
