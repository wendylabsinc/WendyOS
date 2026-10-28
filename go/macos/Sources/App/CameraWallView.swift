import SwiftUI
import ImageIO

@MainActor
final class CameraFeed: ObservableObject, Identifiable {
    let id = UUID()
    let camera: Camera
    let decoder = H264AnnexBDecoder()
    @Published private(set) var jpeg: CGImage?
    @Published private(set) var thermalFrame: ThermalCameraFrame?
    @Published var thermalScale: ThermalCameraScale = .counts
    private(set) var lastVideoReceivedAt: Date?
    private var jpegReceivedAt: Date?
    private var jpegSequence: UInt64 = 0
    private(set) var thermalReceivedAt = Date.distantPast
    @Published private(set) var running = false
    @Published private(set) var receivedVideo = false
    @Published private(set) var error: String?
    private(set) var stream: UInt64 = 0
    private(set) var manuallyPaused = false
    private var startedAt = Date.distantPast
    private var startAttempts = 0
    lazy var colorSource = SiteCameraColorSource(frame: { [weak self] in
        guard let self, self.running, !self.camera.thermal else { return nil }
        if let image = self.jpeg, let received = self.jpegReceivedAt {
            return SiteCameraColorFrame(image: image, received: received, sequence: self.jpegSequence)
        }
        return self.decoder.calibrationFrames.latestColor
    }, acquire: { [weak self] in
        self?.decoder.calibrationFrames.acquireColor() ?? UUID()
    }, release: { [weak self] token in
        self?.decoder.calibrationFrames.releaseColor(token)
    }, unavailableReason: { [weak self] in
        guard let self else { return "Camera unavailable." }
        if self.manuallyPaused { return "Camera paused. Resume its video to add color." }
        if let error = self.error { return PresentedFailure(error).message }
        return self.running ? nil : "Starting camera…"
    })

    func shouldAutoStart(at now: Date) -> Bool {
        guard camera.thermal, !manuallyPaused else { return false }
        if running {
            return now.timeIntervalSince(lastVideoReceivedAt ?? startedAt) >= 15
        }
        return now.timeIntervalSince(startedAt) >= min(30, pow(2, Double(min(startAttempts, 5))))
    }

    func allowAutoplay() { manuallyPaused = false }


    init(camera: Camera) { self.camera = camera }

    func start(stream: UInt64) {
        manuallyPaused = false
        startedAt = Date()
        startAttempts += 1
        decoder.reset()
        jpeg = nil
        jpegReceivedAt = nil
        thermalFrame = nil
        self.stream = stream
        error = nil
        receivedVideo = false
        lastVideoReceivedAt = nil
        running = true
    }

    func pause(manually: Bool = false) {
        if manually { manuallyPaused = true }
        stream = 0
        running = false
        receivedVideo = false
        lastVideoReceivedAt = nil
        error = nil
        decoder.reset()
        jpeg = nil
        jpegReceivedAt = nil
        thermalFrame = nil
    }

    func fail(_ message: String, stream: UInt64) {
        guard running, self.stream == stream else { return }
        error = message
        running = false
        if thermalFrame != nil { jpeg = nil; thermalFrame = nil; receivedVideo = false }
    }

    func calibrationFrame() -> CameraCalibrationFrame? {
        guard running else { return nil }
        if let jpeg, let jpegReceivedAt {
            return CameraCalibrationFrame.jpeg(jpeg, received: jpegReceivedAt, sequence: jpegSequence)
        }
        return decoder.calibrationFrames.latest
    }

    func receive(_ event: BridgeEvent) {
        guard running, stream == event.stream else { return }
        if case .cameraError(let message) = event.payload {
            fail(message, stream: event.stream)
        } else if case .thermal(let frame) = event.payload {
            thermalFrame = frame
            thermalReceivedAt = Date()
            startAttempts = 0
            receivedVideo = true
            lastVideoReceivedAt = Date()
        } else if case .jpeg(let bytes) = event.payload {
            guard let source = CGImageSourceCreateWithData(bytes as CFData, nil),
                  let properties = CGImageSourceCopyPropertiesAtIndex(source, 0, nil) as? [CFString: Any],
                  let width = properties[kCGImagePropertyPixelWidth] as? Int,
                  let height = properties[kCGImagePropertyPixelHeight] as? Int,
                  width > 0, height > 0, width <= 8192, height <= 8192, width * height <= 32_000_000,
                  let image = CGImageSourceCreateImageAtIndex(source, 0, nil) else {
                fail("The camera sent an invalid JPEG frame.", stream: event.stream); return
            }
            jpeg = image
            jpegReceivedAt = Date()
            jpegSequence &+= 1
            receivedVideo = true
            lastVideoReceivedAt = Date()
        } else if case .video(let bytes) = event.payload {
            jpeg = nil
            jpegReceivedAt = nil
            lastVideoReceivedAt = Date()
            decoder.feed(bytes)
            if !receivedVideo { receivedVideo = true }
        }
    }
}

