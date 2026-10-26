import Foundation

private final class ScreenLensHandle: @unchecked Sendable {
    let pointer = WSScreenLensCreate()
    deinit { WSScreenLensDestroy(pointer) }
}

actor ScreenLensCalibration {
    private let handle = ScreenLensHandle()
    private let queue = DispatchQueue(label: "dev.wendy.screen-lens-calibration", qos: .userInitiated)
    func add(_ frame: CameraCalibrationFrame) async throws -> Int {
        try await BlockingWork.run(on: queue) { [handle] in
            guard frame.width > 0, frame.height > 0, frame.gray.count == frame.width*frame.height else { throw SiteLocalizationError.invalidImage }
            let count = frame.gray.withUnsafeBytes { raw in
                WSScreenLensAdd(handle.pointer, raw.bindMemory(to: UInt8.self).baseAddress, Int32(frame.width), Int32(frame.height))
            }
            guard count >= 0 else { throw WendyError(message: count == -2 ? "The stream resolution changed. Restart lens calibration." : "The calibration image could not be processed.") }
            return Int(count)
        }
    }
    func progress() async throws -> ScreenLensProgress {
        try await BlockingWork.run(on: queue) { [handle] in
            let p = WSScreenLensGetProgress(handle.pointer)
            return ScreenLensProgress(count: Int(p.count), detection: Int(p.detection), revision: p.revision,
                coverageReady: p.coverage_ready != 0, coverage: p.coverage,
                minX: p.min_x, maxX: p.max_x, minY: p.min_y, maxY: p.max_y)
        }
    }
    func solve(width: Int, height: Int, codec: String) async throws -> SiteCameraLens {
        try await BlockingWork.run(on: queue) { [handle] in
            let fit = WSScreenLensSolve(handle.pointer)
            guard fit.success != 0 else {
                throw WendyError(message: fit.reason == 3 ? "Move the screen toward each edge of the camera view." : "Tilt the screen left/right and up/down, and vary its distance. Hold it still briefly at each angle.")
            }
            var raw = fit.distortion
            let distortion = withUnsafeBytes(of: &raw) { Array($0.bindMemory(to: Double.self)) }
            let lens = SiteCameraLens(width: width, height: height, fx: fit.fx, fy: fit.fy, cx: fit.cx, cy: fit.cy,
                distortionModel: "brown_conrady", distortion: distortion, measured: true,
                note: "Measured using a 6x9 phone-screen checkerboard across \(fit.matches) views; withheld validation RMS \(String(format: "%.2f", fit.rms)) px",
                captureMode: "\(codec) \(width)x\(height)")
            guard lens.isValid, lens.boundaryRays != nil else { throw SiteLocalizationError.invalidResult }
            return lens
        }
    }
}


struct ScreenLensProgress: Sendable {
    var count = 0, detection = 0
    var revision: UInt64 = 0
    var coverageReady = false
    var coverage = 0.0, minX = 0.0, maxX = 0.0, minY = 0.0, maxY = 0.0
    var nextStep: String {
        if detection == 0 { return "Show the whole checkerboard to Go2 and hold it still until a view is captured." }
        if detection == 1 { return "Bring the checkerboard closer. It is too small in Go2's image to measure accurately." }
        if detection == 2 { return "Move the checkerboard slightly inward so its whole white border stays visible." }
        if !coverageReady && count > 0 {
            if minX > 0.35 { return "Move the checkerboard toward the LEFT side of this camera preview. Keep the whole pattern visible." }
            if maxX < 0.65 { return "Move the checkerboard toward the RIGHT side of this camera preview. Keep the whole pattern visible." }
            if minY > 0.35 { return "Move the checkerboard toward the TOP of this camera preview. Keep the whole pattern visible." }
            if maxY < 0.65 { return "Move the checkerboard toward the BOTTOM of this camera preview. Keep the whole pattern visible." }
            return "Bring the checkerboard closer and show it near the image corners. More of the image needs to be covered."
        }
        return "Tilt the checkerboard left/right and up/down, and vary its distance. Hold still until another view is captured."
    }
}
