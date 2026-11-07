import SwiftUI

/// Shared by viewing and placement. Never borrow calibration from a different
/// gateway, model revision, or an ambiguous source identity.
enum MobileSiteCoveragePolicy {
    static func geometry(camera: SiteCamera, site: Site, records: [SiteCameraMetadata], gateway: SiteGatewayIdentity, now: Date) -> SiteCameraGeometry? {
        guard SiteGatewayIdentity.matches(camera.gatewayID, current: gateway.id, aliases: gateway.aliases) else { return nil }
        if let manual = camera.manualAlignment, let position = camera.position {
            return manual.geometry(position: position, revision: site.modelRevision, now: now)
        }
        let matches = records.filter { $0.sourceID == camera.sourceID }
        guard matches.count == 1, let record = matches.first,
              let pose = record.pose(in: site, now: now), let lens = record.lens,
              lens.isValid, lens.boundaryRays != nil else { return nil }
        if let point = camera.position {
            guard abs(point.x-pose.position.x) < 0.001, abs(point.y-pose.position.y) < 0.001, abs(point.z-pose.position.z) < 0.001 else { return nil }
        }
        return SiteCameraGeometry(pose: pose, lens: lens)
    }
}

struct MobileSiteCoverageScene: View {
    @ObservedObject var model: AppModel
    let saved: StoredSite
    let assets: [MobileSiteAsset]
    var active = true
    let placing: UUID?
    let select: (UUID) -> Void
    let place: (UUID, SitePoint) -> Void
    @Environment(\.scenePhase) private var scenePhase
    @State private var coverage = true
    @State private var imagery = true
    @State private var automaticAlignment = SiteAutomaticAlignment()
    @State private var gateway = SiteGatewayIdentity()
    @State private var records: [SiteCameraMetadata] = []
    @State private var statuses: [UUID: String] = [:]
    @State private var error: String?
    @State private var checked = false
    @State private var metadataSession: UInt64 = 0
    private var available: Bool { active && scenePhase == .active }
    private var geometry: [UUID: SiteCameraGeometry] {
        guard available, metadataSession == model.deviceSession else { return [:] }
        var result: [UUID: SiteCameraGeometry] = [:]
        for camera in saved.site.cameras {
            if let value = MobileSiteCoveragePolicy.geometry(camera: camera, site: saved.site, records: records, gateway: gateway, now: Date()) {
                if let feed = model.cameraFeeds.first(where: { $0.camera.stableID == camera.sourceID }),
                   let width = feed.jpeg?.width ?? feed.decoder.imageSize?.width,
                   let height = feed.jpeg?.height ?? feed.decoder.imageSize?.height,
                   let lens = value.lens, (width != lens.width || height != lens.height) { continue }
                result[camera.id] = value
            }
        }
        return result
    }
    private var sources: [UUID: SiteCameraColorSource] {
        guard imagery, available, placing == nil else { return [:] }
        var result: [UUID: SiteCameraColorSource] = [:]
        for camera in saved.site.cameras where geometry[camera.id] != nil {
            let matches = model.cameraFeeds.filter { $0.camera.stableID == camera.sourceID }
            if matches.count == 1, let feed = matches.first, !feed.camera.thermal { result[camera.id] = feed.colorSource }
        }
        return result
    }
    private var explanation: String {
        if let error { return error }
        if saved.site.cameras.isEmpty { return "Add cameras to show their viewing areas." }
        if !checked { return "Checking camera alignment…" }
        if geometry.isEmpty { return "Open a camera to connect. Wendy will try to align it using the scan references." }
        if imagery, sources.isEmpty { return "Live imagery needs a connected camera and a valid alignment. Lens calibration is optional." }
        let manual = geometry.values.filter { $0.pose.provenance == "Manual visualization" }.count
        if manual > 0 { return "\(geometry.count) viewing areas · \(manual) manually aligned. Visual estimates only." }
        let estimated = geometry.values.filter { $0.lens?.measured != true }.count
        return estimated > 0 ? "\(geometry.count) viewing areas · \(estimated) estimated. Cones show direction, not sensor range." : "\(geometry.count) calibrated viewing areas. Cones show direction, not sensor range."
    }
    var body: some View {
        SiteScene(modelURL: saved.modelURL, site: saved.site,
                  cameraGeometry: geometry, showCameraCoverage: coverage, colorSources: sources,
                  colorStatus: { statuses = $0 }, placingCamera: placing, select: select, place: place)
            .overlay(alignment: .topTrailing) {
                Menu {
                    Toggle("Camera coverage", isOn: $coverage)
                    Toggle("Live imagery", isOn: $imagery)
                    if !saved.site.cameras.isEmpty {
                        Section("Open camera") {
                            ForEach(saved.site.cameras) { camera in Button(camera.name) { select(camera.id) } }
                        }
                    }
                } label: { Label("View", systemImage: "eye").padding(12).background(.regularMaterial, in: Capsule()) }
                    .padding().tint(MobileLensStyle.green)
            }
            .overlay(alignment: .bottomLeading) {
                if coverage || imagery {
                    VStack(alignment: .leading, spacing: 4) {
                        Text(explanation)
                        if geometry.isEmpty {
                            ForEach(saved.site.cameras) { camera in
                                if let status = automaticAlignment.statuses[camera.sourceID] { Text("\(camera.name): \(status)") }
                            }
                        }
                        if imagery {
                            ForEach(saved.site.cameras.filter { sources[$0.id] != nil }) { camera in
                                Text("\(camera.name): \(statuses[camera.id] ?? "Waiting for live imagery…")")
                            }
                        }
                    }.font(.caption).padding(10).background(.regularMaterial, in: RoundedRectangle(cornerRadius: 12)).padding()
                }
            }
            .onChange(of: sources.keys.sorted { $0.uuidString < $1.uuidString }) { _, ids in
                let wanted = Set(saved.site.cameras.filter { ids.contains($0.id) }.map(\.sourceID))
                for feed in model.cameraFeeds where wanted.contains(feed.camera.stableID) { feed.allowAutoplay() }
            }
            .background {
                SiteAutomaticAlignmentView(model: model, site: saved, gatewayID: gateway.id, gatewayAliases: gateway.aliases,
                    enabled: available && placing == nil && metadataSession == model.deviceSession,
                    worker: automaticAlignment)
                SiteCameraColorCapture(model: model, sourceIDs: saved.site.cameras.filter { sources[$0.id] != nil }.map(\.sourceID))
            }
            .task(id: "\(model.deviceSession)-\(available)-\(saved.id)") {
                records = []; gateway = .init(); checked = false; error = nil
                guard available else { return }
                let session = model.deviceSession
                guard let device = model.connected, assets.contains(where: { !$0.robot && $0.id == recordingDeviceID(device) }) else { checked = true; return }
                do {
                    async let info = GoClient.shared.deviceInfo(session: session)
                    async let enrollment = GoClient.shared.deviceEnrollment(session: session)
                    let identity = try await SiteGatewayIdentity.resolve(cloud: SiteGatewayIdentity.cloud(selector: device.selector), enrollment: enrollment, serial: info.serial)
                    guard session == model.deviceSession else { return }
                    gateway = identity
                    while !Task.isCancelled, session == model.deviceSession {
                        do {
                            let value = try await GoClient.shared.cameraMetadata(session: session)
                            try Task.checkCancellation()
                            guard session == model.deviceSession else { return }
                            records = value; metadataSession = session; checked = true; error = nil
                        } catch is CancellationError { return }
                        catch { records = []; checked = true; self.error = "Camera alignment unavailable. Reconnect to the camera and try again." }
                        try await Task.sleep(for: .seconds(5))
                    }
                } catch is CancellationError {} catch { records = []; checked = true; self.error = "Could not check the camera’s device identity." }
            }
    }
}
