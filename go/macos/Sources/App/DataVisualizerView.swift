import SwiftUI
import AppKit
import ImageIO
import AVFoundation

struct DataVisualizerView: View {
    let session: UInt64
    let deviceName: String
    var initialEpisode: String? = nil
    @State private var episodes: [DataVisualEpisode] = []
    @State private var episodeLabels: [String: String] = [:]
    @State private var episode = ""
    @State private var snapshot: DataVisualSnapshot?
    @State private var index = DataVisualIndex()
    @State private var refreshID = 0
    @State private var loadID = 0
    @State private var loadingList = false
    @State private var loadingEpisode = false
    @State private var error: String?
    @State private var cursor = 0.0
    @State private var playhead = DataTimelinePlayhead()
    @State private var playing = false
    @State private var speed = 1.0
    @State private var numericTrack = ""
    @State private var imageTrack = ""
    @State private var selectedTrack = ""
    @State private var viewport: DataTimelineViewport?

    private var loading: Bool { loadingList || loadingEpisode }
    var body: some View {
        VStack(spacing: 0) {
            HStack(spacing: 12) {
                Label("Data visualizer", systemImage: "waveform.path.ecg").font(.headline)
                Picker("Episode", selection: $episode) {
                    Text("Choose an episode").tag("")
                    ForEach(episodes) { item in Text(episodeLabels[item.id] ?? item.title).tag(item.id) }
                }.frame(maxWidth: 440).disabled(loadingList)
                Spacer()
                if loading { ProgressView().controlSize(.small) }
                Text(deviceName).font(.callout).foregroundStyle(.secondary).lineLimit(1)
            }.padding(.horizontal, 20).padding(.vertical, 12)
            Divider()
            if let error {
                FailureNote(message: error).padding(16).frame(maxWidth: .infinity, alignment: .leading)
            }
            if let snapshot {
                workspace(snapshot).opacity(loadingEpisode ? 0.45 : 1).disabled(loadingEpisode)
            } else if loading {
                Spacer(); ProgressView("Loading recorded data"); Spacer()
            } else {
                ContentUnavailableView("Explore a recording", systemImage: "waveform.path.ecg", description: Text("Choose a Wendy Data episode to inspect camera tracks, events, and measurements on one timeline."))
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
            }
        }
        .task(id: refreshID) { await refreshEpisodes() }
        .task(id: "\(episode):\(loadID)") { await loadEpisode() }
        .task(id: playing) {
            guard playing, let snapshot else { return }
            var previous = ProcessInfo.processInfo.systemUptime
            while !Task.isCancelled && playing {
                do { try await Task.sleep(for: .nanoseconds(16_666_667)) } catch { return }
                let now = ProcessInfo.processInfo.systemUptime
                cursor = min(snapshot.end, cursor + (now - previous) * speed)
                previous = now
                if let viewport { self.viewport = viewport.revealing(cursor, within: timelineBounds(snapshot)) }
                if cursor >= snapshot.end { playing = false }
            }
        }
        .onDisappear { playing = false }
        .focusedSceneValue(\.devicePaneRefresh, PaneRefreshAction(enabled: !loading, perform: { refreshID += 1 }))
    }

