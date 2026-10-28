import SwiftUI
import CoreGraphics
import simd

struct SiteAlignmentRequest: Identifiable {
    let id = UUID()
    let site: StoredSite
    let camera: SiteCamera
    let gatewayID: String
    let session: UInt64
}

struct SiteCameraAlignmentView: View {
    @ObservedObject var model: AppModel
    let request: SiteAlignmentRequest
    @Environment(\.dismiss) private var dismiss
    @State private var actions = AsyncActions()
    @State private var busy = false
    @State private var message: String?
    @State private var saved: SiteLocalizationResult?
    @State private var engine = SiteLocalizationEngine()
    @State private var loaded = false
    @State private var previewReady = false
    @State private var attemptedAutomaticAlignment = false
    @State private var screenCalibration = false
    @State private var measuredLens: SiteCameraLens?
    @State private var lensRevision: UInt64 = 0
    @State private var lensSession: UInt64 = 0
    @State private var refreshingCamera = false

    private var onCameraGateway: Bool {
        !request.gatewayID.isEmpty && request.gatewayID == request.camera.gatewayID
            && request.session != 0 && request.session == model.deviceSession
    }

    private var feed: CameraFeed? {
        guard onCameraGateway else { return nil }
        let matching = model.cameraFeeds.filter { $0.camera.stableID == request.camera.sourceID }
        return matching.count == 1 ? matching.first : nil
    }

    var body: some View {
        NavigationStack {
            ScrollView {
                VStack(alignment: .leading, spacing: 18) {
                    Text(SiteCameraLabel(name: request.camera.name, sourceID: request.camera.sourceID).title).font(.title2.bold())
                    if !onCameraGateway {
                        Label("Connect to this camera's gateway", systemImage: "network.slash").font(.headline)
                        Text("This camera belongs to a different gateway or the connection has changed. Open its gateway, then choose Calibrate and Align again.")
                    } else if request.site.visualMapURL == nil {
                        Label("Room scan needed", systemImage: "iphone.gen3.radiowaves.left.and.right").font(.headline)
                        Text("This model has no visual references. On your iPhone, open this camera’s menu and choose Calibrate and Align to scan the area it sees. Then export the site from the Sites menu and import the .wendysite file here. Your existing capture is kept.")
                    } else if let feed {
                        AlignmentLivePreview(model: model, feed: feed) { previewReady = true }
                            .frame(maxWidth: .infinity).aspectRatio(4/3, contentMode: .fit)
                            .background(.black).clipShape(RoundedRectangle(cornerRadius: 12))
                        Text("Keep the fixed camera still. Alignment uses a measured lens when available. Otherwise it can estimate the view using an assumed lens; estimated direction and FOV are less accurate.")
                        if let saved {
                            Label(saved.lens.measured ? "Calibrated alignment saved" : "Estimated alignment saved", systemImage: saved.lens.measured ? "checkmark.circle.fill" : "viewfinder")
                                .foregroundStyle(saved.lens.measured ? Color.green : Color.orange)
                            if !saved.lens.measured { Text("Approximate position, direction and FOV. The lens has not been measured.").font(.callout).foregroundStyle(.secondary) }
                            if let horizontal = saved.lens.horizontalFOV, let vertical = saved.lens.verticalFOV {
                                Text(String(format: "Field of view %.1f° × %.1f°", horizontal, vertical))
                            }
                            Text(String(format: "Direction %.1f° · Tilt %.1f°", saved.pose.yaw, saved.pose.pitch))
                            Text(String(format: "%d matches · %.2f px validation error", saved.inliers, saved.residual)).font(.caption).foregroundStyle(.secondary)
                        } else if attemptedAutomaticAlignment {
                            Button("Retry Automatic Alignment", systemImage: "viewfinder") { actions.submit { await align() } }
                                .buttonStyle(.borderedProminent).disabled(!loaded || busy)
                        }
                        #if os(iOS)
                        Button("Phone Screen Calibration", systemImage: "iphone") { actions.submit { await startScreenCalibration() } }
                            .buttonStyle(.bordered).disabled(busy)
                        Text("For better accuracy, show a pattern on this phone to the camera. The room can stay as it is.")
                            .font(.caption).foregroundStyle(.secondary)
                        #else
                        Text("For a measured lens, use Phone Screen Calibration in the iPhone app. The lens calibration is saved to the gateway.")
                            .font(.caption).foregroundStyle(.secondary)
                        #endif
                        if busy { ProgressView("Locating camera in the room…") }
                        else if !loaded && message == nil { ProgressView("Loading room references…") }
                    } else {
                        Label("Camera unavailable", systemImage: "video.slash").font(.headline)
                        Text("This camera is saved in the room, but the gateway is not currently listing a live feed. Check that it is connected and paired with this gateway.")
                        if let error = model.camerasError { Text(error).foregroundStyle(.secondary) }
                        Button("Refresh cameras", systemImage: "arrow.clockwise") {
                            actions.submit { await refreshCamera() }
                        }
                        .buttonStyle(.borderedProminent)
                        .disabled(refreshingCamera)
                        if refreshingCamera { ProgressView("Looking for camera…") }
                    }
                    if let message {
                        Text(message).foregroundStyle(.secondary).textSelection(.enabled)
                        if saved == nil {
                            Text("No position was assigned. You can retry with clearer room references, or use Place Manually from the camera menu.")
                                .font(.caption).foregroundStyle(.secondary)
                        }
                    }
                }.padding(20)
            }
            .navigationTitle("Camera Alignment")
            .toolbar { ToolbarItem(placement: .confirmationAction) { Button("Done") { dismiss() }.disabled(busy) } }
        }
        #if os(macOS)
        .frame(width: 600, height: 680)
        #endif
        #if os(iOS)
        .fullScreenCover(isPresented: $screenCalibration, onDismiss: {
            if let lens = measuredLens { measuredLens = nil; actions.submit { await saveLens(lens) } }
        }) {
            if let feed { PhoneLensCalibrationView(model: model, feed: feed) { measuredLens = $0 } }
        }
        #endif
        .interactiveDismissDisabled(busy)
        .task { await actions.run() }
        .task { if feed == nil { await refreshCamera() } }
        .task(id: loaded && previewReady) {
            guard loaded, previewReady, !attemptedAutomaticAlignment else { return }
            attemptedAutomaticAlignment = true
            await align()
        }
        .task {
            do {
                try await SiteCameraAlignment.load(request.site, into: engine)
                try Task.checkCancellation()
                loaded = true
            } catch is CancellationError {} catch { message = error.localizedDescription }
        }
    }

