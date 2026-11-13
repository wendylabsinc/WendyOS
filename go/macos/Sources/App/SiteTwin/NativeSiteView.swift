import SwiftUI

/// Site files contain identities and geometry. The active Go session owns all
/// credentials, routing and camera subscriptions on both platforms.
struct NativeSiteView: View {
    @ObservedObject var model: AppModel
    @State private var actions = AsyncActions()
    @State private var gateway = SiteGatewayIdentity()
    private var gatewayID: String { gateway.id }
    @State private var identityError: String?
    @State private var pairing = false
    @State private var alignment: SiteAlignmentRequest?
    @State private var metadata: [SiteCameraMetadata] = []
    @State private var metadataError: String?
    @Environment(\.scenePhase) private var scenePhase
    @State private var automaticAlignment = SiteAutomaticAlignment()
    @State private var cameraResolutions: [String: CGSize] = [:]

    var body: some View {
        SiteWorkspace(gatewayID: gatewayID, gatewayAliases: gateway.aliases, feeds: feeds, metadata: displayMetadata, notices: notices,
                      robotSession: model.deviceSession, robotGatewayName: model.connected?.title ?? "this gateway", controls: {
            Button("Pair cameras", systemImage: "plus") { pairing = true }
        }, onFailure: { error, _ in model.alert = error.localizedDescription }, calibrate: { site, camera in
            alignment = SiteAlignmentRequest(site: site, camera: camera, gatewayID: gatewayID, session: model.deviceSession, gatewayAliases: gateway.aliases)
        }, automaticAlignment: { site, editing in
            AnyView(SiteAutomaticAlignmentView(model: model, site: site, gatewayID: gatewayID, gatewayAliases: gateway.aliases,
                enabled: scenePhase == .active && alignment == nil && !pairing && !editing,
                worker: automaticAlignment))
        }, alignmentStatuses: scenePhase == .active && alignment == nil ? automaticAlignment.statuses : [:], colorCapture: { sourceIDs in
            AnyView(SiteCameraColorCapture(model: model, sourceIDs: sourceIDs))
        }) { sourceID in
            if let feed = model.cameraFeeds.first(where: { $0.camera.stableID == sourceID }) {
                SiteCameraPreview(model: model, feed: feed).id(feed.id)
            }
        }
        .task { await actions.run() }
        .task(id: model.deviceSession) {
            gateway = SiteGatewayIdentity()
            await identifyGateway()
        }
        .task(id: model.deviceSession) {
            metadata = []
            while !Task.isCancelled {
                await refreshMetadata()
                do { try await Task.sleep(for: .seconds(5)) } catch { return }
            }
        }
        .sheet(item: $alignment, onDismiss: { actions.submit { await refreshMetadata() } }) { request in
            SiteCameraAlignmentView(model: model, request: request)
        }
        .sheet(isPresented: $pairing, onDismiss: { actions.submit { await refresh() } }) {
            NavigationStack {
                PairingView(session: model.deviceSession, deviceName: model.connected?.title ?? "Gateway")
                    .toolbar {
                        ToolbarItem(placement: .confirmationAction) {
                            Button("Done") { pairing = false }
                        }
                    }
            }
            #if os(macOS)
            .frame(minWidth: 700, minHeight: 520)
            #endif
        }
    }

    private var feeds: [SiteFeed] {
        // Duplicate identities are ambiguous and must never select a camera.
        let groups = Dictionary(grouping: model.cameraFeeds, by: { $0.camera.stableID })
        return model.cameraFeeds.compactMap { feed in
            guard !feed.camera.stableID.isEmpty, groups[feed.camera.stableID]?.count == 1 else { return nil }
            return SiteFeed(id: feed.camera.stableID, name: feed.camera.name,
                            lastReceivedAt: feed.lastVideoReceivedAt, connected: feed.running,
                            colorSource: feed.camera.thermal ? nil : feed.colorSource, activity: { [weak feed] in
                                (feed?.lastVideoReceivedAt, feed?.running == true)
                            })
        }
    }