    private func workspace(_ data: DataVisualSnapshot) -> some View {
        VStack(spacing: 0) {
            GeometryReader { geometry in
                HSplitView {
                    VStack(alignment: .leading, spacing: 0) {
                        Text("Channels").font(.headline).padding(14)
                        Divider()
                        List(data.tracks) { track in
                            Button { selectedTrack = track.id } label: {
                                HStack(alignment: .firstTextBaseline, spacing: 8) {
                                    Image(systemName: track.symbol)
                                        .foregroundStyle(Color.accentColor)
                                        .frame(width: 18)
                                    VStack(alignment: .leading, spacing: 4) {
                                        Text(track.name).lineLimit(2)
                                            .multilineTextAlignment(.leading)
                                        Text("\(index.channels[track.id]?.count ?? 0) samples · \(track.kind)")
                                            .font(.caption2).foregroundStyle(.secondary)
                                    }.frame(maxWidth: .infinity, alignment: .leading)
                                }.padding(.vertical, 5).frame(maxWidth: .infinity, alignment: .leading).contentShape(Rectangle())
                            }.buttonStyle(.plain).listRowBackground(selectedTrack == track.id ? Color.accentColor.opacity(0.12) : .clear)
                        }.listStyle(.sidebar)
                    }.frame(minWidth: 160, idealWidth: 210, maxWidth: 260)
                        .frame(height: geometry.size.height)
                    VSplitView {
                        HSplitView {
                            imagePanel(data).frame(minWidth: 220, maxWidth: .infinity, minHeight: 180, maxHeight: .infinity)
                            numericPanel(data).frame(minWidth: 260, maxWidth: .infinity, minHeight: 180, maxHeight: .infinity)
                        }
                        HSplitView {
                            eventsPanel(data).frame(minWidth: 220, maxWidth: .infinity, minHeight: 130, maxHeight: .infinity)
                            inspectorPanel(data).frame(minWidth: 260, maxWidth: .infinity, minHeight: 130, maxHeight: .infinity)
                        }
                    }.frame(minWidth: 500)
                        .frame(height: geometry.size.height)
                }.frame(width: geometry.size.width, height: geometry.size.height)
            }
            Divider()
            timeline(data)
            if !data.warnings.isEmpty {
                DisclosureGroup("Recording notes") {
                    ScrollView { Text(data.warnings).font(.caption).textSelection(.enabled).frame(maxWidth: .infinity, alignment: .leading) }.frame(maxHeight: 100)
                }.font(.caption).foregroundStyle(.secondary).padding(.horizontal, 16).padding(.vertical, 7)
            }
        }.onChange(of: selectedTrack) { _, id in
            if index.numeric[id] != nil { numericTrack = id }
            if index.imageTrackIDs.contains(id) { imageTrack = id }
        }
    }

    private func imagePanel(_ data: DataVisualSnapshot) -> some View {
        let tracks = index.imageTracks
        let sample = index.latest(track: imageTrack, at: cursor)
        let video = data.videos.first { $0.track == imageTrack }
        return VStack(spacing: 0) {
            panelHeader("Camera & images", icon: "photo") {
                if !tracks.isEmpty {
                    Picker("Image source", selection: $imageTrack) { ForEach(tracks) { Text($0.name).tag($0.id) } }.labelsHidden().frame(maxWidth: 220)
                }
            }
            ZStack {
                Color.black.opacity(0.92)
                if let video, let first = video.timestamps.first, let last = video.timestamps.last, cursor >= first && cursor <= last {
                    DataVisualVideoPlayer(video: video, cursor: cursor, playing: playing, speed: speed, mediaOwner: data.mediaFiles).id(video.url)
                } else if video == nil, let sample, let image = index.image(for: sample) {
                    Image(nsImage: image).resizable().scaledToFit().padding(8)
                } else {
                    VStack(spacing: 8) {
                        Image(systemName: "photo").font(.largeTitle)
                        Text(tracks.isEmpty ? "No playable media" : "No frame at this time").font(.callout)
                        if tracks.isEmpty { Text("Playback requires a sealed camera clip with a verified timestamp schedule, or recorded JPEG/PNG frames. See recording notes for unavailable clips.").font(.caption).multilineTextAlignment(.center) }
                    }.foregroundStyle(.white.opacity(0.6)).padding(20)
                }
            }.frame(maxWidth: .infinity, maxHeight: .infinity)
            if let video, let frame = DataVisualSearch.latest(video.timestamps, at: cursor, time: { $0 }), cursor <= (video.timestamps.last ?? frame) {
                Text("Recorded frame at \(timeLabel(frame))").font(.caption2).foregroundStyle(.secondary).padding(6)
            } else if video == nil, let sample {
                Text("Frame at \(timeLabel(sample.seconds)) · \(timeLabel(cursor - sample.seconds)) before cursor").font(.caption2).foregroundStyle(.secondary).padding(6)
            }
        }
    }

