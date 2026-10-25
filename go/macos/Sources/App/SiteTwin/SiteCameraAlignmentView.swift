import SwiftUI
import CoreGraphics
import simd

struct SiteAlignmentRequest: Identifiable {
    let id = UUID()
    let site: StoredSite
    let camera: SiteCamera
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
    @State private var screenCalibration = false
    @State private var measuredLens: SiteCameraLens?
    @State private var lensRevision: UInt64 = 0
    @State private var lensSession: UInt64 = 0

    private var feed: CameraFeed? {
        let matching = model.cameraFeeds.filter { $0.camera.stableID == request.camera.sourceID }
        return matching.count == 1 ? matching.first : nil
    }

    var body: some View {
        NavigationStack {
            ScrollView {
                VStack(alignment: .leading, spacing: 18) {
                    Text(SiteCameraLabel(name: request.camera.name, sourceID: request.camera.sourceID).title).font(.title2.bold())
                    if request.site.visualMapURL == nil {
                        Label("Room scan needed", systemImage: "iphone.gen3.radiowaves.left.and.right").font(.headline)
                        Text("This model has no visual references. On your iPhone, open this camera’s menu and choose Calibrate and Align to scan the area it sees. Then export the site from the Sites menu and import the .wendysite file here. Your existing capture is kept.")
                    } else if let feed {
                        AlignmentLivePreview(model: model, feed: feed)
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
                        } else {
                            Button("Align Automatically", systemImage: "viewfinder") { actions.submit { await align() } }
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
                        if busy { ProgressView("Processing camera measurements…") }
                        else if !loaded && message == nil { ProgressView("Loading room references…") }
                    } else {
                        Text("Connect to the gateway that hosts this camera to align it.")
                    }
                    if let message { Text(message).foregroundStyle(.secondary).textSelection(.enabled) }
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
        .task {
            guard let url = request.site.visualMapURL else { return }
            do {
                let site = request.site
                let (map, bytes) = try await BlockingWork.run(on: .global(qos: .userInitiated)) {
                    try SiteStore.validateModel(site.modelURL)
                    let bytes = try Data(contentsOf: site.modelURL)
                    return (try SiteVisualMap.read(url, revision: site.site.modelRevision, model: bytes), bytes)
                }
                try await engine.load(map, revision: site.site.modelRevision, model: bytes)
                try Task.checkCancellation()
                loaded = true
            } catch is CancellationError {} catch { message = error.localizedDescription }
        }
    }

    private func align() async {
        guard let feed, !busy else { return }
        busy = true; message = nil
        defer { busy = false }
        let session = model.deviceSession, source = request.camera.sourceID, stream = feed.stream
        do {
            let records = try await GoClient.shared.cameraMetadata(session: session)
            let matching = records.filter { $0.sourceID == source }
            guard matching.count <= 1 else { throw SiteLocalizationError.invalidResult }
            let record = matching.first
            let first = try await freshFrame(feed, stream: stream)
            var solution: SiteLocalizationResult
            do {
                solution = try await engine.localize(gray: first.gray, width: first.width, height: first.height,
                    revision: request.site.site.modelRevision, lens: record?.lens)
            } catch SiteLocalizationError.noSolution(_, _, _) {
                solution = try await engine.localize(gray: first.gray, width: first.width, height: first.height,
                    revision: request.site.site.modelRevision, lens: record?.lens, estimated: true)
            }
            if solution.fittedLens { solution.lens.captureMode = "\(first.codec) \(first.width)x\(first.height)" }
            try Task.checkCancellation()
            // A second live frame guards against a transient match or moving camera.
            try await Task.sleep(for: .milliseconds(800))
            let second = try await freshFrame(feed, stream: stream, after: first)
            guard second.received > first.received, first.width == second.width, first.height == second.height, first.codec == second.codec else { throw SiteLocalizationError.invalidImage }
            let verification = try await engine.localize(gray: second.gray, width: second.width, height: second.height,
                revision: request.site.site.modelRevision, lens: solution.lens, estimated: !solution.lens.measured)
            let a = solution.pose.position, b = verification.pose.position
            let distance = simd_distance(SIMD3(a.x,a.y,a.z), SIMD3(b.x,b.y,b.z))
            let angle = 2*acos(min(1, abs(simd_dot(solution.pose.rotation, verification.pose.rotation))))
            guard distance <= (solution.lens.measured ? 0.08 : 0.30), angle <= (solution.lens.measured ? 3 : 10) * .pi/180 else {
                throw WendyError(message: solution.lens.measured ? "The camera pose changed between frames. Keep the camera fixed and try again." : "The estimated pose changed too much between frames. Phone Screen Calibration can measure the lens without changing the room.")
            }
            guard model.deviceSession == session, self.feed?.id == feed.id, feed.stream == stream, feed.running,
                  (feed.jpeg?.width ?? feed.decoder.imageSize?.width) == first.width,
                  (feed.jpeg?.height ?? feed.decoder.imageSize?.height) == first.height else { throw SiteLocalizationError.invalidImage }
            solution.pose = verification.pose
            solution.inliers = min(solution.inliers, verification.inliers)
            solution.residual = max(solution.residual, verification.residual)
            try Task.checkCancellation()
            try await GoClient.shared.saveCameraAlignment(session: session, sourceID: source,
                expectedRevision: record?.revision ?? 0, result: solution)
            saved = solution
        } catch is CancellationError {} catch { message = error.localizedDescription }
    }

    private func startScreenCalibration() async {
        guard !busy else { return }
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
            message = "Lens calibration saved. Tap Align Automatically to position the camera using this measured lens."
        } catch { message = error.localizedDescription }
    }

    private func freshFrame(_ feed: CameraFeed, stream: UInt64, after previous: CameraCalibrationFrame? = nil) async throws -> CameraCalibrationFrame {
        let deadline = ContinuousClock.now.advanced(by: .seconds(8))
        while ContinuousClock.now < deadline {
            try Task.checkCancellation()
            guard feed.running, feed.stream == stream, self.feed?.id == feed.id else { throw SiteLocalizationError.invalidImage }
            if let frame = feed.calibrationFrame(), frame.isFresh(at: Date()),
               previous == nil || (frame.sequence != previous?.sequence && frame.received > previous!.received) { return frame }
            try await Task.sleep(for: .milliseconds(150))
        }
        throw WendyError(message: "No fresh camera image arrived for alignment. Check that the live video is updating, then try again.")
    }

}

private struct AlignmentLivePreview: View {
    @State private var ownedStream: UInt64 = 0
    @ObservedObject var model: AppModel
    @ObservedObject var feed: CameraFeed
    var body: some View {
        ZStack {
            CameraFeedDisplay(feed: feed)
            if let error = feed.error { Text(error).foregroundStyle(.white).padding() }
            else if !feed.receivedVideo { ProgressView("Connecting…").tint(.white).foregroundStyle(.white) }
        }
        .task {
            feed.decoder.calibrationFrames.setEnabled(true)
            ownedStream = await model.playOwnedCamera(feed, inline: true)
        }
        .onDisappear {
            if ownedStream != 0, feed.stream == ownedStream {
                feed.decoder.calibrationFrames.setEnabled(false)
                model.pauseCamera(feed)
            }
        }
    }
}
