import ImageIO
import SwiftUI
import UniformTypeIdentifiers

/// One approved "find this" request from chat. Linux owns the search; this
/// object connects the twin view to it and supplies decoded Go2 camera frames.
@MainActor
final class RobotFindMission: ObservableObject {
    // A chat card can leave and re-enter the lazy message list. The approved
    // search must be sent once, so missions outlive their views.
    // ponytail: kept until the app quits; evict with the conversation if chats ever persist.
    private static var missions: [String: RobotFindMission] = [:]
    static func mission(_ key: String) -> RobotFindMission {
        if let existing = missions[key] { return existing }
        let created = RobotFindMission()
        missions[key] = created
        return created
    }

    let robots = SiteRobotMapModel()
    @Published private(set) var sites: [StoredSite] = []
    @Published var siteID: UUID?
    @Published private(set) var status = RobotFindStatus()
    @Published private(set) var problem: String?
    @Published private(set) var sighting: CGImage?
    @Published private(set) var started = false
    private var store: SiteStore?
    private var missionHandle: UInt64 = 0
    private var frames: [UInt64: CGImage] = [:]
    private var wanted: (view: UInt64, since: Date)?
    private var uploaded: UInt64 = 0
    private var camera: (decoder: H264AnnexBDecoder, lease: UUID)?

    var site: StoredSite? { sites.first { $0.id == siteID } }
    var finished: Bool { problem != nil || (started && !status.State.isEmpty && !status.searching) }

    func prepare(model: AppModel, session: UInt64) async {
        guard store == nil, model.deviceSession == session, let device = model.connected else { return }
        do {
            let cloud = SiteGatewayIdentity.cloud(selector: device.selector)
            async let info = GoClient.shared.deviceInfo(session: session)
            async let enrollment = GoClient.shared.deviceEnrollment(session: session)
            let hardware = try? await info
            let account = try? await enrollment
            let gateway = try SiteGatewayIdentity.resolve(cloud: cloud, enrollment: account, serial: hardware?.serial)
            let opened = try await SiteStore.remoteStore(session: session, gatewayID: gateway.id)
            let listed = try await opened.list()
            guard !Task.isCancelled else { return }
            store = opened
            sites = listed
            if site == nil { siteID = listed.first?.id }
            if listed.isEmpty { problem = "This device has no digital twin yet. Create a site under Sites, then ask again." }
        } catch {
            if !Task.isCancelled { problem = error.localizedDescription }
        }
    }

    /// Go2 adds its LiDAR to the room while it walks; keep the rendered map current.
    func refreshMap() async {
        while !Task.isCancelled {
            do { try await Task.sleep(for: .seconds(3)) } catch { return }
            guard let store, !finished, let listed = try? await store.list(), !Task.isCancelled else { continue }
            sites = listed
        }
    }

    func use(_ decoder: H264AnnexBDecoder) {
        guard camera?.decoder !== decoder else { return }
        releaseCamera()
        camera = (decoder, decoder.calibrationFrames.acquireColor())
    }

    func releaseCamera() {
        if let camera { camera.decoder.calibrationFrames.releaseColor(camera.lease) }
        camera = nil
    }

    func stop() {
        robots.stop()
    }

    func run(query: String, bark: Bool, model: String, port: UInt32) async {
        while !Task.isCancelled {
            do { try await Task.sleep(for: .milliseconds(300)) } catch { return }
            let handle = robots.handle
            guard handle != 0, problem == nil else { continue }
            if !started {
                guard robots.reading?.ready == true else { continue }
                started = true
                missionHandle = handle
                do {
                    try await robots.find(query: query, bark: bark, model: model, port: port)
                    status = RobotFindStatus(State: "searching", Message: "Starting the search", Query: query)
                } catch { problem = error.localizedDescription }
                continue
            }
            guard status.searching else { continue }
            guard handle == missionHandle else {
                // A new connection is a new Linux session with no search in it.
                problem = "Go2 reconnected, which ended the search. Ask again to restart it."
                continue
            }
            do {
                let next = try await SiteRobotClient.shared.findStatus(handle)
                guard !Task.isCancelled else { return }
                if next != status { status = next }
                if next.Seen, sighting == nil { sighting = frames[next.BoxView] }
                if next.WantsView != 0, next.WantsView != uploaded { await upload(next.WantsView, handle: handle) }
            } catch {
                if !Task.isCancelled { problem = error.localizedDescription }
            }
        }
    }

