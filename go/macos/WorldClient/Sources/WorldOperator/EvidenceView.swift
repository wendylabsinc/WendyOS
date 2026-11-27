import SwiftUI
import AVFoundation
import ImageIO
#if os(macOS)
import AppKit
#else
import UIKit
#endif

struct EvidenceView: View {
    let preview: EvidencePreview
    @Environment(\.dismiss) private var dismiss
    @State private var player: AVAudioPlayer?
    @State private var playing = false
    @State private var error: String?
    @State private var now = Date()

    var body: some View {
        ScrollView { content }.background(WorldStyle.background).preferredColorScheme(.dark)
#if os(macOS)
        .frame(minWidth: 560, minHeight: 580)
#endif
        .onDisappear { player?.stop() }
        .onReceive(Timer.publish(every: 0.1, on: .main, in: .common).autoconnect()) { time in
            now = time
            if !preview.available(at: time) { player?.stop(); playing = false }
            else if playing && player?.isPlaying != true { playing = false }
        }
    }

    // Shared with the headless view check; this is the actual sheet content.
    var content: some View {
        VStack(alignment: .leading, spacing: 22) {
                HStack {
                    VStack(alignment: .leading, spacing: 7) {
                        Text("Captured evidence").font(.title2.weight(.semibold))
                        Text(preview.item.name).foregroundStyle(.secondary)
                    }
                    Spacer()
                    Button { dismiss() } label: { Image(systemName: "xmark.circle.fill").font(.title2).foregroundStyle(.secondary) }.buttonStyle(.plain).accessibilityLabel("Close evidence")
                }
                Badge(label: "SHA-256 VERIFIED", color: WorldStyle.accent)
                if preview.item.nativeReference != nil {
                    Text(preview.fromCache ? "Saved simulation media" : "Captured simulation media").font(.caption).foregroundStyle(.secondary)
                }
                if !preview.available(at: now) {
                    Text("This media has expired.").foregroundStyle(.orange)
                } else if preview.item.contentType.hasPrefix("image/") { imageView }
                else if preview.item.contentType == "audio/wav" {
                    VStack(spacing: 20) {
                        Image(systemName: "waveform").font(.system(size: 80, weight: .ultraLight)).foregroundStyle(.purple)
                        Text("Audio from the captured sample").font(.headline)
                        Button {
                            guard preview.available(at: Date()) else { player?.stop(); playing = false; now = Date(); return }
                            if playing { player?.stop(); playing = false }
                            else {
                                do {
                                    let audio = try AVAudioPlayer(data: preview.data)
                                    audio.prepareToPlay(); playing = audio.play(); player = audio
                                    if !playing { error = "Audio playback could not start." }
                                } catch { self.error = "This audio sample could not be decoded." }
                            }
                        } label: { Label(playing ? "Stop audio" : "Play audio", systemImage: playing ? "stop.fill" : "play.fill") }.buttonStyle(.borderedProminent)
                        if let duration = player?.duration { Text(String(format: "%.2f seconds", duration)).font(.caption).foregroundStyle(.secondary) }
                    }.frame(maxWidth: .infinity, minHeight: 230).padding(20).card()
                } else {
                    Label("Verified bytes received; no preview for this media type.", systemImage: "doc").padding(20).card()
                }
                if let error { Text(error).foregroundStyle(.orange) }
                DetailRow(title: "Content type", value: preview.item.contentType)
                DetailRow(title: "Size", value: ByteCountFormatter.string(fromByteCount: Int64(preview.data.count), countStyle: .file))
                DetailRow(title: "SHA-256", value: preview.item.digest)
                if let owner = preview.item.nativeReference?.source {
                    DetailRow(title: "Source", value: text(owner["device"]) + " · " + text(owner["app"]) + "/" + text(owner["service"]))
                    DetailRow(title: "Stream", value: text(owner["stream"]))
                }
                if let until = preview.retainUntil { DetailRow(title: "Media retained until", value: until.formatted(date: .abbreviated, time: .standard)) }
        }.padding(28).frame(maxWidth: 820, alignment: .leading)
    }

    @ViewBuilder private var imageView: some View {
        if safeImageDimensions {
#if os(macOS)
        if let image = NSImage(data: preview.data) {
            Image(nsImage: image).resizable().scaledToFit().frame(maxWidth: .infinity, maxHeight: 480).background(.black, in: RoundedRectangle(cornerRadius: 12))
        } else { Text("Verified image bytes could not be decoded.").foregroundStyle(.orange) }
#else
        if let image = UIImage(data: preview.data) {
            Image(uiImage: image).resizable().scaledToFit().frame(maxWidth: .infinity, maxHeight: 480).background(.black, in: RoundedRectangle(cornerRadius: 12))
        } else { Text("Verified image bytes could not be decoded.").foregroundStyle(.orange) }
#endif
        } else { Text("Image dimensions are missing or exceed the preview limit.").foregroundStyle(.orange) }
    }

    private var safeImageDimensions: Bool {
        guard let source = CGImageSourceCreateWithData(preview.data as CFData, nil),
              let properties = CGImageSourceCopyPropertiesAtIndex(source, 0, nil) as? [CFString: Any],
              let width = properties[kCGImagePropertyPixelWidth] as? NSNumber,
              let height = properties[kCGImagePropertyPixelHeight] as? NSNumber else { return false }
        return width.intValue > 0 && height.intValue > 0 && width.intValue <= 4096 && height.intValue <= 4096
    }
}
