import AVFoundation
import ImageIO
import SwiftUI

enum RecordingPoster {
    case image(UIImage)
    case waveform([UInt8])
}

// Share decoded thumbnails between the device card, the picker and playback.
// Only the on-screen rows request media; the account/device identity is part of every key.
@MainActor @Observable final class RecordingPosters {
    private struct Key: Hashable { let device: String; let episode: String }
    private var entries: [Key: RecordingPoster] = [:]
    private var order: [Key] = []

    func poster(device: Device, episode: String) -> RecordingPoster? {
        entries[Key(device: recordingDeviceID(device), episode: episode)]
    }

    func load(device: Device, episode: String, file: String) async {
        let key = Key(device: recordingDeviceID(device), episode: episode)
        guard entries[key] == nil else { return }
        do {
            // A row that only flashes by while scrolling shouldn't start a transfer.
            try await Task.sleep(for: .milliseconds(180))
            guard entries[key] == nil else { return }
            let (format, bytes) = try await GoClient.shared.recordingThumbnail(device: device, episode: episode, file: file)
            let result: RecordingPoster
            switch format {
            case "audio": result = .waveform(Array(bytes))
            case "image":
                guard let image = await Self.still(bytes) else { return }
                result = .image(UIImage(cgImage: image))
            case "video":
                guard let image = try await Self.movie(bytes) else { return }
                result = .image(UIImage(cgImage: image))
            default: return
            }
            try Task.checkCancellation()
            insert(result, for: key)
        } catch { /* Keep the type-specific artwork; a later appearance can try again. */ }
    }

    func remember(_ data: DataVisualSnapshot, device: Device, episode: String) async {
        let key = Key(device: recordingDeviceID(device), episode: episode)
        guard entries[key] == nil else { return }
        var preview: CGImage?
        if let video = data.videos.first { preview = await Self.frame(video.url) }
        if preview == nil, let bytes = data.samples.lazy.compactMap(\.image).first {
            preview = await Self.still(bytes)
        }
        if let preview, !Task.isCancelled { insert(.image(UIImage(cgImage: preview)), for: key) }
    }

    private func insert(_ poster: RecordingPoster, for key: Key) {
        guard entries[key] == nil else { return }
        if order.count >= 64 { entries.removeValue(forKey: order.removeFirst()) }
        entries[key] = poster
        order.append(key)
    }

    @concurrent private static func still(_ bytes: Data) async -> CGImage? {
        guard let source = CGImageSourceCreateWithData(bytes as CFData, nil) else { return nil }
        let options: [CFString: Any] = [kCGImageSourceCreateThumbnailFromImageAlways: true,
                                        kCGImageSourceThumbnailMaxPixelSize: 190,
                                        kCGImageSourceCreateThumbnailWithTransform: true]
        return CGImageSourceCreateThumbnailAtIndex(source, 0, options as CFDictionary)
    }

    @concurrent private static func movie(_ bytes: Data) async throws -> CGImage? {
        let url = FileManager.default.temporaryDirectory.appendingPathComponent("wendy-thumb-\(UUID()).mp4")
        defer { try? FileManager.default.removeItem(at: url) }
        try bytes.write(to: url)
        return await frame(url)
    }

    @concurrent private static func frame(_ url: URL) async -> CGImage? {
        let generator = AVAssetImageGenerator(asset: AVURLAsset(url: url))
        generator.appliesPreferredTrackTransform = true
        generator.maximumSize = CGSize(width: 190, height: 190)
        return try? await generator.image(at: .zero).image
    }
}

extension EnvironmentValues {
    @Entry var recordingPosters: RecordingPosters?
}
