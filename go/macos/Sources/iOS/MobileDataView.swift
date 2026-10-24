import SwiftUI
import Charts

struct DataVisualizerView: View {
    let session: UInt64
    let deviceName: String
    var initialEpisode: String?
    @State private var episodes: [DataVisualEpisode] = []
    @State private var episode = ""
    @State private var data: DataVisualSnapshot?
    @State private var track = ""
    @State private var cursor = 0.0
    @State private var loading = false
    @State private var error: String?
    @State private var generation = 0
    @State private var playing = false
    @State private var viewport = DataTimelineViewport(start: 0, end: 1)
    @State private var timestamps: [String: [Double]] = [:]
    var body: some View {
        VStack(spacing: 0) {
            if initialEpisode == nil && !episodes.isEmpty {
                Picker("Episode", selection: $episode) {
                    ForEach(episodes) { Text($0.title + " · " + $0.started.formatted(date: .abbreviated, time: .shortened)).tag($0.id) }
                }.padding().pickerStyle(.menu)
            }
            if loading { ProgressView("Loading recording…").frame(maxWidth: .infinity, maxHeight: .infinity) }
            else if let error { FailureView(title: "Could not load recording", error: error) { Task { await load() } } }
            else if let data {
                ScrollView {
                    VStack(alignment: .leading, spacing: 16) {
                        Picker("Channel", selection: $track) { ForEach(data.tracks) { Text($0.name).tag($0.id) } }.pickerStyle(.menu)
                        if let video = data.videos.first(where: { $0.track == track }) {
                            if video.frame(at: cursor) != nil {
                                DataVisualVideoPlayer(video: video, cursor: cursor, playing: playing, speed: 1, mediaOwner: data.mediaFiles)
                                    .aspectRatio(4 / 3, contentMode: .fit).background(.black).clipShape(RoundedRectangle(cornerRadius: 12))
                            } else { ContentUnavailableView("No frame at this time", systemImage: "video") }
                        } else if let bytes = data.latest(track: track, at: cursor)?.image, let image = UIImage(data: bytes) {
                            Image(uiImage: image).resizable().scaledToFit()
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
                            Text(sample.detail).font(.caption.monospaced()).textSelection(.enabled)
                        } else { Text("No sample before the cursor").foregroundStyle(.secondary) }
                        if !data.warnings.isEmpty { Text(data.warnings).font(.caption).foregroundStyle(.secondary) }
                    }.padding()
                }.refreshable { playing = false; await refresh() }
                Divider()
                MobileTimeline(data: data, timestamps: timestamps, cursor: $cursor, playing: $playing, viewport: $viewport)
            } else { ContentUnavailableView("No recordings", systemImage: "film.stack", description: Text("Campaign recordings will appear here."))
                .mobileRefreshablePlaceholder { await refresh() } }
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
        .task(id: episode) { if !episode.isEmpty { await load() } }
        .focusedSceneValue(\.devicePaneRefresh, PaneRefreshAction(enabled: !loading, perform: { Task { await refresh() } }))
    }
    private func refresh() async {
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
    private func load() async {
        generation += 1; let request = generation
        loading = true; error = nil; playing = false
        do {
            let snapshot = try await GoClient.shared.visualEpisode(session: session, episode: episode)
            guard request == generation, !Task.isCancelled else { return }
            data = snapshot; cursor = snapshot.start
            viewport = .fitted(start: snapshot.start, end: snapshot.end)
            timestamps = Dictionary(grouping: snapshot.samples, by: \.track).mapValues { $0.map(\.seconds).sorted() }
            track = snapshot.videos.first?.track ?? snapshot.tracks.first?.id ?? ""
        } catch { if request == generation { self.error = error.localizedDescription } }
        if request == generation { loading = false }
    }
}
private struct MobileTimeline: View {
    let data: DataVisualSnapshot
    let timestamps: [String: [Double]]
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
                            Text(track.name).font(.caption2).lineLimit(1).frame(width: 110, alignment: .leading)
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
                                        context.fill(Path(CGRect(x: x, y: 3, width: 1.5, height: size.height - 6)), with: .color(.purple))
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
            }.frame(height: min(CGFloat(data.tracks.count) * 30, 150)).scrollIndicators(.visible)
            Text("\(data.tracks.count) channels · Drag tracks to seek · Pinch to zoom").font(.caption2).foregroundStyle(.secondary)
        }.padding(12)
            .simultaneousGesture(MagnifyGesture().onChanged { value in
                if zoomOrigin == nil { zoomOrigin = viewport }
                if let zoomOrigin { viewport = zoomOrigin.zoomed(by: value.magnification, anchor: 0.5, within: bounds) }
            }.onEnded { _ in zoomOrigin = nil })
    }
}
