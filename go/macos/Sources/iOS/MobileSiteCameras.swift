import SwiftUI

/// Fixed camera placement is model-specific. Imported identities are never
/// used as connection targets: a camera must match this site's known devices.
struct MobileSiteCameras: View {
    @ObservedObject var model: AppModel
    @ObservedObject var store: MobileSitesStore
    let siteID: UUID
    let initialModel: UUID
    @Environment(\.dismiss) private var dismiss
    @State private var modelID: UUID?
    @State private var saved: StoredSite?
    @State private var placing: UUID?
    @State private var preview: SiteCamera?
    @State private var calibrating: SiteCamera?
    @State private var geometry: [UUID: SiteCameraGeometry] = [:]
    @State private var panel = false
    @State private var panelHeight: PresentationDetent = .height(260)
    @State private var adding = false
    @State private var busy = false
    @State private var error: String?
    @State private var actions = AsyncActions()
    private var site: MobileSiteRecord? { store.sites.first { $0.id == siteID } }
    private var activeID: UUID { modelID ?? initialModel }

    var body: some View {
        NavigationStack {
            Group {
                if let saved {
                    MobileSiteCoverageScene(model: model, saved: saved, assets: site?.assets ?? [], active: preview == nil && calibrating == nil && !adding, placing: busy ? nil : placing,
                              select: { id in preview = saved.site.cameras.first { $0.id == id } },
                              place: { id, point in updatePosition(id, point) })
                        .safeAreaInset(edge: .top) {
                            if let camera = saved.site.cameras.first(where: { $0.id == placing }) {
                                Label("Tap the model to place \(camera.name)", systemImage: "plus.circle.dashed")
                                    .font(.subheadline).padding(10).background(.regularMaterial, in: Capsule()).padding(.horizontal)
                            }
                        }
                } else if let error { ContentUnavailableView("Model unavailable", systemImage: "cube", description: Text(error)) }
                else { ProgressView("Loading model…") }
            }
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .principal) {
                    Menu {
                        ForEach(site?.models ?? []) { link in
                            Button(link.name) { modelID = link.id }
                        }
                    } label: {
                        HStack { Text(site?.models.first { $0.id == activeID }?.name ?? "Whole site").lineLimit(1); Image(systemName: "chevron.down").font(.caption) }
                    }.disabled(busy)
                }
                ToolbarItem(placement: .confirmationAction) { Button("Done") { panel = false; dismiss() }.disabled(busy) }
                ToolbarItem(placement: .bottomBar) { Button("Cameras", systemImage: "camera") { panel = true } }
            }
        }.tint(MobileLensStyle.green)
            .task { await actions.run() }
            .task(id: activeID) {
                saved = nil; placing = nil; preview = nil; error = nil
                do {
                    let value = try await store.models.load(id: activeID)
                    try Task.checkCancellation()
                    saved = value; panel = true
                    await refreshGeometry()
                } catch { if !Task.isCancelled { self.error = error.localizedDescription } }
            }
            .sheet(isPresented: $panel) {
                cameraPanel
                    .presentationDetents([.height(260), .large], selection: $panelHeight)
                    .presentationBackgroundInteraction(.enabled(upThrough: .height(260)))
                    .interactiveDismissDisabled()
            }
    }
    private var cameraPanel: some View {
        NavigationStack {
            List {
                if let saved {
                    Section {
                        ForEach(saved.site.cameras) { camera in
                            VStack(alignment: .leading, spacing: 8) {
                                Button {
                                    placing = camera.id; panelHeight = .height(260)
                                } label: {
                                    Label {
                                        VStack(alignment: .leading, spacing: 3) {
                                            Text(camera.name).foregroundStyle(Color.primary)
                                            Text(placing == camera.id ? "Tap the model to place" : camera.position == nil ? "Not placed" : (camera.manualAlignment != nil ? "Manually aligned" : geometry[camera.id]?.lens?.measured == true ? "Aligned · calibrated ✓" : geometry[camera.id] != nil ? "Aligned · estimated" : "Placed manually"))
                                                .font(.caption).foregroundStyle(.secondary)
                                        }
                                    } icon: { Image(systemName: camera.position == nil ? "camera" : "checkmark.circle.fill") }
                                }.buttonStyle(.plain).disabled(busy)
                                HStack {
                                    Button("Live preview", systemImage: "play.rectangle") { placing = nil; preview = camera }.buttonStyle(.borderless)
                                    Button("Align camera") { placing = nil; calibrating = camera }.buttonStyle(.borderless)
                                    Spacer()
                                    Menu {
                                        Button("Move camera") { placing = camera.id; panelHeight = .height(260) }
                                        Button("Remove from model", role: .destructive) { remove(camera.id) }
                                    } label: { Image(systemName: "ellipsis.circle") }
                                    .accessibilityLabel("Options for \(camera.name)")
                                }.font(.subheadline).disabled(busy)
                            }.listRowBackground(placing == camera.id ? MobileLensStyle.green.opacity(0.12) : Color(uiColor: .secondarySystemGroupedBackground))
                        }
                        Button("Add cameras", systemImage: "plus") { adding = true }.disabled(busy)
                            .listRowBackground(Color(uiColor: .secondarySystemGroupedBackground))
                    } footer: {
                        Text("Hold and drag a marker to adjust it. Robot cameras move with the robot, so they aren’t listed. Manual placement does not calibrate the camera.")
                    }
                }
                if busy { ProgressView("Saving placement…") }
                if let error { Text(error).foregroundStyle(.red) }
            }
            .scrollContentBackground(.hidden).background(MobileSitesStyle.background)
            .navigationTitle("Place Cameras · \(saved?.site.cameras.filter { $0.position != nil }.count ?? 0) of \(saved?.site.cameras.count ?? 0) placed")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .confirmationAction) { Button("Done") { panel = false; dismiss() }.disabled(busy) }
                if placing != nil { ToolbarItem(placement: .cancellationAction) { Button("Cancel placement") { placing = nil }.disabled(busy) } }
            }
            .sheet(isPresented: $adding) {
                MobileSiteCameraPicker(model: model, site: site, existing: saved?.site.cameras ?? []) { camera in
                    guard var value = saved, !busy else { return }
                    value.site.cameras.append(camera)
                    persist(value, text: "\(camera.name) added to \(value.site.name)", nextPlacement: camera.id)
                }
            }
            .sheet(item: $calibrating, onDismiss: { actions.submit { saved = try? await store.models.load(id: activeID); await refreshGeometry() } }) { camera in
                if let saved {
                    MobileSiteCameraLive(model: model, site: site, camera: camera, calibrationModel: saved, store: store) { result in
                        geometry[camera.id] = SiteCameraGeometry(pose: result.pose, lens: result.lens)

                    }
                }
            }
            .sheet(item: $preview, onDismiss: {
                actions.submit {
                    do { saved = try await store.models.load(id: activeID); await refreshGeometry() }
                    catch { self.error = error.localizedDescription }
                }
            }) { camera in
                MobileSiteCameraLive(model: model, site: site, camera: camera, calibrationModel: saved, store: store)
            }
        }.tint(MobileLensStyle.green)
    }
    private func refreshGeometry() async {
        geometry = [:]
        guard let saved, let device = model.connected, model.selectedDeviceReady else { return }
        let session = model.deviceSession
        do {
            async let info = GoClient.shared.deviceInfo(session: session)
            async let enrollment = GoClient.shared.deviceEnrollment(session: session)
            let identity = try await SiteGatewayIdentity.resolve(cloud: SiteGatewayIdentity.cloud(selector: device.selector), enrollment: enrollment, serial: info.serial)
            let records = try await GoClient.shared.cameraMetadata(session: session)
            guard session == model.deviceSession else { return }
            for camera in saved.site.cameras where SiteGatewayIdentity.matches(camera.gatewayID, current: identity.id, aliases: identity.aliases) {
                geometry[camera.id] = MobileSiteCoveragePolicy.geometry(camera: camera, site: saved.site, records: records, gateway: identity, now: Date())
            }
        } catch { self.error = "Calibration status unavailable: " + error.localizedDescription }
    }
    private func persist(_ value: StoredSite, text: String, nextPlacement: UUID?) {
        busy = true; error = nil
        actions.submit {
            defer { busy = false }
            do {
                try await store.saveCameraModel(value.site, siteID: siteID, event: text)
                saved = value; placing = nextPlacement; panelHeight = .height(260)
            } catch { self.error = error.localizedDescription }
        }
    }
    private func updatePosition(_ id: UUID, _ point: SitePoint) {
        guard !busy, var value = saved, let index = value.site.cameras.firstIndex(where: { $0.id == id }), point.isValid else { return }
        geometry[id] = nil
        value.site.cameras[index].position = point
        persist(value, text: "\(value.site.cameras[index].name) placed in \(value.site.name)", nextPlacement: nil)
    }
    private func remove(_ id: UUID) {
        guard !busy, var value = saved, let camera = value.site.cameras.first(where: { $0.id == id }) else { return }
        value.site.cameras.removeAll { $0.id == id }
        persist(value, text: "\(camera.name) removed from \(value.site.name)", nextPlacement: nil)
    }
}

