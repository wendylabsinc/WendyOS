import Foundation
import Testing
import SwiftUI
@preconcurrency import SceneKit
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

    @Test func ambiguousNativeFitIsDisplayOnly() throws {
        var output = WSLocalization()
        output.reason = 8; output.fitted_lens = 2; output.inliers = 12
        output.fx = 640; output.fy = 640; output.cx = 320; output.cy = 240; output.rms = 1
        output.world_from_camera = (1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 1, 2, 3, 1)
        let revision = UUID()
        do {
            _ = try SiteLocalizationResult.decode(output, width: 640, height: 480, revision: revision, existing: nil)
            Issue.record("An ambiguous candidate must never return a saveable alignment result")
        } catch SiteLocalizationError.unsure(let candidate) {
            #expect(candidate.pose.position == SitePoint(x: 1, y: 2, z: 3))
            #expect(candidate.lens?.measured == false)
            #expect(candidate.pose.isCurrent(revision: revision, now: Date()))
            #expect(candidate.pose.validUntil.timeIntervalSince(candidate.pose.observedAt) == 300)
        }
        output.inliers = 0
        #expect(throws: (any Error).self) {
            try SiteLocalizationResult.decode(output, width: 640, height: 480, revision: revision, existing: nil)
        }
        output.fitted_lens = 0
        do {
            _ = try SiteLocalizationResult.decode(output, width: 640, height: 480, revision: revision, existing: nil)
            Issue.record("A failed refinement must not supply a candidate")
        } catch SiteLocalizationError.noSolution(let reason, _, _) {
            #expect(reason == 8)
        }
    }

    @Test func candidatesExpireAndCannotCrossWorkspaces() {
        let now = Date()
        let context = SiteAutomaticAlignmentContext(site: Site(name: "Room"), modelURL: URL(filePath: "/room.usdz"),
            mapURL: URL(filePath: "/map.json"), gatewayID: "gateway", session: 7, enabled: true)
        var solution = result(measured: false)
        solution.pose.modelRevision = context.site.modelRevision
        solution.pose.observedAt = now
        solution.pose.validUntil = now.addingTimeInterval(300)
        let candidates = SiteAlignmentCandidates(context: context,
            values: ["camera": SiteCameraGeometry(pose: solution.pose, lens: solution.lens)])
        #expect(candidates.visible(in: context, at: now).count == 1)
        #expect(candidates.visible(in: context, at: now.addingTimeInterval(300)).isEmpty)
        var changed = context; changed.session = 8
        #expect(candidates.visible(in: changed, at: now).isEmpty)
        changed = context; changed.site.replaceModel()
        #expect(candidates.visible(in: changed, at: now).isEmpty)
        changed = context; changed.enabled = false
        #expect(candidates.visible(in: changed, at: now).isEmpty)
        changed = context; changed.gatewayID = "other"
        #expect(candidates.visible(in: changed, at: now).isEmpty)
        changed = context; changed.mapURL = URL(filePath: "/new-map.json")
        #expect(candidates.visible(in: changed, at: now).isEmpty)
        changed = context; changed.site.cameras.append(.init(gatewayID: "gateway", sourceID: "camera", name: "Camera"))
        #expect(candidates.visible(in: changed, at: now).isEmpty)
    }

    @MainActor @Test func twinRendersUnsureCandidateWithoutReplacingConfirmedOrManualPositions() async throws {
        let url = FileManager.default.temporaryDirectory.appendingPathComponent("unsure-room-\(UUID()).scn")
        defer { try? FileManager.default.removeItem(at: url) }
        let room = SCNScene()
        let floor = SCNNode(geometry: SCNBox(width: 4, height: 0.05, length: 4, chamferRadius: 0))
        floor.geometry?.firstMaterial?.diffuse.contents = NSColor.darkGray
        room.rootNode.addChildNode(floor)
        #expect(room.write(to: url, options: nil, delegate: nil, progressHandler: nil))
        var site = Site(name: "Candidate preview")
        let camera = SiteCamera(gatewayID: "gateway", sourceID: "camera", name: "Camera")
        site.cameras = [camera]
        var solution = result(measured: false)
        solution.pose.modelRevision = site.modelRevision
        let candidate = SiteCameraGeometry(pose: solution.pose, lens: solution.lens)
        func content(_ site: Site, confirmed: [UUID: SiteCameraGeometry] = [:],
                     candidates: [UUID: SiteCameraGeometry]) -> SiteScene {
            SiteScene(modelURL: url, site: site, cameraGeometry: confirmed, cameraCandidates: candidates,
                      placingCamera: nil, select: { _ in }, place: { _, _ in })
        }
        let host = NSHostingView(rootView: content(site, candidates: [camera.id: candidate]))
        let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 800, height: 600),
                              styleMask: [.titled], backing: .buffered, defer: false)
        window.contentView = host
        window.orderBack(nil)
        defer { window.orderOut(nil); window.contentView = nil }
        func settle() async throws {
            try await Task.sleep(for: .milliseconds(300))
            host.layoutSubtreeIfNeeded()
        }
        func findScene(_ view: NSView) -> SCNView? {
            if let view = view as? SCNView { return view }
            return view.subviews.lazy.compactMap { findScene($0) }.first
        }
        try await settle()
        let view = try #require(findScene(host))
        func marker() throws -> SCNNode {
            try #require(view.scene?.rootNode.childNode(withName: "sensor:\(camera.id.uuidString)", recursively: true))
        }
        let unsure = try marker()
        #expect(abs(unsure.opacity - 0.65) < 0.000001)
        #expect(unsure.childNodes.contains { ($0.geometry as? SCNText)?.string as? String == "Unsure" })
        #expect(unsure.childNode(withName: "camera-fov", recursively: true) != nil)
        let image = view.snapshot()
        let bitmap = try #require(image.tiffRepresentation.flatMap(NSBitmapImageRep.init(data:)))
        let png = try #require(bitmap.representation(using: .png, properties: [:]))
        try png.write(to: FileManager.default.temporaryDirectory.appendingPathComponent("wendy-unsure-candidate.png"))

        host.rootView = content(site, confirmed: [camera.id: candidate], candidates: [camera.id: candidate])
        try await settle()
        #expect(try marker().opacity == 1)
        #expect(try marker().childNodes.allSatisfy { !($0.geometry is SCNText) })

        site.cameras[0].position = SitePoint(x: 1, y: 1, z: 1)
        host.rootView = content(site, candidates: [camera.id: candidate])
        try await settle()
        #expect(try marker().position.x == 1)
        #expect(try marker().opacity == 1)

        site.cameras[0].position = nil
        var expired = candidate
        expired.pose.validUntil = Date().addingTimeInterval(-1)
        host.rootView = content(site, candidates: [camera.id: expired])
        try await settle()
        #expect(view.scene?.rootNode.childNode(withName: "sensor:\(camera.id.uuidString)", recursively: true) == nil)
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
