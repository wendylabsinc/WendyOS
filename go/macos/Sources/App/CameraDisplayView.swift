import AppKit
import AVFoundation
import SwiftUI

/// Layer-hosting `NSView` whose `AVSampleBufferDisplayLayer` fills its bounds.
@available(macOS 15.0, *)
private final class VideoHostView: NSView {
    let displayLayer = AVSampleBufferDisplayLayer()

    override init(frame frameRect: NSRect) {
        super.init(frame: frameRect)
        wantsLayer = true
        layer?.backgroundColor = NSColor.black.cgColor
        displayLayer.backgroundColor = NSColor.black.cgColor
        layer?.addSublayer(displayLayer)
    }

    required init?(coder: NSCoder) { fatalError("init(coder:) has not been implemented") }

    override func layout() {
        super.layout()
        displayLayer.frame = bounds
    }
}

/// SwiftUI wrapper that renders a `H264AnnexBDecoder`'s output.
@available(macOS 15.0, *)
struct CameraDisplayView: NSViewRepresentable {
    let decoder: H264AnnexBDecoder

    func makeCoordinator() -> H264AnnexBDecoder { decoder }

    static func dismantleNSView(_ nsView: NSView, coordinator: H264AnnexBDecoder) {
        if let view = nsView as? VideoHostView { coordinator.detach(layer: view.displayLayer) }
    }

    func makeNSView(context: Context) -> NSView {
        let view = VideoHostView()
        decoder.attach(layer: view.displayLayer)
        return view
    }

    func updateNSView(_ nsView: NSView, context: Context) {}
}
