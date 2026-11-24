import Foundation
@preconcurrency import SceneKit
import ImageIO
import SwiftUI
import Testing
import simd
import CoreVideo
@testable import WendyDesktop

private func lensFrame(sequence: UInt64 = 1, received: Date = Date(timeIntervalSince1970: 1000)) -> CameraCalibrationFrame {
    CameraCalibrationFrame(gray: Data(repeating: 128, count: 640 * 480), width: 640, height: 480,
                           received: received, sequence: sequence, codec: "H264")
}

private func measuredRobotLens() -> SiteCameraLens {
    SiteCameraLens(width: 640, height: 480, fx: 570, fy: 570, cx: 320, cy: 240,
                   distortionModel: "brown_conrady", distortion: [-0.35, 0.08, 0, 0, 0], measured: true,
                   note: "Synthetic verified lens", captureMode: "H264 640x480")
}

@Test func robotLensProfilesAreIsolatedByRobotAndCaptureMode() throws {
    let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: root) }
    let mode = RobotCameraMode(lensFrame())
    let profile = RobotLensProfile(robot: "go2/../test", lens: measuredRobotLens(), measuredAt: .now)
    try RobotLensStore.save(profile, mode: mode, root: root)
    #expect(try RobotLensStore.read(robot: profile.robot, mode: mode, root: root)?.lens == profile.lens)
    #expect(try RobotLensStore.read(robot: "different-go2", mode: mode, root: root) == nil)
    var changedMode = mode; changedMode.codec = "JPEG"
    #expect(try RobotLensStore.read(robot: profile.robot, mode: changedMode, root: root) == nil)
    changedMode = mode; changedMode.width = 1280
    #expect(try RobotLensStore.read(robot: profile.robot, mode: changedMode, root: root) == nil)
    #expect(try FileManager.default.contentsOfDirectory(atPath: root.path).count == 1)
    // Invalid replacements must not destroy an existing measurement.
    var invalid = profile; invalid.lens.measured = false
    #expect(throws: (any Error).self) { try RobotLensStore.save(invalid, mode: mode, root: root) }
    #expect(try RobotLensStore.read(robot: profile.robot, mode: mode, root: root)?.lens == profile.lens)
    invalid = profile; invalid.lens.fx = .nan
    #expect(throws: (any Error).self) { try RobotLensStore.save(invalid, mode: mode, root: root) }
    invalid = profile; invalid.robot = "another-go2"
    try JSONEncoder().encode(invalid).write(to: RobotLensStore.url(robot: profile.robot, mode: mode, root: root))
    #expect(throws: (any Error).self) { try RobotLensStore.read(robot: profile.robot, mode: mode, root: root) }
}

@Test func robotLensCaptureRejectsStaleRepeatedAndChangedStreamFrames() throws {
    let now = Date(timeIntervalSince1970: 1000)
    var samples = RobotLensSamples()
    #expect(try !samples.accept(lensFrame(received: now.addingTimeInterval(-4)), now: now))
    #expect(try !samples.accept(lensFrame(received: now.addingTimeInterval(1)), now: now))
    #expect(samples.mode == nil)
    let first = lensFrame()
    #expect(try samples.accept(first, now: now))
    #expect(try !samples.accept(first, now: now))
    #expect(try !samples.accept(lensFrame(sequence: 2, received: now.addingTimeInterval(-1)), now: now))
    var next = lensFrame(sequence: 2, received: now.addingTimeInterval(1))
    next.codec = "JPEG"
    #expect(throws: (any Error).self) { try samples.accept(next, now: next.received) }
    next.codec = "H264"; next.width = 320; next.gray = Data(count: 320 * 480)
    #expect(throws: (any Error).self) { try samples.accept(next, now: next.received) }
    next = lensFrame(sequence: 2, received: now.addingTimeInterval(1))
    #expect(try samples.accept(next, now: next.received))
}

@Test func exportedGo2CheckerboardIsFullResolutionPNG() throws {
    let document = try RobotCalibrationPattern()
    let source = try #require(CGImageSourceCreateWithData(document.data as CFData, nil))
    let image = try #require(CGImageSourceCreateImageAtIndex(source, 0, nil))
    #expect(image.width == 900 && image.height == 1200)
    #expect(document.data.count > 0)
}


@Test func positionSearchOnlyTurnsAfterExplicitStartAndIsBounded() {
    let now = Date(timeIntervalSince1970: 1000)
    var search = RobotPositionSearch()
    let takeTurn = { (date: Date) in search.takeTurn(now: date) }
    #expect(!takeTurn(now))
    search.start(now: now)
    for _ in 0..<6 { #expect(takeTurn(now)) }
    #expect(!takeTurn(now))
    #expect(!search.active)
    search.start(now: now)
    search.cancel()
    #expect(!takeTurn(now))
    search.start(now: now)
    #expect(!takeTurn(now.addingTimeInterval(301)))
    search.start(now: now)
    search.resumeAfterCalibration(now: now.addingTimeInterval(300))
    #expect(takeTurn(now.addingTimeInterval(301)))
    search.cancel()
    search.resumeAfterCalibration(now: now.addingTimeInterval(500))
    #expect(!takeTurn(now.addingTimeInterval(501)))
}

@Test func rotationOfferFollowsFailedMatchAndDoesNotRepeat() {
    let now = Date(timeIntervalSince1970: 1000)
    var offer = RobotRotationOffer()
    #expect({ !offer.shouldPresent(eligible: true, scanReady: true, now: now) }())
    #expect({ offer.shouldPresent(eligible: true, scanReady: true, failedMatch: true, now: now) }())
    offer.dismiss()
    #expect({ !offer.shouldPresent(eligible: true, scanReady: true, failedMatch: true, now: now.addingTimeInterval(30)) }())
    #expect({ !offer.shouldPresent(eligible: true, scanReady: false, now: now.addingTimeInterval(60)) }())
    // A new observation session can offer again, without reusing old consent.
    offer = RobotRotationOffer()
    #expect({ offer.shouldPresent(eligible: true, scanReady: true, failedMatch: true, now: now) }())
}

@Test func completedRotationSearchDoesNotFallBackIntoEndlessMatching() {
    let now = Date(timeIntervalSince1970: 1000)
    var search = RobotPositionSearch()
    search.start(now: now)
    for _ in 0..<6 { #expect({ search.takeTurn(now: now) }()) }
    // The final view still gets matched before declaring failure.
    #expect({ search.shouldRetryLocalization(pose: true, scanReady: true, measurementsReady: true, aligned: false,
                                            rematchAfterLook: true, attempts: 0, now: now) }())
    #expect({ !search.takeTurn(now: now) }())
    #expect(search.outcome == .exhausted)
    #expect({ !search.shouldRetryLocalization(pose: true, scanReady: true, measurementsReady: true, aligned: false,
                                             rematchAfterLook: false, attempts: 0, now: now) }())
    search.start(now: now)
    #expect(search.outcome == nil)
    #expect({ search.takeTurn(now: now) }())
    #expect({ !search.shouldRetryLocalization(pose: true, scanReady: true, measurementsReady: true, aligned: false,
                                             rematchAfterLook: false, attempts: 0, now: now.addingTimeInterval(301)) }())
    #expect(search.outcome == .timedOut)
}

