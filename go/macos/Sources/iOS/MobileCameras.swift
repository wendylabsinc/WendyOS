import SwiftUI

struct CameraWallView: View {
    @Environment(\.scenePhase) private var scenePhase
    @ObservedObject var model: AppModel
    @State private var fullscreen: CameraFeed?
    @State private var settings: CameraFeed?

    var body: some View {
        ScrollView {
            LazyVStack(alignment: .leading, spacing: 16) {
                if let error = model.camerasError { FailureNote(message: error) }
                else if model.cameraFeeds.isEmpty { ContentUnavailableView("No cameras attached", systemImage: "video.slash") }
                ForEach(model.cameraFeeds) { feed in
                    MobileCameraPlayer(feed: feed, model: model, expand: { fullscreen = feed })
                        .aspectRatio(4.0 / 3.0, contentMode: .fit)
                        .clipShape(.rect(cornerRadius: 18))
                }
                if !model.cameraFeeds.isEmpty {
                    Text("CAMERAS").font(.footnote.weight(.semibold)).foregroundStyle(.secondary).padding(.horizontal, 16)
                    VStack(spacing: 0) {
                        ForEach(model.cameraFeeds) { feed in
                            MobileCameraSettingsLink(feed: feed) { settings = feed }
                            if feed.id != model.cameraFeeds.last?.id { Divider().padding(.leading, 70) }
                        }
                    }.background(RecordingAppearance.card, in: .rect(cornerRadius: 22))
                    Text("Tap a camera to view its available controls.").font(.footnote).foregroundStyle(.secondary).padding(.horizontal, 16)
                }
            }.padding(16)
        }
        .background(MobileLensStyle.background)
        .refreshable { await model.refreshDetails() }
        .scrollBounceBehavior(.always, axes: .vertical)
        .task(id: scenePhase) {
            if scenePhase == .active { await model.showCameraWall() }
            else if scenePhase == .background { model.hideCameraWall() }
        }
        .onDisappear { if fullscreen == nil { model.hideCameraWall() } }
        .onChange(of: model.deviceSession) { _, _ in fullscreen = nil; settings = nil }
        .onChange(of: model.cameraFeeds.map(\.id)) { _, ids in
            if let fullscreen, !ids.contains(fullscreen.id) { self.fullscreen = nil }
            if let settings, !ids.contains(settings.id) { self.settings = nil }
        }
        .fullScreenCover(item: $fullscreen) { feed in
            MobileCameraFullscreen(feed: feed, model: model)
        }
        .sheet(item: $settings) { feed in
            MobileCameraSettings(feed: feed, model: model, session: model.deviceSession)
        }
    }
}

private struct MobileCameraSettingsLink: View {
    @ObservedObject var feed: CameraFeed
    let open: () -> Void
    var body: some View {
        Button(action: open) {
            HStack(spacing: 12) {
                Image(systemName: "video.fill").foregroundStyle(Color.white)
                    .frame(width: 42, height: 42).background(Color.cyan, in: RoundedRectangle(cornerRadius: 11))
                Text(feed.camera.name).font(.body.weight(.semibold)).lineLimit(2)
                    .frame(maxWidth: .infinity, alignment: .leading)
                Image(systemName: "chevron.right").font(.footnote.weight(.semibold)).foregroundStyle(Color.secondary)
            }.padding(16).contentShape(Rectangle())
        }.buttonStyle(.plain).accessibilityLabel("Settings for \(feed.camera.name)")
    }
}

private struct MobileCameraPlayer: View {
    @ObservedObject var feed: CameraFeed
    @ObservedObject var model: AppModel
    var expand: (() -> Void)?
    @State private var actions = AsyncActions()
    @State private var showingError = false

