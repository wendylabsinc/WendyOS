import AppKit
import AVFoundation
import Combine
import Foundation
import ImageIO
import SwiftUI
import WorldClient

struct DemoInspectionPresentation: Identifiable {
    let result: MissionInspectionResult
    let media: DemoInspectionMedia
    var id: String { result.taskID }
}

/// Media is read separately from the saved result. Only complete verified bytes
/// enter the preview; cache retention is the original gateway retention.
@MainActor final class DemoInspectionMedia: ObservableObject {
    @Published private(set) var values: [String: VerifiedEvidence] = [:]
    @Published private(set) var messages: [String: String] = [:]
    @Published private(set) var loading: Set<String> = []
    @Published private(set) var expired: Set<String> = []
    let notice: String?
    private let cache: InspectionMediaCache?
    private let client: WorldClient?

    init(cache: InspectionMediaCache?, client: WorldClient?, notice: String? = nil) {
        self.cache = cache; self.client = client; self.notice = notice
    }

    func load(_ reference: MissionInspectionEvidence) async {
        let id = reference.sha256
        expireLoaded()
        if let loaded = values[id] {
            guard loaded.data.count == reference.size, loaded.contentType == reference.contentType,
                  loaded.sha256 == id else {
                values.removeValue(forKey: id)
                messages[id] = "Media is unavailable. Try again when connected."
                return
            }
            return
        }
        guard !expired.contains(id), loading.insert(id).inserted else { return }
        defer { loading.remove(id) }
        do {
            try Task.checkCancellation()
            if let cache, let saved = try await cache.load(reference: reference) {
                try Task.checkCancellation()
                values[id] = saved; messages[id] = "Saved on this Mac"
                return
            }
            guard let client else {
                messages[id] = "Not saved on this Mac. No media connection is configured."
                return
            }
            let downloaded = try await client.downloadEvidence(digest: id, maxBytes: reference.size)
            guard downloaded.data.count == reference.size, downloaded.contentType == reference.contentType,
                  downloaded.sha256 == id else { throw WorldClientError.invalidEvidence }
            try Task.checkCancellation()
            if let cache {
                try await cache.store(downloaded, reference: reference)
                // The cache may retain an earlier original deadline for these
                // same bytes. Display that bound, never a refreshed copy.
                guard let retained = try await cache.load(reference: reference) else { throw InspectionMediaCacheError.expired }
                try Task.checkCancellation()
                values[id] = retained; messages[id] = "Saved on this Mac"
            } else {
                values[id] = downloaded; messages[id] = "Available now; not saved on this Mac"
            }
        } catch is CancellationError {
            messages[id] = "Loading canceled."
        } catch InspectionMediaCacheError.expired {
            values.removeValue(forKey: id); expired.insert(id)
            messages[id] = "Media retention expired. The inspection details remain saved."
        } catch {
            values.removeValue(forKey: id)
            messages[id] = "Media is unavailable. Try again when connected."
        }
    }

    func expireLoaded(now: Date = Date()) {
        for (id, value) in values where value.retainUntil <= now {
            values.removeValue(forKey: id); expired.insert(id)
            messages[id] = "Media retention expired. The inspection details remain saved."
        }
    }

    func value(for reference: MissionInspectionEvidence, now: Date = Date()) -> VerifiedEvidence? {
        guard let value = values[reference.sha256], value.retainUntil > now,
              value.sha256 == reference.sha256, value.contentType == reference.contentType,
              value.data.count == reference.size else { return nil }
        return value
    }

    func load(_ result: MissionInspectionResult) async {
        for sample in result.samples {
            guard !Task.isCancelled else { return }
            if let reference = sample.evidence { await load(reference) }
        }
    }

    static func image(_ data: Data) -> NSImage? {
        guard let source = CGImageSourceCreateWithData(data as CFData, nil),
              let properties = CGImageSourceCopyPropertiesAtIndex(source, 0, nil) as? [CFString: Any],
              let width = properties[kCGImagePropertyPixelWidth] as? NSNumber,
              let height = properties[kCGImagePropertyPixelHeight] as? NSNumber,
              1...4096 ~= width.intValue, 1...4096 ~= height.intValue else { return nil }
        return NSImage(data: data)
    }
}

struct DemoInspectionResultView: View {
    @State private var actions = TwinActions()

    let presentation: DemoInspectionPresentation
    @ObservedObject private var media: DemoInspectionMedia
    @Environment(\.dismiss) private var dismiss
    private let teal = Color(red: 0.08, green: 0.42, blue: 0.40)
    @State private var investigationShown = false

