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
                throw WendyError(message: Self.failureMessage(reason: fit.reason))
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

    nonisolated static func failureMessage(reason: Int32) -> String {
        switch reason {
        case Int32(WS_SCREEN_LENS_VIEWS):
            return "More distinct views are needed. Move and tilt the checkerboard, pausing at each position."
        case Int32(WS_SCREEN_LENS_COVERAGE):
            return "More of the camera image needs samples. Move the screen toward the edges, keeping its white border visible."
        case Int32(WS_SCREEN_LENS_TILT):
            return "The captured views are too similar in angle. Bring the left edge closer, then the right edge; repeat with the top and bottom edges. Pause at each angle."
        case Int32(WS_SCREEN_LENS_UNCERTAIN):
            return "The views do not constrain the lens accurately enough. Add tilted views at different distances, keeping the whole checkerboard visible."
        case Int32(WS_SCREEN_LENS_DISAGREEMENT):
            return "Different groups of views give different lens measurements. Keep the pattern flat and unchanged; add sharp views at several angles and distances."
        case Int32(WS_SCREEN_LENS_FIT):
            return "The lens model does not fit the captured corners accurately enough. Keep the pattern flat, avoid glare, and pause so each new view is sharp."
        case Int32(WS_SCREEN_LENS_VALIDATION):
            return "The lens fit failed on separate validation views. Add sharp, tilted views near the image edges, keeping the whole checkerboard visible."
        default:
            return "The lens calculation failed. Cancel and restart calibration. No new calibration was saved."
        }
    }
}


struct ScreenLensProgress: Sendable {
    var count = 0, detection = 0
    var revision: UInt64 = 0
    var coverageReady = false
    var coverage = 0.0, minX = 0.0, maxX = 0.0, minY = 0.0, maxY = 0.0
    // These describe positions in the retained samples, not inferred phone
    // orientation or a percentage of calibration accuracy.
    func hasSample(toward side: ScreenLensSide) -> Bool {
        guard count > 0 else { return false }
        switch side {
        case .left: return minX <= 0.35
        case .right: return maxX >= 0.65
        case .top: return minY <= 0.35
        case .bottom: return maxY >= 0.65
        }
    }
    var phoneMotion: ScreenLensMotion {
        if detection == 0 { return .showPattern }
        if detection == 1 { return .closer }
        if detection == 2 { return .inward }
        if !coverageReady {
            if let missing = ScreenLensSide.allCases.first(where: { !hasSample(toward: $0) }) { return .position(missing) }
            return .closer
        }
        // Advance the demonstration only with accepted samples. Animation time
        // must never mark an angle as measured or calibration as complete.
        return (revision / 2).isMultiple(of: 2) ? .sideEdges : .topBottomEdges
    }
    func shouldSolve(after revision: UInt64) -> Bool {
        count >= 12 && coverageReady && self.revision >= revision && self.revision - revision >= 3
    }
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

enum ScreenLensSide: String, CaseIterable, Sendable {
    case left = "Left", right = "Right", top = "Top", bottom = "Bottom"
}

enum ScreenLensMotion: Equatable, Sendable {
    case showPattern, closer, inward, position(ScreenLensSide), sideEdges, topBottomEdges

    var instruction: String {
        switch self {
        case .showPattern: return "Face this screen toward the camera. Keep the whole checkerboard visible."
        case .closer: return "Move the phone closer to the camera. Keep the whole white border visible."
        case .inward: return "Move the phone back toward the middle of the camera preview."
        case .position(let side): return "Move the phone toward the \(side.rawValue.lowercased()) of the camera preview."
        case .sideEdges: return "Bring the left edge closer to the camera, then the right edge. Pause at each angle."
        case .topBottomEdges: return "Bring the top edge closer to the camera, then the bottom edge. Pause at each angle."
        }
    }
}
