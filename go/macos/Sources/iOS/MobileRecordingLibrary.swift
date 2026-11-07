import Foundation

enum MobileRecordingKind: String, Equatable, CaseIterable {
    case mixed = "Mixed", video = "Video", audio = "Audio", rosbag = "ROS bag", images = "Images", dataset = "Dataset", unknown = "Recording"

    init(agentKind: String) {
        switch agentKind {
        case "mixed": self = .mixed
        case "video": self = .video
        case "audio": self = .audio
        case "rosbag": self = .rosbag
        case "images": self = .images
        case "dataset": self = .dataset
        default: self = .unknown
        }
    }
}

struct MobileRecordingEpisode: Identifiable, Equatable {
    let id, title: String
    let started: Date?
    let bytes: Int64
    let duration: Double?
    let trigger: String?
    let kind: MobileRecordingKind
    let synced: Bool?
    let topics: [String]
    var detailsLoaded = false
    var campaign: String?
    var previewFile: String?
    var previewCounts: [UInt64] = []
    static let detailBatchLimit = 64

    // Agents retain the actual event name in the trigger reason. Keep custom
    // events filterable even when the handoff has no predefined label for them.
    static func triggerLabel(_ reason: String) -> String? {
        let raw = reason.hasPrefix("event:") ? String(reason.dropFirst(6)) : reason
        let label = raw.replacingOccurrences(of: "_", with: " ")
            .replacingOccurrences(of: "-", with: " ").split(whereSeparator: \.isWhitespace).joined(separator: " ")
        switch label.lowercased() {
        case "": return nil
        case "person", "person detected": return "Person"
        case "door opened", "door open": return "Door opened"
        case "motion", "motion detected": return "Motion"
        case "schedule", "scheduled": return "Schedule"
        case "manual": return "Manual"
        case "loud sound", "loud sound detected": return "Loud sound"
        default: return label.prefix(1).uppercased() + label.dropFirst()
        }
    }
}

struct MobileRecordingLibrary {
    let device: Device
    let model: String?
    let episodes: [MobileRecordingEpisode]
    let warning: String?
}

// The share sheet owns the export until it finishes reading all the files.
final class MobileRecordingExport: Identifiable, Sendable {
    let id = UUID()
    let urls: [URL]
    private let directory: URL

    fileprivate init(staged paths: [URL], episodes: [MobileRecordingEpisode]) throws {
        guard !paths.isEmpty, paths.count == episodes.count else {
            throw WendyError(message: "The device returned an incomplete export.")
        }
        let manager = FileManager.default
        directory = manager.temporaryDirectory.appendingPathComponent("wendy-exports-\(id)", isDirectory: true)
        try manager.createDirectory(at: directory, withIntermediateDirectories: false)
        var files: [URL] = []
        do {
            for (index, pair) in zip(paths, episodes).enumerated() {
                let title = Self.fileTitle(pair.1.title)
                let filename = String(format: "%03d", index + 1) + "-\(title).zip"
                let url = directory.appendingPathComponent(filename)
                try manager.moveItem(at: pair.0, to: url)
                files.append(url)
            }
        } catch {
            try? manager.removeItem(at: directory)
            throw error
        }
        urls = files
    }

    private static func fileTitle(_ value: String) -> String {
        var safe = ""
        let letters = CharacterSet.alphanumerics.union(CharacterSet(charactersIn: "-_ "))
        for scalar in value.unicodeScalars {
            let piece = letters.contains(scalar) ? String(scalar) : "-"
            if safe.utf8.count + piece.utf8.count > 100 { break }
            safe += piece
        }
        return safe.isEmpty ? "Recording" : safe
    }

    deinit { try? FileManager.default.removeItem(at: directory) }
}

extension GoClient {
    private static let recordingQueue = DispatchQueue(label: "dev.wendy.recording-library",
                                                        qos: .userInitiated, attributes: .concurrent)

    // One automatic media read at a time, independent of catalog and playback.
    private static let thumbnailQueue = DispatchQueue(label: "dev.wendy.recording-thumbnails", qos: .utility)

    private func readRecordings<T: Sendable>(
        device: Device, queue: DispatchQueue = GoClient.recordingQueue, _ operation: @escaping @Sendable (UInt64, UnsafeMutablePointer<CChar>, Int32) throws -> T
    ) async throws -> T {
        guard !device.lite else { throw WendyError(message: "Wendy Lite does not store recordings.") }
        try Task.checkCancellation()
        let request = WDBeginRecordingRead()
        guard request != 0 else { throw WendyError(message: "Recording access is paused. Refresh to try again.") }
        defer { WDEndRecordingRead(request) }
        return try await withTaskCancellationHandler {
            try await BlockingWork.run(on: queue) {
                try Self.withCString(device.selector) { selector in
                    try operation(request, selector, device.requiresMTLS ? 1 : 0)
                }
            }
        } onCancel: {
            WDEndRecordingRead(request)
        }
    }