    /// Linux asks for a view only after Go2 has stopped. Send a frame decoded
    /// after that request was seen, never an older one from a previous heading.
    private func upload(_ view: UInt64, handle: UInt64) async {
        if wanted?.view != view { wanted = (view, Date()) }
        guard let since = wanted?.since, let frame = camera?.decoder.calibrationFrames.latestColor,
              frame.received > since, let jpeg = Self.jpeg(frame.image) else { return }
        uploaded = view
        frames = [view: frame.image]
        // A refused frame means Linux moved on; the next poll says what it wants.
        try? await SiteRobotClient.shared.findView(handle, view: view, jpeg: jpeg)
    }

    private static func jpeg(_ image: CGImage) -> Data? {
        let data = NSMutableData()
        guard let destination = CGImageDestinationCreateWithData(data, UTType.jpeg.identifier as CFString, 1, nil) else { return nil }
        CGImageDestinationAddImage(destination, image, [kCGImageDestinationLossyCompressionQuality: 0.8] as CFDictionary)
        return CGImageDestinationFinalize(destination) ? data as Data : nil
    }
}

/// Chat card for `find_with_robot`: a third-person view that follows Go2
/// through the digital twin while Linux searches for the object.
struct RobotFindCard: View {
    let query: String
    let bark: Bool
    let session: UInt64
    let owner: String
    let modelName: String
    let modelPort: UInt32

    var body: some View {
        let mission = RobotFindMission.mission("\(session)/\(owner)/\(query)")
        RobotFindView(mission: mission, robots: mission.robots, query: query, bark: bark, session: session,
            modelName: modelName, modelPort: modelPort)
    }
}

private struct RobotFindView: View {
    @EnvironmentObject private var app: AppModel
    @Environment(\.scenePhase) private var scenePhase
    @ObservedObject var mission: RobotFindMission
    @ObservedObject var robots: SiteRobotMapModel
    let query: String
    let bark: Bool
    let session: UInt64
    let modelName: String
    let modelPort: UInt32

    private var current: Bool { app.deviceSession == session }

    private var headline: String {
        if !current { return "This search belonged to an earlier device connection." }
        if let problem = mission.problem { return problem }
        if mission.started { return mission.status.Message.isEmpty ? "Starting the search" : mission.status.Message }
        if mission.site == nil { return "Opening the digital twin…" }
        if robots.serial.isEmpty { return robots.error ?? "Looking for a paired Go2…" }
        if robots.reading?.aligned == true { return robots.reading?.message ?? "Getting Go2 ready…" }
        if robots.searchConclusion != nil { return "Go2 could not work out where it is. Place it in Sites, then ask again." }
        return "Finding where Go2 is: \(robots.statusLabel)"
    }

    private var symbol: String {
        if mission.problem != nil || !current { return "exclamationmark.triangle.fill" }
        switch mission.status.State {
        case "found": return "checkmark.circle.fill"
        case "not-found": return "questionmark.circle.fill"
        case "failed": return "exclamationmark.triangle.fill"
        case "stopped": return "stop.circle.fill"
        default: return mission.status.Phase == "thinking" ? "eye" : "dot.radiowaves.left.and.right"
        }
    }

    private var detail: String {
        var parts: [String] = []
        let status = mission.status
        if status.Viewpoints > 0 { parts.append(status.Viewpoints == 1 ? "1 place searched" : "\(status.Viewpoints) places searched") }
        if status.Views > 0 { parts.append(status.Views == 1 ? "1 camera view checked" : "\(status.Views) camera views checked") }
        if status.Remembered { parts.append("started where it was last seen") }
        if status.Barked { parts.append("Go2 barked") }
        if !status.BarkFailure.isEmpty { parts.append("No bark: \(status.BarkFailure)") }
        return parts.joined(separator: " · ")
    }