    private func refreshCamera() async {
        guard onCameraGateway, !refreshingCamera else { return }
        refreshingCamera = true
        defer { refreshingCamera = false }
        await model.refreshDetails()
    }

    private func align() async {
        guard let feed, !busy else { return }
        busy = true; message = nil
        defer { busy = false }
        do {
            saved = try await SiteCameraAlignment.align(model: model, request: request, feed: feed, engine: engine)
        } catch is CancellationError {} catch { message = error.localizedDescription }
    }

    private func startScreenCalibration() async {
        guard onCameraGateway, feed != nil, !busy else { return }
        busy = true; message = nil
        defer { busy = false }
        let session = model.deviceSession
        do {
            let records = try await GoClient.shared.cameraMetadata(session: session)
            try Task.checkCancellation()
            guard model.deviceSession == session else { throw SiteLocalizationError.invalidImage }
            lensRevision = records.first { $0.sourceID == request.camera.sourceID }?.revision ?? 0
            lensSession = session
            screenCalibration = true
        } catch { message = error.localizedDescription }
    }

    private func saveLens(_ lens: SiteCameraLens) async {
        busy = true
        defer { busy = false }
        do {
            guard model.deviceSession == lensSession else { throw SiteLocalizationError.invalidImage }
            try await GoClient.shared.saveCameraLens(session: lensSession, sourceID: request.camera.sourceID, expectedRevision: lensRevision, lens: lens)
            saved = nil
            message = "Lens calibration saved. Retry Automatic Alignment to position the camera using this measured lens."
        } catch { message = error.localizedDescription }
    }

}

private struct AlignmentLivePreview: View {
    @State private var ownedStream: UInt64 = 0
    @State private var calibrationLease: UUID?
    @ObservedObject var model: AppModel
    @ObservedObject var feed: CameraFeed
    let ready: () -> Void
    var body: some View {
        ZStack {
            CameraFeedDisplay(feed: feed)
            if let error = feed.error { Text(error).foregroundStyle(.white).padding() }
            else if !feed.receivedVideo { ProgressView("Connecting…").tint(.white).foregroundStyle(.white) }
        }
        .task {
            calibrationLease = feed.decoder.calibrationFrames.acquire()
            ownedStream = await model.playOwnedCamera(feed, inline: true)
            guard !Task.isCancelled else { return }
            ready()
        }
        .onDisappear {
            if let calibrationLease { feed.decoder.calibrationFrames.release(calibrationLease) }
            if ownedStream != 0, feed.stream == ownedStream {
                model.pauseCamera(feed, manually: false)
            }
        }
    }
}