@Test func rotationOfferWaitsForSparseScansAndResetsWhenIneligible() {
    let now = Date(timeIntervalSince1970: 1000)
    var offer = RobotRotationOffer()
    #expect({ !offer.shouldPresent(eligible: true, scanReady: false, now: now) }())
    #expect({ !offer.shouldPresent(eligible: true, scanReady: false, now: now.addingTimeInterval(4)) }())
    // Losing the pose or entering placement/calibration restarts the grace period.
    #expect({ !offer.shouldPresent(eligible: false, scanReady: false, failedMatch: true, now: now.addingTimeInterval(5)) }())
    #expect({ !offer.shouldPresent(eligible: true, scanReady: false, now: now.addingTimeInterval(6)) }())
    #expect({ offer.shouldPresent(eligible: true, scanReady: false, now: now.addingTimeInterval(11)) }())
}

@Test func rotationOfferAllowsPassiveMatchingBeforePrompting() {
    let now = Date(timeIntervalSince1970: 1000)
    var offer = RobotRotationOffer()
    #expect({ !offer.shouldPresent(eligible: true, scanReady: false, now: now) }())
    #expect({ !offer.shouldPresent(eligible: true, scanReady: true, now: now.addingTimeInterval(6)) }())
    #expect({ !offer.shouldPresent(eligible: true, scanReady: false, now: now.addingTimeInterval(7)) }())
    #expect({ !offer.shouldPresent(eligible: true, scanReady: false, now: now.addingTimeInterval(11)) }())
    #expect({ offer.shouldPresent(eligible: true, scanReady: false, now: now.addingTimeInterval(12)) }())
}

@MainActor @Test func rotationSearchRejectsDisconnectedOrStalePrompt() {
    let model = SiteRobotMapModel()
    #expect(!model.canStartRotationSearch)
    model.startRotationSearch()
    #expect(!model.showRotationPrompt)
    #expect(!model.localizing)
}


@Test func calibrationGuidanceNamesMissingImageRegions() {
    var progress = ScreenLensProgress(count: 15, detection: 4, revision: 15, coverageReady: false,
                                      coverage: 0.2, minX: 0.4, maxX: 0.6, minY: 0.4, maxY: 0.6)
    #expect(progress.nextStep.contains("LEFT"))
    progress.minX = 0.2
    #expect(progress.nextStep.contains("RIGHT"))
    progress.maxX = 0.8
    #expect(progress.nextStep.contains("TOP"))
    progress.minY = 0.2
    #expect(progress.nextStep.contains("BOTTOM"))
    progress.detection = 1
    #expect(progress.nextStep.contains("closer"))
    progress.detection = 0
    #expect(progress.nextStep.contains("whole checkerboard"))
    progress.detection = 4; progress.coverageReady = true
    #expect(progress.nextStep.contains("Tilt"))
}

@Test func phoneCalibrationMarksOnlyRecordedPositions() {
    var progress = ScreenLensProgress()
    #expect(ScreenLensSide.allCases.allSatisfy { !progress.hasSample(toward: $0) })
    #expect(progress.phoneMotion == .showPattern)
    progress = ScreenLensProgress(count: 12, detection: 4, revision: 12, coverageReady: false,
                                  coverage: 0.2, minX: 0.4, maxX: 0.6, minY: 0.4, maxY: 0.6)
    #expect(progress.phoneMotion == .position(.left))
    progress.minX = 0.2
    #expect(progress.hasSample(toward: .left))
    #expect(progress.phoneMotion == .position(.right))
    progress.maxX = 0.8
    #expect(progress.phoneMotion == .position(.top))
    progress.minY = 0.2
    #expect(progress.phoneMotion == .position(.bottom))
    progress.maxY = 0.8
    #expect(ScreenLensSide.allCases.allSatisfy { progress.hasSample(toward: $0) })
    // Four positions alone do not establish enough image coverage.
    #expect(progress.phoneMotion == .closer)
    #expect(!progress.shouldSolve(after: 0))
    progress.coverageReady = true
    #expect(progress.phoneMotion == .sideEdges)
    progress.revision = 14
    #expect(progress.phoneMotion == .topBottomEdges)
    progress.detection = 1
    #expect(progress.phoneMotion == .closer)
    progress.detection = 2
    #expect(progress.phoneMotion == .inward)
    progress.detection = 0
    #expect(progress.phoneMotion == .showPattern)
}

@Test func phoneCalibrationKeepsSolvingWhenRetainedSampleCountIsFull() {
    var progress = ScreenLensProgress(count: 30, detection: 4, revision: 30, coverageReady: true)
    #expect(!progress.shouldSolve(after: 30))
    progress.revision = 32
    #expect(!progress.shouldSolve(after: 30))
    progress.revision = 33
    #expect(progress.shouldSolve(after: 30))
    // Replacements can lose an earlier edge sample. Do not keep it checked.
    progress.coverageReady = false
    progress.minX = 0.45
    #expect(!progress.hasSample(toward: .left))
    #expect(!progress.shouldSolve(after: 30))
    progress.coverageReady = true
    progress.count = 11
    #expect(!progress.shouldSolve(after: 0))
    #expect(!progress.shouldSolve(after: 100))
}

@Test func fullCircleSearchKeepsItsChosenDirection() {
    var search = RobotPositionSearch()
    search.start(now: .now)
    search.recordDirection(-1)
    #expect(search.direction == -1)
    search.recordDirection(1)
    #expect(search.direction == -1)
    search.cancel()
    #expect(search.direction == 0)
}

