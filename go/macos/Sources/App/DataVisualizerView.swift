import SwiftUI
import AppKit
import ImageIO
import AVFoundation
import SceneKit
import UniformTypeIdentifiers

struct DataVisualizerView: View {
    @State private var actions = AsyncActions()
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
    @State private var sensorTrack = ""
    @State private var exporting = false
    @State private var recordingSheet = false
    @State private var exportedURL: URL?
    @State private var viewport: DataTimelineViewport?

    private var loading: Bool { loadingList || loadingEpisode }
    var body: some View {
        actionContent.task { await actions.run() }
    }

    @ViewBuilder private var actionContent: some View {
        VStack(spacing: 0) {
            HStack(spacing: 12) {
                Label("Data visualizer", systemImage: "waveform.path.ecg").font(.headline)
                Picker("Episode", selection: $episode) {
                    Text("Choose an episode").tag("")
                    ForEach(episodes) { item in Text(episodeLabels[item.id] ?? item.title).tag(item.id) }
                }.frame(maxWidth: 440).disabled(loadingList)
                Button { recordingSheet = true } label: { Label("Record", systemImage: "record.circle") }
                Menu {
                    Button("Full episode (.zip)") { exportEpisode() }
                    Button("Visible measurements (.csv)") { exportMeasurements() }.disabled(snapshot == nil)
                    Button("LiDAR preview at cursor (.ply)") { exportPoints() }.disabled(index.latest(track: sensorTrack, at: cursor)?.points.isEmpty != false)
                } label: { Label(exporting ? "Exporting…" : "Export", systemImage: "square.and.arrow.up") }.disabled(episode.isEmpty || loading || exporting)
                if let exportedURL { Button("Show export") { NSWorkspace.shared.activateFileViewerSelecting([exportedURL]) } }
                Spacer()
                if loading || exporting { ProgressView().controlSize(.small) }
                Text(deviceName).font(.callout).foregroundStyle(.secondary).lineLimit(1)
            }.padding(.horizontal, 12).padding(.vertical, 8)
            Divider()
            if let error {
                FailureNote(message: error).padding(16).frame(maxWidth: .infinity, alignment: .leading)
            }
            if let snapshot {
                workspace(snapshot).opacity(loadingEpisode ? 0.45 : 1).disabled(loadingEpisode)
            } else if loading {
                Spacer(); ProgressView("Loading recorded data"); Spacer()
            } else {
                ContentUnavailableView("Explore a recording", systemImage: "waveform.path.ecg", description: Text("Choose a Wendy Data episode to replay cameras and audio, inspect LiDAR, events, and measurements on one timeline."))
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
        .sheet(isPresented: $recordingSheet) {
            DataVisualRecordingSheet(session: session, deviceName: deviceName) { id in
                refreshID += 1
                if let id { episode = id }
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
                            sensorPanel(data).frame(minWidth: 260, maxWidth: .infinity, minHeight: 180, maxHeight: .infinity)
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
            if let track = data.tracks.first(where: { $0.id == id }), track.kind == "lidar" || track.kind == "audio" { sensorTrack = id } else if index.numeric[id] != nil { sensorTrack = "" }
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

    private func sensorPanel(_ data: DataVisualSnapshot) -> some View {
        let track = data.tracks.first { $0.id == sensorTrack }
        let sample = index.latest(track: sensorTrack, at: cursor)
        return VStack(spacing: 0) {
            if data.tracks.contains(where: { $0.kind == "audio" || $0.kind == "lidar" }) {
                HStack {
                    Picker("Sensor", selection: $sensorTrack) {
                        Text("Measurements").tag("")
                        ForEach(data.tracks.filter { $0.kind == "audio" || $0.kind == "lidar" }) { Text($0.name).tag($0.id) }
                    }
                }.padding(8)
            }
            if track?.kind == "lidar" {
                if let sample {
                    DataVisualPointCloud(sample: sample, recording: index.id)
                    Text("Sensor frame · meters · drag to orbit, scroll to zoom").font(.caption2).foregroundStyle(.secondary).padding(6)
                    Text("Frame at \(timeLabel(sample.seconds)) · \(sample.points.count) preview points").font(.caption2).foregroundStyle(.secondary).padding(.bottom, 6)
                } else { ContentUnavailableView("No LiDAR frame at this time", systemImage: "cube.transparent").frame(maxHeight: .infinity) }
            } else if track?.kind == "audio" {
                let segment = data.audio.last { $0.track == sensorTrack && cursor >= $0.start && cursor < $0.end }
                Label(segment == nil ? "No audio at this time" : playing ? "Playing audio" : "Audio paused", systemImage: "waveform").padding(12)
                DataVisualPlot(recording: index.id, track: sensorTrack, samples: index.numeric[sensorTrack] ?? [], range: 0...1, start: visibleWindow(data).start, end: visibleWindow(data).end, cursor: cursor)
                    .overlay { timelineInteraction(data) }.clipped().padding(12)
                if let segment {
                    DataVisualAudioPlayer(segment: segment, cursor: cursor, playing: playing, speed: speed, owner: data.mediaFiles).id(segment.id).frame(height: 18)
                }
                Text("Peak amplitude · audio follows the playback cursor").font(.caption2).foregroundStyle(.secondary).padding(8)
            } else { numericPanel(data) }
        }
    }

    private func exportEpisode() {
        let panel = NSSavePanel()
        panel.allowedContentTypes = [.zip]
        panel.nameFieldStringValue = "wendy-episode.zip"
        panel.message = "Export the complete recording, including original ROS bags, WAV audio, video, and metadata."
        guard panel.runModal() == .OK, let destination = panel.url else { return }
        let selectedEpisode = episode
        exporting = true; error = nil; exportedURL = nil
        actions.submit {
            defer { exporting = false }
            do { try await GoClient.shared.exportVisualEpisode(session: session, episode: selectedEpisode, destination: destination); exportedURL = destination }
            catch { self.error = error.localizedDescription }
        }
    }
    private func exportMeasurements() {
        guard let snapshot else { return }
        let panel = NSSavePanel(); panel.allowedContentTypes = [.commaSeparatedText]; panel.nameFieldStringValue = "measurements.csv"
        panel.message = "Export the measurements loaded in this viewer. Use Full episode for the complete original data."
        guard panel.runModal() == .OK, let url = panel.url else { return }
        do { try DataVisualExport.csv(snapshot.samples).write(to: url, atomically: true, encoding: .utf8); exportedURL = url }
        catch { self.error = error.localizedDescription }
    }
    private func exportPoints() {
        guard let sample = index.latest(track: sensorTrack, at: cursor), !sample.points.isEmpty else { return }
        let panel = NSSavePanel(); panel.allowedContentTypes = [UTType(filenameExtension: "ply") ?? .data]; panel.nameFieldStringValue = "lidar-preview.ply"
        panel.message = "Export this preview in sensor coordinates. Use Full episode to retain all points and fields."
        guard panel.runModal() == .OK, let url = panel.url else { return }
        do { try DataVisualExport.ply(sample).write(to: url, atomically: true, encoding: .utf8); exportedURL = url }
        catch { self.error = error.localizedDescription }
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
                Picker("Speed", selection: $speed) { Text("½×").tag(0.5); Text("1×").tag(1.0); Text("2×").tag(2.0); if data.audio.isEmpty { Text("4×").tag(4.0) } }.labelsHidden().frame(width: 65)
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
            if !value.audio.isEmpty { speed = min(speed, 2) }
            index = DataVisualIndex(value)
            sensorTrack = value.tracks.first(where: { $0.kind == "lidar" || $0.kind == "audio" })?.id ?? ""
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
            if sample.value == nil && sample.image == nil && sample.points.isEmpty && !sample.label.hasPrefix("Frame ") { eventSamples.append(sample) }
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
    nonisolated static func == (lhs: Self, rhs: Self) -> Bool { lhs.recording == rhs.recording && lhs.event.id == rhs.event.id }
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
struct DataVisualNativeVideoPlayer: NSViewRepresentable {
    let video: DataVisualVideo
    let cursor: Double
    let playing: Bool
    let speed: Double
    let controller: DataVisualVideoController
    func makeCoordinator() -> DataVisualVideoController { controller }
    func makeNSView(context: Context) -> NSView {
        let host = DataVisualPlayerHost(frame: .zero)
        host.playerLayer.player = context.coordinator.player
        return host
    }
    func updateNSView(_ nsView: NSView, context: Context) {
        guard let frame = DataVisualSearch.latest(video.timestamps, at: cursor, time: { $0 }), let origin = video.timestamps.first else { return }
        context.coordinator.update(position: cursor - origin, frame: frame - origin, playing: playing, speed: speed)
    }
    static func dismantleNSView(_ nsView: NSView, coordinator: DataVisualVideoController) { coordinator.stop() }
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
    nonisolated static func == (lhs: Self, rhs: Self) -> Bool {
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
    nonisolated static func == (lhs: Self, rhs: Self) -> Bool {
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


private struct DataVisualPointCloud: NSViewRepresentable {
    let sample: DataVisualSample
    let recording: UUID
    @MainActor final class Coordinator { var sampleID = -1; var recording: UUID?; var framedRecording: UUID? }
    func makeCoordinator() -> Coordinator { Coordinator() }
    func makeNSView(context: Context) -> SCNView {
        let view = SCNView()
        view.scene = SCNScene(); view.backgroundColor = .black
        view.allowsCameraControl = true; view.autoenablesDefaultLighting = false
        let camera = SCNNode(); camera.camera = SCNCamera(); camera.camera?.zFar = 10000
        camera.position = SCNVector3(0, -10, 7); camera.look(at: SCNVector3Zero, up: SCNVector3(0, 0, 1), localFront: SCNVector3(0, 0, -1))
        view.scene?.rootNode.addChildNode(camera); view.pointOfView = camera
        let points = SCNNode(); points.name = "points"; view.scene?.rootNode.addChildNode(points)
        for (axis, color) in [(SCNVector3(1, 0, 0), NSColor.systemRed), (SCNVector3(0, 1, 0), NSColor.systemGreen), (SCNVector3(0, 0, 1), NSColor.systemBlue)] {
            let source = SCNGeometrySource(vertices: [SCNVector3Zero, axis])
            let element = SCNGeometryElement(indices: [Int32(0), 1], primitiveType: .line)
            let geometry = SCNGeometry(sources: [source], elements: [element])
            let material = SCNMaterial(); material.diffuse.contents = color; material.lightingModel = .constant; geometry.materials = [material]
            view.scene?.rootNode.addChildNode(SCNNode(geometry: geometry))
        }
        return view
    }
    func updateNSView(_ view: SCNView, context: Context) {
        guard context.coordinator.sampleID != sample.id || context.coordinator.recording != recording else { return }
        context.coordinator.sampleID = sample.id; context.coordinator.recording = recording
        let source = SCNGeometrySource(vertices: sample.points.map { SCNVector3($0.x, $0.y, $0.z) })
        let element = SCNGeometryElement(indices: sample.points.indices.map { Int32($0) }, primitiveType: .point)
        element.pointSize = 3; element.minimumPointScreenSpaceRadius = 1; element.maximumPointScreenSpaceRadius = 4
        let geometry = SCNGeometry(sources: [source], elements: [element])
        let material = SCNMaterial(); material.diffuse.contents = NSColor.systemCyan; material.lightingModel = .constant; geometry.materials = [material]
        view.scene?.rootNode.childNode(withName: "points", recursively: false)?.geometry = geometry
        if context.coordinator.framedRecording != recording, !sample.points.isEmpty {
            let radius = min(1000, max(1, sample.points.reduce(Float(0)) { max($0, simd_length($1)) }))
            view.pointOfView?.position = SCNVector3(0, -radius * 2, radius * 1.5)
            view.pointOfView?.look(at: SCNVector3Zero, up: SCNVector3(0, 0, 1), localFront: SCNVector3(0, 0, -1))
            context.coordinator.framedRecording = recording
        }
    }
}

private struct DataVisualAudioPlayer: NSViewRepresentable {
    let segment: DataVisualAudio
    let cursor: Double
    let playing: Bool
    let speed: Double
    let owner: DataVisualMediaFiles
    @MainActor final class Coordinator {
        let owner: DataVisualMediaFiles
        var player: AVAudioPlayer?
        var error: String?
        init(segment: DataVisualAudio, owner: DataVisualMediaFiles) {
            self.owner = owner
            do { player = try AVAudioPlayer(contentsOf: segment.url) }
            catch { self.error = error.localizedDescription }
            player?.enableRate = true; player?.prepareToPlay()
        }
        isolated deinit { player?.stop() }
    }
    func makeCoordinator() -> Coordinator { Coordinator(segment: segment, owner: owner) }
    func makeNSView(context: Context) -> NSTextField {
        let label = NSTextField(labelWithString: ""); label.textColor = .systemRed; label.font = .systemFont(ofSize: 11); return label
    }
    func updateNSView(_ view: NSTextField, context: Context) {
        view.stringValue = context.coordinator.error ?? ""
        guard let player = context.coordinator.player else { return }
        let position = max(0, cursor - segment.start)
        if !playing { player.pause(); player.currentTime = position; return }
        if !player.isPlaying || abs(player.currentTime - position) > 0.15 { player.currentTime = position }
        player.rate = Float(speed)
        if !player.isPlaying && !player.play() { view.stringValue = "Audio playback could not start." }
    }
    static func dismantleNSView(_ view: NSTextField, coordinator: Coordinator) { coordinator.player?.stop() }
}


private struct DataVisualRecordingSheet: View {
    @State private var actions = AsyncActions()
    let session: UInt64
    let deviceName: String
    let changed: (String?) -> Void
    @Environment(\.dismiss) private var dismiss
    @State private var sources: [CampaignSource] = []
    @State private var selected: Set<String> = []
    @State private var name = "Sensor recording"
    @State private var active: DataVisualEpisode?
    @State private var busy = true
    @State private var error: String?
    @State private var ready = false
    var body: some View {
        actionContent.task { await actions.run() }
    }

    @ViewBuilder private var actionContent: some View {
        VStack(alignment: .leading, spacing: 16) {
            Text("Record on \(deviceName)").font(.title2.bold())
            if let active {
                Label("Recording: \(active.title)", systemImage: "record.circle").foregroundStyle(.red)
                Text("Recording continues on the device when this window closes. Stop and save to inspect or export it.").foregroundStyle(.secondary)
            } else {
                TextField("Recording name", text: $name).textFieldStyle(.roundedBorder)
                ScrollView {
                    VStack(alignment: .leading, spacing: 12) {
                        ForEach(["camera", "audio", "ros2", "telemetry"], id: \.self) { kind in
                            let items = sources.filter { $0.kind == kind }
                            if !items.isEmpty {
                                Text(kind == "ros2" ? "ROS 2 topics and domains" : kind.capitalized).font(.headline)
                                ForEach(items) { source in
                                    Toggle(source.title, isOn: Binding(get: { selected.contains(source.id) }, set: { on in
                                        if on { selected.insert(source.id) } else { selected.remove(source.id) }
                                    })).disabled(!source.healthy).font(.callout)
                                }
                            }
                        }
                    }.frame(maxWidth: .infinity, alignment: .leading)
                }
                Text("Select a ROS 2 domain for all topics, or individual topics. Recording starts immediately and stays on the device until exported or uploaded.").font(.caption).foregroundStyle(.secondary)
            }
            if let error { FailureNote(message: error) }
            HStack {
                if busy { ProgressView().controlSize(.small) }
                Spacer()
                Button("Close") { dismiss() }.disabled(busy)
                if active != nil {
                    Button("Stop and save") { actions.submit { await stop() } }.disabled(busy)
                } else {
                    Button("Start recording") { actions.submit { await start() } }.disabled(busy || !ready || selected.isEmpty)
                }
            }
        }.padding(24).frame(width: 640, height: 540).interactiveDismissDisabled(busy)
            .task { await refresh() }
    }
    private func refresh() async {
        busy = true; defer { busy = false }
        do {
            active = try await GoClient.shared.visualRecording(session: session, action: WD_RECORDING_STATUS)
            if active == nil { sources = try await GoClient.shared.campaignSources(session: session) }
            ready = true
        } catch { self.error = error.localizedDescription }
    }
    private func start() async {
        busy = true; error = nil; defer { busy = false }
        do { active = try await GoClient.shared.visualRecording(session: session, action: WD_RECORDING_START, name: name, sources: selected); changed(nil) }
        catch { self.error = error.localizedDescription }
    }
    private func stop() async {
        busy = true; error = nil; defer { busy = false }
        do { let sealed = try await GoClient.shared.visualRecording(session: session, action: WD_RECORDING_STOP); changed(sealed?.id); dismiss() }
        catch { self.error = error.localizedDescription }
    }
}
