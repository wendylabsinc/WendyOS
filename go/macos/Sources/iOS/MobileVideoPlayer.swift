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

struct MobileRecordingAudio: UIViewRepresentable {
    let segment: DataVisualAudio
    let owner: DataVisualMediaFiles
    let cursor: Double
    let playing: Bool

    @MainActor final class Coordinator {
        let owner: DataVisualMediaFiles
        var player: AVAudioPlayer?
        var error: String?
        init(segment: DataVisualAudio, owner: DataVisualMediaFiles) {
            self.owner = owner
            do {
                player = try AVAudioPlayer(contentsOf: segment.url)
                player?.prepareToPlay()
            } catch { self.error = error.localizedDescription }
        }
    }
    func makeCoordinator() -> Coordinator { Coordinator(segment: segment, owner: owner) }
    func makeUIView(context: Context) -> UILabel {
        let label = UILabel()
        label.font = .preferredFont(forTextStyle: .caption1)
        label.textColor = .systemRed
        return label
    }
    func updateUIView(_ label: UILabel, context: Context) {
        label.text = context.coordinator.error
        guard let player = context.coordinator.player else { return }
        let position = max(0, cursor - segment.start)
        if !playing { player.pause(); player.currentTime = position; return }
        if !player.isPlaying || abs(player.currentTime - position) > 0.15 { player.currentTime = position }
        if !player.isPlaying && !player.play() { label.text = "Audio playback could not start." }
    }
    static func dismantleUIView(_ label: UILabel, coordinator: Coordinator) { coordinator.player?.stop() }
}
