import SwiftUI

@MainActor
final class SiteRobotTelemetry: ObservableObject {
    @Published var reading: SiteRobotReading?
}

struct SiteRobotRenderHost<Content: View>: View {
    @ObservedObject var model: SiteRobotMapModel
    @ObservedObject private var telemetry: SiteRobotTelemetry
    private let content: (SiteRobotRenderState) -> Content

    init(model: SiteRobotMapModel, @ViewBuilder content: @escaping (SiteRobotRenderState) -> Content) {
        self.model = model
        self.telemetry = model.telemetry
        self.content = content
    }

    var body: some View { content(model.render) }
}

struct SiteRobotCameraHost: View {
    @ObservedObject var model: SiteRobotMapModel
    let height: CGFloat
    var body: some View {
        if !model.serial.isEmpty {
            SiteRobotCameraView(handle: model.handle, onFrame: model.acceptCameraFrame, onDecoder: model.acceptCameraDecoder).frame(height: height)
        }
    }
}

@MainActor
final class SiteRobotMapModel: ObservableObject {
    private let observationClient: any SiteRobotObserving
    private let turningClient: any SiteRobotTurning
    private let searchClient: any SiteRobotSearching

    init(observationClient: any SiteRobotObserving = SiteRobotClient.shared,
         turningClient: any SiteRobotTurning = SiteRobotClient.shared,
         searchClient: any SiteRobotSearching = SiteRobotClient.shared) {
        self.observationClient = observationClient
        self.turningClient = turningClient
        self.searchClient = searchClient
    }

    @Published var robots: [UnitreeRobot] = []
    @Published var serial = ""
    @Published private(set) var reading: SiteRobotReading?
    let telemetry = SiteRobotTelemetry()
    @Published var error: String?
    @Published var connecting = false
    @Published var reconnectGeneration = 0
    @Published var candidates: [SiteRobotCandidate] = []
    @Published var candidateIndex = 0
    @Published var localizing = false
    @Published var fusionReport: RobotFusionReport?
    @Published var showScan = false
    @Published var showObstacles = true
    @Published var visualStatus = "Waiting for a fresh Go2 camera frame."
    @Published var referenceCount = 0
    private var cameraFrame: CameraCalibrationFrame?
    @Published private(set) var cameraDecoder: H264AnnexBDecoder?
    @Published private(set) var lensCalibrating = false
    @Published var showLensCalibration = false
    @Published private(set) var showRotationPrompt = false
    private var rotationOffer = RobotRotationOffer()
    private var positionSearch = RobotPositionSearch()
    @Published private(set) var searchConclusion: String?
    @Published private(set) var searchTurns = 0
    @Published private(set) var rotationState: RobotRotationState = .idle
    private var rotationStartPending = false
    @Published private(set) var measuredLens: SiteCameraLens?
    private var lensSession: RobotLensSession?
    private var visual = RobotVisualLocalization()
    private var savedMap: RobotSavedMap?
    private var siteRevision = UUID()
    private var lastEvidence: UInt64 = 0
    private var suggestionFreshness: RobotSuggestionFreshness?
    private var matchSequence: UInt64 = 0
    private var geometryURL: URL?
    private var geometryScale = 1.0
    private var explorationEpoch = UUID()
    private var lastReadingAt = Date.distantPast
    private var lastPosition: SitePoint?
    private var lastYaw = 0.0
    @Published var selected = true
    @Published var placing = false
    @Published var anchor: SitePoint?
    @Published var destination: SitePoint?
    @Published private(set) var handle: UInt64 = 0
    private var remoteSearching = false
    private var remoteSearchSequence: UInt64 = 0
    private var frame = ""
    private var generation = 0
    private var actionGeneration: UInt64 = 0
    private var automaticAttempts = 0
    private var nextAutomaticAttempt = Date.distantPast
    private var automaticPaused = false
    private var awaitingLook = false
    private var rematchAfterLook = false
    let actions = AsyncActions()

    var statusLabel: String {
        if connecting { return "Connecting…" }
        if reading?.disconnected == true || (handle == 0 && error != nil) { return "Go2 disconnected" }
        if lensCalibrating { return "Measuring camera lens…" }
        if searchConclusion != nil { return "Search finished without a match" }
        if let title = rotationState.title { return title }
        if reading?.moving == true {
            return reading?.message == "Route changed to avoid a blocked area." ? "Moving along an alternative route" : "Moving"
        }
        if localizing { return searchTurns > 0 ? "Matching view after turn \(searchTurns) of 12…" : "Locating Go2…" }
        if !candidates.isEmpty { return "Confirm suggested position" }
        if reading?.ready == true {
            return reading?.usingArrivalTiming == true ? "Ready · supervised timing" : "Ready for orders"
        }
        if reading?.pose == true, reading?.aligned != true {
            return reading?.scanReady == true ? "Position not confirmed" : "Waiting for room landmarks"
        }
        return reading?.pose == true ? "Live pose · navigation unavailable" : "Waiting for telemetry"
    }

    func setArrivalTiming(_ enabled: Bool) {
        guard handle != 0 else { return }
        actionGeneration += 1
        destination = nil
        candidates = []; localizing = false
        let request = actionGeneration, h = handle, epoch = generation
        actions.submit {
            guard request == self.actionGeneration, epoch == self.generation else { return }
            do {
                try await SiteRobotClient.shared.arrivalTiming(h, sequence: request, enabled: enabled)
                if request == self.actionGeneration, epoch == self.generation { self.error = nil; self.scheduleAutomaticLocalization() }
            } catch { if epoch == self.generation { self.error = error.localizedDescription } }
        }
    }

