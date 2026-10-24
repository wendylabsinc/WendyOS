import Foundation

struct DataVisualEpisode: Identifiable {
    let id, name, state: String
    let started: Date
    let bytes: Int64
    var title: String { name.isEmpty ? id : name }
}
struct CampaignClip: Identifiable {
    let id, campaign, state, reason: String
    let started: Date
    let bytes: Int64
    let duration: Double
    var durationLabel: String {
        guard duration >= 0 else { return "—" }
        return Duration.seconds(duration).formatted(.time(pattern: .minuteSecond))
    }
}
struct CampaignClipsSnapshot {
    let clips: [CampaignClip]
    let warning: String
}
struct DataVisualTrack: Identifiable {
    let id, name, kind, detail: String
    var symbol: String {
        switch kind { case "camera", "image": return "camera"; case "numeric", "telemetry": return "waveform.path"; default: return "text.bubble" }
    }
}
struct DataVisualSample: Identifiable {
    let id: Int
    let track, label, detail, unit: String
    let timestamp: Int64
    let value: Double?
    let image: Data?
    var seconds: Double { Double(timestamp) / 1_000_000_000 }
}
struct DataVisualSnapshot {
    let clock, bootID, warnings: String
    let start, end: Double
    let tracks: [DataVisualTrack]
    let samples: [DataVisualSample]
    let videos: [DataVisualVideo]
    let mediaFiles: DataVisualMediaFiles
    var duration: Double { max(end - start, 0.001) }
    func latest(track: String, at cursor: Double) -> DataVisualSample? {
        samples.last { $0.track == track && $0.seconds <= cursor }
    }
}

struct DataVisualVideo: Identifiable {
    let track: String
    let url: URL
    let timestamps: [Double]
    var id: String { track }
    func frame(at cursor: Double) -> Double? { timestamps.last { $0 <= cursor } }
}

// A snapshot owns its bounded, verified media downloads. Discarding it removes
// temporary files after its player has released the snapshot reference.
final class DataVisualMediaFiles {
    private var urls: [URL] = []
    func adopt(_ source: URL) throws -> URL {
        let url = FileManager.default.temporaryDirectory.appendingPathComponent("wendy-data-\(UUID().uuidString).mp4")
        try FileManager.default.moveItem(at: source, to: url)
        urls.append(url)
        return url
    }
    deinit { for url in urls { try? FileManager.default.removeItem(at: url) } }
}

extension GoClient {
    func campaignClips(session: UInt64) async throws -> CampaignClipsSnapshot {
        try await Self.visualPerform {
            guard let p = WDCampaignClips(session) else { throw WendyError(message: "Missing campaign recordings.") }
            defer { WDCampaignClipsFree(p) }
            try Self.check(p.pointee.status)
            let clips = UnsafeBufferPointer(start: p.pointee.items, count: p.pointee.count).map {
                CampaignClip(id: Self.string($0.id), campaign: Self.string($0.campaign), state: Self.string($0.state), reason: Self.string($0.reason),
                             started: Date(timeIntervalSince1970: Double($0.started_ns) / 1e9), bytes: $0.size_bytes, duration: $0.duration_seconds)
            }
            return CampaignClipsSnapshot(clips: clips, warning: Self.string(p.pointee.warning))
        }
    }
    func visualEpisodes(session: UInt64) async throws -> [DataVisualEpisode] {
        try await Self.visualPerform {
            guard let p = WDVisualEpisodes(session) else { throw WendyError(message: "Missing episode list.") }
            defer { WDVisualEpisodesFree(p) }
            try Self.check(p.pointee.status)
            return UnsafeBufferPointer(start: p.pointee.items, count: p.pointee.count).map {
                DataVisualEpisode(id: Self.string($0.id), name: Self.string($0.name), state: Self.string($0.state),
                                  started: Date(timeIntervalSince1970: Double($0.started_ns) / 1_000_000_000), bytes: $0.size_bytes)
            }.sorted { $0.started == $1.started ? $0.id < $1.id : $0.started > $1.started }
        }
    }
    func visualEpisode(session: UInt64, episode: String) async throws -> DataVisualSnapshot {
        try await Self.visualPerform {
            try Self.withCString(episode) { episode in
                guard let p = WDVisualEpisodeLoad(session, episode) else { throw WendyError(message: "Missing episode data.") }
                defer { WDVisualSnapshotFree(p) }
                try Self.check(p.pointee.status)
                let tracks = UnsafeBufferPointer(start: p.pointee.tracks, count: p.pointee.track_count).map {
                    DataVisualTrack(id: Self.string($0.id), name: Self.string($0.name), kind: Self.string($0.kind), detail: Self.string($0.detail))
                }
                let samples = UnsafeBufferPointer(start: p.pointee.samples, count: p.pointee.sample_count).enumerated().map { index, s in
                    DataVisualSample(id: index, track: Self.string(s.track), label: Self.string(s.label), detail: Self.string(s.detail), unit: Self.string(s.unit),
                                     timestamp: s.timestamp_ns, value: s.has_value == 0 ? nil : s.value,
                                     image: s.image_count > 0 ? s.image.map { Data(bytes: $0, count: s.image_count) } : nil)
                }
                let media = DataVisualMediaFiles()
                let videos = try UnsafeBufferPointer(start: p.pointee.videos, count: p.pointee.video_count).map { video in
                    let path = Self.string(video.local_path)
                    guard !path.isEmpty else { throw WendyError(message: "The recorded video file is missing.") }
                    return DataVisualVideo(track: Self.string(video.track), url: try media.adopt(URL(fileURLWithPath: path)),
                                           timestamps: UnsafeBufferPointer(start: video.timestamps, count: video.timestamp_count).map { Double($0) / 1_000_000_000 })
                }
                return DataVisualSnapshot(clock: Self.string(p.pointee.clock), bootID: Self.string(p.pointee.boot_id), warnings: Self.string(p.pointee.warnings),
                                          start: Double(p.pointee.start_ns) / 1_000_000_000, end: Double(p.pointee.end_ns) / 1_000_000_000,
                                          tracks: tracks, samples: samples, videos: videos, mediaFiles: media)
            }
        }
    }
}

// Episode downloads can be large. Keep them off the shared command queue so
// device selection, app actions, and other panes remain responsive.
private enum DataVisualWorker {
    static let queue = DispatchQueue(label: "sh.wendy.data-visualizer", qos: .userInitiated)
}

private extension GoClient {
    static func visualPerform<T>(_ work: @escaping () throws -> T) async throws -> T {
        try await withCheckedThrowingContinuation { continuation in
            DataVisualWorker.queue.async {
                do { continuation.resume(returning: try work()) }
                catch { continuation.resume(throwing: error) }
            }
        }
    }
}