    private func numericPanel(_ data: DataVisualSnapshot) -> some View {
        let tracks = index.numericTracks
        let samples = index.numeric[numericTrack] ?? []
        let sample = index.latest(track: numericTrack, at: cursor)
        return VStack(spacing: 0) {
            panelHeader("Plot", icon: "waveform.path") {
                if !tracks.isEmpty {
                    Picker("Channel", selection: $numericTrack) { ForEach(tracks) { Text($0.name).tag($0.id) } }.labelsHidden().frame(maxWidth: 220)
                }
            }
            if samples.isEmpty {
                ContentUnavailableView("No numeric channel", systemImage: "waveform.path", description: Text("Record telemetry or a TimeSeriesBatch stream to plot measurements."))
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
            } else {
                HStack(alignment: .firstTextBaseline) {
                    Text(sample?.value.map { $0.formatted(.number.precision(.fractionLength(0...3))) } ?? "—").font(.title2.monospacedDigit())
                    Text(sample?.unit ?? "").font(.caption).foregroundStyle(.secondary)
                    Spacer()
                    if let sample { Text(timeLabel(sample.seconds)).font(.caption.monospacedDigit()).foregroundStyle(.secondary) }
                }.padding(.horizontal, 16).padding(.top, 8)
                DataVisualPlot(recording: index.id, track: numericTrack, samples: samples, range: index.numericRanges[numericTrack] ?? 0...1, start: visibleWindow(data).start, end: visibleWindow(data).end, cursor: cursor)
                    .overlay { timelineInteraction(data) }
                    .clipped().padding(16)
                Text("Recorded samples only; gaps are not interpolated.").font(.caption2).foregroundStyle(.secondary).padding(.bottom, 8)
            }
        }
    }

    private func eventsPanel(_ data: DataVisualSnapshot) -> some View {
        let events = index.events(through: cursor)
        return VStack(spacing: 0) {
            panelHeader("Events", icon: "text.bubble") { Text("Through cursor").font(.caption).foregroundStyle(.secondary) }
            if events.isEmpty {
                ContentUnavailableView("No events at this time", systemImage: "text.bubble")
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
            } else {
                ScrollViewReader { proxy in
                    List(events.reversed()) { event in
                        Button {
                            selectedTrack = event.track; cursor = event.seconds; playing = false
                            viewport = visibleWindow(data).revealing(event.seconds, within: timelineBounds(data))
                        } label: {
                            DataVisualEventRow(event: event, recording: index.id).equatable()
                                .frame(maxWidth: .infinity, alignment: .leading).contentShape(Rectangle())
                        }.buttonStyle(.plain)
                    }.listStyle(.plain)
                }
            }
        }
    }

    private func inspectorPanel(_ data: DataVisualSnapshot) -> some View {
        let track = data.tracks.first { $0.id == selectedTrack }
        let sample = index.latest(track: selectedTrack, at: cursor)
        return VStack(alignment: .leading, spacing: 0) {
            panelHeader("Inspector", icon: "slider.horizontal.3") { EmptyView() }
            ScrollView {
                VStack(alignment: .leading, spacing: 10) {
                    if let track {
                        Text(track.name).font(.headline)
                        Text(track.detail).font(.caption).foregroundStyle(.secondary)
                        if let sample {
                            LabeledContent("Recorded time", value: timeLabel(sample.seconds))
                            LabeledContent("Age at cursor", value: timeLabel(cursor - sample.seconds))
                            if !sample.label.isEmpty { Text(sample.label).fontWeight(.medium) }
                            if !sample.detail.isEmpty { Text(sample.detail).font(.system(.caption, design: .monospaced)).textSelection(.enabled) }
                        } else { Text("No sample before the cursor.").foregroundStyle(.secondary) }
                    } else { Text("Select a channel or event to inspect its timestamp and metadata.").foregroundStyle(.secondary) }
                }.font(.callout).frame(maxWidth: .infinity, alignment: .leading).padding(16)
            }
        }
    }