    func recordingLibrary(device: Device) async throws -> MobileRecordingLibrary {
        try await readRecordings(device: device) { request, selector, secure in
            guard let p = WDRecordingLibrary(request, selector, secure) else {
                throw WendyError(message: "Missing recording library.")
            }
            defer { WDRecordingLibraryFree(p) }
            try Self.check(p.pointee.status)
            let episodes = UnsafeBufferPointer(start: p.pointee.items, count: p.pointee.count)
                .map { Self.recordingEpisode($0) }
            let warning = Self.string(p.pointee.warning)
            let model = Self.string(p.pointee.model)
            return MobileRecordingLibrary(device: device, model: model.isEmpty ? nil : model,
                                          episodes: episodes, warning: warning.isEmpty ? nil : warning)
        }
    }

    func recordingDetails(device: Device, episodes: [MobileRecordingEpisode]) async throws -> [MobileRecordingEpisode] {
        guard !episodes.isEmpty else { return [] }
        guard episodes.count <= MobileRecordingEpisode.detailBatchLimit else {
            throw WendyError(message: "Too many recordings in one request.")
        }
        return try await readRecordings(device: device) { request, selector, secure in
            let strings = CStringStorage()
            defer { withExtendedLifetime(strings) {} }
            var ids: [UnsafeMutablePointer<CChar>?] = episodes.map { strings.add($0.id) }
            return try ids.withUnsafeMutableBufferPointer { buffer in
                guard let p = WDRecordingDetails(request, selector, secure, buffer.baseAddress, buffer.count) else {
                    throw WendyError(message: "Missing recording details.")
                }
                defer { WDRecordingLibraryFree(p) }
                try Self.check(p.pointee.status)
                guard p.pointee.count == episodes.count else {
                    throw WendyError(message: "The device returned an incomplete batch of recording details.")
                }
                let rows = UnsafeBufferPointer(start: p.pointee.items, count: p.pointee.count)
                return try zip(episodes, rows).map { existing, row in
                    guard Self.string(row.id) == existing.id else {
                        throw WendyError(message: "The device returned details for another recording.")
                    }
                    return row.details_loaded == 0 ? existing : Self.recordingEpisode(row, index: existing)
                }
            }
        }
    }

    private static func recordingEpisode(_ row: WDRecordingEpisode, index: MobileRecordingEpisode? = nil) -> MobileRecordingEpisode {
        let id = string(row.id)
        let title = string(row.title)
        let synced: Bool? = switch string(row.upload_state) {
        case "uploaded": true
        case "local", "pending", "uploading", "failed": false
        default: nil
        }
        return MobileRecordingEpisode(id: id, title: index?.title ?? (title.isEmpty ? id : title),
            started: index?.started ?? (row.started_ns == 0 ? nil : Date(timeIntervalSince1970: Double(row.started_ns) / 1e9)),
            bytes: index?.bytes ?? row.size_bytes, duration: row.duration_seconds < 0 ? nil : row.duration_seconds,
            trigger: MobileRecordingEpisode.triggerLabel(string(row.trigger)),
            kind: MobileRecordingKind(agentKind: string(row.kind)), synced: synced,
            topics: string(row.topics).split(separator: "\n").map(String.init), detailsLoaded: row.details_loaded != 0,
            campaign: row.campaign.map { String(cString: $0) }, previewFile: row.preview_file.map { String(cString: $0) },
            previewCounts: Array(UnsafeBufferPointer(start: row.preview_counts, count: row.preview_count)))
    }

    func recordingThumbnail(device: Device, episode: String, file: String) async throws -> (String, Data) {
        try await readRecordings(device: device, queue: Self.thumbnailQueue) { request, selector, secure in
            try Self.withCString(episode) { id in
                try Self.withCString(file) { descriptor in
                    guard let p = WDRecordingThumbnail(request, selector, secure, id, descriptor) else {
                        throw WendyError(message: "Missing thumbnail.")
                    }
                    defer { WDRecordingThumbnailFree(p) }
                    try Self.check(p.pointee.status)
                    guard let bytes = p.pointee.bytes, p.pointee.count > 0 else { throw WendyError(message: "Empty thumbnail.") }
                    return (Self.string(p.pointee.format), Data(bytes: bytes, count: p.pointee.count))
                }
            }
        }
    }

    func visualEpisode(device: Device, episode: String) async throws -> DataVisualSnapshot {
        try await readRecordings(device: device) { request, selector, secure in
            try Self.withCString(episode) {
                try Self.decodeVisualSnapshot(WDRecordingPlay(request, selector, secure, $0))
            }
        }
    }

    func exportRecordingEpisodes(device: Device, episodes: [MobileRecordingEpisode]) async throws -> MobileRecordingExport {
        try await readRecordings(device: device) { request, selector, secure in
            let strings = CStringStorage()
            defer { withExtendedLifetime(strings) {} }
            var ids: [UnsafeMutablePointer<CChar>?] = episodes.map { strings.add($0.id) }
            return try ids.withUnsafeMutableBufferPointer { buffer in
                guard let p = WDRecordingExport(request, selector, secure, buffer.baseAddress, buffer.count) else {
                    throw WendyError(message: "Missing recording export.")
                }
                defer { WDRecordingExportsFree(p) }
                try Self.check(p.pointee.status)
                let paths = UnsafeBufferPointer(start: p.pointee.paths, count: p.pointee.count)
                    .map { URL(fileURLWithPath: Self.string($0)) }
                return try MobileRecordingExport(staged: paths, episodes: episodes)
            }
        }
    }
}
