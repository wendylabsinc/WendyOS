import Foundation
import Observation
import QuartzCore
import simd

@MainActor @Observable
final class SpatialRobotSearch {
    var showPrompt = false
    var state: RobotRotationState = .idle
    var candidate: RobotFusionReport.Candidate?
    var message = "Look around to detect the floor and at least two walls."
    private(set) var working = false
    private var offer = RobotRotationOffer()
    private var search = RobotPositionSearch()
    private var generation = 0
    private var handle: UInt64 = 0
    private var frame = ""
    private var attempts = 0
    private var paused = false
    private var awaitingLook = false
    private var pendingTurn = false
    private var readingAfter: TimeInterval = 0
    private var nextAttempt = Date.distantPast
    private var matchSequence: UInt64 = 0
    private var matchedAt = Date.distantPast
    private let client: any RobotSearching

    init(client: any RobotSearching = RobotSearchClient()) { self.client = client }

    private func prepare(_ model: SpatialModel) {
        if handle != model.handle || frame != model.tracking.frameID {
            cancel(); handle = model.handle; frame = model.tracking.frameID
            paused = false; offer = RobotRotationOffer(); attempts = 0; nextAttempt = .distantPast
        }
    }
    func cancel() {
        generation += 1; search.cancel(); awaitingLook = false; pendingTurn = false
        showPrompt = false; candidate = nil; state = .idle; paused = true
    }
    func dismiss() { showPrompt = false; offer.dismiss(); paused = true }