private struct MobileSiteCameraPicker: View {
    @ObservedObject var model: AppModel
    let site: MobileSiteRecord?
    let existing: [SiteCamera]
    let add: (SiteCamera) -> Void
    @Environment(\.dismiss) private var dismiss
    @State private var gateway = SiteGatewayIdentity()
    @State private var selectedDevice: Device?
    @State private var session: UInt64?
    @State private var busy = false
    @State private var error: String?
    @State private var search = ""
    @State private var actions = AsyncActions()
    private var devices: [Device] {
        let ids = Set((site?.assets ?? []).filter { !$0.robot }.map(\.id))
        return HomeWidgetInventory.devices(model).filter { ids.contains(recordingDeviceID($0)) }
            .sorted {
                let first = isOnline($0), second = isOnline($1)
                return first != second ? first : $0.title.localizedStandardCompare($1.title) == .orderedAscending
            }
    }
    private func isOnline(_ device: Device) -> Bool {
        ["Online", "Connected", "Nearby"].contains(HomeWidgetInventory.status(device, model: model))
    }
    private var feeds: [CameraFeed] {
        guard session == model.deviceSession else { return [] }
        let groups = Dictionary(grouping: model.cameraFeeds, by: { $0.camera.stableID })
        return model.cameraFeeds.filter { feed in
            let id = feed.camera.stableID
            return !id.isEmpty && groups[id]?.count == 1 &&
                !existing.contains { SiteGatewayIdentity.matches($0.gatewayID, current: gateway.id, aliases: gateway.aliases) && $0.sourceID == id } &&
                (search.isEmpty || feed.camera.name.localizedCaseInsensitiveContains(search))
        }
    }
    var body: some View {
        NavigationStack {
            List {
                Section("Site devices") {
                    ForEach(devices) { device in
                        Button { connect(device) } label: {
                            HStack {
                                Circle().fill(isOnline(device) ? MobileLensStyle.green : Color.gray).frame(width: 8, height: 8)
                                Text(device.title).foregroundStyle(isOnline(device) ? Color.primary : Color.secondary)
                                Spacer()
                                if selectedDevice?.id == device.id { Image(systemName: "checkmark") }
                            }
                        }.disabled(busy)
                    }
                    if devices.isEmpty { Text("This site has no available fixed-camera devices.").foregroundStyle(.secondary) }
                }
                if session == model.deviceSession, !gateway.id.isEmpty {
                    Section("Fixed cameras") {
                        ForEach(feeds) { feed in
                            Button {
                                add(SiteCamera(gatewayID: gateway.id, sourceID: feed.camera.stableID, name: feed.camera.name))
                                dismiss()
                            } label: { Label(feed.camera.name, systemImage: "camera") }
                        }
                        if feeds.isEmpty { Text("No additional cameras found").foregroundStyle(.secondary) }
                    }
                }
                if busy { ProgressView("Finding cameras…") }
                if let error { Text(error).foregroundStyle(.red) }
            }.scrollContentBackground(.hidden).background(MobileSitesStyle.background)
                .searchable(text: $search, prompt: "Search cameras")
                .navigationTitle("Add Camera").navigationBarTitleDisplayMode(.inline)
                .toolbar { ToolbarItem(placement: .cancellationAction) { Button("Cancel") { dismiss() } } }
        }.tint(MobileLensStyle.green).task { await actions.run() }
    }
    private func connect(_ device: Device) {
        busy = true; error = nil; gateway = .init(); session = nil; selectedDevice = device
        let scope = model.mobileCloudSelectionKey
        actions.submit {
            defer { busy = false }
            do {
                let identity = try await connectSiteCameraGateway(model, device: device)
                guard !Task.isCancelled, scope == model.mobileCloudSelectionKey else { return }
                gateway = identity; session = model.deviceSession
            } catch { if !Task.isCancelled { self.error = error.localizedDescription } }
        }
    }
}