    private func timeline(_ data: DataVisualSnapshot) -> some View {
        let window = visibleWindow(data)
        let visibleRows = min(data.tracks.count, 6)
        let trackHeight = CGFloat(max(1, visibleRows) * 24 - 4)
        return VStack(spacing: 5) {
            HStack(spacing: 10) {
                Button {
                    cursor = data.start; playing = false
                    viewport = window.revealing(data.start, within: timelineBounds(data))
                } label: { Image(systemName: "backward.end.fill") }.help("Go to beginning")
                Button {
                    if cursor >= data.end { cursor = data.start }
                    playing.toggle()
                } label: { Image(systemName: playing ? "pause.fill" : "play.fill") }.help(playing ? "Pause" : "Play")
                Picker("Speed", selection: $speed) { Text("½×").tag(0.5); Text("1×").tag(1.0); Text("2×").tag(2.0); Text("4×").tag(4.0) }.labelsHidden().frame(width: 65)
                Text(timeLabel(cursor)).font(.system(.callout, design: .monospaced)).monospacedDigit()
                Spacer()
                Text("Drag to seek · Scroll sideways to pan").font(.caption2).foregroundStyle(.secondary)
                Button { zoomTimeline(data, factor: 0.5) } label: { Image(systemName: "minus.magnifyingglass") }.help("Zoom out")
                Button { zoomTimeline(data, factor: 2) } label: { Image(systemName: "plus.magnifyingglass") }.help("Zoom in")
                Button("Fit") { viewport = timelineBounds(data) }.help("Show the entire episode")
            }
            HStack(spacing: 8) {
                Text("Channels (\(data.tracks.count))").font(.caption2.weight(.medium)).foregroundStyle(.secondary).frame(width: 150, alignment: .leading)
                timelineInteraction(data, ruler: true).frame(height: 30)
                    .clipShape(RoundedRectangle(cornerRadius: 3))
            }
            ScrollView(.vertical) {
                VStack(spacing: 4) {
                    ForEach(data.tracks) { track in
                        HStack(spacing: 8) {
                            Text(track.name).font(.caption2).lineLimit(1).frame(width: 150, alignment: .leading)
                            DataVisualTrackStrip(recording: index.id, track: track.id, samples: index.channels[track.id] ?? [], start: window.start, end: window.end, cursor: cursor)
                                .frame(height: 20).clipped()
                                .overlay(alignment: .leading) {
                                    if (index.channels[track.id] ?? []).isEmpty {
                                        Text("No samples").font(.caption2).foregroundStyle(.tertiary)
                                            .padding(.horizontal, 6).allowsHitTesting(false)
                                    }
                                }
                                .overlay { timelineInteraction(data, track: track.id) }
                        }
                    }
                }
            }.frame(height: trackHeight)
                .scrollIndicators(.visible)
            HStack {
                Text("\(data.clock) · episode time · \(data.samples.count) samples")
                Spacer()
                if data.tracks.count > visibleRows {
                    Text("Scroll vertically for all \(data.tracks.count) channels · Option-drag to pan · Pinch to zoom")
                } else {
                    Text("Option-drag to pan · Pinch to zoom")
                }
            }.help("Command-scroll also zooms the time window.")
                .font(.caption2).foregroundStyle(.secondary)
        }.padding(.horizontal, 16).padding(.vertical, 8)
    }

    private func timelineBounds(_ data: DataVisualSnapshot) -> DataTimelineViewport {
        .fitted(start: data.start, end: data.end)
    }
    private func visibleWindow(_ data: DataVisualSnapshot) -> DataTimelineViewport {
        (viewport ?? timelineBounds(data)).clamped(to: timelineBounds(data))
    }
    private func timelineInteraction(_ data: DataVisualSnapshot, ruler: Bool = false, track: String? = nil) -> some View {
        DataTimelineInteraction(window: visibleWindow(data), cursor: cursor, ruler: ruler, playhead: playhead, seek: { time in
            playing = false; cursor = min(max(time, data.start), data.end)
            if let track { selectedTrack = track }
        }, pan: { seconds in
            playing = false
            viewport = visibleWindow(data).panned(by: seconds, within: timelineBounds(data))
        }, zoom: { factor, anchor in
            playing = false
            viewport = visibleWindow(data).zoomed(by: factor, anchor: anchor, within: timelineBounds(data))
        })
    }
    private func zoomTimeline(_ data: DataVisualSnapshot, factor: Double) {
        let window = visibleWindow(data)
        let anchor = cursor >= window.start && cursor <= window.end ? (cursor - window.start) / window.span : 0.5
        viewport = window.zoomed(by: factor, anchor: anchor, within: timelineBounds(data))
    }

