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
