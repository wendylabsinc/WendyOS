import SwiftUI
import Charts

struct DataVisualizerView: View {
    @Environment(\.recordingPosters) private var posters
    @State private var actions = AsyncActions()
    let session: UInt64
    let deviceName: String
    var initialEpisode: String?
    var recordingDevice: Device?
    var recordingKind: MobileRecordingKind?
    var startPlaying = false
    @State private var episodes: [DataVisualEpisode] = []
    @State private var episode = ""
    @State private var data: DataVisualSnapshot?
    @State private var track = ""
    @State private var showingChannels = false
    @State private var cursor = 0.0
    @State private var loading = false
    @State private var error: String?
    @State private var generation = 0
    @State private var playing = false
    @State private var didStartPlayback = false
    @State private var viewport = DataTimelineViewport(start: 0, end: 1)
    @State private var timestamps: [String: [Double]] = [:]
    var body: some View {
        actionContent.task { await actions.run() }
    }

    @ViewBuilder private var actionContent: some View {
        VStack(spacing: 0) {
            if initialEpisode == nil && !episodes.isEmpty {
                Picker("Episode", selection: $episode) {
                    ForEach(episodes) { Text($0.title + " · " + $0.started.formatted(date: .abbreviated, time: .shortened)).tag($0.id) }
                }.padding().pickerStyle(.menu)
            }
            if loading && data == nil { ProgressView("Loading recording…").frame(maxWidth: .infinity, maxHeight: .infinity) }
            else if let error, data == nil { FailureView(title: "Could not load recording", error: error) { actions.submit { await refresh() } } }
            else if let data {
                ScrollView {
                    VStack(alignment: .leading, spacing: 16) {
                        if loading { ProgressView("Loading playback…").accessibilityIdentifier("recording.loadingPlayback") }
                        if let error {
                            Text(PresentedFailure(error).message).font(.footnote).foregroundStyle(.secondary)
                            Button("Retry playback") { actions.submit { await load() } }
                        }
                        Button {
                            playing = false
                            showingChannels = true
                        } label: {
                            HStack(spacing: 12) {
                                Image(systemName: data.tracks.first { $0.id == track }?.symbol ?? "waveform")
                                    .font(.title2)
                                VStack(alignment: .leading, spacing: 4) {
                                    Text("Channel").font(.caption).foregroundStyle(.secondary)
                                    Text(data.tracks.first { $0.id == track }?.name ?? "Choose a channel")
                                        .font(.subheadline.weight(.semibold)).lineLimit(2).truncationMode(.middle)
                                }
                                Spacer(minLength: 0)
                                Image(systemName: "chevron.up.chevron.down").font(.caption.weight(.semibold))
                            }.padding(16).frame(maxWidth: .infinity, alignment: .leading)
                                .background(MobileLensStyle.card, in: RoundedRectangle(cornerRadius: 18))
                                .contentShape(Rectangle())
                        }.buttonStyle(.plain).accessibilityIdentifier("recording.channels")
                        if let video = data.videos.first(where: { $0.track == track }) {
                            if video.frame(at: cursor) != nil {
                                DataVisualVideoPlayer(video: video, cursor: cursor, playing: playing, speed: 1, mediaOwner: data.mediaFiles)
                                    .aspectRatio(4 / 3, contentMode: .fit).background(.black).clipShape(RoundedRectangle(cornerRadius: 12))
                            } else { ContentUnavailableView("No frame at this time", systemImage: "video") }
                        } else if let bytes = data.latest(track: track, at: cursor)?.image, let image = UIImage(data: bytes) {
                            Image(uiImage: image).resizable().scaledToFit()
                        }
                        if let segment = data.audio.first(where: { $0.track == track && $0.start <= cursor && cursor < $0.end }) {
                            MobileRecordingAudio(segment: segment, owner: data.mediaFiles, cursor: cursor, playing: playing)
                                .id(segment.url).frame(height: 20)
                        }
                        let numeric = data.samples.filter { $0.track == track && $0.value != nil }
                        if !numeric.isEmpty {
                            Chart {
                                ForEach(numeric) { sample in
                                    LineMark(x: .value("Time", sample.seconds), y: .value("Value", sample.value!))
                                }
                                RuleMark(x: .value("Cursor", cursor)).foregroundStyle(.orange)
                            }.frame(height: 180)
                        }
                        if let sample = data.latest(track: track, at: cursor) {
                            Text(sample.label).font(.headline)
                            Text(String(format: "Recorded at %+.3f s", sample.seconds)).font(.caption).foregroundStyle(.secondary)
                            DisclosureGroup("Sample details") {
                                Text(sample.detail).font(.caption.monospaced()).textSelection(.enabled)
                                    .frame(maxWidth: .infinity, alignment: .leading).padding(.top, 8)
                            }.font(.subheadline)
                                .padding(16).background(MobileLensStyle.card, in: RoundedRectangle(cornerRadius: 16))
                        } else { Text("No sample before the cursor").foregroundStyle(.secondary) }
                        if !data.warnings.isEmpty { Text(data.warnings).font(.caption).foregroundStyle(.secondary) }
                    }.padding()
                }.refreshable { playing = false; await refresh() }
                Divider()
                MobileTimeline(data: data, timestamps: timestamps, selectedTrack: $track, cursor: $cursor, playing: $playing, viewport: $viewport)
            } else { ContentUnavailableView("No recordings", systemImage: "film.stack", description: Text("Campaign recordings will appear here."))
                .mobileRefreshablePlaceholder { await refresh() } }
        }
        .background(MobileLensStyle.background)
        .sheet(isPresented: $showingChannels) {
            if let data {
                MobileRecordingChannels(tracks: data.tracks, selection: $track)
                    .presentationDetents([.large])
                    .presentationDragIndicator(.visible)
            }
        }
        .task { await refresh() }
        .task(id: playing) {
            guard playing, let data else { return }
            let started = ContinuousClock.now
            let origin = cursor
            while !Task.isCancelled {
                let elapsed = started.duration(to: .now).components
                let seconds = Double(elapsed.seconds) + Double(elapsed.attoseconds) / 1e18
                cursor = min(data.end, origin + seconds)
                viewport = viewport.revealing(cursor, within: .fitted(start: data.start, end: data.end))
                if cursor >= data.end { playing = false; return }
                do { try await Task.sleep(for: .milliseconds(33)) } catch { return }
            }
        }
        .onDisappear { playing = false }
        .task(id: data.map { ObjectIdentifier($0.mediaFiles) }) {
            if let data, let recordingDevice, let initialEpisode {
                await posters?.remember(data, device: recordingDevice, episode: initialEpisode)
            }
        }
        .task(id: episode) { if !episode.isEmpty { await load() } }
        .focusedSceneValue(\.devicePaneRefresh, PaneRefreshAction(enabled: !loading, perform: { actions.submit { await refresh() } }))
    }
    private func refresh() async {
        if recordingDevice != nil, let initialEpisode {
            if episode == initialEpisode { await load() }
            else { episode = initialEpisode }
            return
        }
        do {
            episodes = try await GoClient.shared.visualEpisodes(session: session)
            if let initialEpisode { episodes = episodes.filter { $0.id == initialEpisode } }
            let selected = initialEpisode ?? episode
            let previous = episode
            episode = episodes.contains(where: { $0.id == selected }) ? selected : episodes.first?.id ?? ""
            error = nil
            if episode.isEmpty { data = nil }
            else if episode == previous { await load() }
        } catch { self.error = error.localizedDescription }
    }
    private func show(_ snapshot: DataVisualSnapshot, playback: Bool) {
        data = snapshot; cursor = snapshot.start
        viewport = .fitted(start: snapshot.start, end: snapshot.end)
        timestamps = Dictionary(grouping: snapshot.samples, by: \.track).mapValues { $0.map(\.seconds).sorted() }
        track = snapshot.videos.first?.track ?? snapshot.audio.first?.track ?? snapshot.tracks.first?.id ?? ""
        if playback && startPlaying && !didStartPlayback {
            didStartPlayback = true
            playing = snapshot.end > snapshot.start
        }
    }
    private func load() async {
        generation += 1; let request = generation
        let selectedEpisode = episode
        loading = true; error = nil; playing = false; data = nil
        do {
            let snapshot: DataVisualSnapshot
            if let recordingDevice {
                let playbackMode = MobileRecordingLoadMode.playback(for: recordingKind)
                snapshot = try await withThrowingTaskGroup(of: (Bool, DataVisualSnapshot?).self) { group in
                    group.addTask {
                        (true, try await GoClient.shared.visualEpisode(device: recordingDevice, episode: selectedEpisode, mode: playbackMode))
                    }
                    group.addTask {
                        (false, try? await GoClient.shared.visualEpisode(device: recordingDevice, episode: selectedEpisode, mode: .summary))
                    }
                    defer { group.cancelAll() }
                    for try await (isPlayback, result) in group {
                        guard let result else { continue }
                        if isPlayback { return result }
                        if request == generation, !Task.isCancelled, !result.tracks.isEmpty {
                            show(result, playback: false)
                        }
                    }
                    throw CancellationError()
                }
            } else {
                snapshot = try await GoClient.shared.visualEpisode(session: session, episode: selectedEpisode)
            }
            guard request == generation, !Task.isCancelled else { return }
            show(snapshot, playback: true)
        } catch { if request == generation, !Task.isCancelled { self.error = error.localizedDescription } }
        if request == generation { loading = false }
    }

}
private struct MobileTimeline: View {
    let data: DataVisualSnapshot
    let timestamps: [String: [Double]]
    @Binding var selectedTrack: String
    @Binding var cursor: Double
    @Binding var playing: Bool
    @Binding var viewport: DataTimelineViewport
    @State private var panOrigin: DataTimelineViewport?
    @State private var zoomOrigin: DataTimelineViewport?
    private var bounds: DataTimelineViewport { .fitted(start: data.start, end: data.end) }
    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack {
                Button {
                    if cursor >= data.end { cursor = data.start }
                    playing.toggle()
                } label: { Image(systemName: playing ? "pause.fill" : "play.fill") }
                    .accessibilityLabel(playing ? "Pause recording" : "Play recording")
                Text(String(format: "%+.3f s", cursor)).monospacedDigit()
                Spacer()
                Button { viewport = viewport.zoomed(by: 0.5, anchor: 0.5, within: bounds) } label: { Image(systemName: "minus.magnifyingglass") }.accessibilityLabel("Zoom out")
                Button { viewport = viewport.zoomed(by: 2, anchor: 0.5, within: bounds) } label: { Image(systemName: "plus.magnifyingglass") }.accessibilityLabel("Zoom in")
                Button("Fit") { viewport = bounds }
            }.font(.caption).buttonStyle(.bordered).controlSize(.small)
            GeometryReader { geometry in
                HStack {
                    Text(String(format: "%+.2f s", viewport.start))
                    Spacer()
                    Text("Drag ruler to pan")
                    Spacer()
                    Text(String(format: "%+.2f s", viewport.end))
                }.font(.caption2).foregroundStyle(.secondary).frame(maxHeight: .infinity)
                    .contentShape(Rectangle())
                    .gesture(DragGesture(minimumDistance: 3).onChanged { value in
                        if panOrigin == nil { panOrigin = viewport }
                        if let panOrigin { viewport = panOrigin.panned(by: -value.translation.width / max(geometry.size.width, 1) * panOrigin.span, within: bounds) }
                    }.onEnded { _ in panOrigin = nil })
            }.frame(height: 28)
            ScrollView(.vertical) {
                VStack(spacing: 6) {
                    ForEach(data.tracks) { track in
                        HStack {
                            Button {
                                playing = false
                                selectedTrack = track.id
                            } label: {
                                Text(track.name).font(.caption2.weight(selectedTrack == track.id ? .semibold : .regular))
                                    .lineLimit(2).truncationMode(.middle)
                                    .frame(width: 110, alignment: .leading).frame(minHeight: 44)
                                    .contentShape(Rectangle())
                            }.buttonStyle(.plain)
                                .foregroundStyle(selectedTrack == track.id ? MobileLensStyle.accent : .primary)
                                .accessibilityLabel("Select \(track.name)")
                                .accessibilityAddTraits(selectedTrack == track.id ? .isSelected : [])
                            GeometryReader { geometry in
                                Canvas { context, size in
                                    let values = timestamps[track.id] ?? []
                                    let start = DataVisualSearch.upperBound(values, at: viewport.start - 0.000_001, time: { $0 })
                                    let end = DataVisualSearch.upperBound(values, at: viewport.end, time: { $0 })
                                    var lastPixel = -2
                                    for time in values[start..<end] {
                                        let x = (time - viewport.start) / viewport.span * size.width
                                        let pixel = Int(x)
                                        guard pixel != lastPixel else { continue }
                                        lastPixel = pixel
                                        context.fill(Path(CGRect(x: x, y: 3, width: 1.5, height: size.height - 6)), with: .color(selectedTrack == track.id ? MobileLensStyle.accent : .secondary.opacity(0.45)))
                                    }
                                    if cursor >= viewport.start && cursor <= viewport.end {
                                        let x = (cursor - viewport.start) / viewport.span * size.width
                                        context.fill(Path(CGRect(x: x, y: 0, width: 2, height: size.height)), with: .color(.orange))
                                    }
                                }
                                .background(.quaternary, in: RoundedRectangle(cornerRadius: 4))
                                .contentShape(Rectangle())
                                .gesture(DragGesture(minimumDistance: 0).onChanged { value in
                                    playing = false
                                    selectedTrack = track.id
                                    cursor = viewport.time(at: value.location.x / max(geometry.size.width, 1))
                                })
                                .accessibilityLabel("\(track.name) timeline")
                                .accessibilityValue(String(format: "%.3f seconds", cursor))
                                .accessibilityAdjustableAction { direction in
                                    playing = false
                                    cursor = min(max(cursor + (direction == .increment ? 0.1 : -0.1), data.start), data.end)
                                    viewport = viewport.revealing(cursor, within: bounds)
                                }
                            }.frame(height: 24)
                        }
                    }
                }
            }.frame(height: min(CGFloat(data.tracks.count) * 50, 150)).scrollIndicators(.visible)
            Text("\(data.tracks.count) channels · Drag tracks to seek · Pinch to zoom").font(.caption2).foregroundStyle(.secondary)
        }.padding(12)
            .simultaneousGesture(MagnifyGesture().onChanged { value in
                if zoomOrigin == nil { zoomOrigin = viewport }
                if let zoomOrigin { viewport = zoomOrigin.zoomed(by: value.magnification, anchor: 0.5, within: bounds) }
            }.onEnded { _ in zoomOrigin = nil })
    }
}

