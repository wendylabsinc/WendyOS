import SwiftUI
import UniformTypeIdentifiers

struct MobileSiteCalibration: View {
    @ObservedObject var model: AppModel
    let request: SiteAlignmentRequest
    @ObservedObject var feed: CameraFeed
    let onSaved: (SiteLocalizationResult) async -> Void
    let onManualSaved: (SitePoint, SiteManualAlignment) async throws -> Void
    var onBusyChanged: (Bool) -> Void = { _ in }
    @State private var actions = AsyncActions()
    @State private var engine = SiteLocalizationEngine()
    @State private var lensEngine = ScreenLensCalibration()
    @State private var progress = ScreenLensProgress()
    @State private var lens: SiteCameraLens?
    @State private var result: SiteLocalizationResult?
    @State private var revision: UInt64?
    @State private var alignmentStream: UInt64 = 0
    @State private var busy = false
    private enum Operation { case alignment, calibration }
    @State private var operation = Operation.alignment
    @State private var calibrationMessage: String?
    @State private var saved = false
    @State private var manualPosition: SitePoint?
    @State private var manualConfiguration: SiteManualAlignment?
    @State private var manual = false
    @State private var phonePattern = false
    @State private var pattern: RobotCalibrationPattern?
    @State private var exporting = false
    @State private var previewReady = false
    @State private var attemptedAutomatic = false
    @State private var alignmentStatus = "Checking saved alignment…"
    @State private var alignmentDetail = ""
    @State private var alignmentFailed = false
    private var current: Bool { request.isCurrent(session: model.deviceSession) }
    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 18) {
                Text("Align camera").font(.title2.bold())
                if current {
                    if !phonePattern {
                        AlignmentLivePreview(model: model, feed: feed) { previewReady = true }
                            .aspectRatio(4/3, contentMode: .fit)
                            .background(.black).clipShape(RoundedRectangle(cornerRadius: 20))
                    }
                    VStack(alignment: .leading, spacing: 6) {
                        HStack {
                            if busy && operation == .alignment { ProgressView() }
                            else { Image(systemName: alignmentFailed ? "exclamationmark.triangle" : saved ? "checkmark.circle.fill" : "viewfinder") }
                            Text(alignmentStatus).font(.headline)
                        }
                        if !alignmentDetail.isEmpty { Text(alignmentDetail).font(.subheadline).foregroundStyle(.secondary) }
                    }.frame(maxWidth: .infinity, alignment: .leading).padding(14)
                        .background(.quaternary, in: RoundedRectangle(cornerRadius: 14))
                        .accessibilityElement(children: .combine).accessibilityIdentifier("sites.camera.alignmentStatus")
                    if request.site.visualMapURL == nil {
                        Label("This model has no scan reference photos. You can save lens calibration, but alignment needs a scan with visual references.", systemImage: "viewfinder").font(.subheadline).foregroundStyle(.secondary)
                    } else {
                        Button(attemptedAutomatic ? "Retry alignment" : "Align automatically", systemImage: "viewfinder") {
                            perform { try await alignAndSave() }
                        }.buttonStyle(.borderedProminent)

                    }
                    Button("Adjust manually", systemImage: "slider.horizontal.3") { manual = true }
                        .buttonStyle(.bordered)
                    if let result {
                        Label(String(format: result.lens.measured ? "Aligned · %.2f px" : "Estimated alignment · %.2f px", result.residual), systemImage: "checkmark.circle").foregroundStyle(MobileLensStyle.green)
                    }
                    if result != nil, !saved {
                        Button("Save alignment") { perform(failureTitle: "Could not save alignment") { try await save() } }
                            .buttonStyle(.borderedProminent)
                    }
                    DisclosureGroup("Improve accuracy (optional)") {
                        VStack(alignment: .leading, spacing: 12) {
                            Label(lens?.measured == true ? "Lens calibrated" : "Checkerboard · \(progress.count) views", systemImage: "checkerboard.rectangle").font(.headline)
                            Text("Keep the camera still. Capture at least 12 distinct views with the checkerboard tilted and near each edge.").font(.subheadline).foregroundStyle(.secondary)
                            HStack {
                                Button("Save checkerboard") {
                                    do { pattern = try RobotCalibrationPattern(); exporting = true }
                                    catch { calibrationMessage = "Could not prepare checkerboard: " + error.localizedDescription }
                                }
                                Spacer()
                                Button("Use this phone") { phonePattern = true }
                            }.font(.subheadline)
                            Button("Capture View", systemImage: "camera") { perform(.calibration) { try await capture() } }.buttonStyle(.borderedProminent)
                            if request.site.visualMapURL == nil, lens?.measured == true, !saved {
                                Button("Save lens calibration") { perform(.calibration) { try await save() } }.buttonStyle(.bordered)
                            }
                            if busy && operation == .calibration { ProgressView("Checking calibration view…") }
                            if let calibrationMessage {
                                Text(calibrationMessage).font(.subheadline).foregroundStyle(.secondary)
                                    .accessibilityIdentifier("sites.camera.calibrationMessage")
                            }
                        }.frame(maxWidth: .infinity, alignment: .leading)
                    }
                } else {
                    Text("Connection changed. Close this sheet and reconnect to the camera.")
                }
            }.padding().disabled(busy || revision == nil)
        }.background(MobileSitesStyle.background).tint(MobileLensStyle.green)
        .interactiveDismissDisabled(busy)
        .onChange(of: busy) { _, value in onBusyChanged(value) }
        .task { await actions.run() }
        .task(id: previewReady && revision != nil && current) {
            guard previewReady, revision != nil, current, !attemptedAutomatic, request.site.visualMapURL != nil, request.camera.position == nil else { return }
            attemptedAutomatic = true
            perform { try await alignAndSave() }
        }
        .task {
            do {
                let records = try await GoClient.shared.cameraMetadata(session: request.session).filter { $0.sourceID == request.camera.sourceID }
                guard current, records.count <= 1 else { throw SiteLocalizationError.invalidResult }
                revision = records.first?.revision ?? 0
                lens = records.first?.lens.flatMap { $0.measured ? $0 : nil }
                if let geometry = MobileSiteCoveragePolicy.geometry(camera: request.camera, site: request.site.site, records: records,
                    gateway: SiteGatewayIdentity(id: request.gatewayID, aliases: request.gatewayAliases), now: Date()) {
                    alignmentStatus = request.camera.manualAlignment != nil ? "Manually aligned" : geometry.lens?.measured == true ? "Saved alignment · calibrated lens" : "Saved alignment · estimated"
                    alignmentDetail = "Last aligned " + geometry.pose.observedAt.formatted(date: .abbreviated, time: .shortened)
                } else if request.camera.position != nil {
                    alignmentStatus = "Saved placement · needs alignment"
                    alignmentDetail = "Wendy has kept the saved position. Tap Align automatically to verify or update it."
                } else {
                    alignmentStatus = request.site.visualMapURL == nil ? "Needs scan references" : "Waiting for live video"
                    alignmentDetail = request.site.visualMapURL == nil ? "This model cannot be aligned automatically without scan reference photos." : "Alignment will start when the camera is ready."
                }
            } catch {
                alignmentStatus = "Could not check alignment"; alignmentFailed = true
                alignmentDetail = error.localizedDescription
            }
        }
        .fileExporter(isPresented: $exporting, document: pattern, contentType: .png, defaultFilename: "Wendy-checkerboard") { outcome in
            if case .failure(let error) = outcome { calibrationMessage = "Could not save checkerboard: " + error.localizedDescription }
        }
        .sheet(isPresented: $manual) {
            MobileManualCameraAlignment(model: model, request: request, feed: feed, initialPosition: manualPosition, initialAlignment: manualConfiguration) { position, alignment in
                try await onManualSaved(position, alignment)
                manualPosition = position; manualConfiguration = alignment
                result = nil; saved = true; alignmentFailed = false
                alignmentStatus = "Manually aligned"
                alignmentDetail = "Saved with the 3D model. Tap Done to view the projection."
            }
        }
        .fullScreenCover(isPresented: $phonePattern) {
            PhoneLensCalibrationView(model: model, feed: feed) { lens = $0; result = nil; saved = false; calibrationMessage = "Lens measured. Retry alignment to apply the improved lens measurement." }
        }
    }
    private func perform(_ operation: Operation = .alignment, failureTitle: String = "Could not align camera", _ work: @escaping @MainActor () async throws -> Void) {
        busy = true; self.operation = operation
        if operation == .calibration { calibrationMessage = nil }
        actions.submit {
            defer { busy = false }
            do {
                guard current else { throw WendyError(message: "Camera connection changed.") }
                try await work()
            } catch {
                switch operation {
                case .alignment:
                    alignmentStatus = failureTitle
                    alignmentDetail = error.localizedDescription
                    alignmentFailed = true
                case .calibration:
                    calibrationMessage = error.localizedDescription
                }
            }
        }
    }
    private func alignAndSave() async throws {
        guard let revision else { return }
        result = nil; saved = false; attemptedAutomatic = true
        alignmentStatus = "Aligning camera…"; alignmentDetail = "Matching live video to the scan. Keep the camera still."; alignmentFailed = false
        try await SiteCameraAlignment.load(request.site, into: engine)
        // Retry fresh frames without weakening the geometric or two-frame validation.
        for attempt in 1...3 {
            alignmentDetail = "Matching live video to the scan · attempt \(attempt) of 3"
            do {
                result = try await SiteCameraAlignment.align(model: model, request: request, feed: feed, engine: engine,
                    persist: false, lensOverride: lens, expectedRevision: revision)
                break
            } catch let error as SiteLocalizationError {
                guard case .noSolution = error, attempt < 3 else { throw error }
                try await Task.sleep(for: .seconds(1))
            }
        }
        alignmentStream = feed.stream
        try await save()
    }
    private func capture() async throws {
        guard let frame = feed.calibrationFrame(), frame.isFresh(at: Date()) else { throw WendyError(message: "Waiting for fresh camera video.") }
        let previous = progress.revision
        _ = try await lensEngine.add(frame)
        progress = try await lensEngine.progress()
        if progress.count >= 12, progress.coverageReady {
            let measured = try await lensEngine.solve(width: frame.width, height: frame.height, codec: frame.codec)
            lens = measured; result = nil; saved = false
            calibrationMessage = "Lens measured. Retry alignment to apply the improved lens measurement."
        } else if previous == progress.revision {
            calibrationMessage = "No calibration view accepted. Show the whole checkerboard, change its angle or position, and try again."
        } else {
            calibrationMessage = "Calibration view accepted · \(progress.count) views collected."
        }
    }
    private func save() async throws {
        guard let revision, current else { throw SiteLocalizationError.invalidImage }
        if let result {
            guard feed.running, feed.stream == alignmentStream,
                  let frame = feed.calibrationFrame(), frame.isFresh(at: Date()),
                  frame.width == result.lens.width, frame.height == result.lens.height else {
                throw WendyError(message: "Camera stream changed. Align to the model again before saving.")
            }
            try await GoClient.shared.saveCameraAlignment(session: request.session, sourceID: request.camera.sourceID, expectedRevision: revision, result: result)
            await onSaved(result)
        } else if let lens, request.site.visualMapURL == nil {
            try await GoClient.shared.saveCameraLens(session: request.session, sourceID: request.camera.sourceID, expectedRevision: revision, lens: lens)
        } else { return }
        saved = true
        if result == nil {
            calibrationMessage = "Lens calibration saved on the device. Model alignment is still needed."
        } else {
            alignmentFailed = false
            alignmentStatus = result?.lens.measured == true ? "Aligned and saved" : "Aligned and saved · estimated"
            alignmentDetail = "Tap Done to see the viewing area and projected video in the 3D model."
        }
        let records = try await GoClient.shared.cameraMetadata(session: request.session).filter { $0.sourceID == request.camera.sourceID }
        guard current, records.count == 1 else { throw SiteLocalizationError.invalidResult }
        self.revision = records[0].revision
    }
}