@Test func explicitPositionSearchRetriesSparseViewsWithoutAnotherClick() {
    let now = Date(timeIntervalSince1970: 1000)
    var search = RobotPositionSearch()
    // Passive observation must not start a turn to compensate for sparse scans.
    #expect({ !search.shouldRetryLocalization(pose: true, scanReady: false, measurementsReady: true, aligned: false,
                                          rematchAfterLook: false, attempts: 0, now: now) }())
    search.start(now: now)
    // Camera delays may exceed the passive retry count. Keep the explicit
    // search alive, even if the robot retains an old room alignment.
    #expect({ search.shouldRetryLocalization(pose: true, scanReady: false, measurementsReady: true, aligned: true,
                                         rematchAfterLook: false, attempts: 5, now: now.addingTimeInterval(10)) }())
    #expect({ search.takeTurn(now: now.addingTimeInterval(11)) }())
    search.recordDirection(-1)
    // Completing a turn must lead to matching/reacquisition even while the
    // new view still lacks enough landmarks for scanReady.
    #expect({ search.shouldRetryLocalization(pose: true, scanReady: false, measurementsReady: true, aligned: true,
                                         rematchAfterLook: true, attempts: 0, now: now.addingTimeInterval(15)) }())
    #expect({ search.takeTurn(now: now.addingTimeInterval(16)) }())
    #expect(search.remainingTurns == 4)
    #expect(search.direction == -1)
    #expect({ !search.shouldRetryLocalization(pose: false, scanReady: false, measurementsReady: true, aligned: false,
                                          rematchAfterLook: true, attempts: 0, now: now.addingTimeInterval(17)) }())
    // Waiting never extends the original deadline or rearms a cancelled search.
    #expect({ !search.shouldRetryLocalization(pose: true, scanReady: false, measurementsReady: true, aligned: false,
                                          rematchAfterLook: true, attempts: 0, now: now.addingTimeInterval(301)) }())
    #expect({ !search.takeTurn(now: now.addingTimeInterval(302)) }())
    search.start(now: now)
    search.cancel()
    #expect({ !search.shouldRetryLocalization(pose: true, scanReady: false, measurementsReady: true, aligned: false,
                                          rematchAfterLook: true, attempts: 0, now: now) }())
}

@Test func passivePositionMatchingKeepsItsRetryAndAlignmentLimits() {
    var search = RobotPositionSearch()
    #expect({ search.shouldRetryLocalization(pose: true, scanReady: true, measurementsReady: true, aligned: false,
                                         rematchAfterLook: false, attempts: 2, now: .now) }())
    #expect({ !search.shouldRetryLocalization(pose: true, scanReady: true, measurementsReady: true, aligned: false,
                                          rematchAfterLook: false, attempts: 3, now: .now) }())
    #expect({ !search.shouldRetryLocalization(pose: true, scanReady: true, measurementsReady: true, aligned: true,
                                          rematchAfterLook: false, attempts: 0, now: .now) }())
    #expect(!search.active)
}

@Test func calibrationSuspendsSearchRetriesAndDeadline() {
    let now = Date(timeIntervalSince1970: 1000)
    var search = RobotPositionSearch()
    search.start(now: now)
    #expect({ !search.shouldRetryLocalization(pose: true, scanReady: true, measurementsReady: true, aligned: false,
                                          rematchAfterLook: false, attempts: 0,
                                          now: now.addingTimeInterval(400), calibrating: true) }())
    #expect(search.active)
    search.resumeAfterCalibration(now: now.addingTimeInterval(500))
    #expect({ search.shouldRetryLocalization(pose: true, scanReady: false, measurementsReady: true, aligned: false,
                                         rematchAfterLook: true, attempts: 0, now: now.addingTimeInterval(501)) }())
    #expect({ search.takeTurn(now: now.addingTimeInterval(501)) }())
}

@Test func positionSearchWaitsForTimingRecoveryWithoutSpendingTurnsOrExtendingDeadline() {
    let now = Date(timeIntervalSince1970: 1000)
    var search = RobotPositionSearch()
    search.start(now: now)
    #expect({ !search.shouldRetryLocalization(pose: true, scanReady: false, measurementsReady: false, aligned: false,
        rematchAfterLook: true, attempts: 5, now: now.addingTimeInterval(20)) }())
    #expect(search.active)
    #expect(search.remainingTurns == 6)
    #expect({ search.shouldRetryLocalization(pose: true, scanReady: false, measurementsReady: true, aligned: false,
        rematchAfterLook: true, attempts: 5, now: now.addingTimeInterval(21)) }())
    #expect({ search.takeTurn(now: now.addingTimeInterval(21)) }())
    #expect(search.remainingTurns == 5)
    #expect({ !search.shouldRetryLocalization(pose: true, scanReady: false, measurementsReady: false, aligned: false,
        rematchAfterLook: true, attempts: 0, now: now.addingTimeInterval(301)) }())
    #expect(search.outcome == .timedOut)
    #expect({ !search.shouldRetryLocalization(pose: true, scanReady: true, measurementsReady: true, aligned: false,
        rematchAfterLook: true, attempts: 0, now: now.addingTimeInterval(302)) }())
    search.start(now: now)
    search.cancel()
    #expect({ !search.shouldRetryLocalization(pose: true, scanReady: false, measurementsReady: true, aligned: false,
        rematchAfterLook: true, attempts: 0, now: now.addingTimeInterval(22)) }())
}

