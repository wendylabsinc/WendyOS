import SwiftUI
import Observation

struct SiteAlignmentSchedule {
    private struct Entry { var failures = 0; var next = Date.distantPast }
    private var entries: [String: Entry] = [:]

    func isDue(_ source: String, at now: Date) -> Bool { now >= (entries[source]?.next ?? .distantPast) }
    mutating func completed(_ source: String, success: Bool, at now: Date) -> TimeInterval {
        var entry = entries[source] ?? Entry()
        entry.failures = success ? 0 : min(entry.failures + 1, 5)
        let delay: TimeInterval = success ? 60 : min(300, 15 * pow(2, Double(entry.failures)))
        entry.next = now.addingTimeInterval(delay)
        entries[source] = entry
        return delay
    }
}

struct SiteAutomaticAlignmentContext: Equatable {
    var site: Site
    var modelURL: URL
    var mapURL: URL?
    var gatewayID: String
    var session: UInt64
    var enabled: Bool
}

@MainActor @Observable
final class SiteAutomaticAlignment {
    private(set) var statuses: [String: String] = [:]
    private var generation = UUID()
    private var running = false

    func run(context: SiteAutomaticAlignmentContext, model: AppModel) async {
        let token = UUID()
        generation = token
        statuses = [:]
        // A cancelled native solve must finish before another loop starts.
        while running {
            do { try await Task.sleep(for: .milliseconds(100)) } catch { return }
            guard generation == token else { return }
        }
        guard context.enabled, context.session != 0, !context.gatewayID.isEmpty, !Task.isCancelled else { return }
        running = true
        defer { running = false }
        let cameras = context.site.cameras.filter { $0.gatewayID == context.gatewayID && $0.position == nil }
        guard !cameras.isEmpty else { return }
        let site = StoredSite(site: context.site, modelURL: context.modelURL, visualMapURL: context.mapURL)
        guard site.visualMapURL != nil else {
            for camera in cameras { statuses[camera.sourceID] = "Automatic alignment needs a room reference scan." }
            return
        }
        let engine = SiteLocalizationEngine()
        do { try await SiteCameraAlignment.load(site, into: engine) }
        catch {
            guard !Task.isCancelled, generation == token else { return }
            for camera in cameras { statuses[camera.sourceID] = error.localizedDescription }
            return
        }
        var schedule = SiteAlignmentSchedule()
        while !Task.isCancelled, generation == token, model.deviceSession == context.session {
            for camera in cameras {
                guard !Task.isCancelled, generation == token, model.deviceSession == context.session else { return }
                guard schedule.isDue(camera.sourceID, at: Date()) else { continue }
                let matches = model.cameraFeeds.filter { $0.camera.stableID == camera.sourceID }
                guard matches.count == 1, let feed = matches.first, !feed.camera.thermal else {
                    statuses[camera.sourceID] = "Waiting for an available visual camera."
                    _ = schedule.completed(camera.sourceID, success: false, at: Date())
                    continue
                }
                if feed.manuallyPaused {
                    statuses[camera.sourceID] = "Automatic alignment paused with the camera."
                    continue
                }
                statuses[camera.sourceID] = "Checking camera position…"
                do {
                    let result = try await align(model: model, feed: feed, site: site, camera: camera,
                        context: context, token: token, engine: engine)
                    guard !Task.isCancelled, generation == token else { return }
                    statuses[camera.sourceID] = result.lens.measured ? "Position checked automatically" : "Approximate position checked automatically"
                    _ = schedule.completed(camera.sourceID, success: true, at: Date())
                } catch {
                    guard !Task.isCancelled, generation == token, model.deviceSession == context.session else { return }
                    _ = schedule.completed(camera.sourceID, success: false, at: Date())
                    statuses[camera.sourceID] = "\(error.localizedDescription) Will retry automatically."
                }
            }
            do { try await Task.sleep(for: .seconds(2)) } catch { return }
        }
    }

    private func align(model: AppModel, feed: CameraFeed, site: StoredSite, camera: SiteCamera,
                       context: SiteAutomaticAlignmentContext, token: UUID,
                       engine: SiteLocalizationEngine) async throws -> SiteLocalizationResult {
        let lease = feed.decoder.calibrationFrames.acquire()
        var ownedStream: UInt64 = 0
        defer {
            feed.decoder.calibrationFrames.release(lease)
            // Never stop a preview that took ownership while we were solving.
            if ownedStream != 0, feed.stream == ownedStream { model.pauseCamera(feed, manually: false) }
        }
        if !feed.running { ownedStream = await model.playOwnedCamera(feed, inline: true) }
        let request = SiteAlignmentRequest(site: site, camera: camera, gatewayID: context.gatewayID, session: context.session)
        return try await SiteCameraAlignment.align(model: model, request: request, feed: feed, engine: engine,
            automatic: true, isCurrent: { self.generation == token })
    }
}

struct SiteAutomaticAlignmentView: View {
    @ObservedObject var model: AppModel
    let site: StoredSite
    let gatewayID: String
    let enabled: Bool
    let worker: SiteAutomaticAlignment

    private var context: SiteAutomaticAlignmentContext {
        .init(site: site.site, modelURL: site.modelURL, mapURL: site.visualMapURL,
              gatewayID: gatewayID, session: model.deviceSession, enabled: enabled)
    }
    var body: some View {
        Color.clear.frame(width: 0, height: 0)
            .task(id: context) { await worker.run(context: context, model: model) }
    }
}