struct CameraFeedDisplay: View {
    @ObservedObject var feed: CameraFeed
    var body: some View {
        if let frame = feed.thermalFrame {
            ThermalCameraDisplay(feed: feed, frame: frame)
        } else if let image = feed.jpeg {
            Image(decorative: image, scale: 1).resizable().scaledToFit()
        } else {
            CameraDisplayView(decoder: feed.decoder)
        }
    }
}

#if os(macOS)
struct CameraWallView: View {
    @State private var actions = AsyncActions()
    @ObservedObject var model: AppModel

    var body: some View {
        actionContent.task { await actions.run() }
    }

    @ViewBuilder private var actionContent: some View {
        VStack(alignment: .leading, spacing: 16) {
            HStack {
                Label("Camera wall", systemImage: "rectangle.grid.2x2").font(.headline)
                Text(String(model.cameraFeeds.count) + " cameras").foregroundStyle(.secondary)
                Spacer()
            }
            if let error = model.camerasError {
                FailureView(title: "Could not load cameras", error: error) { actions.submit { await model.refreshDetails() } }
            } else if model.cameraFeeds.isEmpty {
                ContentUnavailableView("No cameras found", systemImage: "video.slash")
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
            } else {
                ScrollView {
                    LazyVGrid(columns: [GridItem(.adaptive(minimum: 360), spacing: 16)], spacing: 16) {
                        ForEach(model.cameraFeeds) { feed in
                            CameraTile(feed: feed,
                                       play: { actions.submit { await model.playCamera(feed) } },
                                       pause: { model.pauseCamera(feed) })
                        }
                    }.padding(.bottom, 16)
                }
            }
        }
        .padding(.horizontal, 28).padding(.bottom, 12)
        .task { await model.showCameraWall() }
        .onDisappear { model.hideCameraWall() }
    }
}

private struct CameraTile: View {
    @ObservedObject var feed: CameraFeed
    let play: () -> Void
    let pause: () -> Void
    @StateObject private var fullscreen = CameraFullscreenController()

    var body: some View {
        CameraPlayer(feed: feed, play: play, pause: pause) {
            fullscreen.open(feed: feed, play: play, pause: pause)
        }
        .aspectRatio(4.0 / 3.0, contentMode: .fit)
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
    @State private var showingDetails = false
    @FocusState private var focusedControl: Int?

    private var showControls: Bool { hovering || focusedControl != nil || !feed.running }

    private var status: String {
        if let error = feed.error {
            return error.localizedCaseInsensitiveContains("not sending video") ? "No video received" : "Camera unavailable"
        }
        return feed.running ? (feed.receivedVideo ? "Live" : "Connecting…") : "Paused"
    }

    var body: some View {
        ZStack {
            Color.black
            CameraFeedDisplay(feed: feed)
            if feed.error != nil {
                Image(systemName: "video.slash")
                    .font(.system(size: 32, weight: .light)).foregroundStyle(.white.opacity(0.45))
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
        .overlay(alignment: .bottom) {
            HStack(alignment: .center, spacing: 12) {
                VStack(alignment: .leading, spacing: 4) {
                    Text(feed.camera.name).font(.headline).lineLimit(2).help(feed.camera.name)
                    Text(status).font(.caption).foregroundStyle(.white.opacity(0.7))
                }.frame(maxWidth: .infinity, alignment: .leading)
                if let error = feed.error {
                    Button("Retry", action: play).buttonStyle(.bordered)
                    let failure = PresentedFailure(error)
                    if failure.details != failure.message {
                        Button { showingDetails = true } label: { Image(systemName: "info.circle") }
                            .buttonStyle(.plain).help("Camera error details")
                            .accessibilityLabel("Camera error details")
                            .popover(isPresented: $showingDetails) {
                                ScrollView { Text(failure.details).font(.caption.monospaced()).textSelection(.enabled).padding(16) }
                                    .frame(width: 360, height: 180)
                            }
                    }
                }
                Button(action: toggleFullScreen) {
                    Image(systemName: isFullScreen ? "arrow.down.right.and.arrow.up.left" : "arrow.up.left.and.arrow.down.right")
                        .font(.system(size: 15, weight: .semibold))
                        .frame(width: 32, height: 32).contentShape(Rectangle())
                }
                .buttonStyle(.plain).focused($focusedControl, equals: 2)
                .accessibilityLabel((isFullScreen ? "Exit full screen " : "Full screen ") + feed.camera.name)
                .help(isFullScreen ? "Exit full screen" : "Full screen")
                .opacity(showControls ? 1 : 0).allowsHitTesting(showControls)
            }
            .foregroundStyle(.white).padding(16).padding(.top, 28)
            .background(LinearGradient(colors: [.clear, .black.opacity(0.85)], startPoint: .top, endPoint: .bottom))
            .opacity(feed.thermalFrame == nil || showControls ? 1 : 0)
            .allowsHitTesting(feed.thermalFrame == nil || showControls)
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

#endif