struct MobileSiteLiveCameras: View {
    @ObservedObject var model: AppModel
    @ObservedObject var store: MobileSitesStore
    let site: MobileSiteRecord
    let link: MobileSiteModelLink
    @State private var cameras: [SiteCamera] = []
    @State private var savedModel: StoredSite?
    @State private var preview: SiteCamera?
    @State private var error: String?
    @State private var gateway = SiteGatewayIdentity()
    private func feed(for camera: SiteCamera) -> CameraFeed? {
        guard let device = model.connected, site.assets.contains(where: { !$0.robot && $0.id == recordingDeviceID(device) }),
              SiteGatewayIdentity.matches(camera.gatewayID, current: gateway.id, aliases: gateway.aliases) else { return nil }
        let feeds = model.cameraFeeds.filter { $0.camera.stableID == camera.sourceID }
        return feeds.count == 1 ? feeds.first : nil
    }
    var body: some View {
        Section("Live cameras") {
            if cameras.isEmpty {
                Text(error ?? "Add cameras to \(link.name) to see them here.").foregroundStyle(.secondary)
            } else {
                LazyVGrid(columns: [GridItem(.flexible()), GridItem(.flexible())], spacing: 12) {
                    ForEach(cameras) { camera in
                        Button { preview = camera } label: {
                            VStack(alignment: .leading, spacing: 8) {
                                ZStack {
                                    if preview == nil, let feed = feed(for: camera) {
                                        SiteCameraPreview(model: model, feed: feed).id(feed.id).allowsHitTesting(false)
                                    } else {
                                    RoundedRectangle(cornerRadius: 16).fill(Color.black.opacity(0.85))
                                    Image(systemName: "play.circle.fill").font(.largeTitle).foregroundStyle(.white)
                                    }
                                }.frame(height: 100).clipShape(RoundedRectangle(cornerRadius: 16))
                                Text(camera.name).font(.subheadline.weight(.semibold)).foregroundStyle(.primary).lineLimit(2)
                                Text("Watch live").font(.caption).foregroundStyle(MobileLensStyle.green)
                            }
                        }.buttonStyle(.plain)
                    }
                }
            }
        }
        .task(id: "\(link.id)-\(store.events.count)") {
            cameras = []; error = nil
            do {
                let value = try await store.models.load(id: link.id)
                savedModel = value; cameras = value.site.cameras
            }
            catch { self.error = error.localizedDescription }
        }
        .task(id: model.deviceSession) {
            gateway = SiteGatewayIdentity()
            guard let device = model.connected else { return }
            let session = model.deviceSession
            do {
                async let info = GoClient.shared.deviceInfo(session: session)
                async let enrollment = GoClient.shared.deviceEnrollment(session: session)
                let identity = try await SiteGatewayIdentity.resolve(cloud: SiteGatewayIdentity.cloud(selector: device.selector), enrollment: enrollment, serial: info.serial)
                guard session == model.deviceSession else { return }
                gateway = identity
            } catch { /* Other gateways remain tap-to-connect. */ }
        }
        .sheet(item: $preview) { camera in MobileSiteCameraLive(model: model, site: site, camera: camera, calibrationModel: savedModel, store: store) }
    }
}

