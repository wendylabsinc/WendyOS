import SwiftUI

// A tile owns its stream, independently of the device detail connection. The
// full-screen presentation observes the same feed instead of opening a second one.
struct IndependentCameraWidget: View {
    let device: Device
    let resource: String
    let title: String
    let enabled: Bool
    @Environment(\.scenePhase) private var phase
    @State private var playing = false
    @State private var expanded = false
    @State private var onScreen = true
    @StateObject private var preview = IndependentCameraPreview()
    private var running: Bool { playing && phase == .active && (enabled || expanded) && (onScreen || expanded) }
    var body: some View {
        surface
            .clipShape(.rect(cornerRadius: 12))
            .onScrollVisibilityChange(threshold: 0.05) { onScreen = $0 }
            .task(id: running) { if running { await preview.run(device: device, resource: resource) } }
            .fullScreenCover(isPresented: $expanded) {
                ZStack(alignment: .topTrailing) {
                    Color.black.ignoresSafeArea()
                    surface
                    Button("Close", systemImage: "xmark") { expanded = false }
                        .labelStyle(.iconOnly).padding().background(.ultraThinMaterial, in: .circle).padding()
                }.preferredColorScheme(.dark)
            }
    }
    private var surface: some View {
        ZStack {
            Color.black
            if let feed = preview.feed { CameraFeedDisplay(feed: feed) }
            if preview.feed == nil {
                VStack(spacing: 8) {
                    if playing && preview.error == nil { ProgressView().tint(.white) }
                    else { Image(systemName: "play.circle.fill").font(.largeTitle) }
                    Text(preview.error ?? title).font(.caption).lineLimit(2)
                }.foregroundStyle(.white).padding(8)
            }
            VStack {
                HStack {
                    Button(playing ? "Stop" : "Watch camera") { playing.toggle() }
                        .font(.caption.bold()).padding(8).background(.black.opacity(0.65), in: .capsule)
                        .disabled(!enabled && !expanded)
                    Spacer()
                }
                Spacer(minLength: 10)
                HStack {
                    Spacer()
                    if !expanded {
                        Button { playing = true; expanded = true } label: {
                            Image(systemName: "arrow.up.left.and.arrow.down.right").frame(width: 44, height: 44)
                        }.background(.black.opacity(0.65), in: .circle)
                            .accessibilityLabel("Open camera full screen").disabled(!enabled)
                    }
                }
            }.padding(6).foregroundStyle(.white)
        }.frame(maxWidth: .infinity, maxHeight: .infinity).frame(minHeight: 80)
    }
}

@MainActor
final class IndependentCameraPreview: ObservableObject {
    @Published private(set) var feed: CameraFeed?
    @Published private(set) var error: String?
    private let queue = DispatchQueue(label: "dev.wendy.mobile.widget-preview", qos: .userInitiated)
    func run(device: Device, resource: String) async {
        let handle = WDPreviewCreate()
        guard handle != 0 else { error = "Could not start camera."; return }
        error = nil
        await withTaskCancellationHandler {
            do {
                let cameras = try await BlockingWork.run(on: queue) {
                    try GoClient.withCString(device.selector) {
                        try GoClient.readCameras(WDPreviewCameras(handle, $0, device.requiresMTLS ? 1 : 0))
                    }
                }
                let matches = cameras.filter { $0.stableID == resource }
                guard matches.count == 1, let camera = matches.first else {
                    throw WendyError(message: "Camera unavailable. Edit this widget to choose another.")
                }
                try Task.checkCancellation()
                let current = CameraFeed(camera: camera)
                current.start(stream: handle); feed = current
                try await BlockingWork.run(on: queue) {
                    try GoClient.withCString(resource) { try GoClient.finish(WDPreviewPlay(handle, $0)) }
                }
                while !Task.isCancelled {
                    let events = try await BlockingWork.run(on: queue) { try GoClient.readEvents(WDPreviewPoll(handle)) }
                    for event in events { current.receive(event) }
                    if let failure = current.failure { throw failure }
                    if current.shouldAutoStart(at: Date()) { throw WendyError(message: "Camera stopped sending frames. Tap Stop, then Watch camera to retry.") }
                    try await Task.sleep(for: .milliseconds(33))
                }
            } catch {
                if !Task.isCancelled { self.error = PresentedFailure(error.localizedDescription).message }
            }
        } onCancel: { WDPreviewCancel(handle) }
        if feed?.stream == handle { feed?.pause(); feed = nil }
        _ = try? await BlockingWork.run(on: queue, unlessCancelled: false) { WDPreviewClose(handle) }
    }
}
