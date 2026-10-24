import SwiftUI

@MainActor
final class CameraFeed: ObservableObject, Identifiable {
    let id = UUID()
    let camera: Camera
    let decoder = H264AnnexBDecoder()
    @Published private(set) var running = false
    @Published private(set) var receivedVideo = false
    @Published private(set) var error: String?
    private(set) var stream: UInt64 = 0

    init(camera: Camera) { self.camera = camera }

    func start(stream: UInt64) {
        decoder.reset()
        self.stream = stream
        error = nil
        receivedVideo = false
        running = true
    }

    func pause() {
        stream = 0
        running = false
        receivedVideo = false
        error = nil
        decoder.reset()
    }

    func fail(_ message: String, stream: UInt64) {
        guard running, self.stream == stream else { return }
        error = message
        running = false
    }

    func receive(_ event: BridgeEvent) {
        guard running, stream == event.stream else { return }
        if case .cameraError(let message) = event.payload {
            fail(message, stream: event.stream)
        } else if case .video(let bytes) = event.payload {
            decoder.feed(bytes)
            if !receivedVideo { receivedVideo = true }
        }
    }
}

struct CameraWallView: View {
    @ObservedObject var model: AppModel

    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            HStack {
                Label("Camera wall", systemImage: "rectangle.grid.2x2").font(.headline)
                Text(String(model.cameraFeeds.count) + " cameras").foregroundStyle(.secondary)
                Spacer()
            }
            if let error = model.camerasError {
                FailureView(title: "Could not load cameras", error: error) { Task { await model.refreshDetails() } }
            } else if model.cameraFeeds.isEmpty {
                ContentUnavailableView("No cameras found", systemImage: "video.slash")
            } else {
                ScrollView {
                    LazyVGrid(columns: [GridItem(.adaptive(minimum: 360), spacing: 16)], spacing: 16) {
                        ForEach(model.cameraFeeds) { feed in
                            CameraTile(feed: feed,
                                       play: { Task { await model.playCamera(feed) } },
                                       pause: { model.pauseCamera(feed) })
                        }
                    }.padding(.bottom, 16)
                }
            }
        }
        .padding(.horizontal, 28).padding(.bottom, 12)
        .onAppear { model.showCameraWall() }
        .onDisappear { model.hideCameraWall() }
    }
}

private struct CameraTile: View {
    @ObservedObject var feed: CameraFeed
    let play: () -> Void
    let pause: () -> Void
    @StateObject private var fullscreen = CameraFullscreenController()

    var body: some View {
        VStack(spacing: 0) {
            CameraPlayer(feed: feed, play: play, pause: pause) {
                fullscreen.open(feed: feed, play: play, pause: pause)
            }
            .aspectRatio(4.0 / 3.0, contentMode: .fit)
            HStack {
                VStack(alignment: .leading, spacing: 4) {
                    Text(feed.camera.name).font(.headline).lineLimit(1).help(feed.camera.name)
                    Text(feed.error != nil ? "Unavailable" : feed.running ? (feed.receivedVideo ? "Streaming" : "Connecting") : "Paused")
                        .font(.caption).foregroundStyle(.secondary)
                }
                Spacer()
            }.padding(12)
        }
        .background(Color(nsColor: .controlBackgroundColor))
        .clipShape(RoundedRectangle(cornerRadius: 12))
        .overlay(RoundedRectangle(cornerRadius: 12).strokeBorder(.quaternary))
        .onDisappear { fullscreen.close() }
    }
}

struct CameraPlayer: View {
    @ObservedObject var feed: CameraFeed
    let play: () -> Void
    let pause: () -> Void
    var isFullScreen = false
    let toggleFullScreen: () -> Void
    @State private var hovering = false
    @FocusState private var focusedControl: Int?

    private var showControls: Bool { hovering || focusedControl != nil || !feed.running }

    var body: some View {
        ZStack {
            Color.black
            CameraDisplayView(decoder: feed.decoder)
            if let error = feed.error {
                VStack(spacing: 12) {
                    Image(systemName: "video.slash").font(.title)
                    Text(PresentedFailure(error).message).multilineTextAlignment(.center)
                    Button("Retry", action: play)
                    DisclosureGroup("Technical details") {
                        ScrollView {
                            Text(PresentedFailure(error).details).font(.caption.monospaced()).textSelection(.enabled)
                        }.frame(maxHeight: 65)
                    }.font(.caption)
                }.padding(20).frame(maxWidth: 480).background(.regularMaterial, in: RoundedRectangle(cornerRadius: 12))
            } else {
                if feed.running && !feed.receivedVideo {
                    ProgressView("Waiting for video…").colorScheme(.dark)
                }
                Button(action: feed.running ? pause : play) {
                    Image(systemName: feed.running ? "pause.fill" : "play.fill")
                        .font(.system(size: 24, weight: .semibold)).foregroundStyle(.white)
                        .frame(width: 64, height: 64)
                        .background(.black.opacity(0.55), in: Circle())
                        .overlay(Circle().strokeBorder(.white.opacity(0.3)))
                        .contentShape(Circle())
                }
                .buttonStyle(.plain).focused($focusedControl, equals: 1)
                .accessibilityLabel((feed.running ? "Pause " : "Play ") + feed.camera.name)
                .help(feed.running ? "Pause video" : "Play video")
                .opacity(showControls ? 1 : 0)
                .allowsHitTesting(showControls)
            }
        }
        .overlay(alignment: .bottomTrailing) {
            Button(action: toggleFullScreen) {
                Image(systemName: isFullScreen ? "arrow.down.right.and.arrow.up.left" : "arrow.up.left.and.arrow.down.right")
                    .font(.system(size: 16, weight: .semibold)).foregroundStyle(.white)
                    .frame(width: 40, height: 40)
                    .background(.black.opacity(0.55), in: Circle())
                    .contentShape(Circle())
            }
            .buttonStyle(.plain).focused($focusedControl, equals: 2)
            .accessibilityLabel((isFullScreen ? "Exit full screen " : "Full screen ") + feed.camera.name)
            .help(isFullScreen ? "Exit full screen" : "Full screen")
            .padding(14).opacity(showControls ? 1 : 0).allowsHitTesting(showControls)
        }
        .clipped().onHover { hovering = $0 }
        .animation(.easeInOut(duration: 0.15), value: showControls)
    }
}

@MainActor
final class CameraFullscreenController: NSObject, ObservableObject, NSWindowDelegate {
    private var window: NSWindow?

    func open(feed: CameraFeed, play: @escaping () -> Void, pause: @escaping () -> Void) {
        if let window { window.makeKeyAndOrderFront(nil); return }
        let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 960, height: 720),
                              styleMask: [.titled, .closable, .resizable], backing: .buffered, defer: false)
        window.title = feed.camera.name
        window.backgroundColor = .black
        window.isReleasedWhenClosed = false
        window.collectionBehavior = [.fullScreenPrimary]
        window.delegate = self
        window.contentView = NSHostingView(rootView:
            CameraPlayer(feed: feed, play: play, pause: pause, isFullScreen: true) { [weak self] in self?.close() }
                .frame(maxWidth: .infinity, maxHeight: .infinity)
                .onExitCommand { [weak self] in self?.close() })
        self.window = window
        window.center()
        window.makeKeyAndOrderFront(nil)
        window.toggleFullScreen(nil)
    }
    func close() { window?.close() }
    func windowDidExitFullScreen(_ notification: Notification) { close() }
    func windowWillClose(_ notification: Notification) {
        window?.contentView = nil
        window = nil
    }
}
