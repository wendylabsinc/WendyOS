import SwiftUI
import Observation
import QuartzCore
import simd

struct SpatialLabel: Identifiable {
    let id: String
    let name: String
    let lite: Bool
    var position: SIMD3<Float>
}
struct SummonPreview {
    let point: SIMD3<Float>
    let wearer: SIMD3<Float>
    let created: TimeInterval
    let frame: String
    let handle: UInt64
}

@MainActor @Observable
final class SpatialModel {
    let tracking = SpatialTracking()
    let robotSearch: SpatialRobotSearch
    init(robotSearch: SpatialRobotSearch = SpatialRobotSearch()) { self.robotSearch = robotSearch }
    let assistant = SpatialAssistant()
    var initialized = false
    var busy = false
    var error: String?
    var signedIn = false
    var connectionNeedsLogin = false
    var failedDevice: SpatialDevice?
    var retryAfterLogin = false
    var notice = "Connect a Wendy gateway to see its robot dogs."
    var devices: [SpatialDevice] = []
    var connected: SpatialDevice?
    var robots: [SpatialRobot] = []
    var robot: SpatialRobot?
    var session: UInt64 = 0
    var handle: UInt64 = 0
    var reading: SpatialReading?
    var readingAt: TimeInterval = 0
    var sequence: UInt64 = 0
    var active = true
    var immersive = false
    var alignedFrame: String?
    var center: SIMD3<Float>?
    var front: SIMD3<Float>?
    var selectingAlignment = false
    var alignmentStarted: TimeInterval = 0
    var preview: SummonPreview?
    var movingWearer: SIMD3<Float>?
    var labels: [SpatialLabel] = []
    var telemetry = "No telemetry"
    var telemetryAt: TimeInterval = 0
    var loginURL: URL?
    var site: SpatialSite?
    private var siteGeometry: SiteRobotGeometryPacket?
    private var siteGeometryURL: URL?
    var usesTwinForRobotMatching: Bool {
        tracking.tracked && siteGeometry != nil && siteGeometryURL == site?.modelURL
            && registration?.model == site?.modelURL && registration?.frame == tracking.frameID
    }
    var robotLocalizationGeometry: SiteRobotGeometryPacket? {
        if usesTwinForRobotMatching, let siteGeometry, let registration {
            return SpatialRoomGeometry.transformed(siteGeometry, by: simd_inverse(registration.siteFromFrame))
        }
        return tracking.roomGeometry
    }
    func loadSiteGeometry() async {
        siteGeometry = nil; siteGeometryURL = nil
        guard let site else { return }
        do {
            let geometry = try await SiteRobotGeometry.semantic(site.modelURL, scale: Double(site.metresPerUnit))
            guard !Task.isCancelled, self.site?.modelURL == site.modelURL,
                  geometry.architecture.count <= 90_000, geometry.furniture.count <= 90_000 else { return }
            siteGeometry = geometry; siteGeometryURL = site.modelURL
        } catch { /* Live detected geometry remains available for unsupported twins. */ }
    }
    var scanExportURL: URL?
    private(set) var roomOperation: String?
    var roomStatus: String?
    var changingSite: Bool { roomOperation != nil }
    func startScan() async {
        guard !changingSite, !tracking.scan.saving else { return }
        await invalidateAlignment()
        await tracking.startScan()
    }
    @discardableResult func saveScan(name: String) async -> Bool {
        guard !changingSite else { return false }
        roomOperation = "Saving twin…"; roomStatus = nil; error = nil
        defer { roomOperation = nil }
        await stop()
        do {
            let (saved, archive) = try await tracking.scan.save(name: name)
            try await SpatialClient.work { try saved.remember() }
            site = saved; scanExportURL = archive; replay = nil
            registration = nil; registrationStep = nil
            if tracking.tracked, tracking.scan.frame == tracking.frameID, !tracking.scan.interrupted {
                registration = try SpatialRegistration.solve(siteA: .zero, siteB: [2, 0, 0], frameA: .zero, frameB: [2, 0, 0], frame: tracking.frameID, model: saved.modelURL)
            }
            notice = registration == nil ? "Room saved. Use Locate me to align your position." : "Room saved. Your position is aligned during this AR session."
            roomStatus = "Saved \(saved.name) on this Vision Pro. Open tabletop to view it, or export a copy to Files."
            return true
        } catch {
            self.error = error.localizedDescription
            roomStatus = "Could not save the twin: \(error.localizedDescription)"
            return false
        }
    }
    func prepareTwinExport() async -> URL? {
        guard !changingSite, let site else { return nil }
        roomOperation = "Preparing twin for export…"
        defer { roomOperation = nil }
        do {
            let archive = try await SpatialClient.work { try site.writeArchive() }
            scanExportURL = archive
            return archive
        } catch { self.error = error.localizedDescription; return nil }
    }
    var registration: SpatialRegistration?
    var registrationStep: Int?
    var siteReferences: [SIMD3<Float>] = []
    var roomReferences: [SIMD3<Float>] = []
    var registeredWearer: SIMD3<Float>? {
        guard let registration, registration.frame == tracking.frameID,
              registration.model == site?.modelURL, tracking.tracked,
              let wearer = tracking.wearer else { return nil }
        return registration.point(wearer)
    }
    var robotMapStatus: String {
        guard site != nil else { return "Import or scan a room to place Go2 on the map." }
        guard let registration, registration.frame == tracking.frameID,
              registration.model == site?.modelURL, tracking.tracked else {
            return "Align your room to connect this map to your surroundings."
        }
        guard handle != 0 else { return "Select Go2 in Devices to show it on this map." }
        guard reading?.disconnected != true else { return "Go2 disconnected. Reconnect it in Devices." }
        guard alignedFrame == tracking.frameID else {
            if robotSearch.candidate != nil { return "Go2 position found. Confirm robot position in Devices." }
            return "Room aligned. Locate Go2 separately using Find Go2 in this room or Align in AR in Devices."
        }
        guard let reading, reading.pose, reading.aligned,
              (0...0.5).contains(CACurrentMediaTime() - readingAt) else {
            return "Go2 is aligned; waiting for fresh robot telemetry."
        }
        return "Go2 position is live on this map."
    }
    func beginRegistration() {
        guard site != nil, tracking.tracked else { error = "Import a digital twin and open AR overlays first."; return }
        registration = nil; siteReferences = []; roomReferences = []; registrationStep = 0
        notice = "In the tabletop twin, select a recognizable floor point such as a doorway corner."
    }
    func selectSitePoint(_ point: SIMD3<Float>) {
        guard registrationStep == 0 || registrationStep == 2 else { return }
        siteReferences.append(point); registrationStep = (registrationStep ?? 0) + 1
        notice = "Select the matching point on the detected floor in your real room."
    }
    func confirmRegistration() {
        guard registrationStep == 4, siteReferences.count == 2, roomReferences.count == 2,
              tracking.tracked, let site else { return }
        do {
            registration = try SpatialRegistration.solve(siteA: siteReferences[0], siteB: siteReferences[1],
                frameA: roomReferences[0], frameB: roomReferences[1], frame: tracking.frameID, model: site.modelURL)
            registrationStep = nil
            notice = "Your position and facing direction now follow inside-out tracking in the twin. Alignment is manual."
        } catch { self.error = error.localizedDescription }
    }
    var recording = false
    var recordingStart: TimeInterval = 0
    var samples: [SpatialSample] = []
    var replay: SpatialRecording?
    var replayIndex: Double = 0
    var exportURL: URL?