    private var displayMetadata: [SiteCameraMetadata] {
        metadata.map { record in
            var result = record
            if let size = cameraResolutions[record.sourceID], let lens = record.lens {
                // A resolution change can mean a different sensor crop. Do not
                // silently scale calibration unless the publisher supplies it.
                if size.width != CGFloat(lens.width) || size.height != CGFloat(lens.height) { result.lens = nil }
            }
            return result
        }
    }

    private var notices: [String] {
        var messages = [identityError, model.camerasError, metadataError].compactMap { $0 }
        if !unidentifiedCameraNames.isEmpty {
            messages.append("\(model.connected?.title ?? "This gateway") has no persistent camera ID for \(unidentifiedCameraNames.joined(separator: ", ")). Those cameras cannot be saved in the model yet.")
        }
        return messages
    }

    private var unidentifiedCameraNames: [String] {
        model.cameraFeeds.filter { $0.camera.stableID.isEmpty }.map { $0.camera.name }
    }

    private func refresh() async {
        await model.refreshDetails()
        await identifyGateway()
        await refreshMetadata()
    }

    private func refreshMetadata() async {
        // Keep resolution validation current without a timer around the workspace.
        var resolutions: [String: CGSize] = [:]
        for feed in model.cameraFeeds {
            if let width = feed.jpeg?.width ?? feed.decoder.imageSize?.width,
               let height = feed.jpeg?.height ?? feed.decoder.imageSize?.height {
                resolutions[feed.camera.stableID] = CGSize(width: width, height: height)
            }
        }
        if cameraResolutions != resolutions { cameraResolutions = resolutions }
        let session = model.deviceSession
        do {
            let result = try await GoClient.shared.cameraMetadata(session: session)
            guard !Task.isCancelled, model.deviceSession == session else { return }
            if metadata != result { metadata = result }
            metadataError = nil
        } catch {
            guard !Task.isCancelled, model.deviceSession == session else { return }
            metadata = []
            metadataError = "Camera calibration unavailable: \(error.localizedDescription)"
        }
    }

    private func identifyGateway() async {
        guard let device = model.connected else { return }
        let session = model.deviceSession
        do {
            // Enrollment supplies the same tenant/device identity over LAN and Cloud.
            // Hardware identity remains an alias for placements saved by older apps.
            let cloud = SiteGatewayIdentity.cloud(selector: device.selector)
            async let info = GoClient.shared.deviceInfo(session: session)
            async let enrollment = GoClient.shared.deviceEnrollment(session: session)
            let hardware = try? await info
            let account = try? await enrollment
            let identity = try SiteGatewayIdentity.resolve(cloud: cloud, enrollment: account, serial: hardware?.serial)
            guard !Task.isCancelled, model.deviceSession == session else { return }
            gateway = identity
            identityError = nil
        } catch {
            guard !Task.isCancelled, model.deviceSession == session else { return }
            gateway = SiteGatewayIdentity()
            identityError = error.localizedDescription
        }
    }
}

struct SiteGatewayIdentity: Equatable {
    var id = ""
    var aliases: Set<String> = []

    static func matches(_ saved: String, current: String, aliases: Set<String> = []) -> Bool {
        !current.isEmpty && !saved.isEmpty && (saved == current || aliases.contains(saved))
    }

    static func resolve(cloud: String?, enrollment: DeviceEnrollmentSnapshot?, serial: String?) throws -> Self {
        let enrolled = enrollment.flatMap { account -> String? in
            guard account.enrolled, let tenant = UUID(uuidString: account.organizationID),
                  let device = UUID(uuidString: account.deviceID) else { return nil }
            return "cloud-v2/\(tenant.uuidString.lowercased())/\(device.uuidString.lowercased())"
        }
        if let cloud, let enrolled, cloud != enrolled {
            throw WendyError(message: "The connected gateway's enrollment does not match its Cloud identity. Reconnect to the gateway.")
        }
        let hardware = enrollment.flatMap { account -> String? in
            guard let serial, !serial.isEmpty, !account.enrolled || !account.organizationID.isEmpty else { return nil }
            return "hardware-v1/\(account.organizationID)/\(serial)"
        }
        guard let id = cloud ?? enrolled ?? hardware else {
            throw WendyError(message: "This gateway has no persistent identity. Connect through its Cloud entry to save camera placements.")
        }
        return Self(id: id, aliases: Set([id, hardware].compactMap { $0 }))
    }

