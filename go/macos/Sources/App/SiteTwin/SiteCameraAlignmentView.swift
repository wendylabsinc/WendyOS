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
                VStack(alignment: .leading, spacing: 20) {
                    Label(SiteCameraLabel(name: request.camera.name, sourceID: request.camera.sourceID).title, systemImage: "video")
                        .font(.subheadline.weight(.medium))
                        .foregroundStyle(.secondary)
                    if !onCameraGateway {
                        Label("Connect to this camera's gateway", systemImage: "network.slash").font(.headline)
                        Text("This camera belongs to a different gateway or the connection has changed. Open its gateway, then choose Calibrate and Align again.")
                    } else if request.site.visualMapURL == nil {
                        Label("Room scan needed", systemImage: "iphone.gen3.radiowaves.left.and.right").font(.headline)
                        Text("This model has no visual references. On your iPhone, open this camera’s menu and choose Calibrate and Align to scan the area it sees. Then export the site from the Sites menu and import the .wendysite file here. Your existing capture is kept.")
                    } else if let feed {
                        AlignmentLivePreview(model: model, feed: feed) { previewReady = true }
                            .frame(maxWidth: .infinity).aspectRatio(4/3, contentMode: .fit)
                            .background(.black).clipShape(RoundedRectangle(cornerRadius: 20))
                        alignmentStatus
                        if !busy, saved == nil, attemptedAutomaticAlignment {
                            Button("Retry alignment", systemImage: "arrow.clockwise") { actions.submit { await align() } }
                                .buttonStyle(.borderedProminent).disabled(!loaded)
                        }
                        if let saved {
                            if !saved.lens.measured {
                                calibrationAction
                            }
                            alignmentDetails(saved)
                        } else if !busy {
                            DisclosureGroup("Lens calibration") {
                                calibrationAction.padding(.top, 8)
                            }
                            .font(.subheadline)
                        }
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
                        DisclosureGroup("Details") {
                            Text(message)
                                .textSelection(.enabled)
                                .frame(maxWidth: .infinity, alignment: .leading)
                                .padding(.top, 8)
                        }
                        .font(.subheadline).foregroundStyle(.secondary)
                    }
                }.padding(20)
            }
            .navigationTitle("Camera alignment")
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
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

    private var alignmentStatus: some View {
        HStack(alignment: .top, spacing: 12) {
            if busy || (saved == nil && message == nil) {
                ProgressView().padding(.top, 2)
                VStack(alignment: .leading, spacing: 4) {
                    Text(loaded ? "Aligning camera…" : "Loading room…").font(.headline)
                    Text("Keep the camera still.").font(.subheadline).foregroundStyle(.secondary)
                }
            } else if let saved {
                Image(systemName: saved.lens.measured ? "checkmark.circle.fill" : "viewfinder")
                    .font(.title3)
                    .foregroundStyle(saved.lens.measured ? Color.green : Color.orange)
                VStack(alignment: .leading, spacing: 4) {
                    Text(saved.lens.measured ? "Alignment saved" : "Estimated alignment saved")
                        .font(.headline)
                    Text(saved.lens.measured ? "Your camera is positioned in the room." : "Position and field of view are approximate.")
                        .font(.subheadline).foregroundStyle(.secondary)
                }
            } else {
                Image(systemName: "viewfinder").font(.title3).foregroundStyle(.secondary)
                VStack(alignment: .leading, spacing: 4) {
                    Text("Camera not aligned").font(.headline)
                    Text("Retry alignment or choose Place Manually from the camera menu.")
                        .font(.subheadline).foregroundStyle(.secondary)
                }
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(16)
        .background(.quaternary.opacity(0.5), in: RoundedRectangle(cornerRadius: 16))
        .accessibilityElement(children: .combine)
    }

    private var calibrationAction: some View {
        VStack(alignment: .leading, spacing: 8) {
            #if os(iOS)
            Button {
                actions.submit { await startScreenCalibration() }
            } label: {
                Label("Improve accuracy", systemImage: "iphone")
                    .frame(maxWidth: .infinity)
            }
            .buttonStyle(.borderedProminent)
            .controlSize(.large)
            .disabled(busy)
            Text("Use this phone to calibrate the camera lens.")
                .font(.caption).foregroundStyle(.secondary)
            #else
            Text("To improve accuracy, calibrate the lens in the iPhone app.")
                .font(.subheadline).foregroundStyle(.secondary)
            #endif
        }
    }

    private func alignmentDetails(_ result: SiteLocalizationResult) -> some View {
        DisclosureGroup("Alignment details") {
            VStack(alignment: .leading, spacing: 12) {
                LabeledContent("Lens", value: result.lens.measured ? "Calibrated" : "Estimated")
                if let horizontal = result.lens.horizontalFOV, let vertical = result.lens.verticalFOV {
                    LabeledContent("Field of view", value: String(format: "%.1f° × %.1f°", horizontal, vertical))
                }
                LabeledContent("Direction", value: String(format: "%.1f°", result.pose.yaw))
                LabeledContent("Tilt", value: String(format: "%.1f°", result.pose.pitch))
                LabeledContent("Matches", value: "\(result.inliers)")
                LabeledContent("Validation error", value: String(format: "%.2f px", result.residual))
                if result.lens.measured { calibrationAction }
            }
            .padding(.top, 12)
        }
        .font(.subheadline)
        .tint(.secondary)
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
            message = "Lens calibration saved. Retry alignment to position the camera using the measured lens."
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
