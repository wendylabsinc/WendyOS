import SwiftUI
import AVFoundation

private final class DataVisualPlayerHost: UIView {
    override class var layerClass: AnyClass { AVPlayerLayer.self }
    var playerLayer: AVPlayerLayer { layer as! AVPlayerLayer }
}

struct DataVisualNativeVideoPlayer: UIViewRepresentable {
    let video: DataVisualVideo
    let cursor: Double
    let playing: Bool
    let speed: Double
    let controller: DataVisualVideoController
    func makeCoordinator() -> DataVisualVideoController { controller }
    func makeUIView(context: Context) -> UIView {
        let host = DataVisualPlayerHost(frame: .zero)
        host.playerLayer.player = context.coordinator.player
        return host
    }
    func updateUIView(_ nsView: UIView, context: Context) {
        guard let frame = DataVisualSearch.latest(video.timestamps, at: cursor, time: { $0 }), let origin = video.timestamps.first else { return }
        context.coordinator.update(position: cursor - origin, frame: frame - origin, playing: playing, speed: speed)
    }
    static func dismantleUIView(_ nsView: UIView, coordinator: DataVisualVideoController) { coordinator.stop() }
}

