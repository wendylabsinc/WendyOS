#if os(iOS)
import ARKit
import Accelerate
import Foundation
import Observation
import simd

@Observable @MainActor
final class SiteReferenceCapture {
    let engine = SiteLocalizationEngine()
    var landmarkCount = 0
    var message = "Looking for LiDAR depth…"
    private var frames = 0
    private var lastTransform: simd_float4x4?
    private var interrupted = false

    func run(session: ARSession) async {
        while !Task.isCancelled {
            if frames >= 200 { message = "Visual reference frame limit reached"; return }
            if let frame = session.currentFrame {
                if case .normal = frame.camera.trackingState {
                    if interrupted { message = "Tracking was interrupted. Restart this scan for camera alignment."; return }
                    let transform = frame.camera.transform
                    let moved = lastTransform.map { simd_distance($0.columns.3, transform.columns.3) > 0.12 || simd_dot($0.columns.2, transform.columns.2) < 0.985 } ?? true
                    if moved, let reference = Self.copy(frame) {
                        do {
                            landmarkCount = try await engine.add(reference)
                            frames += 1; lastTransform = transform
                            message = landmarkCount >= 12000
                                ? "Updating visual references from new viewpoints"
                                : "\(landmarkCount) visual references"
                        } catch { message = error.localizedDescription; return }
                    }
                } else if frames > 0 {
                    // A relocalization may change the world frame. Do not mix
                    // landmarks from coordinate frames we cannot reconcile.
                    if case .limited(.relocalizing) = frame.camera.trackingState { interrupted = true }
                }
            }
            do { try await Task.sleep(for: .milliseconds(600)) } catch { return }
        }
    }

    func snapshot(model: Data, revision: UUID) async throws -> SiteVisualMap {
        guard !interrupted else { throw SiteLocalizationError.coordinateMismatch }
        return try await engine.snapshot(model: model, revision: revision)
    }

    private static func copy(_ frame: ARFrame) -> SiteReferenceFrame? {
        guard let depth = frame.sceneDepth, let confidence = depth.confidenceMap else { return nil }
        let image = frame.capturedImage, depthBuffer = depth.depthMap
        CVPixelBufferLockBaseAddress(image, .readOnly)
        CVPixelBufferLockBaseAddress(depthBuffer, .readOnly)
        CVPixelBufferLockBaseAddress(confidence, .readOnly)
        defer {
            CVPixelBufferUnlockBaseAddress(image, .readOnly)
            CVPixelBufferUnlockBaseAddress(depthBuffer, .readOnly)
            CVPixelBufferUnlockBaseAddress(confidence, .readOnly)
        }
        guard CVPixelBufferGetPlaneCount(image) >= 1,
              let imageBase = CVPixelBufferGetBaseAddressOfPlane(image, 0),
              let depthBase = CVPixelBufferGetBaseAddress(depthBuffer),
              let confidenceBase = CVPixelBufferGetBaseAddress(confidence) else { return nil }
        let iw = CVPixelBufferGetWidthOfPlane(image, 0), ih = CVPixelBufferGetHeightOfPlane(image, 0)
        let scale = min(1, 960.0/Double(max(iw, ih)))
        let w = Int(Double(iw)*scale), h = Int(Double(ih)*scale)
        let dw = CVPixelBufferGetWidth(depthBuffer), dh = CVPixelBufferGetHeight(depthBuffer)
        guard dw == CVPixelBufferGetWidth(confidence), dh == CVPixelBufferGetHeight(confidence), dw <= 1024, dh <= 1024 else { return nil }
        var gray = Data(count: w*h)
        var source = vImage_Buffer(data: imageBase, height: vImagePixelCount(ih), width: vImagePixelCount(iw), rowBytes: CVPixelBufferGetBytesPerRowOfPlane(image, 0))
        let status = gray.withUnsafeMutableBytes { raw in
            var target = vImage_Buffer(data: raw.baseAddress, height: vImagePixelCount(h), width: vImagePixelCount(w), rowBytes: w)
            return vImageScale_Planar8(&source, &target, nil, vImage_Flags(kvImageHighQualityResampling))
        }
        guard status == kvImageNoError else { return nil }
        var depths = [Float](), confidences = [UInt8]()
        depths.reserveCapacity(dw*dh); confidences.reserveCapacity(dw*dh)
        for y in 0..<dh {
            depths.append(contentsOf: UnsafeBufferPointer(start: depthBase.advanced(by: y*CVPixelBufferGetBytesPerRow(depthBuffer)).assumingMemoryBound(to: Float.self), count: dw))
            confidences.append(contentsOf: UnsafeBufferPointer(start: confidenceBase.advanced(by: y*CVPixelBufferGetBytesPerRow(confidence)).assumingMemoryBound(to: UInt8.self), count: dw))
        }
        let k = frame.camera.intrinsics
        var optical = frame.camera.transform
        optical.columns.1 *= -1; optical.columns.2 *= -1
        let transform = (0..<4).flatMap { col in (0..<4).map { row in Double(optical[col][row]) } }
        return SiteReferenceFrame(gray: gray, width: w, height: h, depth: depths, confidence: confidences,
            depthWidth: dw, depthHeight: dh,
            intrinsics: [Double(k[0][0])*Double(w)/Double(iw), Double(k[1][1])*Double(h)/Double(ih), Double(k[2][0])*Double(w)/Double(iw), Double(k[2][1])*Double(h)/Double(ih)], opticalToWorld: transform)
    }
}
#endif