    func setUnderBodyAllowance(_ enabled: Bool) {
        guard handle != 0 else { return }
        actionGeneration += 1
        destination = nil; candidates = []; localizing = false
        let request = actionGeneration, h = handle, epoch = generation
        actions.submit {
            guard request == self.actionGeneration, epoch == self.generation else { return }
            do {
                try await SiteRobotClient.shared.underBodyAllowance(h, sequence: request, enabled: enabled)
                if request == self.actionGeneration, epoch == self.generation { self.error = nil; self.scheduleAutomaticLocalization() }
            } catch { if epoch == self.generation { self.error = error.localizedDescription } }
        }
    }

    func setAllowUnobservedFloor(_ enabled: Bool) {
        guard handle != 0 else { return }
        actionGeneration += 1
        destination = nil; candidates = []; localizing = false
        let request = actionGeneration, h = handle, epoch = generation
        actions.submit {
            guard request == self.actionGeneration, epoch == self.generation else { return }
            do {
                try await SiteRobotClient.shared.allowUnobservedFloor(h, sequence: request, enabled: enabled)
                if request == self.actionGeneration, epoch == self.generation { self.error = nil; self.scheduleAutomaticLocalization() }
            } catch { if epoch == self.generation { self.error = error.localizedDescription } }
        }
    }

    var render: SiteRobotRenderState {
        let reading = telemetry.reading
        let live = reading?.pose == true && reading?.aligned == true
        let candidate = candidates.indices.contains(candidateIndex) ? candidates[candidateIndex] : nil
        let position = placing ? anchor : candidate?.position ?? lastPosition
        return SiteRobotRenderState(explorationEpoch: explorationEpoch, receivedAt: lastReadingAt, position: position,
            yaw: candidate?.yaw ?? lastYaw, joints: reading?.joints ?? [], selected: selected,
            fresh: live && candidate == nil, moving: reading?.moving == true, route: reading?.route ?? [], destination: destination, placing: placing, scan: showScan ? fusionReport?.overlay ?? [] : [],
            hazards: showObstacles && live && !placing && candidate == nil && reading?.hazardsReady == true ? reading?.hazards ?? [] : [])
    }
    func load(session: UInt64) async {
        guard session != 0 else { robots = []; serial = ""; return }
        do {
            let result = try await UnitreeClient.shared.robots(session: session)
            guard !Task.isCancelled else { return }
            robots = result.robots.filter(\.paired)
            if !robots.contains(where: { $0.serial == serial }) { serial = robots.first?.serial ?? "" }
        } catch { if !Task.isCancelled { self.error = error.localizedDescription } }
    }
    func observe(session: UInt64, site: Site, modelURL: URL, active: Bool) async {
        // Replace the whole handle so camera, alignment and pending actions all
        // belong to the new subscription. Recovery never repeats a motion order.
        for attempt in 0...3 {
            if attempt > 0 {
                do { try await Task.sleep(for: .seconds(1 << (attempt - 1))) }
                catch { return }
            }
            guard !Task.isCancelled else { return }
            let retry = await observeConnection(session: session, site: site, modelURL: modelURL,
                active: active, recovering: attempt > 0)
            guard retry, !Task.isCancelled else { return }
        }
    }