@MainActor enum MobileSiteCameraFeedLookup {
    static func resolve(sourceID: String, session: UInt64, currentSession: UInt64, feeds: [CameraFeed]) -> CameraFeed? {
        guard !sourceID.isEmpty, session != 0, session == currentSession else { return nil }
        let matches = feeds.filter { $0.camera.stableID == sourceID }
        return matches.count == 1 ? matches.first : nil
    }
}

struct MobileSiteCameraLive: View {
    @ObservedObject var model: AppModel
    let site: MobileSiteRecord?
    let camera: SiteCamera
    var calibrationModel: StoredSite? = nil
    var store: MobileSitesStore? = nil
    var onAligned: (SiteLocalizationResult) -> Void = { _ in }
    @State private var identity = SiteGatewayIdentity()
    @State private var calibrationBusy = false
    @State private var connectionStatus = "Finding camera…"
    @State private var cameraSession: UInt64 = 0
    @Environment(\.dismiss) private var dismiss
    @State private var feed: CameraFeed?
    @State private var error: String?
    @State private var busy = false
    @State private var actions = AsyncActions()
    private var devices: [Device] {
        let ids = Set((site?.assets ?? []).filter { !$0.robot }.map(\.id))
        guard model.mobileScopeIsCurrent else { return [] }
        var seen = Set<String>()
        let inventory = HomeWidgetInventory.devices(model) + [model.connected].compactMap { $0 }
        return inventory.filter { ids.contains(recordingDeviceID($0)) && seen.insert(recordingDeviceID($0)).inserted }
    }
    var body: some View {
        NavigationStack {
            VStack {
                if let feed, let saved = calibrationModel {
                    MobileSiteCalibration(model: model, request: SiteAlignmentRequest(site: saved, camera: camera, gatewayID: identity.id, session: model.deviceSession, gatewayAliases: identity.aliases), feed: feed, onSaved: alignmentSaved, onManualSaved: manualAlignmentSaved, onBusyChanged: { calibrationBusy = $0 }).id(feed.id)
                } else if let feed { SiteCameraPreview(model: model, feed: feed).id(feed.id).aspectRatio(4/3, contentMode: .fit) }
                else {
                    VStack(spacing: 16) {
                        Image(systemName: "video").font(.largeTitle).foregroundStyle(MobileLensStyle.green)
                        Text(busy ? connectionStatus : "Camera unavailable").font(.headline)
                        if !busy {
                            Text("Wendy checks the devices assigned to this site for the saved camera. Check that its device is online and still belongs to this site.")
                                .font(.subheadline).foregroundStyle(.secondary).multilineTextAlignment(.center)
                            Button("Try again", systemImage: "arrow.clockwise") { findCamera() }.buttonStyle(.borderedProminent)
                        }
                    }.frame(maxWidth: .infinity).padding(24)

                }
                if busy { ProgressView() }
                if let error { Text(error).foregroundStyle(.secondary).padding() }
            }.frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .top).padding().background(MobileSitesStyle.background).navigationTitle(camera.name).navigationBarTitleDisplayMode(.inline)
                .toolbar { ToolbarItem(placement: .confirmationAction) { Button("Done") { dismiss() }.disabled(calibrationBusy) } }
        }.tint(MobileLensStyle.green)
            .task { await actions.run() }
            .task(id: "\(model.mobileScopeIsCurrent)-\(devices.map(\.id).joined(separator: "|"))") { if feed == nil { findCamera() } }
            .onChange(of: model.cameraFeeds.map(\.id)) { _, _ in
                guard !busy, cameraSession != 0 else { return }
                feed = MobileSiteCameraFeedLookup.resolve(sourceID: camera.sourceID, session: cameraSession,
                    currentSession: model.deviceSession, feeds: model.cameraFeeds)
                if feed != nil { error = nil }
                else { error = model.camerasError ?? "The camera is no longer listed by its device. Waiting for the next refresh; you can also try again." }
            }
            .onChange(of: model.deviceSession) { _, session in
                guard !busy, cameraSession != 0, cameraSession != session else { return }
                feed = nil
                error = "The device connection changed. Tap Try again to reconnect to this camera."
            }
    }
    private func manualAlignmentSaved(_ position: SitePoint, _ alignment: SiteManualAlignment) async throws {
        guard let store, let site, let calibrationModel, model.mobileScopeIsCurrent else {
            throw WendyError(message: "Reopen the camera from its site to save alignment.")
        }
        var value = try await store.models.load(id: calibrationModel.id)
        guard value.site.modelRevision == alignment.modelRevision,
              let index = value.site.cameras.firstIndex(where: { $0.id == camera.id }) else {
            throw WendyError(message: "The model changed. Reopen alignment.")
        }
        value.site.cameras[index].position = position
        value.site.cameras[index].manualAlignment = alignment
        try await store.saveCameraModel(value.site, siteID: site.id, event: "\(camera.name) manually aligned")
    }
    private func alignmentSaved(_ result: SiteLocalizationResult) async {
        onAligned(result)
        guard let store, let site, let calibrationModel else { return }
            do {
                var value = try await store.models.load(id: calibrationModel.id)
                guard let index = value.site.cameras.firstIndex(where: { $0.id == camera.id }) else { return }
                value.site.cameras[index].position = result.pose.position
                            value.site.cameras[index].manualAlignment = nil
                try await store.saveCameraModel(value.site, siteID: site.id, event: "\(camera.name) \(result.lens.measured ? "aligned" : "aligned approximately")")
            } catch { self.error = "Alignment saved on the device, but the local placement could not be updated: " + error.localizedDescription }
    }
    private func findCamera() {
        guard !busy else { return }
        busy = true; error = nil; feed = nil; cameraSession = 0
        connectionStatus = "Finding camera…"
        actions.submit {
            defer { busy = false }
            let assigned = (site?.assets ?? []).filter { !$0.robot }
            guard !assigned.isEmpty else {
                error = "No devices are assigned to this site. Add a device in Edit devices and robots."
                return
            }
            guard model.mobileScopeIsCurrent else {
                error = "Your account is still loading. Wendy will retry when it is ready."
                return
            }
            let scope = model.mobileCloudSelectionKey
            if devices.isEmpty {
                connectionStatus = "Loading the site’s devices…"
                await model.refreshCloud()
                await model.refreshNearby(automatic: true)
                let deadline = ContinuousClock.now.advanced(by: .seconds(30))
                while (model.cloudLoading || model.nearbyLoading), ContinuousClock.now < deadline {
                    do { try await Task.sleep(for: .milliseconds(200)) } catch { return }
                }
            }
            guard !Task.isCancelled, model.mobileScopeIsCurrent, scope == model.mobileCloudSelectionKey else { return }
            var failures: [String] = []
            let connectedID = model.connected?.id
            let candidates = devices.sorted { a, b in
                func priority(_ device: Device) -> Int {
                    if SiteGatewayIdentity.cloud(selector: device.selector) == camera.gatewayID || "cloud-v2/" + recordingDeviceID(device) == camera.gatewayID { return 0 }
                    return device.id == connectedID ? 1 : 2
                }
                return priority(a) < priority(b)
            }
            for device in candidates {
                guard !Task.isCancelled else { return }
                connectionStatus = "Finding camera on \(device.title)…"
                do {
                    let identity = try await connectSiteCameraGateway(model, device: device)
                    try Task.checkCancellation()
                    guard SiteGatewayIdentity.matches(camera.gatewayID, current: identity.id, aliases: identity.aliases) else { continue }
                    let matches = model.cameraFeeds.filter { $0.camera.stableID == camera.sourceID }
                    guard matches.count == 1 else {
                        error = "The camera’s device is \(device.title), but it is not reporting this camera uniquely. Check its camera connection and try again."
                        return
                    }
                    self.identity = identity
                    cameraSession = model.deviceSession
                    feed = matches[0]
                    return
                } catch is CancellationError { return }
                catch { failures.append("\(device.title): \(error.localizedDescription)") }
            }
            if !failures.isEmpty { error = failures.joined(separator: "\n\n"); return }
            if candidates.isEmpty {
                let names = assigned.map(\.name).joined(separator: ", ")
                let detail = [model.cloudError, model.nearbyError].compactMap { $0 }.joined(separator: "\n")
                error = "This site has \(assigned.count) assigned device(s): \(names). They are not available in the current device list."
                    + (detail.isEmpty ? " Pull to refresh Devices, or tap Try again." : "\n\n" + detail)
            } else {
                error = "The saved camera could not be found on this site’s available devices. Its placement may refer to an old pairing."
            }
        }
    }
}

@MainActor private func connectSiteCameraGateway(_ model: AppModel, device: Device) async throws -> SiteGatewayIdentity {
    let alreadyConnected = model.selectedDeviceReady && model.connected?.id == device.id
    await model.connect(device)
    try Task.checkCancellation()
    guard model.selectedDeviceReady, model.connected?.id == device.id else { throw WendyError(message: model.connectionError ?? "Could not connect to the camera device.") }
    let session = model.deviceSession
    if alreadyConnected { await model.refreshDetails() }
    if let error = model.camerasError { throw WendyError(message: error) }
    async let info = GoClient.shared.deviceInfo(session: session)
    async let enrollment = GoClient.shared.deviceEnrollment(session: session)
    let hardware = try await info
    let account = try await enrollment
    try Task.checkCancellation()
    guard model.deviceSession == session else { throw WendyError(message: "The device connection changed. Try again.") }
    return try SiteGatewayIdentity.resolve(cloud: SiteGatewayIdentity.cloud(selector: device.selector), enrollment: account, serial: hardware.serial)
}