    init(presentation: DemoInspectionPresentation) {
        self.presentation = presentation; media = presentation.media
    }

    private var result: MissionInspectionResult { presentation.result }
    private var captured: String {
        ISO8601DateFormatter().string(from: Date(timeIntervalSince1970: Double(result.capturedUnixMS) / 1000))
    }
    private var position: String { result.position.map { String(format: "%.2f", $0) }.joined(separator: ", ") }

    var body: some View {
        Group {

        ScrollView { content }
            .frame(width: 820, height: 720)
            .background(Color(red: 0.97, green: 0.97, blue: 0.95)).preferredColorScheme(.light)
            .sheet(isPresented:$investigationShown){InvestigationPanel(model:DemoInvestigations.shared)}
            .task(id: result.taskID) { await media.load(result) }
            .onReceive(Timer.publish(every: 0.1, on: .main, in: .common).autoconnect()) { media.expireLoaded(now: $0) }

        }.task { await actions.run() }
}

    // The same content can be rendered headlessly without a native scroll view.
    var content: some View {
        VStack(alignment: .leading, spacing: 20) {
                HStack(alignment: .top) {
                    VStack(alignment: .leading, spacing: 5) {
                        Text("Inspection result").font(.title2.weight(.semibold))
                        Label("HISTORICAL SIMULATION", systemImage: "clock.arrow.circlepath")
                            .font(.caption.weight(.semibold)).foregroundStyle(teal)
                    }
                    Spacer()
                    Button("Save investigation") { investigationShown=true;actions.submit { [self] in await DemoInvestigations.shared.prepare(result)} }
                    Button("Close") { dismiss() }
                }
                Grid(alignment: .leading, horizontalSpacing: 20, verticalSpacing: 7) {
                    GridRow { Text("Captured at"); Text(captured).textSelection(.enabled) }
                    GridRow { Text("Simulation time"); Text(String(format: "%.3f seconds", Double(result.simulationTimeNS) / 1e9)).monospacedDigit() }
                    GridRow { Text("Position · X, Y, Z"); Text(position + " m").monospacedDigit() }
                }.font(.callout)
                Text("These samples were captured for the saved inspection. They do not describe the current scene.")
                    .font(.callout).foregroundStyle(.secondary)
                if let notice = media.notice { Text(notice).font(.caption).foregroundStyle(.secondary) }
                LazyVGrid(columns: [GridItem(.flexible()), GridItem(.flexible())], alignment: .leading, spacing: 16) {
                    ForEach(result.samples.filter { $0.kind == "camera" }, id: \.sensorID) { sample in
                        camera(sample)
                    }
                }
                ForEach(result.samples.filter { $0.kind == "gas" }, id: \.sensorID) { sample in
                    if let gas = sample.gas {
                        VStack(alignment: .leading, spacing: 7) {
                            Label("Gas reading", systemImage: "aqi.medium").font(.headline)
                            Text(String(format: "%.2f ± %.2f ppm", gas.valuePPM, gas.uncertaintyPPM))
                                .font(.system(size: 25, weight: .semibold, design: .rounded)).monospacedDigit()
                            Text(gas.species).font(.callout).textSelection(.enabled)
                            Text("Simulated measurement · " + sample.sensorID).font(.caption).foregroundStyle(.secondary)
                        }.frame(maxWidth: .infinity, alignment: .leading).padding(16).background(.white, in: RoundedRectangle(cornerRadius: 12))
                    }
                }
                ForEach(result.samples.filter { $0.kind == "audio" }, id: \.sensorID) { sample in
                    if let reference = sample.evidence {
                        DemoInspectionAudio(sample: sample, reference: reference, media: media)
                    }
                }
                DisclosureGroup("Record details") {
                    VStack(alignment: .leading, spacing: 7) {
                        Text("Gateway-retained result for the signed inspection instruction. Image and audio bytes are checked against their recorded SHA-256 digests.")
                        Text("Task: " + result.taskID)
                        Text("Run: " + result.runID)
                        Text("Frame: " + result.frameID + " / " + result.frameRevision)
                        Text("Capture sequence: " + String(result.sequence))
                        ForEach(result.samples, id: \.sensorID) { sample in
                            if let ref = sample.evidence {
                                Text(sample.sensorID + ": " + ref.sha256).font(.system(.caption, design: .monospaced))
                                    .fixedSize(horizontal: false, vertical: true)
                            }
                        }
                    }.font(.caption).textSelection(.enabled).padding(.top, 8)
                }
        }.padding(28)
            .foregroundStyle(Color(red: 0.14, green: 0.22, blue: 0.24))
            .background(Color(red: 0.97, green: 0.97, blue: 0.95)).preferredColorScheme(.light)
    }