    private func observeConnection(session: UInt64, site: Site, modelURL: URL, active: Bool, recovering: Bool) async -> Bool {
        generation += 1
        explorationEpoch = UUID()
        let epoch = generation
        telemetry.reading = nil; reading = nil; anchor = nil; lastPosition = nil; lastYaw = 0; destination = nil; placing = false; error = nil
        handle = 0; connecting = false; remoteSearching = false
        searchConclusion = nil; searchTurns = 0
        rotationState = .idle; rotationStartPending = false
        showRotationPrompt = false; rotationOffer = RobotRotationOffer()
        lensSession = nil; lensCalibrating = false; measuredLens = nil; awaitingLook = false; rematchAfterLook = false; positionSearch.cancel(); showLensCalibration = false
        candidates = []; localizing = false; candidateIndex = 0
        suggestionFreshness = nil
        geometryURL = modelURL; geometryScale = site.metresPerModelUnit
        siteRevision = site.modelRevision; fusionReport = nil; savedMap = nil; cameraFrame = nil; cameraDecoder = nil; referenceCount = 0; lastEvidence = 0
        scheduleAutomaticLocalization()
        frame = "\(site.id.uuidString)/\(site.modelRevision.uuidString)"
        guard active, session != 0, !serial.isEmpty else { return false }
        connecting = true
        var owned: UInt64 = 0
        var retry = false
        do {
            owned = try await observationClient.open(session: session, serial: serial)
            guard !Task.isCancelled, epoch == generation else {
                await observationClient.close(owned); return false
            }
            handle = owned; connecting = false
            while !Task.isCancelled {
                let result = try await observationClient.poll(owned)
                guard epoch == generation, !Task.isCancelled else { break }
                accept(result)
                if result.disconnected {
                    actionGeneration += 1
                    error = result.message
                    retry = true
                    break
                }
                if remoteSearching {
                    let progress = try await searchClient.searchStatus(owned)
                    guard epoch == generation, !Task.isCancelled else { break }
                    searchTurns = progress.SearchTurns
                    if progress.SearchState == "searching" {
                        rotationState = result.moving ? .turning(searchTurns) : .matching(searchTurns)
                    } else {
                        remoteSearching = false; positionSearch.cancel(); localizing = false
                        if let report = progress.Fusion, progress.SearchState == "complete" {
                            fusionReport = report
                            candidates = (report.candidates ?? []).map { SiteRobotCandidate(position: SitePoint(x: $0.X, y: $0.FloorY, z: $0.Z), yaw: $0.Yaw, rms: $0.RMS, coverage: $0.Coverage) }
                            matchSequence = remoteSearchSequence; candidateIndex = 0
                            rotationState = .idle
                        } else { error = progress.SearchMessage; rotationState = .stopped(progress.SearchMessage) }
                    }
                }
                try await Task.sleep(for: .milliseconds(100))
            }
        } catch {
            if epoch == generation, !Task.isCancelled {
                self.error = error.localizedDescription
                retry = recovering
            }
        }
        if owned != 0 { await observationClient.close(owned) }
        if epoch == generation {
            handle = 0; connecting = false; telemetry.reading = nil; reading = nil; showRotationPrompt = false
            positionSearch.cancel(); awaitingLook = false; rematchAfterLook = false
            candidates = []; localizing = false; destination = nil; fusionReport = nil
            rotationState = .idle; rotationStartPending = false
        }
        return retry && epoch == generation && !Task.isCancelled
    }
    func accept(_ result: SiteRobotReading) {
        lastReadingAt = .now
        if result.aligned != true && telemetry.reading?.aligned == true { explorationEpoch = UUID() }
        if result.pose && result.aligned {
            lastPosition = SitePoint(x: result.x, y: result.floorY, z: result.z)
            lastYaw = result.yaw
        }
        telemetry.reading = result
        if reading?.hasSameControls(as: result) != true { reading = result }
        if showRotationPrompt && (!result.pose || result.aligned || result.moving) { showRotationPrompt = false }
    }

    func acceptCameraDecoder(_ decoder: H264AnnexBDecoder) {
        if cameraDecoder !== decoder { cameraDecoder = decoder }
    }

    func acceptCameraFrame(_ frame: CameraCalibrationFrame) {
        if frame.isFresh(at: Date()), cameraFrame == nil || frame.received > cameraFrame!.received { cameraFrame = frame }
    }

    func beginLensCalibration() throws -> RobotLensSession {
        guard handle != 0, !serial.isEmpty, lensSession == nil else {
            throw WendyError(message: "Connect Go2 before measuring its lens.")
        }
        let search = positionSearch
        stop()
        positionSearch = search
        let token = RobotLensSession(handle: handle, robot: serial)
        lensSession = token; lensCalibrating = true
        return token
    }

    func lensCalibrationFrame(_ token: RobotLensSession) throws -> CameraCalibrationFrame? {
        guard lensSession == token, handle == token.handle, serial == token.robot else {
            throw WendyError(message: "The Go2 connection changed. Close and reopen lens calibration.")
        }
        return cameraFrame
    }

    func saveMeasuredLens(_ lens: SiteCameraLens, session token: RobotLensSession, mode: RobotCameraMode) async throws {
        guard let frame = try lensCalibrationFrame(token), frame.isFresh(at: .now), RobotCameraMode(frame) == mode else {
            throw WendyError(message: "Wait for fresh frames in the same Go2 camera mode before saving calibration.")
        }
        let profile = RobotLensProfile(robot: token.robot, lens: lens, measuredAt: .now)
        try Task.checkCancellation()
        try await BlockingWork.run(on: .global(qos: .utility)) { try RobotLensStore.save(profile, mode: mode) }
        try Task.checkCancellation()
        _ = try lensCalibrationFrame(token)
        measuredLens = lens
        visualStatus = "Measured Go2 lens saved. Waiting to match the room."
    }

    func endLensCalibration(_ token: RobotLensSession, completed: Bool = false) {
        guard lensSession == token else { return }
        lensSession = nil; lensCalibrating = false
        if completed {
            positionSearch.resumeAfterCalibration(now: .now)
            rematchAfterLook = true
            scheduleAutomaticLocalization()
        } else { positionSearch.cancel(); automaticPaused = true }
    }