    var body: some View {
        ZStack {
            Color.black
            CameraFeedDisplay(feed: feed)
            if feed.running && !feed.receivedVideo { ProgressView("Waiting for video…").tint(.white).foregroundStyle(.white) }
            if !feed.running { Image(systemName: feed.error == nil ? "pause.fill" : "video.slash").font(.largeTitle).foregroundStyle(.white.opacity(0.6)) }
        }
        .contentShape(Rectangle())
        .onTapGesture { expand?() }
        .overlay(alignment: .bottom) {
            VStack(alignment: .leading, spacing: 6) {
                HStack(alignment: .center, spacing: 8) {
                    VStack(alignment: .leading, spacing: 4) {
                        Text(feed.camera.name).font(.subheadline.weight(.semibold)).lineLimit(2)
                        if let error = feed.error {
                            Button { showingError = true } label: {
                                Label(PresentedFailure(error).message, systemImage: "exclamationmark.triangle").font(.caption).lineLimit(2)
                            }.buttonStyle(.plain)
                        } else {
                            Text(feed.running ? (feed.receivedVideo ? "Live" : "Connecting…") : "Paused").font(.caption)
                        }
                    }.frame(maxWidth: .infinity, alignment: .leading)
                    Button {
                        if feed.running { model.pauseCamera(feed) } else { actions.submit { await model.playCamera(feed) } }
                    } label: {
                        Image(systemName: feed.error != nil ? "arrow.clockwise" : feed.running ? "pause.fill" : "play.fill")
                            .frame(width: 44, height: 44).background(.black.opacity(0.5), in: Circle())
                    }.buttonStyle(.plain)
                        .accessibilityLabel("\(feed.error != nil ? "Retry" : feed.running ? "Pause" : "Play") \(feed.camera.name)")
                    if let expand {
                        Button(action: expand) { Image(systemName: "arrow.up.left.and.arrow.down.right").frame(width: 44, height: 44) }
                            .buttonStyle(.plain).accessibilityLabel("Full screen \(feed.camera.name)")
                    }
                }
                TimelineView(.periodic(from: .now, by: 1)) { context in
                    HStack {
                        if let resolution = feed.resolution { Text(resolution) }
                        Spacer(minLength: 8)
                        if let received = feed.lastVideoReceivedAt {
                            Text("Received \(received.formatted(date: .omitted, time: .standard))")
                            if feed.running && context.date.timeIntervalSince(received) > 5 { Text("Stale").foregroundStyle(.yellow) }
                        }
                    }.font(.caption2.monospacedDigit()).foregroundStyle(.white.opacity(0.85))
                }
            }.foregroundStyle(.white).padding(12).padding(.top, 24)
                .background(LinearGradient(colors: [.clear, .black.opacity(0.85)], startPoint: .top, endPoint: .bottom))
        }
        .clipped()
        .task { await actions.run() }
        .alert("Camera unavailable", isPresented: $showingError) { Button("Close", role: .cancel) {} } message: { Text(feed.error ?? "No current error") }
    }
}

private struct MobileCameraFullscreen: View {
    @Environment(\.dismiss) private var dismiss
    @Environment(\.scenePhase) private var scenePhase
    @ObservedObject var feed: CameraFeed
    @ObservedObject var model: AppModel
    var body: some View {
        ZStack(alignment: .topTrailing) {
            Color.black.ignoresSafeArea()
            MobileCameraPlayer(feed: feed, model: model)
            Button { dismiss() } label: {
                Image(systemName: "xmark").font(.headline).frame(width: 44, height: 44).background(.black.opacity(0.6), in: Circle())
            }.foregroundStyle(.white).padding().accessibilityLabel("Close full screen")
        }
        .task(id: scenePhase) {
            if scenePhase == .active { await model.showCameraWall() }
            else if scenePhase == .background { model.hideCameraWall() }
        }
        .onChange(of: model.cameraFeeds.map(\.id)) { _, ids in if !ids.contains(feed.id) { dismiss() } }
    }
}

struct MobileCameraControl: Sendable, Identifiable {
    let name: String
    let value: Int32
    let minimum: Int32
    let maximum: Int32
    let step: Int32
    let mutable: Bool
    var id: String { name }
    var title: String { name.replacingOccurrences(of: "_", with: " ").capitalized }
}