    @ViewBuilder private func camera(_ sample: MissionInspectionSample) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            Label(sample.sensorID == "camera-a" ? "Camera A" : "Camera B", systemImage: "camera").font(.headline)
            if let reference = sample.evidence {
                if let value = media.value(for: reference), let image = DemoInspectionMedia.image(value.data) {
                    Image(nsImage: image).resizable().scaledToFit().frame(maxWidth: .infinity, maxHeight: 240)
                        .background(.black.opacity(0.03), in: RoundedRectangle(cornerRadius: 8))
                } else {
                    ZStack {
                        RoundedRectangle(cornerRadius: 8).fill(.black.opacity(0.04))
                        Image(systemName: "photo").font(.system(size: 32, weight: .light)).foregroundStyle(.secondary)
                    }.frame(height: 180)
                    if media.values[reference.sha256] != nil {
                        Text("Image bytes were verified but could not be displayed.").font(.caption).foregroundStyle(.secondary)
                    }
                }
                mediaStatus(reference)
            }
            if let camera = sample.camera { Text("\(camera.width) × \(camera.height) · inspection capture").font(.caption).foregroundStyle(.secondary) }
        }.frame(maxWidth: .infinity, alignment: .leading).padding(14).background(.white, in: RoundedRectangle(cornerRadius: 12))
    }

    @ViewBuilder private func mediaStatus(_ reference: MissionInspectionEvidence) -> some View {
        if media.loading.contains(reference.sha256) { ProgressView("Loading image…").controlSize(.small) }
        else if let status = media.messages[reference.sha256] { Text(status).font(.caption).foregroundStyle(.secondary) }
        if media.values[reference.sha256] == nil && !media.expired.contains(reference.sha256) {
            Button("Load image") { actions.submit { [self] in  await media.load(reference) } }.controlSize(.small)
                .disabled(media.loading.contains(reference.sha256))
        }
    }
}

private struct DemoInspectionAudio: View {
    @State private var actions = TwinActions()

    let sample: MissionInspectionSample
    let reference: MissionInspectionEvidence
    @ObservedObject var media: DemoInspectionMedia
    @State private var player: AVAudioPlayer?
    @State private var error: String?

    var body: some View {
        Group {

        VStack(alignment: .leading, spacing: 10) {
            Label("Inspection audio", systemImage: "waveform").font(.headline)
            if let audio = sample.audio {
                Text(String(format: "%.2f seconds · %d Hz · %d channel%@", Double(audio.durationNS) / 1e9,
                            audio.sampleRateHz, audio.channels, audio.channels == 1 ? "" : "s"))
                    .font(.callout).monospacedDigit()
            }
            if let value = media.value(for: reference) {
                TimelineView(.periodic(from: .now, by: 0.1)) { _ in
                    let playing = player?.isPlaying == true
                    Button {
                        guard value.retainUntil > Date() else { player?.stop(); media.expireLoaded(); return }
                        if player?.isPlaying == true { player?.stop() }
                        else {
                            do {
                                let sound = try AVAudioPlayer(data: value.data)
                                sound.prepareToPlay(); player = sound; error = nil
                                if !sound.play() { error = "Audio playback could not start." }
                            } catch { self.error = "Verified audio could not be decoded." }
                        }
                    } label: { Label(playing ? "Stop audio" : "Play audio", systemImage: playing ? "stop.fill" : "play.fill") }
                        .buttonStyle(.bordered)
                }
            } else if !media.expired.contains(reference.sha256) {
                Button("Load audio") { actions.submit { [self] in  await media.load(reference) } }.disabled(media.loading.contains(reference.sha256))
            }
            if media.loading.contains(reference.sha256) { ProgressView("Loading audio…").controlSize(.small) }
            if let status = media.messages[reference.sha256] { Text(status).font(.caption).foregroundStyle(.secondary) }
            if let error { Text(error).font(.caption).foregroundStyle(.secondary) }
            Text("Simulated recording · " + sample.sensorID).font(.caption).foregroundStyle(.secondary)
        }.frame(maxWidth: .infinity, alignment: .leading).padding(16).background(.white, in: RoundedRectangle(cornerRadius: 12))
            .onDisappear { player?.stop() }
            .onChange(of: media.values[reference.sha256]?.retainUntil) { _, deadline in
                if deadline == nil || deadline! <= Date() { player?.stop() }
            }

        }.task { await actions.run() }
}
}