    func beginAlignment() {
        guard !lensCalibrating else { return }
        stop()
        if geometryURL?.pathExtension == "scn", let url = geometryURL,
           let bytes = try? Data(contentsOf: url.deletingLastPathComponent().appendingPathComponent("points.json")),
           let points = try? JSONDecoder().decode([SitePoint].self, from: bytes), points.isEmpty {
            actionGeneration += 1
            let request = actionGeneration, h = handle, f = frame, epoch = generation
            actions.submit {
                do {
                    try await SiteRobotClient.shared.align(h, sequence: request, frame: f, position: SitePoint(x: 0, y: 0, z: 0), yaw: 0)
                    if epoch == self.generation { self.selected = true; self.placing = false }
                } catch { if epoch == self.generation { self.error = error.localizedDescription } }
            }
            return
        }
        candidates = []
        selected = true; anchor = nil; lastPosition = nil; destination = nil; placing = true
    }
    func point(_ point: SitePoint, immediate: Bool) {
        guard selected, point.isValid, handle != 0, !lensCalibrating else { return }
        if !candidates.isEmpty { error = "Confirm the suggested Go2 position before choosing a destination."; return }
        if placing {
            guard reading?.pose == true else { error = "Wait for a live pose before aligning Go2."; return }
            guard let anchor else { self.anchor = point; return }
            guard hypot(point.x-anchor.x, point.z-anchor.z) >= 0.25 else { error = "Choose a heading point at least 25 cm ahead of Go2."; return }
            actionGeneration += 1
            let request = actionGeneration
            let h = handle, f = frame, epoch = generation
            let yaw = atan2(-(point.z-anchor.z), point.x-anchor.x)
            actions.submit {
                guard request == self.actionGeneration, epoch == self.generation else { return }
                do {
                    try await SiteRobotClient.shared.align(h, sequence: request, frame: f, position: anchor, yaw: yaw)
                    guard request == self.actionGeneration, self.generation == epoch else { return }
                    self.explorationEpoch = UUID()
                    self.placing = false; self.error = nil
                } catch { if self.generation == epoch { self.error = error.localizedDescription } }
            }
            return
        }
        guard let reading, reading.aligned else { error = "Wait for a suggested Go2 position and confirm it, or place it manually."; return }
        guard abs(point.y-reading.floorY) < 0.15 else { error = "Choose a point on the same level floor as Go2."; return }
        destination = point
        command(move: immediate)
    }
    func command(move: Bool) {
        guard let destination, handle != 0, !lensCalibrating else { return }
        actionGeneration += 1
        let request = actionGeneration, h = handle, f = frame, epoch = generation
        actions.submit {
            guard request == self.actionGeneration, epoch == self.generation else { return }
            do {
                try await SiteRobotClient.shared.destination(h, sequence: request, frame: f, point: destination, move: move)
                if request == self.actionGeneration, epoch == self.generation { self.error = nil }
            } catch { if request == self.actionGeneration, epoch == self.generation { self.error = error.localizedDescription } }
        }
    }

    func stop() {
        actionGeneration += 1
        remoteSearching = false
        error = nil
        automaticPaused = true; awaitingLook = false; rematchAfterLook = false; positionSearch.cancel()
        candidates = []; localizing = false
        placing = false; destination = nil
        searchConclusion = nil; searchTurns = 0
        rotationState = .idle; rotationStartPending = false
        dismissRotationPrompt()
        let h = handle, request = actionGeneration
        actions.submit { if h != 0 { await self.turningClient.stop(h, sequence: request) } }
    }

    private func scheduleAutomaticLocalization() {
        automaticAttempts = 0
        automaticPaused = false
        nextAutomaticAttempt = Date.now.addingTimeInterval(1)
    }

    private func waitForLocalizationMeasurements() {
        if positionSearch.active { rotationState = .waitingForMeasurements }
        error = "Waiting for fresh LiDAR and odometry. Try locating again when measurements are ready."
        scheduleAutomaticLocalization()
    }

    var canStartRotationSearch: Bool {
        handle != 0 && reading?.pose == true && reading?.aligned != true && reading?.moving != true &&
        !localizing && !lensCalibrating && !showLensCalibration && !placing && candidates.isEmpty && !awaitingLook && !positionSearch.active
    }

    private func offerRotationIfNeeded(failedMatch: Bool = false) {
        let eligible = canStartRotationSearch && !automaticPaused && !positionSearch.active
        if rotationOffer.shouldPresent(eligible: eligible, scanReady: reading?.scanReady == true,
                                       failedMatch: failedMatch, now: .now) {
            showRotationPrompt = true
        }
    }

    func dismissRotationPrompt() {
        showRotationPrompt = false
        rotationOffer.dismiss()
    }

    func startRotationSearch() {
        guard handle != 0, let url = geometryURL, !remoteSearching else { return }
        dismissRotationPrompt()
        actionGeneration += 1
        let request = actionGeneration, h = handle, epoch = generation, f = frame, scale = geometryScale
        remoteSearchSequence = request; remoteSearching = false; localizing = true
        searchConclusion = nil; searchTurns = 0; rotationState = .matching(0)
        actions.submit {
            do {
                let mesh = try await SiteRobotGeometry.semantic(url, scale: scale)
                guard epoch == self.generation, request == self.actionGeneration else { return }
                try await self.searchClient.search(h, sequence: request,
                    request: RobotFusionRequest(frame: f, architecture: mesh.architecture, furniture: mesh.furniture, visual: nil, reference: []))
                if epoch == self.generation, request == self.actionGeneration { self.remoteSearching = true }
            } catch {
                guard epoch == self.generation, request == self.actionGeneration else { return }
                self.remoteSearching = false; self.localizing = false
                self.error = error.localizedDescription; self.rotationState = .stopped(error.localizedDescription)
            }
        }
    }


    func offerRotationRetry() {
        guard canStartRotationSearch else { return }
        showRotationPrompt = true
    }

