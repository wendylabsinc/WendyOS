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
        if lensCalibrating { return "Measuring camera lens…" }
        if searchConclusion != nil { return "Search finished without a match" }
        if reading?.moving == true { return "Moving" }
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
            fresh: live && candidate == nil, moving: reading?.moving == true, route: reading?.route ?? [], destination: destination, placing: placing, scan: showScan ? fusionReport?.overlay ?? [] : [])
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
        generation += 1
        explorationEpoch = UUID()
        let epoch = generation
        telemetry.reading = nil; reading = nil; anchor = nil; lastPosition = nil; lastYaw = 0; destination = nil; placing = false; error = nil
        handle = 0; connecting = false
        searchConclusion = nil; searchTurns = 0
        showRotationPrompt = false; rotationOffer = RobotRotationOffer()
        lensSession = nil; lensCalibrating = false; measuredLens = nil; awaitingLook = false; rematchAfterLook = false; positionSearch.cancel(); showLensCalibration = false
        candidates = []; localizing = false; candidateIndex = 0
        geometryURL = modelURL; geometryScale = site.metresPerModelUnit
        siteRevision = site.modelRevision; fusionReport = nil; savedMap = nil; cameraFrame = nil; cameraDecoder = nil; referenceCount = 0; lastEvidence = 0
        scheduleAutomaticLocalization()
        frame = "\(site.id.uuidString)/\(site.modelRevision.uuidString)"
        guard active, session != 0, !serial.isEmpty else { return }
        connecting = true
        var owned: UInt64 = 0
        do {
            owned = try await SiteRobotClient.shared.open(session: session, serial: serial)
            guard !Task.isCancelled, epoch == generation else {
                await SiteRobotClient.shared.close(owned); return
            }
            handle = owned; connecting = false
            while !Task.isCancelled {
                let polledAction = actionGeneration
                let pollWasAwaitingLook = awaitingLook
                let result = try await SiteRobotClient.shared.poll(owned)
                guard epoch == generation, !Task.isCancelled else { break }
                accept(result)
                if awaitingLook && pollWasAwaitingLook && polledAction == actionGeneration && !result.moving && result.lookState != 1 {
                    awaitingLook = false
                    if result.lookState == 2 {
                        positionSearch.recordDirection(result.lookDirection)
                        rematchAfterLook = true
                        nextAutomaticAttempt = Date.now.addingTimeInterval(1)
                        automaticAttempts = 0; automaticPaused = false
                    } else {
                        positionSearch.cancel(); automaticPaused = true
                        error = result.message
                    }
                }
                if result.observationRevision != lastEvidence {
                    lastEvidence = result.observationRevision
                    automaticAttempts = 0
                }
                if !automaticPaused && !localizing && !candidates.isEmpty,
                   suggestionFreshness?.shouldRefresh(observationRevision: result.observationRevision, now: .now) == true {
                    candidates = []
                    suggestionFreshness = nil
                    automaticAttempts = 0
                }
                let retryLocalization = positionSearch.shouldRetryLocalization(
                    pose: result.pose, scanReady: result.scanReady, aligned: result.aligned,
                    rematchAfterLook: rematchAfterLook, attempts: automaticAttempts, now: .now,
                    calibrating: lensCalibrating || showLensCalibration)
                if positionSearch.outcome != nil && searchConclusion == nil { finishUnmatchedSearch() }
                if retryLocalization && !result.moving && !awaitingLook && !placing && !localizing && candidates.isEmpty &&
                    !automaticPaused && !lensCalibrating && !showRotationPrompt && Date.now >= nextAutomaticAttempt {
                    automaticAttempts += 1
                    nextAutomaticAttempt = Date.now.addingTimeInterval(5)
                    locate()
                }
                offerRotationIfNeeded()
                try await Task.sleep(for: .milliseconds(100))
            }
        } catch {
            if epoch == generation, !Task.isCancelled { self.error = error.localizedDescription }
        }
        if owned != 0 { await SiteRobotClient.shared.close(owned) }
        if epoch == generation { handle = 0; connecting = false; telemetry.reading = nil; reading = nil; showRotationPrompt = false }
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
        if !result.pose || result.aligned || result.moving { showRotationPrompt = false }
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
    private func lookAutomatically() {
        guard handle != 0, !lensCalibrating, reading?.moving != true else { return }
        guard positionSearch.takeTurn(now: .now) else {
            finishUnmatchedSearch()
            return
        }
        searchTurns = 12 - positionSearch.remainingTurns
        actionGeneration += 1
        let request = actionGeneration, h = handle, epoch = generation
        automaticPaused = true; candidates = []; localizing = false; placing = false; destination = nil; error = nil
        actions.submit {
            guard request == self.actionGeneration, epoch == self.generation else { return }
            do {
                try await SiteRobotClient.shared.look(h, sequence: request, direction: self.positionSearch.direction)
                guard request == self.actionGeneration, epoch == self.generation else { return }
                self.awaitingLook = true
            } catch {
                if request == self.actionGeneration, epoch == self.generation {
                    self.positionSearch.cancel(); self.error = error.localizedDescription
                }
            }
        }
    }

    func stop() {
        actionGeneration += 1
        automaticPaused = true; awaitingLook = false; rematchAfterLook = false; positionSearch.cancel()
        candidates = []; localizing = false
        placing = false; destination = nil
        searchConclusion = nil; searchTurns = 0
        dismissRotationPrompt()
        let h = handle, request = actionGeneration
        actions.submit { if h != 0 { await SiteRobotClient.shared.stop(h, sequence: request) } }
    }

    private func scheduleAutomaticLocalization() {
        automaticAttempts = 0
        automaticPaused = false
        nextAutomaticAttempt = Date.now.addingTimeInterval(1)
    }

    var canStartRotationSearch: Bool {
        handle != 0 && reading?.pose == true && reading?.aligned != true && reading?.moving != true &&
        !localizing && !lensCalibrating && !showLensCalibration && !placing && candidates.isEmpty && !awaitingLook
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
        guard canStartRotationSearch else { dismissRotationPrompt(); return }
        dismissRotationPrompt()
        searchConclusion = nil; searchTurns = 0
        positionSearch.start(now: .now)
        scheduleAutomaticLocalization()
        locate()
    }

    private func finishUnmatchedSearch() {
        let timedOut = positionSearch.outcome == .timedOut
        stop()
        searchConclusion = timedOut
            ? "The search reached its time limit without a reliable position match."
            : "Go2 completed a full rotation, but the observations still do not identify a reliable position."
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
                var stored = try await BlockingWork.run(on: .global(qos: .userInitiated)) {
                    var map = try RobotReferenceStore.read(model: url, robot: robot, frame: f)
                    let exclusions = try SiteRobotGeometry.referenceExclusions(url, scale: scale)
                    map.surfaces.removeAll { surface in
                        exclusions.contains(x: surface.position.X, y: surface.position.Z, z: -surface.position.Y)
                    }
                    return map
                }
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
                            self.automaticPaused = true
                            self.showLensCalibration = true
                            return
                        }
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
                    } catch is CancellationError { throw CancellationError() }
                    catch SiteLocalizationError.noSolution(let reason, let matches, let inliers) {
                        let detail: String
                        switch reason {
                        case 3: detail = "Matches occupy too little of the image to verify a position."
                        case 7: detail = "Matches lie on one plane, so this view cannot determine the camera lens."
                        default: detail = "The matches did not pass camera geometry validation."
                        }
                        self.visualStatus = "Camera: \(matches) matches, \(inliers) consistent. \(detail) More room references from Go2's height are needed."
                    }
                    catch { self.visualStatus = "Camera: \(error.localizedDescription)" }
                } else {
                    self.visualStatus = "No fresh camera reference yet. Matching measured geometry."
                    if self.positionSearch.active {
                        // A delayed IDR frame is not a cancelled search. Stay
                        // stopped and let polling retry within the search's
                        // original deadline, without requiring another click.
                        self.visualStatus = "Waiting for a fresh Go2 camera frame. The position search will resume automatically."
                        self.scheduleAutomaticLocalization()
                        return
                    }
                }
                guard request == self.actionGeneration, epoch == self.generation else { return }
                self.savedMap = stored
                if self.positionSearch.active && self.reading?.scanReady != true {
                    self.lookAutomatically()
                    return
                }
                let input = RobotFusionRequest(frame: f, architecture: mesh.architecture, furniture: mesh.furniture, visual: hint, reference: stored.surfaces)
                let report = try await SiteRobotClient.shared.fuse(h, sequence: request, request: input)
                guard request == self.actionGeneration, epoch == self.generation else { return }
                self.fusionReport = report
                self.candidates = (report.candidates ?? []).map { SiteRobotCandidate(position: SitePoint(x:$0.X,y:$0.FloorY,z:$0.Z),yaw:$0.Yaw,rms:$0.RMS,coverage:$0.Coverage) }
                noMatch = self.candidates.isEmpty
                if !noMatch { self.showRotationPrompt = false }
                self.candidateIndex = 0; self.matchSequence = request
                self.suggestionFreshness = self.candidates.isEmpty ? nil : RobotSuggestionFreshness(observationRevision: report.observationRevision, received: .now)
                if self.candidates.isEmpty { self.error = report.message }
                if stored.lens?.measured == true {
                    let value = stored
                    try await BlockingWork.run(on: .global(qos: .utility)) { try RobotReferenceStore.save(value, model: url) }
                }
                guard request == self.actionGeneration, epoch == self.generation else { return }
                if !self.candidates.isEmpty { self.positionSearch.cancel() }
                else if self.positionSearch.active { self.lookAutomatically() }
            } catch {
                if request == self.actionGeneration, epoch == self.generation {
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
                if let stored = self.savedMap, let observations = self.fusionReport?.reference, let url = self.geometryURL {
                    let value = RobotReferenceStore.merging(observations, into: stored)
                    do {
                        try await BlockingWork.run(on: .global(qos: .utility)) { try RobotReferenceStore.save(value, model: url) }
                        if epoch == self.generation { self.savedMap = value; self.referenceCount = value.surfaces.count }
                    } catch {
                        if epoch == self.generation { self.error = "Position confirmed, but the reference map could not be saved: \(error.localizedDescription)" }
                    }
                }
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
    @State private var expanded = true
    @State private var settingsExpanded = false
    @State private var contentHeight: CGFloat = 320

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
                    if expanded { model.stop() }
                    expanded.toggle()
                } label: { Image(systemName: expanded ? "chevron.down" : "chevron.up") }
                    .buttonStyle(.plain).accessibilityLabel("Toggle robot controls")
            }
            if expanded {
                if model.robots.isEmpty {
                    Text("Pair Go2 with \(gatewayName) to locate it on this floor.")
                        .font(.caption).foregroundStyle(.secondary)
                    Button("Pair Go2", systemImage: "link") { pairing = true }
                        .buttonStyle(.borderedProminent).disabled(session == 0)
                } else {
                    if model.robots.count > 1 {
                        Picker("Robot", selection: $model.serial) {
                            ForEach(model.robots) { Text($0.name.isEmpty ? $0.serial : $0.name).tag($0.serial) }
                        }.labelsHidden()
                    }
                    SiteRobotStatus(model: model)
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
                    RobotLocalizationDiagnostics(model: model)
                    if model.destination != nil {
                        Button("Move here", systemImage: "arrow.up.right") { model.command(move: true) }
                            .buttonStyle(.borderedProminent).tint(.green)
                            .disabled(model.reading?.ready != true || model.reading?.route.isEmpty != false)
                    }
                    SiteRobotSupervisionSummary(model: model)
                    DisclosureGroup("Settings and help", isExpanded: $settingsExpanded) {
                        VStack(alignment: .leading, spacing: 12) {
                            Go2Preview().frame(height: 100).clipShape(RoundedRectangle(cornerRadius: 10))
                            SiteRobotTimingControls(model: model)
                            Text("Click to preview · Right-click to move · Esc to stop")
                                .font(.caption2).foregroundStyle(.secondary)
                            Text("Position follows aligned odometry. LiDAR can retain old observations.")
                                .font(.caption2).foregroundStyle(.secondary)
                            HStack {
                                Button("Manage pairing") { model.stop(); pairing = true }
                                Spacer()
                                Button("Reconnect") { model.reconnectGeneration += 1 }
                                    .disabled(model.connecting || model.reading?.moving == true)
                            }.font(.caption)
                        }.padding(.top, 8)
                    }.font(.caption)
                }
                if let error = model.error, error != model.reading?.message {
                    Text(error).font(.caption).foregroundStyle(.orange).fixedSize(horizontal: false, vertical: true)
                }
            }
        }
        .padding(16).frame(width: 286)
        .transaction { $0.animation = nil }
        .onGeometryChange(for: CGFloat.self) { $0.size.height } action: { contentHeight = $0 }
    }

    var body: some View {
        ScrollView { panelContent }
        .scrollBounceBehavior(.basedOnSize)
        .frame(width: 286, height: min(contentHeight, max(160, availableHeight)))
        .background(.regularMaterial, in: RoundedRectangle(cornerRadius: 16))
        .clipShape(RoundedRectangle(cornerRadius: 16))
        .overlay(RoundedRectangle(cornerRadius: 16).strokeBorder(model.selected ? Color.green.opacity(0.5) : Color.white.opacity(0.1)))
        .modifier(SiteRobotRotationPrompt(model: model))
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
            Text("No reliable position match was found. Go2 will turn in 30° steps and retry matching after each turn. Keep Go2 in sight and its turning area clear.")
        }
    }
}

struct SiteRobotStatus: View {
    @ObservedObject var model: SiteRobotMapModel
    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack(spacing: 6) {
                Circle().fill(model.reading?.ready == true ? Color.green : .orange).frame(width: 6, height: 6)
                Text(model.statusLabel).font(.caption)
                if model.connecting { ProgressView().controlSize(.small) }
            }
            if let message = model.reading?.message, !message.isEmpty,
               message != "Live LiDAR and odometry.", message != "Route ready", message != model.statusLabel {
                Text(message).font(.caption).foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }.sheet(isPresented: $model.showLensCalibration) { RobotLensCalibrationView(model: model) }
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
        Text("Allows unobserved floor along the route. Detected obstacles, drops and lost telemetry still stop Go2. Unseen hazards cannot be ruled out; stay with Go2 and keep Stop within reach.")
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
        } else if model.localizing {
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
    }
}
