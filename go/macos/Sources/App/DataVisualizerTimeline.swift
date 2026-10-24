import AppKit
import SwiftUI
import QuartzCore

// Shared native playhead: pointer feedback never waits for a SwiftUI workspace
// refresh or a video seek. Weak subscribers include the ruler, tracks and plot.
@MainActor
final class DataTimelinePlayhead {
    private let views = NSHashTable<DataTimelineInputView>.weakObjects()
    private weak var dragOwner: DataTimelineInputView?
    private var position: Double?

    func attach(_ view: DataTimelineInputView) {
        views.add(view)
        if let position { view.cursor = position }
    }
    func detach(_ view: DataTimelineInputView) {
        views.remove(view)
        if dragOwner === view { dragOwner = nil }
    }
    func synchronize(_ time: Double) {
        guard dragOwner == nil else { return }
        move(to: time)
    }
    func drag(to time: Double, owner: DataTimelineInputView) {
        dragOwner = owner
        move(to: time)
    }
    func endDrag(owner: DataTimelineInputView) {
        if dragOwner === owner { dragOwner = nil }
    }
    private func move(to time: Double) {
        guard position != time else { return }
        position = time
        for view in views.allObjects { view.cursor = time }
    }
}

struct DataTimelineInteraction: NSViewRepresentable {
    let window: DataTimelineViewport
    let cursor: Double
    var ruler = false
    var playhead: DataTimelinePlayhead? = nil
    let seek: (Double) -> Void
    let pan: (Double) -> Void
    let zoom: (Double, Double) -> Void

    func makeNSView(context: Context) -> DataTimelineInputView {
        let view = DataTimelineInputView(frame: .zero)
        view.setAccessibilityElement(ruler)
        if ruler {
            view.setAccessibilityRole(.slider)
            view.setAccessibilityLabel("Recording timeline")
            view.setAccessibilityHelp("Drag to seek. Scroll horizontally or Option-drag to pan. Pinch to zoom.")
        }
        return view
    }
    func updateNSView(_ view: DataTimelineInputView, context: Context) {
        let changed = view.windowRange != window || view.drawRuler != ruler
        if view.playhead !== playhead {
            view.playhead?.detach(view)
            view.playhead = playhead
            playhead?.attach(view)
        }
        view.windowRange = window
        view.drawRuler = ruler
        if let playhead { playhead.synchronize(cursor) }
        else if !view.hasPendingSeek { view.cursor = cursor }
        view.updatePlayheadLayer()
        view.seek = seek
        view.pan = pan
        view.zoom = zoom
        if ruler {
            view.setAccessibilityValue(view.cursor)
            view.setAccessibilityMinValue(window.start)
            view.setAccessibilityMaxValue(window.end)
            if changed { view.needsDisplay = true }
        }
    }
}

final class DataTimelineInputView: NSView {
    var windowRange = DataTimelineViewport(start: 0, end: 1)
    var cursor = 0.0 { didSet { if cursor != oldValue { updatePlayheadLayer() } } }
    var drawRuler = false
    weak var playhead: DataTimelinePlayhead?
    private let playheadLayer = CAShapeLayer()

    override init(frame frameRect: NSRect) {
        super.init(frame: frameRect)
        wantsLayer = true
        layer?.addSublayer(playheadLayer)
    }
    required init?(coder: NSCoder) { fatalError("init(coder:) has not been implemented") }

    override func layout() {
        super.layout()
        updatePlayheadLayer()
    }

    func updatePlayheadLayer() {
        CATransaction.begin()
        CATransaction.setDisableActions(true)
        playheadLayer.frame = bounds
        playheadLayer.isHidden = cursor < windowRange.start || cursor > windowRange.end
        let x = (cursor - windowRange.start) / windowRange.span * max(bounds.width, 1)
        let path = CGMutablePath()
        if drawRuler {
            path.move(to: CGPoint(x: x - 5, y: 18))
            path.addLine(to: CGPoint(x: x + 5, y: 18))
            path.addLine(to: CGPoint(x: x, y: 25))
            path.closeSubpath()
        }
        path.addRect(CGRect(x: x - 0.75, y: drawRuler ? 25 : 0, width: 1.5,
                            height: max(0, bounds.height - (drawRuler ? 25 : 0))))
        playheadLayer.path = path
        playheadLayer.fillColor = NSColor.systemOrange.cgColor
        CATransaction.commit()
    }
    var seek: (Double) -> Void = { _ in }
    var pan: (Double) -> Void = { _ in }
    var zoom: (Double, Double) -> Void = { _, _ in }
    private var lastPoint = CGPoint.zero
    private var panning = false
    private var pendingSeek: Double?
    private var scheduledSeek: DispatchWorkItem?
    private var cachedRuler: NSImage?
    private var cachedWindow: DataTimelineViewport?
    private var cachedSize = CGSize.zero
    private var cachedAppearance: NSAppearance.Name?
    var hasPendingSeek: Bool { pendingSeek != nil }
    override var acceptsFirstResponder: Bool { drawRuler }
    override var isFlipped: Bool { true }