    var displayedReading: SpatialReading? {
        if let replay, !replay.samples.isEmpty { return replay.samples[min(Int(replayIndex), replay.samples.count - 1)].reading }
        return reading
    }
    var canPreview: Bool {
        active && immersive && tracking.tracked && alignedFrame == tracking.frameID && handle != 0 && replay == nil
            && reading?.ready == true && reading?.pose == true && reading?.aligned == true
            && reading?.disconnected == false && (0...0.5).contains(CACurrentMediaTime() - readingAt)
    }
    var canMove: Bool {
        guard canPreview, let preview, let wearer = tracking.wearer,
              preview.frame == alignedFrame, preview.handle == handle else { return false }
        return SpatialGeometry.previewValid(created: preview.created, now: CACurrentMediaTime(), wearer: wearer, originalWearer: preview.wearer)
    }
    func nextSequence() -> UInt64 { sequence += 1; return sequence }
    func boot() async {
        guard !initialized else { return }
        do {
            let base = try FileManager.default.url(for: .applicationSupportDirectory, in: .userDomainMask, appropriateFor: nil, create: true)
            try await SpatialClient.initialize(directory: base.appendingPathComponent("Wendy").path)
            initialized = true
            site = try? await SpatialClient.work { try SpatialSite.restore() }
            await refreshAccount()
            await discover(cloud: false)
        } catch { self.error = error.localizedDescription }
    }
    func discover(cloud: Bool) async {
        guard initialized, !busy else { return }
        busy = true; defer { busy = false }
        do {
            let found = try await SpatialClient.devices(cloud: cloud)
            devices = Dictionary((devices + found).map { ($0.id, $0) }, uniquingKeysWith: { _, new in new }).values.sorted { $0.name < $1.name }
            notice = found.isEmpty ? "No devices found. Enter a gateway address or sign in to Wendy Cloud." : "Choose a device to connect or place its label in AR."
        } catch { self.error = error.localizedDescription }
    }
    func connect(_ device: SpatialDevice) async {
        guard initialized, !busy else { return }
        busy = true; defer { busy = false }
        error = nil; connectionNeedsLogin = false; failedDevice = nil
        await closeRobot()
        session = 0; connected = nil; robots = []; telemetryAt = 0
        do {
            let result = try await SpatialClient.connect(device.selector)
            guard !Task.isCancelled, active else { await SpatialClient.disconnect(); return }
            session = result; connected = device
            notice = "Connected to \(device.name)"
            if !device.lite {
                do { robots = try await SpatialClient.robots(session: result) }
                catch { notice = "Connected to \(device.name). Robot inventory unavailable: \(error.localizedDescription)" }
            }
        } catch {
            failedDevice = device
            await refreshAccount()
            let raw = error.localizedDescription
            connectionNeedsLogin = SpatialConnectionRecovery.needsLogin(signedIn: signedIn, error: raw)
            self.error = connectionNeedsLogin
                ? "Sign in to Wendy on this Vision Pro to authenticate with \(device.name), then retry. Nearby discovery works without a login, but enrolled devices require your account credentials.\n\nConnection details: \(raw)"
                : raw
        }
    }
    func refreshAccount() async { signedIn = (try? await SpatialClient.signedIn()) ?? false }
    func beginLogin() async {
        error = nil
        do { try await SpatialClient.login() }
        catch { self.error = error.localizedDescription }
    }
    func loginCompleted() async {
        loginURL = nil
        await refreshAccount()
        retryAfterLogin = signedIn && failedDevice != nil
        await resumeAfterLogin()
    }
    func resumeAfterLogin() async {
        guard active, !busy, retryAfterLogin, let device = failedDevice else { return }
        retryAfterLogin = false
        await connect(device)
    }
    func selectRobot(_ selected: SpatialRobot) async {
        guard !busy, session != 0, selected.connected else { return }
        busy = true; defer { busy = false }
        error = nil
        await closeRobot()
        do {
            let opened = try await SpatialClient.open(session: session, serial: selected.id)
            guard !Task.isCancelled, active else { await SpatialClient.close(opened); return }
            sequence = 0; handle = opened; robot = selected; replay = nil
            notice = "Open AR overlays and align \(selected.name) with the floor."
        } catch { self.error = error.localizedDescription }
    }
    func closeRobot() async {
        robotSearch.cancel()
        let old = handle
        handle = 0; robot = nil; reading = nil; alignedFrame = nil
        preview = nil; movingWearer = nil; selectingAlignment = false; recording = false
        center = nil; front = nil
        if old != 0 { await SpatialClient.close(old) }
    }
    func setRobotSetting(_ setting: SiteRobotSetting, enabled: Bool) async {
        guard !busy, active, handle != 0, replay == nil else { return }
        busy = true; defer { busy = false }
        let current = handle, order = nextSequence()
        preview = nil; movingWearer = nil
        robotSearch.cancel()
        reading?.ready = false
        do {
            try await SiteRobotSession.setSetting(current, sequence: order, setting: setting, enabled: enabled)
            guard current == handle, !Task.isCancelled else { return }
            error = nil
            notice = "Supervision setting changed. Waiting for a new scan."
        } catch { if current == handle { self.error = error.localizedDescription } }
    }
    func stop() async {
        robotSearch.cancel()
        preview = nil; movingWearer = nil
        let old = handle, order = nextSequence()
        if old != 0 { await SpatialClient.stop(old, sequence: order) }
    }
    func deactivate() async {
        active = false
        reading = nil; telemetryAt = 0
        await invalidateAlignment()
    }
    func invalidateAlignment() async {
        alignedFrame = nil; center = nil; front = nil; selectingAlignment = false
        await stop()
    }
    var alignmentBlocker: String? {
        guard handle != 0 else { return "Select a connected robot before aligning." }
        guard replay == nil else { return "Exit replay before aligning a live robot." }
        guard active else { return "Return to the Wendy window before aligning." }
        guard immersive else { return "Open AR overlays before aligning." }
        guard tracking.tracked else { return "Headset tracking is unavailable. \(tracking.message)" }
        guard let reading else { return "Waiting for robot telemetry. Reconnect the robot if this persists." }
        if reading.disconnected { return "Reconnect the robot to resume alignment. \(reading.message)" }
        guard reading.pose else { return "Robot pose is unavailable. \(reading.message)" }
        guard (0...0.5).contains(CACurrentMediaTime() - readingAt) else {
            return "Robot telemetry is stale. Reconnect the robot to resume alignment."
        }
        return nil
    }
    func beginAlignment() async {
        guard !busy else { return }
        if let blocker = alignmentBlocker { error = blocker; return }
        await invalidateAlignment()
        if let blocker = alignmentBlocker { error = blocker; return }
        error = nil
        selectingAlignment = true; alignmentStarted = CACurrentMediaTime()
        notice = "Select the detected floor beneath Go2's body, then a point directly in front of it. Keep Go2 stationary."
    }
    func selectFloor(_ point: SIMD3<Float>) {
        guard tracking.tracked, SpatialGeometry.finite(point) else { return }
        if registrationStep == 1 || registrationStep == 3, tracking.tracked {
            roomReferences.append(point); registrationStep = (registrationStep ?? 1) + 1
            notice = registrationStep == 2 ? "Select a second floor point in the twin, at least one metre from the first." : "Confirm room alignment, then check your marker in the twin."
            return
        }
        guard selectingAlignment, tracking.tracked, SpatialGeometry.finite(point) else { return }
        if center == nil { center = point; notice = "Now select the floor directly in front of Go2, along its nose direction." }
        else { front = point; notice = "Check the position and forward arrow, then confirm alignment." }
    }
    func confirmAlignment() async {
        guard !busy, selectingAlignment, let center, let front, handle != 0,
              active, tracking.tracked, CACurrentMediaTime() - alignmentStarted < 30,
              abs(center.y - front.y) < 0.1, reading?.pose == true, reading?.moving != true else {
            error = "Start alignment again with Go2 stationary on the same detected floor."; return
        }
        busy = true; defer { busy = false }
        let current = handle, frame = tracking.frameID, invalidation = tracking.alignmentInvalidated
        do {
            let yaw = try SpatialGeometry.heading(center: center, front: front)
            try await SpatialClient.align(current, sequence: nextSequence(), frame: frame, center: center, yaw: yaw)
            guard !Task.isCancelled, active, current == handle, frame == tracking.frameID,
                  invalidation == tracking.alignmentInvalidated, tracking.tracked else { await stop(); return }
            alignedFrame = frame; selectingAlignment = false
            notice = "Aligned manually. Check that the overlay matches Go2 before calling it over."
        } catch { self.error = error.localizedDescription }
    }
    func callRobot() async {
        guard !busy, canPreview, let reading, let wearer = tracking.wearer else {
            error = "Come here needs a fresh robot pose, ready navigation, and confirmed AR alignment."; return
        }
        busy = true; defer { busy = false }
        preview = nil
        do {
            let point = try SpatialGeometry.summon(robot: reading.position, wearer: wearer, floor: reading.position.y)
            let candidate = SummonPreview(point: point, wearer: wearer, created: CACurrentMediaTime(), frame: tracking.frameID, handle: handle)
            let order = nextSequence()
            try await SpatialClient.destination(handle, sequence: order, frame: candidate.frame, point: point, wearer: wearer, move: false)
            guard !Task.isCancelled, order == sequence, canPreview, candidate.handle == handle,
                  candidate.frame == alignedFrame else { return }
            preview = candidate
            notice = "Review the route. Go2 will stop 1.5 metres short of your current position."
        } catch { self.error = error.localizedDescription }
    }
    func confirmCall() async {
        guard !busy, canMove, let preview else { error = "The preview expired or you moved. Choose Come here again."; return }
        busy = true; defer { busy = false }
        let order = nextSequence()
        self.preview = nil
        do {
            try await SpatialClient.destination(preview.handle, sequence: order, frame: preview.frame, point: preview.point, wearer: preview.wearer, move: true)
            guard !Task.isCancelled, order == sequence, canPreview, preview.handle == handle,
                  let wearer = tracking.wearer,
                  SpatialGeometry.previewValid(created: preview.created, now: CACurrentMediaTime(), wearer: wearer, originalWearer: preview.wearer) else {
                await stop(); return
            }
            movingWearer = preview.wearer
            notice = "Go2 is coming to the previewed position. Moving away cancels the call."
        } catch { self.error = error.localizedDescription }
    }
    func pollRobot(_ current: UInt64) async {
        guard current != 0 else { return }
        while !Task.isCancelled, current == handle {
            do {
                // No heartbeat when the app is inactive. The backend lease also
                // expires if SwiftUI fails to deliver the lifecycle callback.
                guard active else { await stop(); return }
                if let original = movingWearer,
                   !tracking.tracked || !immersive || tracking.wearer.map({ simd_distance($0, original) > 0.3 }) != false {
                    await stop()
                }
                let value = try await SpatialClient.poll(current)
                guard !Task.isCancelled, current == handle else { return }
                reading = value; readingAt = CACurrentMediaTime()
                if !value.pose || value.disconnected {
                    // Invalidate once when evidence is lost. Repeating Stop on
                    // every missing-pose poll can congest a failing connection.
                    if alignedFrame != nil || selectingAlignment || preview != nil || movingWearer != nil || value.moving {
                        await invalidateAlignment()
                    }
                    // This handle cannot recover after its stream has closed.
                    // Keep the reason visible until the user reconnects.
                    if value.disconnected { return }
                }
                if !value.moving { movingWearer = nil }
                if recording {
                    var bounded = value
                    bounded.hazards = Array(value.hazards.prefix(128)); bounded.route = Array(value.route.prefix(128))
                    samples.append(SpatialSample(elapsed: CACurrentMediaTime() - recordingStart, reading: bounded))
                    if samples.count >= 1800 { recording = false; notice = "Recording complete: three minutes of spatial observations." }
                }
                try await Task.sleep(for: .milliseconds(100))
            } catch is CancellationError { break }
            catch { self.error = error.localizedDescription; reading = nil; await invalidateAlignment(); break }
        }
        if current == handle { await stop() }
    }
    func pollTelemetry(_ current: UInt64) async {
        guard current != 0 else { return }
        while !Task.isCancelled, session == current, active {
            do {
                let summary = try await SpatialClient.summary(session: current)
                guard session == current, !Task.isCancelled else { return }
                telemetry = summary; telemetryAt = CACurrentMediaTime()
            } catch { telemetry = error.localizedDescription; telemetryAt = 0 }
            do { try await Task.sleep(for: .seconds(3)) } catch { return }
        }
    }
    func placeLabel(_ device: SpatialDevice) {
        guard let point = tracking.placementPoint() else { error = "Open AR overlays and wait for tracking first."; return }
        labels.removeAll { $0.id == device.id }
        labels.append(SpatialLabel(id: device.id, name: device.name, lite: device.lite, position: point))
        notice = "Drag the label onto \(device.name). Placement is manual and lasts for this AR session."
    }
    func startRecording() {
        guard handle != 0, replay == nil else { return }
        samples = []; recordingStart = CACurrentMediaTime(); recording = true; exportURL = nil
    }
    func replayCapture() async {
        await stop(); recording = false
        guard !samples.isEmpty else { return }
        replay = SpatialRecording(robot: robot?.name ?? "Go2", samples: samples); replayIndex = 0
    }
    func exportCapture() async {
        guard let capture = replay ?? (samples.isEmpty ? nil : SpatialRecording(robot: robot?.name ?? "Go2", samples: samples)) else { return }
        do {
            exportURL = try await SpatialClient.work {
                try capture.validate()
                let url = FileManager.default.temporaryDirectory.appendingPathComponent("Wendy-spatial-\(UUID().uuidString).json")
                let data = try JSONEncoder().encode(capture)
                guard data.count <= 64 * 1024 * 1024 else { throw SpatialFailure("Recording exceeds the 64 MB export limit.") }
                try data.write(to: url, options: .atomic)
                return url
            }
        } catch { self.error = error.localizedDescription }
    }
    enum ImportedFile { case room, recording }
    @discardableResult func importFile(_ url: URL) async -> ImportedFile? {
        guard !changingSite else { error = "Wait for the current room to finish saving or importing."; return nil }
        roomOperation = "Importing \(url.lastPathComponent)…"; roomStatus = nil
        defer { roomOperation = nil }
        error = nil; connectionNeedsLogin = false
        await stop()
        do {
            if url.pathExtension.lowercased() == "json" {
                let imported = try await SpatialClient.work {
                    let data = try SpatialFileImport.read(url, maximumBytes: 64 * 1024 * 1024)
                    let value = try JSONDecoder().decode(SpatialRecording.self, from: data)
                    try value.validate(); return value
                }
                replay = imported; replayIndex = 0; recording = false
                await invalidateAlignment()
                roomStatus = "Imported recording for \(imported.robot)."
                return .recording
            } else {
                let imported = try await SpatialClient.work { try SpatialSite.load(url) }
                try await SpatialClient.work { try imported.remember() }
                site = imported; scanExportURL = nil
                replay = nil; registration = nil; registrationStep = nil
                notice = "Room imported. Open the tabletop to explore it, or open AR to align your position."
                roomStatus = "Imported \(imported.name). Open tabletop to view it."
                return .room
            }
        } catch {
            self.error = error.localizedDescription
            roomStatus = "Import failed: \(error.localizedDescription)"
            return nil
        }
    }
}