    func locate(recalibrate: Bool = false) {
        guard handle != 0, !localizing, !lensCalibrating, reading?.moving != true, let url = geometryURL else { return }
        actionGeneration += 1
        let request = actionGeneration, h = handle, epoch = generation, f = frame, scale = geometryScale, robot = serial, revision = siteRevision
        rematchAfterLook = false
        localizing = true; candidates = []; placing = false; destination = nil; error = nil
        if positionSearch.active { rotationState = .matching(searchTurns) }
        actions.submit {
            var noMatch = false
            defer {
                if epoch == self.generation, request == self.actionGeneration {
                    self.localizing = false
                    if noMatch { self.offerRotationIfNeeded(failedMatch: true) }
                }
            }
            do {
                let mesh = try await SiteRobotGeometry.semantic(url, scale: scale)
                // The Linux service supplies and persists room references.
                var stored = RobotSavedMap(frame: f, modelDigest: "", robot: robot)
                guard request == self.actionGeneration, epoch == self.generation else { return }
                self.savedMap = stored; self.referenceCount = stored.surfaces.count
                var hint: RobotVisualHint?
                if let first = self.cameraFrame, first.isFresh(at: Date()) {
                    do {
                        let mode = RobotCameraMode(first)
                        let profile = try await BlockingWork.run(on: .global(qos: .userInitiated)) {
                            try RobotLensStore.read(robot: robot, mode: mode)
                        }
                        guard request == self.actionGeneration, epoch == self.generation else { return }
                        let previous = stored.lens.flatMap { mode.matches($0) ? $0 : nil }
                        let lens = profile?.lens ?? (previous?.measured == true || !recalibrate ? previous : nil)
                        self.measuredLens = lens?.measured == true ? lens : nil
                        if self.positionSearch.active && self.measuredLens == nil {
                            self.visualStatus = "Matching LiDAR geometry. Calibrate the camera to include visual measurements."
                        } else {
                            try await self.visual.load(model: url, frame: f, revision: revision)
                            let result = try await self.visual.locate(first: first, lens: lens) {
                                let deadline = ContinuousClock.now.advanced(by: .seconds(3))
                                while ContinuousClock.now < deadline {
                                    try Task.checkCancellation()
                                    guard request == self.actionGeneration, epoch == self.generation else { throw CancellationError() }
                                    if let next = self.cameraFrame, next.sequence != first.sequence,
                                       next.received.timeIntervalSince(first.received) >= 0.5, next.isFresh(at: Date()) { return next }
                                    try await Task.sleep(for: .milliseconds(100))
                                }
                                throw WendyError(message: "Waiting for a second fresh camera frame.")
                            }
                            hint = result.0
                            if result.1.measured { stored.lens = result.1 }
                            self.visualStatus = result.1.measured ? "Calibrated camera pose verified on two frames." : "Estimated camera pose verified on two frames; used as supporting evidence."
                        }
                    } catch is CancellationError { throw CancellationError() }
                    catch SiteLocalizationError.noSolution(let reason, let matches, let inliers) {
                        let detail: String
                        switch reason {
                        case 1: detail = "Too few shared features were found between this view and the room references."
                        case 3: detail = "Matches occupy too little of the image to verify a position."
                        case 7: detail = "Matches lie on one plane, so this view cannot determine the camera lens."
                        default: detail = "The matches did not pass camera geometry validation."
                        }
                        self.visualStatus = "Camera: \(matches) matches, \(inliers) consistent. \(detail) More room references from Go2's height are needed."
                    }
                    catch { self.visualStatus = "Camera: \(error.localizedDescription)" }
                } else {
                    self.visualStatus = "No fresh camera reference yet. Matching measured geometry."
                }
                guard request == self.actionGeneration, epoch == self.generation else { return }
                self.savedMap = stored
                if self.reading?.measurementsReady != true {
                    self.waitForLocalizationMeasurements()
                    return
                }
                let input = RobotFusionRequest(frame: f, architecture: mesh.architecture, furniture: mesh.furniture, visual: hint, reference: stored.surfaces)
                let report = try await SiteRobotClient.shared.fuse(h, sequence: request, request: input)
                guard request == self.actionGeneration, epoch == self.generation else { return }
                if report.waitingForObservations {
                    self.waitForLocalizationMeasurements()
                    return
                }
                self.fusionReport = report
                self.candidates = (report.candidates ?? []).map { SiteRobotCandidate(position: SitePoint(x:$0.X,y:$0.FloorY,z:$0.Z),yaw:$0.Yaw,rms:$0.RMS,coverage:$0.Coverage) }
                noMatch = self.candidates.isEmpty
                if !noMatch { self.showRotationPrompt = false }
                self.candidateIndex = 0; self.matchSequence = request
                self.suggestionFreshness = self.candidates.isEmpty ? nil : RobotSuggestionFreshness(observationRevision: report.observationRevision, received: .now)
                if self.candidates.isEmpty { self.error = report.message }

                guard request == self.actionGeneration, epoch == self.generation else { return }
                if !self.candidates.isEmpty { self.positionSearch.cancel(); self.rotationState = .idle }

            } catch {
                if request == self.actionGeneration, epoch == self.generation {
                    if self.positionSearch.active { self.rotationState = .stopped(error.localizedDescription) }
                    self.positionSearch.cancel(); self.error = error.localizedDescription
                }
            }
        }
    }