private actor RotationTestRobot: SiteRobotObserving, SiteRobotTurning, SiteRobotSearching {
    var measurements: Bool
    let rejectTurns: Bool
    var searches: [UInt64] = []
    var searchRequests: [RobotFusionRequest] = []
    var turns: [UInt64] = []
    var stops: [UInt64] = []
    var opens = 0
    var closed: [UInt64] = []
    private var failPoll = false
    var observationRevision: UInt64 = 1
    var fusions: [UInt64] = []
    var resets: [UInt64] = []
    var aligned = false
    var rejectReset = false
    var fusionHasCandidate = true
    private var holdFusion = false
    private var fusionContinuation: CheckedContinuation<Void, Never>?
    private var holdStatus = false
    private var statusContinuation: CheckedContinuation<Void, Never>?
    var statusPending = false
    var progress = SiteRobotClient.RemoteReply(SearchState: "searching", SearchMessage: "Waiting for measurements", SearchTurns: 0)
    init(measurements: Bool, rejectTurns: Bool) { self.measurements = measurements; self.rejectTurns = rejectTurns }
    func open(session: UInt64, serial: String) async throws -> UInt64 { opens += 1; return UInt64(41 + opens) }
    func close(_ handle: UInt64) async { closed.append(handle) }
    func failNextPoll() { failPoll = true }
    func poll(_ handle: UInt64) async throws -> SiteRobotReading {
        if failPoll { failPoll = false; throw WendyError(message: "The connection was interrupted.") }
        return SiteRobotReading(message: "Live LiDAR and odometry.", pose: true, ready: false, aligned: aligned, moving: progress.SearchState == "searching" && progress.SearchTurns > 0,
            arrivalTiming: true, usingArrivalTiming: true, timingUnavailable: false, scanReady: false, measurementsReady: measurements,
            underBodyAllowance: true, x: 0, z: 0, yaw: 0, floorY: 0, observationRevision: observationRevision, route: [], joints: [])
    }
    func setAligned(_ value: Bool) { aligned = value }
    func setRejectReset(_ value: Bool) { rejectReset = value }
    func resetLocalization(_ handle: UInt64, sequence: UInt64, frame: String) async throws {
        resets.append(sequence)
        if rejectReset { throw WendyError(message: "Go2 did not confirm Stop.") }
        // Deliberately retain the old reading until the next poll: restart
        // must not wait for aligned=false to arrive before issuing search.
    }
    func setObservationRevision(_ revision: UInt64) { observationRevision = revision }
    func failSearch(_ message: String = "Live LiDAR and odometry.") {
        progress.SearchState = "failed"; progress.SearchMessage = message
    }
    func configureFusion(candidate: Bool, hold: Bool = false) { fusionHasCandidate = candidate; holdFusion = hold }
    func releaseFusion() { holdFusion = false; fusionContinuation?.resume(); fusionContinuation = nil }
    func fuse(_ handle: UInt64, sequence: UInt64, request: RobotFusionRequest) async throws -> RobotFusionReport {
        fusions.append(sequence)
        if holdFusion { await withCheckedContinuation { fusionContinuation = $0 } }
        return RobotFusionReport(message: fusionHasCandidate ? "Position matched" : "No position match yet", candidates: fusionHasCandidate ? [.init(X: 1, Z: 2, Yaw: 0, FloorY: 0, RMS: 0.1, Coverage: 0.8)] : [],
            tracking: .init(independentViews: 1, novelFraction: 1, selected: 0, message: ""), Accepted: 10, Rejected: 0,
            waitingForObservations: false, visualUsed: false, observationRevision: observationRevision)
    }
    func look(_ handle: UInt64, sequence: UInt64, direction: Int32) async throws { turns.append(sequence) }
    func stop(_ handle: UInt64, sequence: UInt64) async { stops.append(sequence) }
    func search(_ handle: UInt64, sequence: UInt64, request: RobotFusionRequest) async throws {
        searches.append(sequence)
        searchRequests.append(request)
        if rejectTurns { throw WendyError(message: "An obstacle or drop overlaps Go2's body during this turn.") }
    }
    func holdNextFailedStatus() { holdStatus = true }
    func releaseStatus() { statusContinuation?.resume(); statusContinuation = nil }
    func searchStatus(_ handle: UInt64) async throws -> SiteRobotClient.RemoteReply {
        if holdStatus {
            holdStatus = false; statusPending = true
            await withCheckedContinuation { statusContinuation = $0 }
            statusPending = false
            return .init(SearchState: "failed", SearchMessage: "Previous search failed", SearchTurns: 0)
        }
        return progress
    }
    func publish(turns: Int, moves: Int? = nil, phase: String? = nil) {
        progress.SearchTurns = turns; progress.SearchMoves = moves; progress.SearchPhase = phase
    }
}

@MainActor
private func withRotationModel(measurements: Bool = true, rejectTurns: Bool = false,
                               visualMatcher: any RobotVisualMatching = RobotVisualLocalization(),
                               _ body: @MainActor (SiteRobotMapModel, RotationTestRobot) async throws -> Void) async throws {
    let robot = RotationTestRobot(measurements: measurements, rejectTurns: rejectTurns)
    let model = SiteRobotMapModel(observationClient: robot, turningClient: robot, searchClient: robot, visualMatcher: visualMatcher)
    model.serial = "rotation-test-\(UUID())"
    let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
    try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
    defer { try? FileManager.default.removeItem(at: directory) }
    let url = directory.appendingPathComponent("room.scn")
    let scene = SCNScene()
    scene.rootNode.addChildNode(SCNNode(geometry: SCNBox(width: 4, height: 2, length: 4, chamferRadius: 0)))
    #expect(scene.write(to: url, options: nil, delegate: nil, progressHandler: nil))
    try await withThrowingTaskGroup(of: Void.self) { group in
        group.addTask { await model.actions.run() }
        group.addTask { await model.observe(session: 7, site: Site(name: "Rotation test"), modelURL: url, active: true) }
        defer { group.cancelAll() }
        for _ in 0..<100 {
            if model.canStartRotationSearch { break }
            try await Task.sleep(for: .milliseconds(10))
        }
        try #require(model.canStartRotationSearch)
        try await body(model, robot)
    }
}

@MainActor @Test func robotTelemetryRecoversFromPollingErrorWithoutRestartingMovement() async throws {
    try await withRotationModel { model, robot in
        await robot.failNextPoll()
        for _ in 0..<250 {
            if await robot.opens == 2, model.reading?.pose == true { break }
            try await Task.sleep(for: .milliseconds(10))
        }
        #expect(await robot.opens == 2)
        #expect(await robot.closed == [42])
        #expect(model.handle == 43)
        #expect(model.reading?.pose == true)
        #expect(model.error == nil)
        // Automatic localization normally starts after one second. Recovery
        // must keep telemetry flowing without restarting that movement.
        try await Task.sleep(for: .milliseconds(1200))
        #expect(await robot.searches.isEmpty)
        #expect(await robot.turns.isEmpty)
        model.startRotationSearch()
        for _ in 0..<100 {
            if await robot.searches.count == 1 { break }
            try await Task.sleep(for: .milliseconds(10))
        }
        #expect(await robot.searches.count == 1)
    }
}