extension GoClient {
    func cameraControls(camera: Camera, session: UInt64, name: String = "", value: Int32 = 0) async throws -> [MobileCameraControl] {
        try await perform {
            try Self.withCString(camera.stableID) { stable in
                try Self.withCString(name) { key in
                    guard let p = WDCameraControls(session, camera.id, stable, key, value) else { throw WendyError(message: "Missing camera controls") }
                    defer { WDCameraControlsFree(p) }
                    try Self.check(p.pointee.status)
                    return UnsafeBufferPointer(start: p.pointee.items, count: p.pointee.count).map {
                        MobileCameraControl(name: Self.string($0.name), value: $0.value, minimum: $0.minimum, maximum: $0.maximum, step: $0.step, mutable: $0.mutable != 0)
                    }
                }
            }
        }
    }
}

private struct MobileCameraSettings: View {
    @Environment(\.dismiss) private var dismiss
    @ObservedObject var feed: CameraFeed
    @ObservedObject var model: AppModel
    let session: UInt64
    @State private var controls: [MobileCameraControl] = []
    @State private var loading = true
    @State private var saving = false
    @State private var error: String?
    @State private var actions = AsyncActions()

    private var current: Bool { model.deviceSession == session && model.selectedDeviceReady && model.cameraFeeds.contains { $0 === feed } }
    var body: some View {
        NavigationStack {
            Form {
                Section("Stream") {
                    LabeledContent("Resolution", value: feed.resolution ?? "Waiting for video")
                    Text("Resolution is reported by the live stream. Capture format selection is not available on this connection.").font(.footnote).foregroundStyle(.secondary)
                }
                Section("Camera controls") {
                    if loading { ProgressView("Loading controls…") }
                    else if let error {
                        FailureNote(message: error)
                        Button("Retry") { actions.submit { await load() } }
                    } else if controls.isEmpty {
                        Text("This camera does not expose adjustable controls.").foregroundStyle(.secondary)
                    } else {
                        ForEach(controls) { control in
                            MobileCameraControlRow(control: control, disabled: saving || !current) { value in
                                actions.submit { await load(name: control.name, value: value) }
                            }
                        }
                        Text("Values and ranges come from the camera driver. Changes apply now; they are not saved across device restarts.").font(.footnote).foregroundStyle(.secondary)
                    }
                }
            }.scrollContentBackground(.hidden).background(MobileLensStyle.background)
                .navigationTitle(feed.camera.name).navigationBarTitleDisplayMode(.inline)
                .toolbar { ToolbarItem(placement: .confirmationAction) { Button("Done") { dismiss() } } }
        }
        .task { await load() }
        .task { await actions.run() }
        .onChange(of: model.deviceSession) { _, _ in dismiss() }
    }
    private func load(name: String = "", value: Int32 = 0) async {
        guard current, !saving else { return }
        saving = true
        defer { saving = false; loading = false }
        do {
            let result = try await GoClient.shared.cameraControls(camera: feed.camera, session: session, name: name, value: value)
            guard current, !Task.isCancelled else { return }
            controls = result
            error = nil
        } catch {
            guard current, !Task.isCancelled else { return }
            self.error = PresentedFailure(error.localizedDescription).message
        }
    }
}

private struct MobileCameraControlRow: View {
    let control: MobileCameraControl
    let disabled: Bool
    let save: (Int32) -> Void
    @State private var value = ""
    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack {
                Text(control.title).font(.headline)
                Spacer()
                Text("Current \(control.value)").font(.caption).foregroundStyle(.secondary)
            }
            if control.mutable && control.minimum < control.maximum {
                HStack {
                    TextField("Value", text: $value).keyboardType(.numbersAndPunctuation)
                        .textFieldStyle(.roundedBorder).accessibilityLabel(control.title)
                    Button("Apply") { if let parsed = Int32(value) { save(parsed) } }
                        .disabled(disabled || !valid || Int32(value) == control.value)
                }
                Text("Range \(control.minimum)…\(control.maximum), step \(max(1, control.step))").font(.caption).foregroundStyle(.secondary)
            } else { LabeledContent("Read only", value: String(control.value)) }
        }
        .onAppear { value = String(control.value) }
        .onChange(of: control.value) { _, new in value = String(new) }
    }
    private var valid: Bool {
        guard let number = Int32(value), number >= control.minimum, number <= control.maximum else { return false }
        return (Int64(number) - Int64(control.minimum)) % Int64(max(1, control.step)) == 0
    }
}
