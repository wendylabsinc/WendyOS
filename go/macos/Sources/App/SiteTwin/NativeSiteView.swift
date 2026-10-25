import SwiftUI

/// Site files contain identities and geometry. The active Go session owns all
/// credentials, routing and camera subscriptions on both platforms.
struct NativeSiteView: View {
    @ObservedObject var model: AppModel
    @State private var actions = AsyncActions()
    @State private var gatewayID = ""
    @State private var identityError: String?
    @State private var pairing = false
    @State private var alignment: SiteAlignmentRequest?
    @State private var metadata: [SiteCameraMetadata] = []
    @State private var metadataError: String?

    var body: some View {
        TimelineView(.periodic(from: .now, by: 1)) { _ in
            SiteWorkspace(gatewayID: gatewayID, feeds: feeds, metadata: displayMetadata, notices: notices, controls: {
                HStack {
                    Button("Pair Cameras", systemImage: "plus") { pairing = true }
                    Button("Refresh", systemImage: "arrow.clockwise") {
                        actions.submit { await refresh() }
                    }
                }.buttonStyle(.bordered)
            }, onFailure: { error, _ in model.alert = error.localizedDescription }, calibrate: { site, camera in
                alignment = SiteAlignmentRequest(site: site, camera: camera)
            }) { sourceID in
                if let feed = model.cameraFeeds.first(where: { $0.camera.stableID == sourceID }) {
                    SiteCameraPreview(model: model, feed: feed).id(feed.id)
                }
            }
        }
        .task { await actions.run() }
        .task { await identifyGateway() }
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
                            lastReceivedAt: feed.lastVideoReceivedAt, connected: feed.running)
        }
    }

    private var displayMetadata: [SiteCameraMetadata] {
        metadata.map { record in
            var result = record
            if let feed = model.cameraFeeds.first(where: { $0.camera.stableID == record.sourceID }), let lens = record.lens {
                let width = feed.jpeg?.width ?? feed.decoder.imageSize?.width
                let height = feed.jpeg?.height ?? feed.decoder.imageSize?.height
                // A resolution change can mean a different sensor crop. Do not
                // silently scale calibration unless the publisher supplies it.
                if let width, let height, width != lens.width || height != lens.height { result.lens = nil }
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
        let session = model.deviceSession
        do {
            let result = try await GoClient.shared.cameraMetadata(session: session)
            guard !Task.isCancelled, model.deviceSession == session else { return }
            metadata = result
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
            let identity: String
            if let cloud = SiteGatewayIdentity.cloud(selector: device.selector) {
                identity = cloud
            } else {
                async let info = GoClient.shared.deviceInfo(session: session)
                async let enrollment = GoClient.shared.deviceEnrollment(session: session)
                let (hardware, account) = try await (info, enrollment)
                guard !hardware.serial.isEmpty, !account.enrolled || !account.organizationID.isEmpty else {
                    throw WendyError(message: "This gateway has no persistent hardware identity. Connect through its Cloud entry to save camera placements.")
                }
                identity = "hardware-v1/\(account.organizationID)/\(hardware.serial)"
            }
            guard !Task.isCancelled, model.deviceSession == session else { return }
            gatewayID = identity
            identityError = nil
        } catch {
            guard !Task.isCancelled, model.deviceSession == session else { return }
            gatewayID = ""
            identityError = error.localizedDescription
        }
    }
}

enum SiteGatewayIdentity {
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
            else if ownedStream != 0, feed.stream == ownedStream { model.pauseCamera(feed) }
        }
        .onDisappear { if ownedStream != 0, feed.stream == ownedStream { model.pauseCamera(feed) } }
    }

    private func status(at now: Date) -> String {
        guard feed.running else { return feed.error == nil ? "Paused" : "Camera unavailable" }
        guard let received = feed.lastVideoReceivedAt else { return "Waiting for video…" }
        return (0...5).contains(now.timeIntervalSince(received)) ? "Receiving video" : "No recent video"
    }
}