@MainActor @Test func rotationRendersLinuxProgressWithoutIssuingLocalTurns() async throws {
    try await withRotationModel { model, robot in
        model.startRotationSearch()
        for _ in 0..<100 {
            if await robot.searches.count == 1 { break }
            try await Task.sleep(for: .milliseconds(10))
        }
        #expect(await robot.searches.count == 1)
        #expect(await robot.turns.isEmpty)
        for turn in [1, 2] {
            await robot.publish(turns: turn)
            for _ in 0..<100 {
                if model.rotationState == .turning(turn) { break }
                try await Task.sleep(for: .milliseconds(10))
            }
            #expect(model.rotationState == .turning(turn))
        }
        #expect(await robot.searches.count == 1)
        #expect(await robot.turns.isEmpty)
        model.stop()
        #expect(model.rotationState == .idle)
    }
}

@MainActor @Test func cancellingLinuxSearchDoesNotResumeFromLateProgress() async throws {
    try await withRotationModel(measurements: false) { model, robot in
        model.startRotationSearch()
        for _ in 0..<100 {
            if await robot.searches.count == 1 { break }
            try await Task.sleep(for: .milliseconds(10))
        }
        model.stop()
        await robot.publish(turns: 1)
        try await Task.sleep(for: .milliseconds(300))
        #expect(model.rotationState == .idle)
        #expect(await robot.turns.isEmpty)
        #expect(await robot.stops.count == 1)
    }
}

@MainActor @Test func rejectedLinuxSearchShowsReasonAndRequiresExplicitRetry() async throws {
    try await withRotationModel(rejectTurns: true) { model, robot in
        model.startRotationSearch()
        for _ in 0..<100 {
            if model.rotationState.canRetry { break }
            try await Task.sleep(for: .milliseconds(10))
        }
        #expect(model.rotationState.title == "Discovery stopped")
        #expect(model.rotationState.detail?.contains("obstacle") == true)
        #expect(model.canStartRotationSearch)
        try await Task.sleep(for: .milliseconds(300))
        #expect(await robot.searches.count == 1)
        #expect(await robot.turns.isEmpty)
        model.offerRotationRetry()
        #expect(!model.showRotationPrompt)
        for _ in 0..<100 {
            if await robot.searches.count == 2 { break }
            try await Task.sleep(for: .milliseconds(10))
        }
        #expect(await robot.searches.count == 2)
    }
}

@MainActor @Test func liveRobotAutomaticallyStartsOneLinuxSearchWithoutPrompt() async throws {
    try await withRotationModel { model, robot in
        for _ in 0..<200 {
            if await robot.searches.count == 1 { break }
            try await Task.sleep(for: .milliseconds(10))
        }
        #expect(await robot.searches.count == 1)
        #expect(!model.showRotationPrompt)
        #expect(await robot.turns.isEmpty)
        model.stop()
        try await Task.sleep(for: .milliseconds(1200))
        #expect(await robot.searches.count == 1)
    }
}


@Suite struct Go2CameraProfileTests {
    private func mode(width: Int = 1280, height: Int = 720, codec: String = "H.264") -> RobotCameraMode {
        var mode = RobotCameraMode(lensFrame())
        mode.width = width; mode.height = height; mode.codec = codec
        return mode
    }