    static func cloud(selector: String) -> String? {
        guard let url = URLComponents(string: selector), url.scheme == "cloud",
              url.user == nil, url.password == nil, url.query == nil, url.fragment == nil,
              let host = url.host, !host.isEmpty else { return nil }
        let path = url.path.split(separator: "/")
        guard path.count == 4, path[0] == "tenant", path[2] == "asset",
              let tenant = UUID(uuidString: String(path[1])),
              let asset = UUID(uuidString: String(path[3])) else { return nil }
        return "cloud-v2/\(tenant.uuidString.lowercased())/\(asset.uuidString.lowercased())"
    }
}

private struct SiteCameraPreview: View {
    @State private var ownedStream: UInt64 = 0
    @ObservedObject var model: AppModel
    @ObservedObject var feed: CameraFeed
    @Environment(\.scenePhase) private var scenePhase
    @State private var actions = AsyncActions()

    var body: some View {
        ZStack(alignment: .bottomLeading) {
            Color.black
            CameraFeedDisplay(feed: feed)
            VStack(alignment: .leading, spacing: 6) {
                Text(feed.camera.name).font(.headline)
                TimelineView(.periodic(from: .now, by: 1)) { context in
                    Text(status(at: context.date)).font(.caption)
                }
                if let error = feed.error {
                    Text(PresentedFailure(error).message).font(.caption)
                    Button("Retry") { actions.submit { ownedStream = await model.playOwnedCamera(feed, inline: true) } }
                }
            }.foregroundStyle(.white).padding(12)
                .frame(maxWidth: .infinity, alignment: .leading)
                .background(.black.opacity(0.65))
        }
        .task { await actions.run() }
        .task(id: scenePhase) {
            if scenePhase == .active { ownedStream = await model.playOwnedCamera(feed, inline: true) }
            else if ownedStream != 0, feed.stream == ownedStream { model.pauseCamera(feed, manually: false) }
        }
        .onDisappear { if ownedStream != 0, feed.stream == ownedStream { model.pauseCamera(feed, manually: false) } }
    }

    private func status(at now: Date) -> String {
        guard feed.running else { return feed.error == nil ? "Paused" : "Camera unavailable" }
        guard let received = feed.lastVideoReceivedAt else { return "Waiting for video…" }
        return (0...5).contains(now.timeIntervalSince(received)) ? "Receiving video" : "No recent video"
    }
}

/// A twin owns its color subscriptions independently of the selected preview.
private struct SiteCameraColorCapture: View {
    @ObservedObject var model: AppModel
    let sourceIDs: [String]
    var body: some View {
        ForEach(model.cameraFeeds.filter { sourceIDs.contains($0.camera.stableID) && !$0.camera.thermal }) { feed in
            SiteCameraColorFeedCapture(model: model, feed: feed)
        }
    }
}

private struct SiteCameraColorFeedCapture: View {
    @ObservedObject var model: AppModel
    @ObservedObject var feed: CameraFeed
    @Environment(\.scenePhase) private var scenePhase

    var body: some View {
        Color.clear.frame(width: 0, height: 0)
            .task(id: "\(model.deviceSession)/\(scenePhase)") {
                guard scenePhase == .active else { return }
                let session = model.deviceSession
                var ownedStream: UInt64 = 0
                var nextAttempt = Date.distantPast
                defer {
                    if ownedStream != 0, feed.stream == ownedStream { model.pauseCamera(feed, manually: false) }
                }
                while !Task.isCancelled, session == model.deviceSession {
                    if feed.running { nextAttempt = .distantPast }
                    if !feed.running, !feed.manuallyPaused, Date() >= nextAttempt {
                        nextAttempt = Date().addingTimeInterval(10)
                        ownedStream = await model.playOwnedCamera(feed, inline: true)
                    }
                    do { try await Task.sleep(for: .seconds(1)) } catch { return }
                }
            }
    }
}
