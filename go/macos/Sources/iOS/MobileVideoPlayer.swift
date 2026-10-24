import SwiftUI
import AVFoundation

private final class DataVisualPlayerHost: UIView {
    override class var layerClass: AnyClass { AVPlayerLayer.self }
    var playerLayer: AVPlayerLayer { layer as! AVPlayerLayer }
}

struct DataVisualVideoPlayer: UIViewRepresentable {
    let video: DataVisualVideo
    let cursor: Double
    let playing: Bool
    let speed: Double
    let mediaOwner: DataVisualMediaFiles
    @MainActor final class Coordinator {
        let player: AVPlayer
        let owner: DataVisualMediaFiles
        private var seeks = DataVisualSeekState()
        private var readyObservation: NSKeyValueObservation?
        private var scheduledSeek: Task<Void, Never>?
        private var lastSeekHost = -Double.infinity
        private var active = true
        private var wantsPlayback = false
        private var playbackRate = 1.0
        private var playbackPosition = 0.0
        private var playbackHost = 0.0
        private var needsPlaybackAnchor = true
        private var hostTime: Double { CMClockGetTime(CMClockGetHostTimeClock()).seconds }
        init(video: DataVisualVideo, owner: DataVisualMediaFiles) {
            self.owner = owner
            player = AVPlayer(url: video.url)
            player.isMuted = true
            player.actionAtItemEnd = .pause
            // Host-clock anchoring requires this to be false (AVPlayer.h).
            player.automaticallyWaitsToMinimizeStalling = false
            readyObservation = player.currentItem?.observe(\.status, options: [.initial, .new]) { [weak self] item, _ in
                guard item.status == .readyToPlay else { return }
                Task { @MainActor in
                    guard let self, self.active else { return }
                    if self.wantsPlayback { self.startPlayback() } else { self.scheduleSeek() }
                }
            }
        }
        func update(position: Double, frame: Double, playing: Bool, speed: Double) {
            guard active else { return }
            let changed = wantsPlayback != playing || playbackRate != speed
            wantsPlayback = playing
            playbackRate = speed
            playbackPosition = position
            playbackHost = hostTime
            if playing {
                if changed { needsPlaybackAnchor = true }
                if seeks.inFlight == nil { scheduledSeek?.cancel(); scheduledSeek = nil }
                startPlayback()
            } else {
                if changed { player.pause() }
                needsPlaybackAnchor = true
                seeks.request(frame)
                scheduleSeek()
            }
        }
        private func startPlayback() {
            guard active, needsPlaybackAnchor, wantsPlayback, seeks.inFlight == nil,
                  player.currentItem?.status == .readyToPlay else { return }
            seeks = DataVisualSeekState()
            // Establish the shared timeline/host-clock relationship once when
            // playing or changing speed. Normal playback decodes continuously;
            // it does not restart a GOP for each UI cursor tick.
            player.setRate(Float(playbackRate),
                           time: CMTime(seconds: playbackPosition, preferredTimescale: 1_000_000),
                           atHostTime: CMTime(seconds: playbackHost, preferredTimescale: 1_000_000_000))
            needsPlaybackAnchor = false
        }
        private func scheduleSeek() {
            guard active, !wantsPlayback, seeks.inFlight == nil, scheduledSeek == nil,
                  seeks.desired != seeks.completed, player.currentItem?.status == .readyToPlay else { return }
            let delay = max(0, lastSeekHost + 1.0 / 30.0 - hostTime)
            scheduledSeek = Task { [weak self] in
                do { try await Task.sleep(for: .seconds(delay)) }
                catch { return }
                guard let self, self.active, !self.wantsPlayback,
                      let target = self.seeks.begin() else { return }
                self.lastSeekHost = self.hostTime
                self.player.pause()
                let finished = await self.player.seek(
                    to: CMTime(seconds: target, preferredTimescale: 1_000_000),
                    toleranceBefore: .zero, toleranceAfter: .zero)
                guard self.active else { return }
                self.seeks.finish(success: finished)
                self.scheduledSeek = nil
                if self.wantsPlayback { self.startPlayback() }
                else if finished && !Task.isCancelled { self.scheduleSeek() }
            }
        }

        func stop() {
            active = false
            scheduledSeek?.cancel(); scheduledSeek = nil
            readyObservation = nil
            player.pause()
            player.currentItem?.cancelPendingSeeks()
            player.replaceCurrentItem(with: nil)
        }
    }
    func makeCoordinator() -> Coordinator { Coordinator(video: video, owner: mediaOwner) }
    func makeUIView(context: Context) -> UIView {
        let host = DataVisualPlayerHost(frame: .zero)
        host.playerLayer.player = context.coordinator.player
        return host
    }
    func updateUIView(_ nsView: UIView, context: Context) {
        guard let frame = DataVisualSearch.latest(video.timestamps, at: cursor, time: { $0 }), let origin = video.timestamps.first else { return }
        context.coordinator.update(position: cursor - origin, frame: frame - origin, playing: playing, speed: speed)
    }
    static func dismantleUIView(_ nsView: UIView, coordinator: Coordinator) { coordinator.stop() }
}