    @Test func presetRetainsDistortionAndIsNeverAMeasurement() throws {
        let mode = mode()
        let lens = try #require(Go2CameraProfile.lens(for: mode))
        #expect(lens.isValid)
        #expect(!lens.measured)
        #expect(mode.matches(lens))
        #expect(lens.boundaryRays?.count == 32)
        #expect(lens.distortion == [-0.354630, 0.102054, -0.001614, -0.001249, 0])
        #expect(lens.note.contains(Go2CameraProfile.identifier))
        // A preset cannot overwrite a measured per-robot calibration file.
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: root) }
        #expect(throws: (any Error).self) {
            try RobotLensStore.save(RobotLensProfile(robot: "go2", lens: lens, measuredAt: .now), mode: mode, root: root)
        }
        #expect(!FileManager.default.fileExists(atPath: root.path))
    }

    @Test func decodedVideoFrameSelectsThePreset() throws {
        var buffer: CVPixelBuffer?
        #expect(CVPixelBufferCreate(kCFAllocatorDefault, 1280, 720,
            kCVPixelFormatType_420YpCbCr8BiPlanarFullRange, nil, &buffer) == kCVReturnSuccess)
        let pixelBuffer = try #require(buffer)
        let frame = try #require(CameraCalibrationFrame.luma(pixelBuffer, received: .now, sequence: 1))
        #expect(RobotLensSelection.resolve(mode: RobotCameraMode(frame), measured: nil)?.source == .go2Preset)
    }

    @Test func unknownModesNeverScaleOrReuseThePreset() {
        for mode in [mode(width: 1920, height: 1080), mode(width: 640, height: 360),
                     mode(width: 720, height: 1280), mode(height: 800), mode(codec: "JPEG"), mode(codec: "HEVC")] {
            #expect(Go2CameraProfile.lens(for: mode) == nil)
            #expect(RobotLensSelection.resolve(mode: mode, measured: nil) == nil)
        }
    }

    @Test func measuredOverrideWinsAndMismatchedMeasurementsDoNotLeak() throws {
        let mode = mode()
        var measured = try #require(Go2CameraProfile.lens(for: mode))
        measured.measured = true; measured.fx = 870; measured.note = "Measured on this robot"
        let selected = try #require(RobotLensSelection.resolve(mode: mode, measured: measured))
        #expect(selected.source == .measured)
        #expect(selected.lens == measured)
        #expect(RobotLensSelection.resolve(mode: mode, measured: nil, previous: measured)?.source == .measured)
        measured.captureMode = "JPEG 1280x720"
        #expect(RobotLensSelection.resolve(mode: mode, measured: measured)?.source == .go2Preset)
        measured.captureMode = mode.label; measured.fx = .nan
        #expect(RobotLensSelection.resolve(mode: mode, measured: measured)?.source == .go2Preset)
        #expect(RobotLensSelection.resolve(mode: mode, measured: nil, recalibrate: true)?.source == .go2Preset)
    }

    @Test func unsupportedModesKeepTheExistingEstimatedFallback() throws {
        let mode = mode(width: 640, height: 480)
        var previous = measuredRobotLens(); previous.measured = false; previous.captureMode = mode.label
        let selected = try #require(RobotLensSelection.resolve(mode: mode, measured: nil, previous: previous))
        #expect(selected.source == .estimated)
        #expect(selected.lens == previous)
        #expect(RobotLensSelection.resolve(mode: mode, measured: nil, previous: previous, recalibrate: true) == nil)
    }

    @Test func verifiedPoseDoesNotPromotePresetToMeasuredLens() throws {
        let lens = try #require(Go2CameraProfile.lens(for: mode()))
        var output = WSLocalization()
        output.success = 1; output.fitted_lens = 2; output.inliers = 80; output.rms = 1
        output.world_from_camera = (1,0,0,0, 0,1,0,0, 0,0,1,0, 1,2,3,1)
        let result = try SiteLocalizationResult.decode(output, width: 1280, height: 720, revision: UUID(), existing: lens)
        #expect(result.lens == lens)
        #expect(!result.lens.measured)
        #expect(result.pose.provenance.contains("assumed"))
    }

    @Test func cameraMountIsRemovedInWorldAxesForEveryHeadingAndTilt() {
        let bodyPosition = SIMD3<Double>(2, 0.4, -3)
        // Body axes are forward/left/up; camera axes are right/down/forward.
        let opticalToBody = simd_double3x3(columns: (SIMD3(0,-1,0), SIMD3(0,0,-1), SIMD3(1,0,0)))
        let mount = SIMD3<Double>(0.32715, -0.00003, 0.04297)
        for yaw in [0.0, Double.pi / 2, Double.pi, -Double.pi / 2] {
            for pitch in [-0.2, 0.0, 0.2] {
                let rotation = simd_quatd(angle: yaw, axis: SIMD3(0,1,0))
                    * simd_quatd(angle: pitch, axis: SIMD3(1,0,0))
                    * simd_quatd(angle: 0.1, axis: SIMD3(0,0,1))
                let cameraPosition = bodyPosition + rotation.act(mount)
                let pose = SiteCameraPose(modelRevision: nil,
                    position: SitePoint(x: cameraPosition.x, y: cameraPosition.y, z: cameraPosition.z),
                    rotation: (rotation * simd_quatd(opticalToBody)).vector,
                    method: 2, provenance: "fixture", observedAt: .now, validUntil: .now.addingTimeInterval(60))
                #expect(simd_distance(Go2CameraProfile.bodyPosition(camera: pose), bodyPosition) < 1e-9)
            }
        }
    }

    @MainActor @Test func changingCameraModeClearsThePresetAndIgnoresStaleFrames() {
        let model = SiteRobotMapModel()
        var frame = lensFrame(received: .now)
        frame.width = 1280; frame.height = 720; frame.codec = "H.264"
        frame.gray = Data(count: frame.width * frame.height)
        model.acceptCameraFrame(frame)
        #expect(model.lensSelection?.source == .go2Preset)
        var stale = frame; stale.width = 640; stale.received = .distantPast
        model.acceptCameraFrame(stale)
        #expect(model.lensSelection?.source == .go2Preset)
        frame.codec = "JPEG"; frame.received = .now
        model.acceptCameraFrame(frame)
        #expect(model.lensSelection == nil)
    }
}


@MainActor
private final class PresetTestMatcher: RobotVisualMatching {
    var receivedLens: SiteCameraLens?
    var fail = false
    var onLocate: (() -> Void)?
    func load(model: URL, frame: String, revision: UUID) async throws {
        if fail { throw SiteLocalizationError.invalidMap }
    }
    func locate(first: CameraCalibrationFrame, lens: SiteCameraLens?,
                next: @MainActor () async throws -> CameraCalibrationFrame) async throws -> (RobotVisualHint, SiteCameraLens) {
        receivedLens = lens
        onLocate?()
        let lens = try #require(lens)
        return (RobotVisualHint(position: RobotMapPoint(X: 2, Y: 3), yaw: 0.5,
                                at: first.received.timeIntervalSince1970, inliers: 80, residual: 1), lens)
    }
}

@Suite @MainActor struct Go2PresetSearchTests {
    private func frame() -> CameraCalibrationFrame {
        CameraCalibrationFrame(gray: Data(count: 1280 * 720), width: 1280, height: 720,
                               received: .now, sequence: 1, codec: "H.264")
    }

    @Test func activeLinuxSearchReceivesPresetCameraEvidence() async throws {
        let visual = PresetTestMatcher()
        try await withRotationModel(visualMatcher: visual) { model, robot in
            model.acceptCameraFrame(frame())
            model.startRotationSearch()
            for _ in 0..<200 {
                if await robot.searches.count == 1 { break }
                try await Task.sleep(for: .milliseconds(10))
            }
            #expect(visual.receivedLens?.measured == false)
            #expect(visual.receivedLens?.fx == 864.39938)
            #expect(model.lensSelection?.source == .go2Preset)
            let request = try #require(await robot.searchRequests.first)
            #expect(request.visual?.position.X == 2)
            #expect(request.visual?.inliers == 80)
            #expect(await robot.turns.isEmpty)
        }
    }

    @Test func failedCameraMatchStillStartsLidarSearchWithoutAHint() async throws {
        let visual = PresetTestMatcher(); visual.fail = true
        try await withRotationModel(visualMatcher: visual) { model, robot in
            model.acceptCameraFrame(frame())
            model.startRotationSearch()
            for _ in 0..<200 {
                if await robot.searches.count == 1 { break }
                try await Task.sleep(for: .milliseconds(10))
            }
            let request = try #require(await robot.searchRequests.first)
            #expect(request.visual == nil)
            #expect(await robot.turns.isEmpty)
        }
    }

    @Test func stoppingDuringCameraMatchNeverStartsTheRemoteSearch() async throws {
        let visual = PresetTestMatcher()
        try await withRotationModel(visualMatcher: visual) { model, robot in
            visual.onLocate = { model.stop() }
            model.acceptCameraFrame(frame())
            model.startRotationSearch()
            for _ in 0..<200 {
                if visual.receivedLens != nil { break }
                try await Task.sleep(for: .milliseconds(10))
            }
            #expect(visual.receivedLens != nil)
            try await Task.sleep(for: .milliseconds(50))
            #expect(await robot.searches.isEmpty)
            #expect(!model.localizing)
            #expect(await robot.turns.isEmpty)
        }
    }
}