    func confirmPosition() {
        guard candidates.indices.contains(candidateIndex), handle != 0 else { return }
        automaticPaused = true
        actionGeneration += 1
        let request = actionGeneration, h = handle, match = matchSequence, epoch = generation, f = frame, index = candidateIndex
        actions.submit {
            do {
                try await SiteRobotClient.shared.confirmLocalization(h, sequence: request, matchSequence: match, frame: f, candidate: index)
                guard epoch == self.generation, request == self.actionGeneration else { return }
                self.explorationEpoch = UUID()
                self.candidates = []; self.error = nil

            } catch {
                if epoch == self.generation {
                    self.error = error.localizedDescription; self.candidates = []
                    self.scheduleAutomaticLocalization()
                }
            }
        }
    }
}

#if os(macOS)
struct SiteRobotPanel: View {
    @ObservedObject var model: SiteRobotMapModel
    let session: UInt64
    let gatewayName: String
    let site: Site
    let modelURL: URL
    let availableHeight: CGFloat
    @Environment(\.scenePhase) private var scenePhase
    @State private var pairing = false
    @State private var expanded = false
    @State private var settingsExpanded = false
    @State private var contentHeight: CGFloat = 160

    private var panelContent: some View {
        VStack(alignment: .leading, spacing: 12) {
            HStack {
                Label("Go2", systemImage: "pawprint").font(.headline)
                Spacer()
                if !model.robots.isEmpty {
                    Button("Stop", systemImage: "stop.fill") { model.stop() }
                        .buttonStyle(.borderedProminent).tint(.red)
                        .keyboardShortcut(.escape, modifiers: [])
                }
                Button {
                    expanded = false
                } label: { Image(systemName: "xmark") }
                    .buttonStyle(.plain).accessibilityLabel("Close robot controls")
            }
            if expanded {
                if model.robots.isEmpty {
                    Text("Pair Go2 with \(gatewayName) to locate it on this floor.")
                        .font(.caption).foregroundStyle(.secondary)
                    Button("Pair Go2", systemImage: "link") { expanded = false; pairing = true }
                        .buttonStyle(.borderedProminent).disabled(session == 0)
                } else {
                    if model.robots.count > 1 {
                        Picker("Robot", selection: $model.serial) {
                            ForEach(model.robots) { Text($0.name.isEmpty ? $0.serial : $0.name).tag($0.serial) }
                        }.labelsHidden()
                    }
                    SiteRobotStatus(model: model, showsDetails: false)
                    if model.handle != 0 && model.reading?.disconnected != true {
                    if model.placing {
                        Text(model.anchor == nil ? "Click the floor beneath Go2." : "Click directly ahead of Go2 to set its heading.")
                            .font(.callout)
                        Button("Cancel alignment") { model.placing = false; model.anchor = nil }
                    } else if model.candidates.isEmpty {
                        HStack {
                            Spacer()
                            Menu {
                                Button("Select Go2") { model.selected = true }
                                Button("Place Manually") { model.beginAlignment() }.disabled(model.reading?.pose != true)
                            } label: { Image(systemName: "ellipsis") }
                                .menuStyle(.borderlessButton).fixedSize().accessibilityLabel("Position options")
                        }.buttonStyle(.bordered)
                    }
                    SiteRobotLocalizationControls(model: model)
                    if model.destination != nil {
                        Button("Move here", systemImage: "arrow.up.right") { model.command(move: true) }
                            .buttonStyle(.borderedProminent).tint(.green)
                            .disabled(model.reading?.ready != true || model.reading?.route.isEmpty != false)
                    }
                    if model.reading?.moving == true || model.destination != nil {
                        SiteRobotSupervisionSummary(model: model)
                    }
                    }
                    DisclosureGroup("Details", isExpanded: $settingsExpanded) {
                        VStack(alignment: .leading, spacing: 12) {
                            RobotLocalizationDiagnostics(model: model)
                            SiteRobotTimingControls(model: model)
                            Text("Click to preview · Right-click to move · Esc to stop")
                                .font(.caption2).foregroundStyle(.secondary)
                            Text("Position follows aligned odometry. LiDAR can retain old observations.")
                                .font(.caption2).foregroundStyle(.secondary)
                            HStack {
                                Button("Manage pairing") { model.stop(); expanded = false; pairing = true }
                            }.font(.caption)
                        }.padding(.top, 8)
                    }.font(.caption)
                }
            }
        }
        .padding(16).frame(width: 286)
        .transaction { $0.animation = nil }
        .onGeometryChange(for: CGFloat.self) { $0.size.height } action: { contentHeight = $0 }
    }