    private var target: SitePoint? {
        guard mission.status.Located else { return nil }
        return SitePoint(x: mission.status.X, y: robots.reading?.floorY ?? 0, z: mission.status.Z)
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            ZStack {
                if current, let site = mission.site {
                    SiteRobotRenderHost(model: robots) { render in
                        SiteScene(modelURL: site.modelURL, points: site.points, floorTiles: site.floorTiles, revision: site.revision,
                            site: site.site, showCameraCoverage: false, placingCamera: nil, select: { _ in }, place: { _, _ in },
                            robot: render,
                            robotConfirmation: robots.candidates.isEmpty ? nil : { AnyView(SiteRobotPositionConfirmation(model: robots)) },
                            chase: true, target: target)
                    }
                    .task(id: "\(session)/\(robots.serial)/\(site.id)/\(site.site.modelRevision)/\(scenePhase)/\(robots.reconnectGeneration)") {
                        await robots.observe(session: session, site: site.site, modelURL: site.modelURL, active: scenePhase == .active)
                    }
                } else {
                    Color.black.opacity(0.85)
                    if mission.problem == nil && current { ProgressView().tint(.white) }
                }
            }
            .frame(height: 360)
            .overlay(alignment: .topLeading) {
                Label(headline, systemImage: symbol)
                    .font(.callout.weight(.semibold)).lineLimit(3)
                    .padding(.horizontal, 10).padding(.vertical, 7)
                    .background(.regularMaterial, in: RoundedRectangle(cornerRadius: 10))
                    .padding(10)
                    .accessibilityIdentifier("chat.robotFind.status")
            }
            .overlay(alignment: .topTrailing) {
                if mission.sites.count > 1, !mission.started, mission.problem == nil {
                    Picker("Site", selection: $mission.siteID) {
                        ForEach(mission.sites) { Text($0.site.name).tag(Optional($0.id)) }
                    }
                    .labelsHidden().fixedSize()
                    .padding(6).background(.regularMaterial, in: RoundedRectangle(cornerRadius: 10)).padding(10)
                }
            }
            .overlay(alignment: .bottomTrailing) { inset.padding(10) }
            .overlay(alignment: .bottomLeading) {
                if current, robots.handle != 0, !mission.finished {
                    Button(role: .destructive) { mission.stop() } label: {
                        Label("Stop Go2", systemImage: "stop.fill").font(.callout.weight(.semibold))
                    }
                    .buttonStyle(.borderedProminent).tint(.red).padding(10)
                    .accessibilityIdentifier("chat.robotFind.stop")
                }
            }
            .clipShape(RoundedRectangle(cornerRadius: 14))
            Group {
                if !detail.isEmpty { Text(detail) }
                if current, !mission.finished {
                    Text("Go2 moves only while this view is open. Closing it or pressing Stop ends the search.")
                }
            }
            .font(.caption).foregroundStyle(.secondary).padding(.horizontal, 12)
        }
        .padding(.bottom, 10)
        .frame(maxWidth: 720, alignment: .leading)
        .task { await robots.actions.run() }
        .task(id: session) {
            guard current else { return }
            await mission.prepare(model: app, session: session)
            await robots.load(session: session)
        }
        .task(id: session) { await mission.refreshMap() }
        .task(id: session) {
            guard current else { return }
            await mission.run(query: query, bark: bark, model: modelName, port: modelPort)
        }
        .onDisappear { mission.releaseCamera() }
    }

    /// Go2's own camera. Once the object is seen, the frame it was seen in.
    @ViewBuilder private var inset: some View {
        if let sighting = mission.sighting, mission.status.Box.count == 4 {
            let box = mission.status.Box
            Image(decorative: sighting, scale: 1).resizable().aspectRatio(contentMode: .fit)
                .overlay {
                    GeometryReader { geometry in
                        Rectangle().stroke(.orange, lineWidth: 2)
                            .frame(width: box[2] * geometry.size.width, height: box[3] * geometry.size.height)
                            .offset(x: box[0] * geometry.size.width, y: box[1] * geometry.size.height)
                    }
                }
                .frame(width: 240)
                .clipShape(RoundedRectangle(cornerRadius: 10))
                .accessibilityLabel("Go2 camera frame where the \(query) was seen")
        } else if current, !robots.serial.isEmpty, robots.handle != 0 {
            SiteRobotCameraView(handle: robots.handle, onFrame: robots.acceptCameraFrame, onDecoder: { decoder in
                robots.acceptCameraDecoder(decoder)
                mission.use(decoder)
            })
            .frame(width: 240, height: 135)
        }
    }
}