@Suite @MainActor struct Go2DiscoveryTests {
    @Test func discoveryRequestsTranslationAndShowsRemoteMovementPhases() async throws {
        try await withRotationModel { model, robot in
            model.startRotationSearch()
            for _ in 0..<200 {
                if await !robot.searchRequests.isEmpty { break }
                try await Task.sleep(for: .milliseconds(10))
            }
            let request = try #require(await robot.searchRequests.first)
            #expect(request.explore)
            await robot.publish(turns: 3, moves: 1, phase: "advancing")
            for _ in 0..<100 {
                if model.rotationState == .advancing(1) { break }
                try await Task.sleep(for: .milliseconds(10))
            }
            #expect(model.rotationState == .advancing(1))
            #expect(model.searchMoves == 1)
            await robot.publish(turns: 3, moves: 1, phase: "settling")
            for _ in 0..<100 {
                if model.rotationState == .settling(1) { break }
                try await Task.sleep(for: .milliseconds(10))
            }
            #expect(model.rotationState == .settling(1))
            #expect(await robot.turns.isEmpty)
        }
    }

    @Test func waitingForClearViewRemainsActiveAndResumesTheExistingSearch() async throws {
        try await withRotationModel { model, robot in
            model.startRotationSearch()
            for _ in 0..<100 {
                if await robot.searches.count == 1 { break }
                try await Task.sleep(for: .milliseconds(10))
            }
            await robot.publish(turns: 3, moves: 1, phase: "waiting")
            for _ in 0..<100 {
                if model.rotationState == .waitingForClearView { break }
                try await Task.sleep(for: .milliseconds(10))
            }
            #expect(model.rotationState == .waitingForClearView)
            #expect(model.rotationState.title == "Waiting for a clear view")
            #expect(model.rotationState.detail == "Checking fresh LiDAR and floor coverage. Discovery resumes when safe.")
            #expect(!model.rotationState.canRetry)
            #expect(model.localizing)
            #expect(model.error == nil)
            await robot.publish(turns: 3, moves: 2, phase: "advancing")
            for _ in 0..<100 {
                if model.rotationState == .advancing(2) { break }
                try await Task.sleep(for: .milliseconds(10))
            }
            #expect(model.rotationState == .advancing(2))
            #expect(await robot.searches.count == 1)
            #expect(await robot.turns.isEmpty)
            model.stop()
            await robot.publish(turns: 3, moves: 2, phase: "waiting")
            try await Task.sleep(for: .milliseconds(300))
            #expect(model.rotationState == .idle)
            #expect(!model.localizing)
            #expect(await robot.searches.count == 1)
        }
    }

    @Test func rotationOnlyPreferenceDoesNotRequestTranslation() async throws {
        try await withRotationModel { model, robot in
            model.exploreDuringSearch = false
            model.startRotationSearch()
            for _ in 0..<200 {
                if await !robot.searchRequests.isEmpty { break }
                try await Task.sleep(for: .milliseconds(10))
            }
            let request = try #require(await robot.searchRequests.first)
            #expect(!request.explore)
        }
    }