    private func panelHeader<T: View>(_ name: String, icon: String, @ViewBuilder trailing: () -> T) -> some View {
        HStack { Label(name, systemImage: icon).font(.callout.weight(.medium)); Spacer(minLength: 6); trailing() }
            .padding(.horizontal, 12).frame(height: 38).background(.quaternary.opacity(0.4))
    }
    private func refreshEpisodes() async {
        loadingList = true; error = nil; defer { loadingList = false }
        do {
            let items = try await GoClient.shared.visualEpisodes(session: session)
            guard !Task.isCancelled else { return }
            episodes = items
            episodeLabels = Dictionary(uniqueKeysWithValues: items.map { ($0.id, "\($0.title) · \($0.started.formatted(date: .abbreviated, time: .shortened))") })
            if episode.isEmpty, let initialEpisode {
                guard items.contains(where: { $0.id == initialEpisode }) else {
                    error = "This clip is no longer stored on the device. Choose another recording."
                    return
                }
                episode = initialEpisode
                return
            }
            if !items.contains(where: { $0.id == episode }) { episode = items.first?.id ?? "" } else { loadID += 1 }
        } catch { if !Task.isCancelled { self.error = error.localizedDescription } }
    }
    private func loadEpisode() async {
        guard !episode.isEmpty else { snapshot = nil; return }
        loadingEpisode = true; error = nil; playing = false; defer { loadingEpisode = false }
        do {
            let value = try await GoClient.shared.visualEpisode(session: session, episode: episode)
            guard !Task.isCancelled else { return }
            snapshot = value
            index = DataVisualIndex(value)
            cursor = value.videos.first?.timestamps.first ?? value.start
            viewport = timelineBounds(value)
            numericTrack = value.samples.first(where: { $0.value != nil })?.track ?? ""
            imageTrack = value.videos.first?.track ?? value.samples.first(where: { $0.image != nil })?.track ?? ""
            selectedTrack = value.tracks.first?.id ?? ""
        } catch { if !Task.isCancelled { snapshot = nil; self.error = error.localizedDescription } }
    }
}

// Build once per recording. Cursor movement performs binary searches rather
// than filtering the full recording once for every visible channel and panel.
final class DataVisualIndex {
    let id = UUID()
    var channels: [String: [DataVisualSample]] = [:]
    var numeric: [String: [DataVisualSample]] = [:]
    var numericRanges: [String: ClosedRange<Double>] = [:]
    var imageTrackIDs: Set<String> = []
    var imageTracks: [DataVisualTrack] = []
    var numericTracks: [DataVisualTrack] = []
    var populatedTracks: [DataVisualTrack] = []
    private var eventSamples: [DataVisualSample] = []
    private let images = NSCache<NSNumber, NSImage>()
    init(_ data: DataVisualSnapshot? = nil) {
        images.countLimit = 8
        guard let data else { return }
        for sample in data.samples {
            channels[sample.track, default: []].append(sample)
            if let value = sample.value, value.isFinite {
                numeric[sample.track, default: []].append(sample)
                let previous = numericRanges[sample.track] ?? value...value
                numericRanges[sample.track] = min(previous.lowerBound, value)...max(previous.upperBound, value)
            }
            if sample.image != nil { imageTrackIDs.insert(sample.track) }
            if sample.value == nil && sample.image == nil && !sample.label.hasPrefix("Frame ") { eventSamples.append(sample) }
        }
        imageTrackIDs.formUnion(data.videos.map(\.track))
        imageTracks = data.tracks.filter { imageTrackIDs.contains($0.id) }
        numericTracks = data.tracks.filter { numeric[$0.id] != nil }
        populatedTracks = data.tracks.filter { channels[$0.id] != nil }
    }
    func latest(track: String, at cursor: Double) -> DataVisualSample? {
        DataVisualSearch.latest(channels[track] ?? [], at: cursor, time: { $0.seconds })
    }
    func events(through cursor: Double) -> ArraySlice<DataVisualSample> {
        let end = DataVisualSearch.upperBound(eventSamples, at: cursor, time: { $0.seconds })
        return eventSamples[max(0, end - 80)..<end]
    }
    func image(for sample: DataVisualSample) -> NSImage? {
        let key = NSNumber(value: sample.id)
        if let image = images.object(forKey: key) { return image }
        guard let bytes = sample.image, let image = visualImage(bytes) else { return nil }
        images.setObject(image, forKey: key)
        return image
    }
}