    var body: some View {
        HStack(spacing: 12) {
            Button { expanded.toggle() } label: {
                Label("Go2", systemImage: "pawprint").font(.headline)
                Image(systemName: "chevron.up.chevron.down").font(.caption)
            }
            .buttonStyle(.plain)
            .accessibilityLabel("Go2 controls")
            .accessibilityValue(expanded ? "Expanded" : "Collapsed")
            .popover(isPresented: $expanded, arrowEdge: .top) {
                ScrollView { panelContent }
                    .scrollBounceBehavior(.basedOnSize)
                    .frame(width: 286, height: min(contentHeight, min(480, max(160, availableHeight))))
            }
            Spacer()
            if !model.robots.isEmpty {
                Button("Stop", systemImage: "stop.fill") { model.stop() }
                    .buttonStyle(.borderedProminent).tint(.red)
                    .keyboardShortcut(.escape, modifiers: [])
            }
        }
        .padding(16).frame(width: 286)
        .background(.regularMaterial, in: RoundedRectangle(cornerRadius: 16))
        .overlay(RoundedRectangle(cornerRadius: 16).strokeBorder(model.selected ? Color.green.opacity(0.5) : Color.white.opacity(0.1)))
        .onChange(of: expanded) { _, visible in
            if !visible { model.stop() }
        }
        .onChange(of: model.selected) { _, selected in
            if selected { expanded = true }
        }
        .onChange(of: model.destination != nil) { _, hasDestination in
            if hasDestination { expanded = true }
        }
        .modifier(SiteRobotRotationPrompt(model: model, onStart: { expanded = true }))
        .task { await model.actions.run() }
        .task(id: session) { await model.load(session: session) }
        .task(id: "\(session)/\(model.serial)/\(site.id)/\(site.modelRevision)/\(scenePhase)/\(model.reconnectGeneration)") {
            await model.observe(session: session, site: site, modelURL: modelURL, active: scenePhase == .active)
        }
        .sheet(isPresented: $pairing, onDismiss: { model.actions.submit { await model.load(session: session) } }) {
            VStack {
                UnitreePairingView(session: session, deviceName: gatewayName)
                Button("Done") { pairing = false }.padding()
            }.frame(minWidth: 700, minHeight: 520)
        }
    }
}
#endif

struct SiteRobotRotationPrompt: ViewModifier {
    @ObservedObject var model: SiteRobotMapModel
    var enabled = true
    var onStart: () -> Void = {}

    func body(content: Content) -> some View {
        content.alert("Rotate Go2 to find its position?", isPresented: Binding(
            get: { enabled && model.showRotationPrompt },
            set: { if !$0 { model.dismissRotationPrompt() } }
        )) {
            Button("Start rotation") {
                onStart()
                model.startRotationSearch()
            }.disabled(!model.canStartRotationSearch)
            Button("Not now", role: .cancel) { model.dismissRotationPrompt() }
        } message: {
            Text("No reliable position match was found. Go2 will check clearance, turn in 30° steps, and match its LiDAR observations after each turn. A calibrated camera can also help. Keep Go2 in sight and its turning area clear.")
        }
    }
}

struct SiteRobotStatus: View {
    @ObservedObject var model: SiteRobotMapModel
    var showsDetails = true
    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack(spacing: 6) {
                Circle().fill(model.reading?.ready == true ? Color.green : .orange).frame(width: 6, height: 6)
                Text(model.rotationState == .idle ? model.statusLabel : "Go2 position search").font(.caption)
                if model.connecting { ProgressView().controlSize(.small) }
            }
            RobotRotationFeedback(model: model)
            if showsDetails, model.rotationState == .idle, let message = model.reading?.message, !message.isEmpty,
               message != "Live LiDAR and odometry.", message != "Route ready", message != model.statusLabel {
                Text(message).font(.caption).foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
            }
            if model.rotationState == .idle, let error = model.error, !error.isEmpty {
                Text(error).font(.caption).foregroundStyle(.orange)
                    .fixedSize(horizontal: false, vertical: true)
            }
            if model.handle != 0 { SiteRobotObstacleStatus(model: model) }
        }.sheet(isPresented: $model.showLensCalibration) { RobotLensCalibrationView(model: model) }
    }
}

struct SiteRobotObstacleStatus: View {
    @ObservedObject var model: SiteRobotMapModel
    @ObservedObject private var telemetry: SiteRobotTelemetry

    init(model: SiteRobotMapModel) {
        self.model = model
        self.telemetry = model.telemetry
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            Toggle("Show navigation obstacles", isOn: $model.showObstacles).font(.caption)
            if model.showObstacles {
                if telemetry.reading?.aligned != true || model.placing || !model.candidates.isEmpty {
                    Text("Confirm or place Go2's position to show its obstacles on this floor.")
                        .font(.caption2).foregroundStyle(.secondary)
                } else if telemetry.reading?.hazardsReady != true {
                    Text("Waiting for fresh floor and obstacle data. The overlay is hidden.")
                        .font(.caption2).foregroundStyle(.secondary)
                } else {
                    HStack(spacing: 10) {
                        Label("Obstacle", systemImage: "square.fill").foregroundStyle(.red)
                        Label("Drop", systemImage: "square.fill").foregroundStyle(.orange)
                    }.font(.caption2)
                    Text(telemetry.reading?.hazards.isEmpty == true
                         ? "No obstacle footprints in the current scan. Unmarked floor may be unobserved."
                         : "Measured footprints on the floor. Unmarked floor may be unobserved.")
                        .font(.caption2).foregroundStyle(.secondary)
                }
            }
        }.fixedSize(horizontal: false, vertical: true)
    }
}

struct RobotRotationFeedback: View {
    @ObservedObject var model: SiteRobotMapModel