    @Test func oldSearchRepliesRemainDecodable() throws {
        let data = Data(#"{"SearchState":"searching","SearchMessage":"Turning","SearchTurns":1}"#.utf8)
        let reply = try JSONDecoder().decode(SiteRobotClient.RemoteReply.self, from: data)
        #expect(reply.SearchMoves == nil && reply.SearchPhase == nil)
    }
}

@MainActor
@Test func robotStatusNamesMissingPositionAndKeepsFailureVisibleWhenCollapsed() {
    let model = SiteRobotMapModel()
    #expect(model.statusLabel == "Waiting for Go2 position data")
    var reading = SiteRobotReading(message: "Waiting for odometry from Go2.", pose: false, ready: false, aligned: false, moving: false,
        arrivalTiming: false, usingArrivalTiming: false, timingUnavailable: false, scanReady: false, measurementsReady: false,
        underBodyAllowance: false, x: 0, z: 0, yaw: 0, floorY: 0, route: [], joints: [])
    model.accept(reading)
    #expect(model.statusLabel == "Waiting for Go2 position data")
    #expect(model.statusMessage(showsDetails: false) == reading.message)

    reading.pose = true
    reading.message = "Waiting for more room landmarks."
    model.accept(reading)
    #expect(model.statusLabel == "Waiting for room landmarks")
    #expect(model.statusMessage(showsDetails: false) == nil)
    #expect(model.statusMessage(showsDetails: true) == reading.message)

    reading.disconnected = true
    reading.message = "Go2 connection was interrupted."
    model.accept(reading)
    #expect(model.statusLabel == "Go2 disconnected")
    #expect(model.statusMessage(showsDetails: false) == reading.message)
}


@MainActor @Test func failedDiscoveryRecoversFromFreshObservationsWithoutRestartingMotion() async throws {
    try await withRotationModel { model, robot in
        model.startRotationSearch()
        for _ in 0..<100 { if await robot.searches.count == 1 { break }; try await Task.sleep(for: .milliseconds(10)) }
        await robot.failSearch()
        for _ in 0..<100 { if model.rotationState.canRetry { break }; try await Task.sleep(for: .milliseconds(10)) }
        #expect(model.error?.contains("Live LiDAR") == false)
        try await Task.sleep(for: .milliseconds(1200))
        #expect(await robot.fusions.isEmpty)
        await robot.setObservationRevision(2)
        for _ in 0..<200 { if !model.candidates.isEmpty { break }; try await Task.sleep(for: .milliseconds(10)) }
        #expect(model.candidates.count == 1)
        #expect(model.error == nil)
        #expect(model.rotationState == .idle)
        #expect(await robot.fusions.count == 1)
        #expect(await robot.searches.count == 1)
        #expect(await robot.turns.isEmpty)
    }
}

@MainActor @Test func passiveDiscoveryRecoveryIsBoundedAndNeverRetriesTheSameObservation() async throws {
    try await withRotationModel { model, robot in
        await robot.configureFusion(candidate: false)
        model.startRotationSearch()
        for _ in 0..<100 { if await robot.searches.count == 1 { break }; try await Task.sleep(for: .milliseconds(10)) }
        await robot.failSearch("Discovery stopped: LiDAR became stale.")
        for _ in 0..<100 { if model.rotationState.canRetry { break }; try await Task.sleep(for: .milliseconds(10)) }
        for revision in 2...5 {
            await robot.setObservationRevision(UInt64(revision))
            try await Task.sleep(for: .milliseconds(1200))
            #expect(await robot.fusions.count == min(revision - 1, 3))
        }
        try await Task.sleep(for: .milliseconds(1200))
        #expect(await robot.fusions.count == 3)
        #expect(await robot.searches.count == 1)
        #expect(await robot.turns.isEmpty)
        #expect(model.rotationState.canRetry)
    }
}

@MainActor @Test func stopDiscardsPendingPassiveRecoveryAndCancelsFutureAttempts() async throws {
    try await withRotationModel { model, robot in
        await robot.configureFusion(candidate: true, hold: true)
        model.startRotationSearch()
        for _ in 0..<100 { if await robot.searches.count == 1 { break }; try await Task.sleep(for: .milliseconds(10)) }
        await robot.failSearch()
        for _ in 0..<100 { if model.rotationState.canRetry { break }; try await Task.sleep(for: .milliseconds(10)) }
        await robot.setObservationRevision(2)
        for _ in 0..<200 { if await robot.fusions.count == 1 { break }; try await Task.sleep(for: .milliseconds(10)) }
        try #require(await robot.fusions.count == 1)
        model.stop()
        await robot.releaseFusion()
        await robot.setObservationRevision(3)
        try await Task.sleep(for: .milliseconds(1200))
        #expect(model.candidates.isEmpty)
        #expect(model.rotationState == .idle)
        #expect(await robot.fusions.count == 1)
        #expect(await robot.searches.count == 1)
        #expect(await robot.stops.count == 1)
    }
}

@MainActor @Test func explicitDiscoveryRetryClearsPreviousErrorImmediately() async throws {
    try await withRotationModel(rejectTurns: true) { model, robot in
        model.startRotationSearch()
        for _ in 0..<100 { if model.rotationState.canRetry { break }; try await Task.sleep(for: .milliseconds(10)) }
        try #require(model.error != nil)
        model.offerRotationRetry()
        #expect(model.error == nil)
        #expect(model.localizing)
    }
}


@MainActor @Test func restartDiscoveryCanReplaceWrongConfirmedPositionAndPendingCandidate() async throws {
    try await withRotationModel { model, robot in
        await robot.setAligned(true)
        for _ in 0..<100 { if model.reading?.aligned == true { break }; try await Task.sleep(for: .milliseconds(10)) }
        #expect(!model.canStartRotationSearch)
        model.candidates = [.init(position: SitePoint(x: 2, y: 0, z: 3), yaw: 0, rms: 0.1, coverage: 0.8)]
        #expect(model.canRestartDiscovery)
        model.restartDiscovery()
        #expect(model.candidates.isEmpty)
        for _ in 0..<200 { if await robot.searches.count == 1 { break }; try await Task.sleep(for: .milliseconds(10)) }
        let resets = await robot.resets
        let searches = await robot.searches
        #expect(resets.count == 1)
        #expect(searches.count == 1)
        if let reset = resets.first, let search = searches.first { #expect(search > reset) }
        #expect(model.error == nil)
    }
}

@MainActor @Test func restartDiscoveryDoesNotSearchAfterResetStopFailure() async throws {
    try await withRotationModel { model, robot in
        await robot.setRejectReset(true)
        model.restartDiscovery()
        for _ in 0..<100 { if model.rotationState.canRetry { break }; try await Task.sleep(for: .milliseconds(10)) }
        #expect(model.error == "Go2 did not confirm Stop.")
        #expect(await robot.resets.count == 1)
        #expect(await robot.searches.isEmpty)
        #expect(await robot.turns.isEmpty)
    }
}


@MainActor @Test func staleFailedSearchStatusCannotOverrideStopOrNewSearch() async throws {
    for retry in [false, true] {
        try await withRotationModel { model, robot in
            await robot.holdNextFailedStatus()
            model.startRotationSearch()
            for _ in 0..<100 { if await robot.statusPending { break }; try await Task.sleep(for: .milliseconds(10)) }
            try #require(await robot.statusPending)
            model.stop()
            if retry {
                model.startRotationSearch()
                for _ in 0..<100 { if await robot.searches.count == 2 { break }; try await Task.sleep(for: .milliseconds(10)) }
            }
            await robot.releaseStatus()
            try await Task.sleep(for: .milliseconds(300))
            #expect(model.error == nil)
            #expect(!model.rotationState.canRetry)
            #expect(model.localizing == retry)
            #expect(await robot.searches.count == (retry ? 2 : 1))
            #expect(await robot.fusions.isEmpty)
            if !retry { #expect(model.rotationState == .idle) }
        }
    }
}


@MainActor @Test func restartDiscoveryCanReplaceAnActiveStationarySearch() async throws {
    try await withRotationModel { model, robot in
        model.startRotationSearch()
        for _ in 0..<100 { if await robot.searches.count == 1 { break }; try await Task.sleep(for: .milliseconds(10)) }
        try #require(model.localizing)
        try #require(model.reading?.moving == false)
        #expect(model.canRestartDiscovery)
        model.restartDiscovery()
        for _ in 0..<100 { if await robot.searches.count == 2 { break }; try await Task.sleep(for: .milliseconds(10)) }
        #expect(await robot.resets.count == 1)
        #expect(await robot.searches.count == 2)
        #expect(model.error == nil)
    }
}

@MainActor @Test func reconnectRobotRequestsTelemetryWithoutStartingDiscovery() async throws {
    let robot = RotationTestRobot(measurements: true, rejectTurns: false)
    let model = SiteRobotMapModel(observationClient: robot, turningClient: robot, searchClient: robot)
    model.serial = "reconnect-test"
    model.localizing = true
    model.error = "The robot session expired."
    #expect(model.canReconnectRobot)
    model.reconnectRobot()
    #expect(model.reconnectGeneration == 1)
    #expect(!model.localizing)
    #expect(model.rotationState == .idle)
    #expect(model.error == nil)
    #expect(await robot.searches.isEmpty)
    #expect(await robot.turns.isEmpty)
    model.connecting = true
    model.reconnectRobot()
    #expect(model.reconnectGeneration == 1)
}
