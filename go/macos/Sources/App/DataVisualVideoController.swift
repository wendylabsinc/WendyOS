import SwiftUI
import AVFoundation

/// One view-owned loop serializes readiness, playback changes and exact seeks.
@MainActor
final class DataVisualVideoController: ObservableObject {
    let player: AVPlayer
    let owner: DataVisualMediaFiles
    private var seeks = DataVisualSeekState()
    private var readyObservation: NSKeyValueObservation?
    private var changes = AsyncStream<Void>.makeStream(bufferingPolicy: .bufferingNewest(1))
    private let url: URL
    private var frame = 0.0
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
        url = video.url
        player = AVPlayer(url: video.url)
        player.isMuted = true
        player.actionAtItemEnd = .pause
        player.automaticallyWaitsToMinimizeStalling = false
        observeReadiness()
    }

    private func observeReadiness() {
        let continuation = changes.continuation
        readyObservation = player.currentItem?.observe(\.status, options: [.initial, .new]) { _, _ in
            continuation.yield(())
        }
    }

    func update(position: Double, frame: Double, playing: Bool, speed: Double) {
        self.frame = frame
        let changed = wantsPlayback != playing || playbackRate != speed
        wantsPlayback = playing
        playbackRate = speed
        playbackPosition = position
        playbackHost = hostTime
        if playing {
            if changed { needsPlaybackAnchor = true }
        } else {
            if changed { player.pause() }
            needsPlaybackAnchor = true
            seeks.request(frame)
        }
        changes.continuation.yield(())
    }

    func run() async {
        if !active {
            active = true
            changes = AsyncStream<Void>.makeStream(bufferingPolicy: .bufferingNewest(1))
            player.replaceCurrentItem(with: AVPlayerItem(url: url))
            seeks = DataVisualSeekState()
            seeks.request(frame)
            needsPlaybackAnchor = true
            observeReadiness()
        }
        defer { stop() }
        for await _ in changes.stream {
            guard active, !Task.isCancelled else { return }
            guard player.currentItem?.status == .readyToPlay else { continue }
            if wantsPlayback { startPlayback(); continue }
            guard seeks.desired != seeks.completed else { continue }
            do { try await Task.sleep(for: .seconds(max(0, lastSeekHost + 1.0 / 30.0 - hostTime))) }
            catch { return }
            guard active, !wantsPlayback, let target = seeks.begin() else { continue }
            lastSeekHost = hostTime
            player.pause()
            let finished = await player.seek(to: CMTime(seconds: target, preferredTimescale: 1_000_000),
                                             toleranceBefore: .zero, toleranceAfter: .zero)
            seeks.finish(success: finished)
            if wantsPlayback || (finished && seeks.desired != seeks.completed) {
                changes.continuation.yield(())
            }
        }
    }

    private func startPlayback() {
        guard active, needsPlaybackAnchor, seeks.inFlight == nil else { return }
        seeks = DataVisualSeekState()
        player.setRate(Float(playbackRate),
                       time: CMTime(seconds: playbackPosition, preferredTimescale: 1_000_000),
                       atHostTime: CMTime(seconds: playbackHost, preferredTimescale: 1_000_000_000))
        needsPlaybackAnchor = false
    }

    func stop() {
        active = false
        changes.continuation.finish()
        readyObservation = nil
        player.pause()
        player.currentItem?.cancelPendingSeeks()
        player.replaceCurrentItem(with: nil)
    }
}

struct DataVisualVideoPlayer: View {
    let video: DataVisualVideo
    let cursor: Double
    let playing: Bool
    let speed: Double
    @StateObject private var controller: DataVisualVideoController

    init(video: DataVisualVideo, cursor: Double, playing: Bool, speed: Double, mediaOwner: DataVisualMediaFiles) {
        self.video = video; self.cursor = cursor; self.playing = playing; self.speed = speed
        _controller = StateObject(wrappedValue: DataVisualVideoController(video: video, owner: mediaOwner))
    }

    var body: some View {
        DataVisualNativeVideoPlayer(video: video, cursor: cursor, playing: playing, speed: speed, controller: controller)
            .task { await controller.run() }
    }
}
