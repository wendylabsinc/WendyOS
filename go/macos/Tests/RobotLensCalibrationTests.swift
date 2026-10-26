import Foundation
import ImageIO
import Testing
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
    for _ in 0..<12 { #expect(takeTurn(now)) }
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
