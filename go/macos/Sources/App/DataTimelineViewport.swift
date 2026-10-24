import Foundation

// All timeline panels use this same episode-relative window. Panning changes
// the window only; it never rewrites a sample timestamp or moves the cursor.
struct DataTimelineViewport: Equatable {
    var start: Double
    var end: Double
    var span: Double { max(end - start, 0.000_001) }

    static func fitted(start: Double, end: Double) -> Self {
        Self(start: start, end: max(end, start + 0.000_001))
    }
    func clamped(to bounds: Self) -> Self {
        let width = min(span, bounds.span)
        let left = min(max(start, bounds.start), bounds.end - width)
        return Self(start: left, end: left + width)
    }
    func panned(by seconds: Double, within bounds: Self) -> Self {
        guard seconds.isFinite else { return self }
        return Self(start: start + seconds, end: end + seconds).clamped(to: bounds)
    }
    func zoomed(by factor: Double, anchor: Double, within bounds: Self) -> Self {
        guard factor.isFinite, factor > 0, anchor.isFinite else { return self }
        let position = min(max(anchor, 0), 1)
        let fixedTime = start + span * position
        let width = min(max(span / factor, min(0.001, bounds.span)), bounds.span)
        return Self(start: fixedTime - width * position, end: fixedTime + width * (1 - position)).clamped(to: bounds)
    }
    func revealing(_ time: Double, within bounds: Self) -> Self {
        guard time < start || time > end else { return self }
        return Self(start: time - span * 0.8, end: time + span * 0.2).clamped(to: bounds)
    }
    func time(at fraction: Double) -> Double { start + min(max(fraction, 0), 1) * span }
    static func tickSpacing(span: Double, width: Double) -> Double {
        let desired = max(span, 0.000_001) / max(width / 90, 1)
        let magnitude = pow(10, floor(log10(desired)))
        let normalized = desired / magnitude
        let step = normalized <= 1 ? 1.0 : normalized <= 2 ? 2.0 : normalized <= 5 ? 5.0 : 10.0
        return step * magnitude
    }
}

