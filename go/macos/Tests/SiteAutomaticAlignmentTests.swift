import Foundation
import Testing
@testable import WendyDesktop

@Suite struct AutomaticCameraAlignmentTests {
    @Test func retriesBackOffPerCameraAndRecoverAfterSuccess() {
        var schedule = SiteAlignmentSchedule()
        let now = Date(timeIntervalSince1970: 1000)
        #expect(schedule.isDue("a", at: now))
        #expect(schedule.completed("a", success: false, at: now) == 30)
        #expect(!schedule.isDue("a", at: now.addingTimeInterval(29)))
        #expect(schedule.isDue("a", at: now.addingTimeInterval(30)))
        #expect(schedule.isDue("b", at: now))
        for delay in [60.0, 120, 240, 300, 300] {
            #expect(schedule.completed("a", success: false, at: now) == delay)
        }
        #expect(schedule.completed("a", success: true, at: now) == 60)
        #expect(schedule.completed("a", success: false, at: now) == 30)
    }

    @Test func calibrationConsumersCannotDisableEachOther() {
        let frames = H264CalibrationFrames()
        let background = frames.acquire(), preview = frames.acquire()
        #expect(frames.isEnabled)
        frames.release(background)
        #expect(frames.isEnabled)
        frames.reset()
        #expect(frames.isEnabled)
        frames.release(preview)
        #expect(!frames.isEnabled)
        frames.setEnabled(true)
        let next = frames.acquire()
        frames.setEnabled(false)
        #expect(frames.isEnabled)
        frames.release(next)
        #expect(!frames.isEnabled)
    }

    private func result(measured: Bool) -> SiteLocalizationResult {
        let now = Date()
        return .init(lens: .init(width: 640, height: 480, fx: 640, fy: 640, cx: 320, cy: 240,
            distortionModel: "none", distortion: [], measured: measured, note: "fixture", captureMode: "JPEG 640x480"),
            pose: .init(modelRevision: UUID(), position: .init(x: 0, y: 1, z: 0),
                rotation: .init(0, 0, 0, 1), method: 2, provenance: "fixture", observedAt: now,
                validUntil: now.addingTimeInterval(60)), inliers: measured ? 16 : 8, residual: 1, fittedLens: true)
    }
    private func frame(_ sequence: UInt64) -> CameraCalibrationFrame {
        .init(gray: Data(), width: 640, height: 480, received: Date(timeIntervalSince1970: Double(sequence)), sequence: sequence, codec: "JPEG")
    }

    @Test func approximatePositionStillRequiresTwoConsistentFrames() throws {
        let first = result(measured: false)
        var second = first
        second.pose.position.x = 0.2
        second.residual = 3
        let verified = try SiteAlignmentValidation.verified(first, by: second, first: frame(1), second: frame(2))
        #expect(verified.pose.position.x == 0.2)
        #expect(verified.inliers == 8 && verified.residual == 3)
        second.pose.position.x = 0.31
        #expect(throws: (any Error).self) { try SiteAlignmentValidation.verified(first, by: second, first: frame(1), second: frame(2)) }
        second = first
        let angle = 11.0 * Double.pi / 180
        second.pose.rotation = .init(0, sin(angle / 2), 0, cos(angle / 2))
        #expect(throws: (any Error).self) { try SiteAlignmentValidation.verified(first, by: second, first: frame(1), second: frame(2)) }
    }

    @Test func measuredPositionKeepsStricterMovementLimit() {
        let first = result(measured: true)
        var second = first
        second.pose.position.x = 0.09
        #expect(throws: (any Error).self) { try SiteAlignmentValidation.verified(first, by: second, first: frame(1), second: frame(2)) }
    }

    @Test func repeatedFramesAndCaptureChangesCannotVerifyPosition() {
        let solution = result(measured: false)
        let first = frame(1)
        var second = frame(2)
        second.sequence = first.sequence
        #expect(throws: (any Error).self) { try SiteAlignmentValidation.verified(solution, by: solution, first: first, second: second) }
        second = frame(2); second.width = 320
        #expect(throws: (any Error).self) { try SiteAlignmentValidation.verified(solution, by: solution, first: first, second: second) }
        second = frame(2); second.codec = "H.264"
        #expect(throws: (any Error).self) { try SiteAlignmentValidation.verified(solution, by: solution, first: first, second: second) }
        second = frame(2); second.received = first.received
        #expect(throws: (any Error).self) { try SiteAlignmentValidation.verified(solution, by: solution, first: first, second: second) }
    }

    @Test func workspaceChangesInvalidateBackgroundWork() {
        let context = SiteAutomaticAlignmentContext(site: Site(name: "Room"), modelURL: URL(filePath: "/room.usdz"),
            mapURL: URL(filePath: "/map.json"), gatewayID: "gateway", session: 7, enabled: true)
        var changed = context; changed.session = 8
        #expect(changed != context)
        changed = context; changed.site.replaceModel()
        #expect(changed != context)
        changed = context; changed.enabled = false
        #expect(changed != context)
        changed = context; changed.gatewayID = "other gateway"
        #expect(changed != context)
        changed = context; changed.site.cameras.append(.init(gatewayID: "gateway", sourceID: "camera", name: "Camera"))
        #expect(changed != context)
    }
}