private struct DataVisualEventRow: View, Equatable {
    let event: DataVisualSample
    let recording: UUID
    static func == (lhs: Self, rhs: Self) -> Bool { lhs.recording == rhs.recording && lhs.event.id == rhs.event.id }
    var body: some View {
        HStack(alignment: .top) {
            Text(timeLabel(event.seconds)).font(.caption.monospacedDigit()).foregroundStyle(.secondary).frame(width: 80, alignment: .leading)
            VStack(alignment: .leading, spacing: 3) {
                Text(event.label.isEmpty ? event.track : event.label).font(.callout)
                Text(event.track).font(.caption2).foregroundStyle(.secondary).lineLimit(1)
            }
        }
    }
}

private func timeLabel(_ seconds: Double) -> String { String(format: "%+.3f s", seconds) }

private func visualImage(_ bytes: Data) -> NSImage? {
    guard let source = CGImageSourceCreateWithData(bytes as CFData, nil),
          let image = CGImageSourceCreateThumbnailAtIndex(source, 0, [
            kCGImageSourceCreateThumbnailFromImageAlways: true,
            kCGImageSourceThumbnailMaxPixelSize: 2048,
            kCGImageSourceCreateThumbnailWithTransform: true
          ] as CFDictionary) else { return nil }
    return NSImage(cgImage: image, size: .zero)
}

private final class DataVisualPlayerHost: NSView {
    let playerLayer = AVPlayerLayer()
    override init(frame: NSRect) {
        super.init(frame: frame)
        wantsLayer = true
        playerLayer.videoGravity = .resizeAspect
        layer?.addSublayer(playerLayer)
    }
    required init?(coder: NSCoder) { fatalError("init(coder:) has not been implemented") }
    override func layout() { super.layout(); playerLayer.frame = bounds }
}

