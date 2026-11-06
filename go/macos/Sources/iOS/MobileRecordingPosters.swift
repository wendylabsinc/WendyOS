import AVFoundation
import ImageIO
import SwiftUI

// Keep only small previews from recordings the user has already opened. The
// device list can render immediately without downloading video from every board.
@MainActor @Observable final class RecordingPosters {
    private struct Key: Hashable { let device: String; let episode: String }
    private var frames: [Key: UIImage] = [:]
    private var order: [Key] = []

    func image(device: Device, episode: String) -> UIImage? {
        frames[Key(device: recordingDeviceID(device), episode: episode)]
    }

    func remember(_ data: DataVisualSnapshot, device: Device, episode: String) async {
        let key = Key(device: recordingDeviceID(device), episode: episode)
        guard frames[key] == nil else { return }
        let preview: CGImage?
        if let video = data.videos.first {
            let generator = AVAssetImageGenerator(asset: AVURLAsset(url: video.url))
            generator.appliesPreferredTrackTransform = true
            generator.maximumSize = CGSize(width: 190, height: 190)
            preview = try? await generator.image(at: .zero).image
        } else if let bytes = data.samples.lazy.compactMap(\.image).first,
                  let source = CGImageSourceCreateWithData(bytes as CFData, nil) {
            let options: [CFString: Any] = [kCGImageSourceCreateThumbnailFromImageAlways: true,
                                            kCGImageSourceThumbnailMaxPixelSize: 190,
                                            kCGImageSourceCreateThumbnailWithTransform: true]
            preview = CGImageSourceCreateThumbnailAtIndex(source, 0, options as CFDictionary)
        } else { return }
        guard let preview, !Task.isCancelled, frames[key] == nil else { return }
        if order.count >= 32 { frames.removeValue(forKey: order.removeFirst()) }
        frames[key] = UIImage(cgImage: preview)
        order.append(key)
    }
}

extension EnvironmentValues {
    @Entry var recordingPosters: RecordingPosters?
}