private struct MobileRecordingChannels: View {
    let tracks: [DataVisualTrack]
    @Binding var selection: String
    @Environment(\.dismiss) private var dismiss
    @State private var search = ""

    private var filtered: [DataVisualTrack] {
        tracks.filter { search.isEmpty || $0.name.localizedCaseInsensitiveContains(search) || $0.kind.localizedCaseInsensitiveContains(search) }
    }

    var body: some View {
        NavigationStack {
            List(filtered) { track in
                Button {
                    selection = track.id
                    dismiss()
                } label: {
                    HStack(spacing: 12) {
                        Image(systemName: track.symbol).frame(width: 24).foregroundStyle(MobileLensStyle.accent)
                        VStack(alignment: .leading, spacing: 4) {
                            Text(track.name).font(.subheadline.weight(.medium)).fixedSize(horizontal: false, vertical: true)
                            Text(track.kind.capitalized).font(.caption).foregroundStyle(.secondary)
                        }
                        Spacer(minLength: 4)
                        if selection == track.id {
                            Image(systemName: "checkmark.circle.fill").foregroundStyle(MobileLensStyle.accent)
                        }
                    }.frame(minHeight: 44).contentShape(Rectangle())
                }.buttonStyle(.plain)
                    .accessibilityAddTraits(selection == track.id ? .isSelected : [])
            }
            .overlay { if filtered.isEmpty { ContentUnavailableView.search(text: search) } }
            .searchable(text: $search, prompt: "Find a channel")
            .navigationTitle("Channels")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar { ToolbarItem(placement: .confirmationAction) { Button("Done") { dismiss() } } }
        }.tint(MobileLensStyle.accent)
    }
}