// Backpressure for exact seeks: one decode in flight, one replaceable target.
// Finishing a scrub always lands on its final recorded frame.
private struct DataVisualVideoPlayer: NSViewRepresentable {
    let video: DataVisualVideo
    let cursor: Double
    let playing: Bool
    let speed: Double
    let mediaOwner: DataVisualMediaFiles
    final class Coordinator {
        let player: AVPlayer
        let owner: DataVisualMediaFiles
        private var seeks = DataVisualSeekState()
        private var readyObservation: NSKeyValueObservation?
        private var scheduledSeek: DispatchWorkItem?
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
                DispatchQueue.main.async {
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
                scheduledSeek?.cancel(); scheduledSeek = nil
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
            let work = DispatchWorkItem { [weak self] in
                guard let self else { return }
                self.scheduledSeek = nil
                guard self.active, !self.wantsPlayback, let target = self.seeks.begin() else { return }
                self.lastSeekHost = self.hostTime
                self.player.pause()
                self.player.seek(to: CMTime(seconds: target, preferredTimescale: 1_000_000), toleranceBefore: .zero, toleranceAfter: .zero) { [weak self] finished in
                    DispatchQueue.main.async {
                        guard let self, self.active else { return }
                        self.seeks.finish(success: finished)
                        if self.wantsPlayback { self.startPlayback() }
                        else if finished { self.scheduleSeek() }
                    }
                }
            }
            scheduledSeek = work
            DispatchQueue.main.asyncAfter(deadline: .now() + delay, execute: work)
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
    func makeNSView(context: Context) -> NSView {
        let host = DataVisualPlayerHost(frame: .zero)
        host.playerLayer.player = context.coordinator.player
        return host
    }
    func updateNSView(_ nsView: NSView, context: Context) {
        guard let frame = DataVisualSearch.latest(video.timestamps, at: cursor, time: { $0 }), let origin = video.timestamps.first else { return }
        context.coordinator.update(position: cursor - origin, frame: frame - origin, playing: playing, speed: speed)
    }
    static func dismantleNSView(_ nsView: NSView, coordinator: Coordinator) { coordinator.stop() }
}

private struct DataVisualPlot: View {
    let recording: UUID
    let track: String
    let samples: [DataVisualSample]
    let range: ClosedRange<Double>
    let start, end, cursor: Double
    var body: some View {
        DataVisualPlotMarks(recording: recording, track: track, samples: samples, range: range, start: start, end: end)
            .equatable()
            .accessibilityLabel("Numeric samples aligned to the shared timeline")
    }
}

// Cursor-independent canvas: SwiftUI retains the rendered marks between ticks.
private struct DataVisualPlotMarks: View, Equatable {
    let recording: UUID
    let track: String
    let samples: [DataVisualSample]
    let range: ClosedRange<Double>
    let start, end: Double
    static func == (lhs: Self, rhs: Self) -> Bool {
        lhs.recording == rhs.recording && lhs.track == rhs.track && lhs.start == rhs.start && lhs.end == rhs.end
    }
    var body: some View {
        Canvas { context, size in
            let low = range.lowerBound
            let high = max(range.upperBound, low + 0.001)
            let span = max(end - start, 0.001)
            for row in 0...4 {
                let y = CGFloat(row) * size.height / 4
                var line = Path(); line.move(to: CGPoint(x: 0, y: y)); line.addLine(to: CGPoint(x: size.width, y: y))
                context.stroke(line, with: .color(.secondary.opacity(0.14)))
            }
            var marks = Path()
            for sample in samples {
                guard sample.seconds >= start, sample.seconds <= end, let value = sample.value else { continue }
                let x = (sample.seconds - start) / span * size.width
                let y = size.height - (value - low) / (high - low) * size.height
                marks.addEllipse(in: CGRect(x: x - 1.8, y: y - 1.8, width: 3.6, height: 3.6))
            }
            context.fill(marks, with: .color(.accentColor.opacity(0.85)))
        }
    }
}

private struct DataVisualTrackStrip: View {
    let recording: UUID
    let track: String
    let samples: [DataVisualSample]
    let start, end, cursor: Double
    var body: some View {
        DataVisualTrackMarks(recording: recording, track: track, samples: samples, start: start, end: end)
            .equatable()
    }
}

private struct DataVisualTrackMarks: View, Equatable {
    let recording: UUID
    let track: String
    let samples: [DataVisualSample]
    let start, end: Double
    static func == (lhs: Self, rhs: Self) -> Bool {
        lhs.recording == rhs.recording && lhs.track == rhs.track && lhs.start == rhs.start && lhs.end == rhs.end
    }
    var body: some View {
        Canvas { context, size in
            let span = max(end - start, 0.001)
            context.fill(Path(roundedRect: CGRect(origin: .zero, size: size), cornerRadius: 3), with: .color(.secondary.opacity(0.08)))
            var events = Path(), numeric = Path()
            for sample in samples {
                guard sample.seconds >= start, sample.seconds <= end else { continue }
                let x = (sample.seconds - start) / span * size.width
                let rect = CGRect(x: x, y: 3, width: 2, height: size.height - 6)
                if sample.value == nil { events.addRect(rect) } else { numeric.addRect(rect) }
            }
            context.fill(events, with: .color(.purple.opacity(0.6)))
            context.fill(numeric, with: .color(.accentColor.opacity(0.6)))
        }
    }
}