struct MobileManualCameraAlignment: View {
    @ObservedObject var model: AppModel
    let request: SiteAlignmentRequest
    @ObservedObject var feed: CameraFeed
    var initialPosition: SitePoint? = nil
    var initialAlignment: SiteManualAlignment? = nil
    let onSave: (SitePoint, SiteManualAlignment) async throws -> Void
    @Environment(\.dismiss) private var dismiss
    @State private var position = SitePoint(x: 0, y: 1.5, z: 0)
    @State private var yaw = 180.0
    @State private var pitch = 0.0
    @State private var roll = 180.0
    @State private var fov = 70.0
    @State private var placing = false
    @State private var busy = false
    @State private var projectionStatus = "Waiting for live projection…"
    @State private var error: String?
    @State private var actions = AsyncActions()
    private var alignment: SiteManualAlignment {
        SiteManualAlignment(modelRevision: request.site.site.modelRevision, yaw: yaw, pitch: pitch, roll: roll,
            horizontalFOV: fov, width: feed.jpeg?.width ?? feed.decoder.imageSize?.width ?? 1600,
            height: feed.jpeg?.height ?? feed.decoder.imageSize?.height ?? 1200)
    }
    private var site: Site {
        var value = request.site.site
        if let index = value.cameras.firstIndex(where: { $0.id == request.camera.id }) { value.cameras[index].position = position }
        return value
    }
    var body: some View {
        NavigationStack {
            VStack(spacing: 0) {
                SiteScene(modelURL: request.site.modelURL, site: site,
                    cameraGeometry: alignment.geometry(position: position, revision: site.modelRevision, now: Date()).map { [request.camera.id: $0] } ?? [:],
                    colorSources: [request.camera.id: feed.colorSource],
                    colorStatus: { projectionStatus = $0[request.camera.id] ?? "Waiting for live projection…" },
                    placingCamera: placing ? request.camera.id : nil, select: { _ in },
                    place: { _, point in position = point; placing = false })
                    .frame(height: 280)
                    .overlay(alignment: .bottomLeading) {
                        Text(projectionStatus).font(.caption).padding(8)
                            .background(.regularMaterial, in: RoundedRectangle(cornerRadius: 8)).padding(8)
                            .accessibilityIdentifier("sites.manual.projectionStatus")
                    }
                    .overlay(alignment: .topTrailing) {
                        Button(placing ? "Cancel move" : "Move in model", systemImage: "move.3d") { placing.toggle() }
                            .buttonStyle(.borderedProminent).padding(8)
                    }
                Form {
                    Section {
                        Text("Match the projected video to walls and objects. This saves a manual visualization, not a measured calibration.")
                            .font(.caption).foregroundStyle(.secondary)
                    }
                    Section("Position · metres") {
                        Stepper("X  \(position.x, specifier: "%.2f")", value: $position.x, step: 0.1)
                        Stepper("Height  \(position.y, specifier: "%.2f")", value: $position.y, step: 0.1)
                        Stepper("Z  \(position.z, specifier: "%.2f")", value: $position.z, step: 0.1)
                    }
                    Section("Rotation") {
                        adjustment("Turn", value: $yaw, range: -180...180)
                        adjustment("Tilt", value: $pitch, range: -90...90)
                        adjustment("Roll", value: $roll, range: -180...180)
                    }
                    Section("Field of view") { adjustment("Width", value: $fov, range: 20...140) }
                    if let error { Section { Text(error).foregroundStyle(.red) } }
                }
            }
            .navigationTitle("Manual alignment").navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button("Cancel") { dismiss() }.disabled(busy) }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Save") {
                        placing = false; busy = true; error = nil
                        actions.submit {
                            defer { busy = false }
                            do {
                                guard request.isCurrent(session: model.deviceSession) else {
                                    throw WendyError(message: "The device connection changed. Reopen alignment before saving.")
                                }
                                try await onSave(position, alignment)
                                dismiss()
                            } catch { self.error = error.localizedDescription }
                        }
                    }.disabled(busy).accessibilityIdentifier("sites.manual.save")
                }
            }
            .interactiveDismissDisabled(busy)
            .tint(MobileLensStyle.green)
        }
        .background { SiteCameraColorCapture(model: model, sourceIDs: [request.camera.sourceID]) }
        .task { await actions.run() }
        .onAppear {
            position = initialPosition ?? request.camera.position ?? position
            if let saved = initialAlignment ?? request.camera.manualAlignment {
                yaw = saved.yaw; pitch = saved.pitch; roll = saved.roll; fov = saved.horizontalFOV
            }
        }
    }
    private func adjustment(_ name: String, value: Binding<Double>, range: ClosedRange<Double>) -> some View {
        VStack(alignment: .leading) {
            Text("\(name)  \(value.wrappedValue, specifier: "%.0f")°")
            Slider(value: value, in: range, step: 1).accessibilityLabel(name)
        }
    }
}