    private func eligible(_ model: SpatialModel) -> Bool {
        model.active && model.immersive && model.tracking.tracked && model.handle != 0 && model.replay == nil
            && model.reading?.pose == true && model.reading?.disconnected == false
            && (0...0.5).contains(CACurrentMediaTime() - model.readingAt)
            && model.alignedFrame != model.tracking.frameID && !model.selectingAlignment && model.registrationStep == nil
    }
    func canStart(_ model: SpatialModel, geometry: SiteRobotGeometryPacket? = nil) -> Bool {
        eligible(model) && !model.busy && !working && !search.active && !awaitingLook
            && model.reading?.moving != true && candidate == nil && (geometry ?? model.robotLocalizationGeometry) != nil
    }
    func request(_ model: SpatialModel) {
        prepare(model)
        guard canStart(model) else { model.error = "Open AR and look around until the floor and at least two walls are detected, with Go2 stationary and streaming its pose."; return }
        showPrompt = true
    }
    func start(_ model: SpatialModel, geometry: SiteRobotGeometryPacket? = nil, now: Date = .now) {
        prepare(model)
        guard canStart(model, geometry: geometry) else { dismiss(); return }
        offer.dismiss(); showPrompt = false; paused = false; model.error = nil
        search.start(now: now); pendingTurn = true; nextAttempt = now
        state = .waitingForMeasurements
    }
    func run(_ model: SpatialModel, owned: UInt64) async {
        guard owned != 0 else { return }
        while !Task.isCancelled, owned == model.handle, model.active {
            await step(model)
            do { try await Task.sleep(for: .milliseconds(200)) } catch { return }
        }
    }
    private func current(_ model: SpatialModel, ticket: Int, order: UInt64) -> Bool {
        !Task.isCancelled && ticket == generation && handle == model.handle && frame == model.tracking.frameID
            && order == model.sequence && eligible(model)
    }
    func step(_ model: SpatialModel, geometry supplied: SiteRobotGeometryPacket? = nil, now: Date = .now) async {
        prepare(model)
        guard !working, !model.busy else { return }
        guard eligible(model) else {
            showPrompt = false
            if search.active || awaitingLook { await model.stop() }
            return
        }
        guard let geometry = supplied ?? model.robotLocalizationGeometry else {
            message = "Look around to detect the floor and at least two walls."
            showPrompt = false
            if search.active || awaitingLook { await model.stop() }
            return
        }
        guard let reading = model.reading, let evidence = reading.search else { return }
        if candidate != nil {
            if now.timeIntervalSince(matchedAt) <= 15 { return }
            candidate = nil; attempts = 0; nextAttempt = now
            message = "Position suggestion expired. Matching fresh observations."
        }
        if awaitingLook {
            guard model.readingAt > readingAfter else { return }
            if reading.moving || evidence.lookState == 1 { return }
            awaitingLook = false
            guard evidence.lookState == 2 else {
                cancel(); state = .stopped(reading.message); return
            }
            search.recordDirection(evidence.lookDirection)
            state = .matching(12 - search.remainingTurns)
            nextAttempt = now.addingTimeInterval(1)
        }
        let retry = search.shouldRetryLocalization(pose: reading.pose, scanReady: evidence.scanReady,
            measurementsReady: evidence.measurementsReady, aligned: false,
            rematchAfterLook: false, attempts: attempts, now: now)
        if let outcome = search.outcome {
            cancel()
            state = .stopped(outcome == .timedOut ? "The search reached its time limit without a reliable match." : "Go2 completed a full rotation without a reliable match. You can align it manually.")
            return
        }
        if !paused && !search.active && offer.shouldPresent(eligible: !reading.moving,
                scanReady: evidence.scanReady, now: now) { showPrompt = true }
        guard !paused, !showPrompt, retry, !reading.moving, now >= nextAttempt else { return }
        working = true; defer { working = false }
        let ticket = generation, order = model.nextSequence(), owned = handle, targetFrame = frame
        do {
            if search.active && (pendingTurn || !evidence.scanReady) {
                guard search.takeTurn(now: now) else {
                    cancel(); state = .stopped("Go2 completed a full rotation without a reliable match."); return
                }
                pendingTurn = false
                state = .checking(12 - search.remainingTurns)
                try await client.look(owned, sequence: order, direction: search.direction)
                guard current(model, ticket: ticket, order: order) else {
                    if owned == model.handle { await model.stop() }; return
                }
                awaitingLook = true; readingAfter = CACurrentMediaTime()
                state = .turning(12 - search.remainingTurns)
                return
            }
            attempts += 1; nextAttempt = now.addingTimeInterval(5)
            if search.active { state = .matching(12 - search.remainingTurns) }
            message = model.usesTwinForRobotMatching
                ? "Matching Go2's LiDAR with your aligned digital twin."
                : "Matching Go2's LiDAR with the walls and floor detected by Vision Pro."
            let report = try await client.fuse(owned, sequence: order,
                request: RobotFusionRequest(frame: targetFrame, architecture: geometry.architecture,
                    furniture: geometry.furniture, visual: nil, reference: []))
            guard current(model, ticket: ticket, order: order) else { return }
            message = report.message
            if report.candidates?.isEmpty != false && !report.waitingForObservations {
                SpatialRoomGeometry.recordFailure(geometry, message: report.message)
            }
            if report.waitingForObservations {
                attempts = 0
                if search.active { state = .waitingForMeasurements }
                return
            }
            if let found = report.candidates?.first {
                candidate = found; matchSequence = order; matchedAt = now
                search.cancel(); state = .idle; showPrompt = false
                message = "Check the suggested robot position and heading in AR, then confirm."
            } else if search.active {
                pendingTurn = true; nextAttempt = now
            } else if offer.shouldPresent(eligible: true, scanReady: evidence.scanReady, failedMatch: true, now: now) {
                showPrompt = true
            }
        } catch {
            guard ticket == generation, owned == model.handle else { return }
            message = error.localizedDescription
            SpatialRoomGeometry.recordFailure(geometry, message: message)
            if search.active { cancel(); state = .stopped(message) }
            else { paused = true; model.error = message }
        }
    }
    func confirm(_ model: SpatialModel) async {
        guard !working, !model.busy, candidate != nil, eligible(model),
              handle == model.handle, frame == model.tracking.frameID,
              model.reading?.moving != true, Date.now.timeIntervalSince(matchedAt) <= 15 else { return }
        working = true; defer { working = false }
        let ticket = generation, order = model.nextSequence(), targetFrame = frame
        do {
            try await client.confirm(handle, sequence: order, matchSequence: matchSequence, frame: targetFrame, candidate: 0)
            guard current(model, ticket: ticket, order: order) else { await model.stop(); return }
            model.alignedFrame = targetFrame; candidate = nil; paused = true
            model.error = nil; model.notice = "Position confirmed. Check the overlay against Go2 before calling it over."
        } catch { if ticket == generation { model.error = error.localizedDescription; candidate = nil; paused = true } }
    }
}