    var body: some View {
        if let title = model.rotationState.title {
            VStack(alignment: .leading, spacing: 8) {
                HStack(spacing: 8) {
                    if model.rotationState.canRetry {
                        Image(systemName: "exclamationmark.circle").foregroundStyle(.orange)
                    } else {
                        ProgressView().controlSize(.small)
                    }
                    Text(title).font(.caption.weight(.semibold))
                }
                if let detail = model.rotationState.detail {
                    Text(detail).font(.caption).foregroundStyle(.secondary)
                        .fixedSize(horizontal: false, vertical: true)
                }
                if model.rotationState.canRetry {
                    HStack {
                        Button("Try again") { model.offerRotationRetry() }
                            .accessibilityLabel("Try rotation again")
                            .disabled(!model.canStartRotationSearch)
                        Button("Place manually") { model.beginAlignment() }
                            .disabled(model.reading?.pose != true)
                    }.font(.caption).buttonStyle(.bordered)
                } else {
                    Button("Cancel search", role: .cancel) { model.stop() }
                        .font(.caption).buttonStyle(.bordered)
                }
            }
            .padding(10)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(.quaternary, in: RoundedRectangle(cornerRadius: 10))
            .accessibilityElement(children: .contain)
        }
    }
}

struct SiteRobotSupervisionSummary: View {
    @ObservedObject var model: SiteRobotMapModel
    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            if let warning = model.reading?.controlWarning, !warning.isEmpty {
                Text(warning).foregroundStyle(.orange)
            }
            Text(model.reading?.arrivalTiming == true
                 ? "Supervised · sensor age unverified. Keep Go2 in sight."
                 : "Keep Go2 in sight and the route clear.")
            if model.reading?.allowUnobservedFloor == true {
                Text("Supervised navigation: unobserved floor allowed. Keep Go2 in sight and Stop within reach.")
            } else if model.reading?.underBodyAllowance == true {
                Text("Hidden-floor departure enabled. Check beneath Go2.")
            }
        }.font(.caption2).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
    }
}

struct SiteRobotTimingControls: View {
    @ObservedObject var model: SiteRobotMapModel
    var body: some View {
        Toggle("Supervised navigation through unobserved floor", isOn: Binding(
            get: { model.reading?.allowUnobservedFloor ?? true },
            set: { model.setAllowUnobservedFloor($0) }
        )).font(.caption).disabled(model.reading == nil)
        Text("Allows unobserved floor along the route. Go2 stops and tries another route around detected obstacles or drops. Lost telemetry stops movement. Stay with Go2 and keep Stop within reach.")
            .font(.caption2).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
        Group {
            Toggle("Allow hidden floor beneath Go2", isOn: Binding(
                get: { model.reading?.underBodyAllowance ?? true },
                set: { model.setUnderBodyAllowance($0) }
            )).font(.caption).disabled(model.reading == nil)
            Text("Allows departure when the floor beneath Go2 is hidden from its sensors. Check that it is level and clear. Detected obstacles and drops still block movement.")
                .font(.caption2).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
        }
        if model.reading?.timingUnavailable == true || model.reading?.arrivalTiming == true {
            Toggle("Supervised arrival timing", isOn: Binding(
                get: { model.reading?.arrivalTiming == true },
                set: { model.setArrivalTiming($0) }
            )).font(.caption)
            Text(model.reading?.arrivalTiming == true
                 ? "Uses live packet arrivals. Sensor capture age is unverified. Stay with Go2 and keep Stop within reach."
                 : "This firmware rounds LiDAR timestamps. Supervised mode uses packet arrival times for this connection.")
                .font(.caption2).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
        }
    }
}

struct SiteRobotLocalizationControls: View {
    @ObservedObject var model: SiteRobotMapModel
    var body: some View {
        if let conclusion = model.searchConclusion {
            VStack(alignment: .leading, spacing: 8) {
                Text(conclusion).font(.caption).fixedSize(horizontal: false, vertical: true)
                HStack {
                    Button("Try rotation again") { model.offerRotationRetry() }
                        .disabled(!model.canStartRotationSearch)
                    Button("Place Manually") { model.beginAlignment() }
                        .disabled(model.reading?.pose != true)
                }.buttonStyle(.bordered)
            }
        } else if model.localizing && model.rotationState == .idle {
            ProgressView("Locating Go2…").font(.caption)
        } else if model.candidates.indices.contains(model.candidateIndex) {
            let candidate = model.candidates[model.candidateIndex]
            VStack(alignment: .leading, spacing: 8) {
                HStack {
                    Text("Position \(model.candidateIndex+1) of \(model.candidates.count)").font(.subheadline.weight(.semibold))
                    Spacer()
                    if model.candidates.count > 1 {
                        Button("Next") { model.candidateIndex = (model.candidateIndex+1) % model.candidates.count }
                    }
                }
                Text(candidate.coverage < 0.65 || candidate.rms > 0.20
                     ? "Weak match. Check the marker and heading before confirming."
                     : "Check that the marker and heading match Go2.")
                    .font(.caption).foregroundStyle(candidate.coverage < 0.65 || candidate.rms > 0.20 ? .orange : .secondary)
                    .fixedSize(horizontal: false, vertical: true)
                Text(String(format: "%.0f%% surface agreement, not position confidence.", candidate.coverage*100))
                    .font(.caption2).foregroundStyle(.secondary)
                Button("Confirm position") { model.confirmPosition() }
                    .buttonStyle(.borderedProminent)
            }
        }
        Button("Calibrate camera", systemImage: "camera.aperture") {
            model.showLensCalibration = true
        }
        .buttonStyle(.bordered)
        .disabled(model.handle == 0 || model.reading?.moving == true || model.lensCalibrating)
    }
}