    override func draw(_ dirtyRect: NSRect) {
        guard drawRuler else { return }
        if cachedWindow != windowRange || cachedSize != bounds.size || cachedAppearance != effectiveAppearance.name {
            let size = bounds.size
            cachedRuler = NSImage(size: size, flipped: true) { [weak self] _ in
                self?.drawRulerMarks()
                return true
            }
            // Force the drawing handler once into a bitmap representation.
            if let image = cachedRuler, let cg = image.cgImage(forProposedRect: nil, context: nil, hints: nil) {
                cachedRuler = NSImage(cgImage: cg, size: size)
            }
            cachedWindow = windowRange; cachedSize = size; cachedAppearance = effectiveAppearance.name
        }
        cachedRuler?.draw(in: bounds)
    }
    private func drawRulerMarks() {
        NSColor.controlBackgroundColor.withAlphaComponent(0.5).setFill()
        bounds.fill()
        let width = max(bounds.width, 1)
        let step = max(DataTimelineViewport.tickSpacing(span: windowRange.span, width: width), windowRange.start.ulp * 2, windowRange.end.ulp * 2)
        let first = ceil(windowRange.start / step) * step
        let font = NSFont.monospacedDigitSystemFont(ofSize: 10, weight: .regular)
        let attributes: [NSAttributedString.Key: Any] = [.font: font, .foregroundColor: NSColor.secondaryLabelColor]
        // Bound work and derive ticks from their ordinal. Repeated addition can
        // stall at large timestamps when a tick is smaller than Double's ULP.
        let count = min(128, max(1, Int(ceil(windowRange.span / step)) + 1))
        for ordinal in 0..<count {
            let time = first + Double(ordinal) * step
            guard time <= windowRange.end else { break }
            let x = (time - windowRange.start) / windowRange.span * width
            let mark = NSBezierPath()
            mark.move(to: CGPoint(x: x, y: 22)); mark.line(to: CGPoint(x: x, y: bounds.height))
            NSColor.separatorColor.setStroke(); mark.stroke()
            let precision = step >= 1 ? 0 : min(6, max(0, Int(ceil(-log10(step)))))
            let label = String(format: "%+.*f s", precision, abs(time) < step / 1000 ? 0 : time) as NSString
            let labelWidth = label.size(withAttributes: attributes).width
            label.draw(at: CGPoint(x: min(max(2, x + 4), max(2, width - labelWidth - 2)), y: 3), withAttributes: attributes)
        }
    }
    override func viewDidMoveToWindow() {
        super.viewDidMoveToWindow()
        if window == nil {
            scheduledSeek?.cancel(); scheduledSeek = nil; pendingSeek = nil
            playhead?.detach(self)
        } else { playhead?.attach(self) }
    }
    override func mouseDown(with event: NSEvent) {
        window?.makeFirstResponder(self)
        lastPoint = convert(event.locationInWindow, from: nil)
        panning = event.modifierFlags.contains(.option)
        if panning { NSCursor.closedHand.push() } else { seekAt(lastPoint.x) }
    }
    override func mouseDragged(with event: NSEvent) {
        let point = convert(event.locationInWindow, from: nil)
        if panning { pan(-(point.x - lastPoint.x) / max(bounds.width, 1) * windowRange.span) }
        else { seekAt(point.x) }
        lastPoint = point
    }
    override func mouseUp(with event: NSEvent) {
        if panning { NSCursor.pop() }
        else {
            seekAt(convert(event.locationInWindow, from: nil).x)
            flushSeek()
            playhead?.endDrag(owner: self)
        }
        panning = false
    }
    override func scrollWheel(with event: NSEvent) {
        let point = convert(event.locationInWindow, from: nil)
        if event.modifierFlags.contains(.command) {
            zoom(exp(-event.scrollingDeltaY * 0.015), fraction(point.x))
        } else if abs(event.scrollingDeltaX) > abs(event.scrollingDeltaY) || event.modifierFlags.contains(.shift) {
            let delta = abs(event.scrollingDeltaX) > 0 ? event.scrollingDeltaX : event.scrollingDeltaY
            pan(-delta / max(bounds.width, 1) * windowRange.span)
        } else { super.scrollWheel(with: event) }
    }
    override func magnify(with event: NSEvent) {
        zoom(exp(event.magnification), fraction(convert(event.locationInWindow, from: nil).x))
    }
    override func keyDown(with event: NSEvent) {
        let step = windowRange.span / (event.modifierFlags.contains(.shift) ? 10 : 100)
        switch event.keyCode {
        case 123: seek(max(windowRange.start, cursor - step))
        case 124: seek(min(windowRange.end, cursor + step))
        default: super.keyDown(with: event)
        }
    }
    override func accessibilityPerformIncrement() -> Bool { seek(min(windowRange.end, cursor + windowRange.span / 100)); return true }
    override func accessibilityPerformDecrement() -> Bool { seek(max(windowRange.start, cursor - windowRange.span / 100)); return true }
    private func fraction(_ x: Double) -> Double { min(max(x / max(bounds.width, 1), 0), 1) }
    private func seekAt(_ x: Double) {
        let time = windowRange.time(at: fraction(x))
        pendingSeek = time
        cursor = time
        playhead?.drag(to: time, owner: self)
        guard scheduledSeek == nil else { return }
        let work = DispatchWorkItem { [weak self] in self?.flushSeek() }
        scheduledSeek = work
        // Refresh decoded media, events and inspector at 20 Hz while dragging.
        // The native playheads above follow every pointer event; mouseUp flushes
        // the final exact timestamp without waiting for this interval.
        DispatchQueue.main.asyncAfter(deadline: .now() + 1.0 / 20.0, execute: work)
    }
    private func flushSeek() {
        scheduledSeek?.cancel(); scheduledSeek = nil
        guard let time = pendingSeek else { return }
        pendingSeek = nil
        cursor = time
        seek(time)
    }
}
