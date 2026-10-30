import OSLog
@preconcurrency import SceneKit
import SwiftUI
import UniformTypeIdentifiers

public struct SiteFeed: Identifiable {
    public var id: String
    public var name: String
    public var lastReceivedAt: Date?
    public var connected: Bool
    public var colorSource: SiteCameraColorSource?
    private var activity: (@MainActor () -> (Date?, Bool))?

    public init(id: String, name: String, lastReceivedAt: Date? = nil, connected: Bool = false,
                colorSource: SiteCameraColorSource? = nil,
                activity: (@MainActor () -> (Date?, Bool))? = nil) {
        self.id = id
        self.name = name
        self.lastReceivedAt = lastReceivedAt
        self.connected = connected
        self.colorSource = colorSource
        self.activity = activity
    }

    @MainActor public func isReceiving(at now: Date) -> Bool {
        let (receivedAt, isConnected) = activity?() ?? (lastReceivedAt, connected)
        guard isConnected, let lastReceivedAt = receivedAt else { return false }
        return (0...5).contains(now.timeIntervalSince(lastReceivedAt))
    }
}

/// Hosts share site setup and rendering. Credentials and video connections stay
/// with their existing authenticated device session.
@MainActor
public struct SiteWorkspace<Preview: View, Controls: View>: View {
    let gatewayID: String
    let gatewayAliases: Set<String>

    private func onGateway(_ camera: SiteCamera) -> Bool {
        SiteGatewayIdentity.matches(camera.gatewayID, current: gatewayID, aliases: gatewayAliases)
    }
    let feeds: [SiteFeed]
    let metadata: [SiteCameraMetadata]
    let notices: [String]
    let controls: () -> Controls
    let preview: (String) -> Preview
    let onFailure: ((any Error, Bool) -> Void)?
    let calibrate: ((StoredSite, SiteCamera) -> Void)?
    let automaticAlignment: ((StoredSite, Bool) -> AnyView)?
    let alignmentStatuses: [String: String]
    let colorCapture: (([String]) -> AnyView)?
    let robotSession: UInt64
    let robotGatewayName: String
    // Telemetry is observed by the scene and controls, never by the menus.
    @State private var robots = SiteRobotMapModel()
    @State private var fog = SiteFogConfiguration()
    @State private var fogSummary = SiteFogSummary()
    @State private var cameraColorEnabled = false
    @State private var cameraColorStatuses: [UUID: String] = [:]
    #if os(iOS)
    @State private var showingRobotControls = false
    @State private var showingMobileCameras = false
    @State private var pendingMobileCameraAction: MobileCameraAction?
    private enum MobileCameraAction {
        case add, align(SiteCamera), place(UUID)
    }
    #else
    @State private var showingRobotControls = true
    #endif
    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    @State private var actions = AsyncActions()
    @State private var underlyingError: (any Error)?
    @State private var store: SiteStore?
    @State private var sites: [StoredSite] = []
    @State private var selectedSite: UUID?
    @State private var selectedCamera: UUID?
    @State private var setupPreviewID: String?
    @State private var placingCamera: UUID?
    @State private var importing = false
    @State private var exporting = false
    @State private var document: SiteFileDocument?
    @State private var scanning = false
    @State private var alignmentScanCamera: SiteCamera?
    @State private var scanPresentationActive = false
    @State private var pendingAlignment: (StoredSite, SiteCamera)?
    @State private var reconstructing = false
    @State private var loading = true
    @State private var busy = false
    @State private var error: String?
    @State private var loadError = false
    @State private var camerasExpanded = true
    @State private var cameraListHeight: CGFloat?
    @State private var choosingCamera = false
    @State private var cameraToAdd: (camera: SiteCamera, siteID: UUID)?
    @State private var pendingCamera: SiteCamera?
    @State private var removalCandidate: StoredSite?
    @State private var removedSites: [StoredSite] = []
    @State private var showingRemoved = false

    public init(
        gatewayID: String,
        gatewayAliases: Set<String> = [],
        feeds: [SiteFeed],
        metadata: [SiteCameraMetadata] = [],
        notices: [String] = [],
        robotSession: UInt64 = 0,
        robotGatewayName: String = "this gateway",
        @ViewBuilder controls: @escaping () -> Controls,
        onFailure: ((any Error, Bool) -> Void)? = nil,
        calibrate: ((StoredSite, SiteCamera) -> Void)? = nil,
        automaticAlignment: ((StoredSite, Bool) -> AnyView)? = nil,
        alignmentStatuses: [String: String] = [:],
        colorCapture: (([String]) -> AnyView)? = nil,
        @ViewBuilder preview: @escaping (String) -> Preview
    ) {
        self.gatewayID = gatewayID
        self.gatewayAliases = gatewayAliases
        self.feeds = feeds
        self.metadata = metadata
        self.notices = notices
        self.robotSession = robotSession
        self.robotGatewayName = robotGatewayName
        self.controls = controls
        self.onFailure = onFailure
        self.calibrate = calibrate
        self.automaticAlignment = automaticAlignment
        self.alignmentStatuses = alignmentStatuses
        self.colorCapture = colorCapture
        self.preview = preview
    }

    private var current: StoredSite? { sites.first { $0.id == selectedSite } }

    public var body: some View {
        VStack(spacing: 0) {
            if loading {
                ProgressView("Opening Sites…").frame(maxWidth: .infinity, maxHeight: .infinity)
            } else if loadError {
                ContentUnavailableView {
                    Label("Couldn’t Open Sites", systemImage: "exclamationmark.triangle")
                } description: {
                    Text("Your saved sites could not be read. Try opening them again.")
                } actions: {
                    Button("Try Again") { actions.submit { await load() } }
                }
            } else if let current {
                workspace(current)
            } else {
                ScrollView {
                    VStack(spacing: 16) {
                        ContentUnavailableView {
                            Label("Create Your Site", systemImage: "building.2.crop.circle")
                        } description: {
                            Text(
                                "Import a USDZ scan, then place your cameras to open their live views from the model."
                            )
                        } actions: {
                            importButtons
                        }
                        if !feeds.isEmpty {
                            Text("Available Cameras").font(.headline)
                            ForEach(feeds) { feed in
                                Button("Preview \(feed.name)", systemImage: "video") {
                                    setupPreviewID = feed.id
                                }.buttonStyle(.bordered)
                            }
                        }
                        if let sourceID = setupPreviewID,
                            feeds.contains(where: { $0.id == sourceID })
                        {
                            preview(sourceID)
                                .frame(height: 280)
                                .clipShape(RoundedRectangle(cornerRadius: 12))
                            Button("Stop Preview") { setupPreviewID = nil }
                        }
                    }.padding().padding(.top, 160)
                }
            }
        }
        .background {
            if cameraColorEnabled, let current, let colorCapture {
                colorCapture(current.site.cameras.filter { onGateway($0) }.map(\.sourceID))
            }
            if let current, let automaticAlignment {
                automaticAlignment(current, placingCamera != nil || scanning || busy || choosingCamera || importing)
            }
        }
        .onChange(of: gatewayID) { _, _ in
            setupPreviewID = nil; selectedCamera = nil; placingCamera = nil; pendingCamera = nil
            cameraToAdd = nil
        }
        .sheet(isPresented: $choosingCamera, onDismiss: {
            guard let pending = cameraToAdd else { return }
            cameraToAdd = nil
            actions.submit {
                let camera = pending.camera
                guard onGateway(camera), current?.id == pending.siteID,
                      await edit({ $0.cameras.append(camera) }), let current,
                      current.id == pending.siteID, onGateway(camera) else { return }
                selectedCamera = camera.id
                camerasExpanded = false
                if cameraMetadata(camera)?.pose(in: current.site, now: Date()) == nil,
                   automaticAlignment == nil || current.visualMapURL == nil {
                    beginAlignment(camera)
                }
            }
        }) {
            SiteCameraPicker(feeds: availableFeeds, preview: preview) { feed in
                guard let current else { return }
                cameraToAdd = (SiteCamera(gatewayID: gatewayID, sourceID: feed.id, name: feed.name), current.id)
            }
        }
        .confirmationDialog("Remove Capture?", isPresented: Binding(
            get: { removalCandidate != nil }, set: { if !$0 { removalCandidate = nil } }
        ), titleVisibility: .visible, presenting: removalCandidate) { candidate in
            Button("Remove Capture", role: .destructive) {
                actions.submit { await removeCapture(candidate) }
            }
            Button("Cancel", role: .cancel) {}
        } message: { candidate in
            Text("Move \(candidate.site.name) and its camera placements to Recently Removed on this device? You can restore it later.")
        }
        .sheet(isPresented: $showingRemoved) { removedCaptures }

        .overlay(alignment: .topLeading) {
            #if os(iOS)
            HStack(spacing: 12) {
                Text(current?.site.name ?? "Digital Twin").font(.headline).lineLimit(1)
                Spacer(minLength: 8)
                siteMenu.labelStyle(.iconOnly)
                Menu {
                    controls()
                    if current != nil {
                        Divider()
                        Toggle("Camera color", isOn: $cameraColorEnabled)
                        Toggle("Fog of war", isOn: $fog.enabled)
                        if fog.enabled {
                            Button("Reset exploration", systemImage: "arrow.counterclockwise") { fog.reset = UUID() }
                        }
                    }
                    if !notices.isEmpty {
                        Divider()
                        ForEach(notices, id: \.self) { notice in Text(notice) }
                    }
                } label: {
                    Image(systemName: notices.isEmpty ? "slider.horizontal.3" : "exclamationmark.triangle")
                        .frame(minWidth: 44, minHeight: 44)
                }.accessibilityLabel("Twin settings")
            }
            .padding(.leading, 16).padding(.trailing, 4)
            .background(.regularMaterial, in: Capsule())
            .padding(12)
            #else
            VStack(alignment: .leading, spacing: 10) {
                HStack {
                    Text(current?.site.name ?? "Digital Twin").font(.title3.bold())
                    Spacer()
                    siteMenu
                }
                controls()
                if current != nil {
                    cameraColorControls
                    SiteFogControls(configuration: $fog, summary: fogSummary)
                }
                ForEach(notices, id: \.self) { notice in
                    Label(notice, systemImage: "exclamationmark.triangle")
                        .font(.caption).foregroundStyle(.secondary)
                }
            }
            .padding(14)
            .frame(maxWidth: 500, alignment: .leading)
            .background(.regularMaterial, in: RoundedRectangle(cornerRadius: 16))
            .padding(16)
            #endif
        }
        .fileImporter(
            isPresented: $importing,
            allowedContentTypes: [.usdz, .data],
            allowsMultipleSelection: false
        ) { result in
            switch result {
            case .success(let urls):
                guard let url = urls.first else { return }
                actions.submit { await importModel(url, scoped: true) }
            case .failure(let failure):
                if (failure as NSError).code != NSUserCancelledError {
                    report(
                        failure,
                        message: "The model could not be opened. Choose the file again."
                    )
                }
            }
        }
        .fileExporter(
            isPresented: $exporting,
            document: document,
            contentType: .data,
            defaultFilename: "\(current?.site.name ?? "Site").wendysite"
        ) { result in
            if case .failure(let cause) = result, (cause as NSError).code != NSUserCancelledError {
                report(
                    cause,
                    message:
                        "The site could not be exported. Choose a writable folder and try again."
                )
            }
            document = nil
        }
        .alert(
            "Couldn’t Save Site",
            isPresented: Binding(
                get: { error != nil && !loadError && onFailure == nil },
                set: { if !$0 { error = nil } }
            )
        ) {
            Button("OK", role: .cancel) { error = nil }
        } message: {
            Text(error ?? "The site could not be saved. Try again.")
        }
        .overlay {
            if busy {
                ProgressView("Saving Site…").padding().background(
                    .regularMaterial,
                    in: RoundedRectangle(cornerRadius: 12)
                )
            }
        }
        .task { await load() }
        .task { await actions.run() }
        .sheet(isPresented: $reconstructing) {
            SitePhotoReconstruction { result in
                reconstructing = false
                switch result {
                case .success(let url):
                    actions.submit {
                        await importModel(url, scoped: false)
                        try? FileManager.default.removeItem(at: url.deletingLastPathComponent())
                    }
                case .failure(let cause):
                    report(
                        cause,
                        message:
                            "The photographs could not be reconstructed. Try photographs with more overlap and clear surfaces."
                    )
                }
            }
        }
        #if os(iOS)
            .fullScreenCover(isPresented: $scanning, onDismiss: {
                alignmentScanCamera = nil
                scanPresentationActive = false
                if let pendingAlignment {
                    self.pendingAlignment = nil
                    calibrate?(pendingAlignment.0, pendingAlignment.1)
                }
            }) {
                SiteRoomCapture(requiresVisualReferences: alignmentScanCamera != nil) { result in
                    let alignmentCamera = alignmentScanCamera
                    alignmentScanCamera = nil
                    scanning = false
                    switch result {
                    case .success(let url):
                        actions.submit {
                            await importModel(url, scoped: false, alignmentCamera: alignmentCamera)
                            try? FileManager.default.removeItem(at: url.deletingLastPathComponent())
                        }
                    case .failure(let cause):
                        report(
                            cause,
                            message: "The room scan could not be saved. Scan the room again."
                        )
                    }
                }.onAppear { scanPresentationActive = true }
            }
        #endif
    }

    private var siteMenu: some View {
        Menu {
            ForEach(sites) { item in
                Button(item.site.name) {
                    selectedSite = item.id
                    selectedCamera = nil
                    placingCamera = nil
                    pendingCamera = nil
                }
            }
            Divider()
            Button("Import Model or Site", systemImage: "square.and.arrow.down") {
                importing = true
            }
            if SitePhotoReconstruction.isSupported {
                Button("Reconstruct from Photos", systemImage: "photo.stack") {
                    reconstructing = true
                }
            }
            if let current {
                if let store {
                    ShareLink(
                        item: SiteShare(store: store, id: current.id, name: current.site.name) { cause in
                            report(cause, message: "The digital twin could not be shared. Try again.")
                        },
                        preview: SharePreview(current.site.name, image: Image(systemName: "building.2"))
                    ) {
                        Label("Share Digital Twin…", systemImage: "square.and.arrow.up")
                    }
                }
                Button("Export Site", systemImage: "square.and.arrow.up") {
                    actions.submit { await exportSite(current.id) }
                }
                Button("Remove Capture…", systemImage: "trash", role: .destructive) {
                    removalCandidate = current
                }
            }
            #if os(iOS)
                if SiteRoomCapture.isSupported {
                    Button("Scan Room", systemImage: "viewfinder") { alignmentScanCamera = nil; scanning = true }
                }
            #endif
            if !removedSites.isEmpty {
                Divider()
                Button("Recently Removed", systemImage: "clock.arrow.circlepath") { showingRemoved = true }
            }
        } label: {
            Label("Sites", systemImage: "building.2")
        }
        .disabled(busy)
        .accessibilityLabel("Site Actions")
    }


    private var importButtons: some View {
        HStack {
            Button("Import Model") { importing = true }.buttonStyle(.borderedProminent)
            #if os(iOS)
                if SiteRoomCapture.isSupported {
                    Button("Scan Room") { alignmentScanCamera = nil; scanning = true }.buttonStyle(.bordered)
                }
            #endif
        }
    }

    private func colorSources(_ site: Site) -> [UUID: SiteCameraColorSource] {
        guard cameraColorEnabled, placingCamera == nil else { return [:] }
        var result: [UUID: SiteCameraColorSource] = [:]
        for camera in site.cameras where onGateway(camera) {
            if let source = feeds.first(where: { $0.id == camera.sourceID })?.colorSource { result[camera.id] = source }
        }
        return result
    }

    private var cameraColorControls: some View {
        VStack(alignment: .leading, spacing: 5) {
            Toggle("Camera color", isOn: $cameraColorEnabled).toggleStyle(.switch).fixedSize()
            if cameraColorEnabled, let current {
                let cameras = current.site.cameras
                let contributing = cameraColorStatuses.values.filter { $0.hasPrefix("Live camera color") }.count
                Text("\(contributing) of \(cameras.count) cameras contributing").font(.caption)
                ForEach(cameras) { camera in
                    Text("\(SiteCameraLabel(name: camera.name, sourceID: camera.sourceID).title): \(colorStatus(camera))")
                        .font(.caption2).foregroundStyle(.secondary)
                }
                if cameras.isEmpty { Text("Add cameras to this twin to color the model.").font(.caption) }
            }
        }
    }

    private func colorStatus(_ camera: SiteCamera) -> String {
        guard onGateway(camera) else { return "Connect to this camera's gateway." }
        guard let feed = feeds.first(where: { $0.id == camera.sourceID }) else { return "Camera unavailable on this gateway." }
        guard feed.colorSource != nil else { return "This feed does not provide visual color." }
        return cameraColorStatuses[camera.id] ?? "Waiting for camera…"
    }

    private func workspace(_ current: StoredSite) -> some View {
        GeometryReader { geometry in
            SiteRobotRenderHost(model: robots) { robotRender in
                SiteScene(
                    modelURL: current.modelURL,
                    site: current.site,
                    cameraGeometry: cameraGeometry(current.site),
                    colorSources: colorSources(current.site),
                    colorStatus: { cameraColorStatuses = $0 },
                    placingCamera: placingCamera,
                    select: { selectedCamera = $0; camerasExpanded = false
                        robots.selected = false; showingRobotControls = false
                        #if os(iOS)
                        showingMobileCameras = true
                        #endif
                    },
                    place: { id, point in
                        actions.submit {
                            let saved = await edit { site in
                                if var pending = pendingCamera, pending.id == id {
                                    pending.position = point
                                    site.cameras.append(pending)
                                } else if let index = site.cameras.firstIndex(where: { $0.id == id }) {
                                    site.cameras[index].position = point
                                }
                            }
                            if saved { placingCamera = nil; pendingCamera = nil }
                        }
                    },
                    robot: robotRender,
                    robotPoint: { point, immediate in
                        robots.point(point, immediate: immediate)
                        if !robots.placing, robots.destination != nil { showingRobotControls = true }
                    },
                    selectRobot: {
                        robots.selected = true; selectedCamera = nil; camerasExpanded = false; showingRobotControls = true
                    },
                    fog: fogConfiguration(current, robot: robotRender),
                    fogSummary: { fogSummary = $0 }
                )
                #if os(iOS)
                .ignoresSafeArea(.container, edges: .bottom)
                #endif
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity)
            #if os(macOS)
            .overlay(alignment: .bottomLeading) {
                SiteRobotPanel(model: robots, session: robotSession, gatewayName: robotGatewayName, site: current.site, modelURL: current.modelURL, availableHeight: geometry.size.height - 32)
                    .padding(16)
            }
            .overlay(alignment: .bottomTrailing) {
                cameraPanel(current, size: geometry.size).padding(16)
            }
            #else
            .overlay(alignment: .bottom) {
                VStack(spacing: 8) {
                    if placingCamera != nil {
                        Text("Tap the model where the camera is mounted.").font(.callout)
                        Button("Cancel Placement") { placingCamera = nil; pendingCamera = nil }
                    }
                    HStack {
                        MobileSiteRobotPanel(model: robots, session: robotSession, gatewayName: robotGatewayName, site: current.site, modelURL: current.modelURL, isPresented: $showingRobotControls)
                        Button("Cameras", systemImage: "video") {
                            robots.stop(); showingRobotControls = false; robots.selected = false
                            camerasExpanded = true
                            showingMobileCameras = true
                        }.buttonStyle(.bordered)
                    }
                }
                .padding(24)
                .onChange(of: showingRobotControls) { _, visible in
                    if visible { showingMobileCameras = false }
                }
            }
            .sheet(isPresented: $showingMobileCameras, onDismiss: {
                let pending = pendingMobileCameraAction
                pendingMobileCameraAction = nil
                selectedCamera = nil
                switch pending {
                case .add: choosingCamera = true
                case .align(let camera): beginAlignment(camera)
                case .place(let id): pendingCamera = nil; placingCamera = id
                case nil: break
                }
            }) {
                NavigationStack {
                    ScrollView { cameraPanel(current, size: geometry.size).padding(20) }
                        .navigationTitle("Cameras").navigationBarTitleDisplayMode(.inline)
                        .toolbar {
                            ToolbarItem(placement: .confirmationAction) {
                                Button("Done") { showingMobileCameras = false }
                            }
                        }
                }
                .presentationDetents([.medium, .large])
                .presentationDragIndicator(.visible)
            }
            #endif
        }
    }

    private func fogConfiguration(_ current: StoredSite, robot: SiteRobotRenderState) -> SiteFogConfiguration {
        var value = fog
        value.context = "\(gatewayID)/\(robotSession)/\(current.id)/\(current.site.modelRevision)/\(current.site.metresPerModelUnit)/\(robot.explorationEpoch)"
        return value
    }

    private func cameraPanel(_ current: StoredSite, size: CGSize) -> some View {
        VStack(alignment: .leading, spacing: 10) {
            #if os(iOS)
            cameraColorControls
            #endif
            #if os(macOS)
            if selectedCamera == nil {
                SiteRobotCameraHost(model: robots, height: min(210, size.height * 0.3))
            }
            #endif
            if placingCamera != nil {
                Label("Tap the model where the camera is mounted.", systemImage: "hand.tap")
                    .font(.callout)
                Button("Cancel Placement") { placingCamera = nil; pendingCamera = nil }
            } else {
                if let camera = current.site.cameras.first(where: { $0.id == selectedCamera }),
                   onGateway(camera),
                   feeds.contains(where: { $0.id == camera.sourceID }) {
                    HStack {
                        Text(SiteCameraLabel(name: camera.name, sourceID: camera.sourceID).title).font(.headline).lineLimit(2)
                        Spacer()
                        Button { selectedCamera = nil } label: { Image(systemName: "xmark.circle.fill") }
                            .buttonStyle(.plain).accessibilityLabel("Close camera preview")
                    }
                    preview(camera.sourceID)
                        .frame(height: min(200, size.height * 0.3))
                        .clipShape(RoundedRectangle(cornerRadius: 10))
                    Text(geometryDescription(camera, now: Date())).font(.caption)
                        .fixedSize(horizontal: false, vertical: true)
                    if cameraColorEnabled {
                        Text(colorStatus(camera)).font(.caption).foregroundStyle(.secondary)
                    }
                    if let status = alignmentStatuses[camera.sourceID] {
                        Text(status).font(.caption).foregroundStyle(.secondary)
                    }
                    if calibrate != nil {
                        Button("Calibrate and Align", systemImage: "viewfinder") { beginAlignment(camera) }
                            .buttonStyle(.borderedProminent)
                            .disabled(busy)
                    }
                    if let lens = cameraMetadata(camera)?.lens {
                        Text("\(lens.width) × \(lens.height) · \(lens.captureMode)")
                            .font(.caption2).foregroundStyle(.secondary)
                    }
                }
                HStack {
                    Button { camerasExpanded.toggle() } label: {
                        Label {
                            Text("Cameras · \(current.site.cameras.count)")
                        } icon: {
                            Image(systemName: "chevron.right")
                                .rotationEffect(.degrees(camerasExpanded ? 90 : 0))
                        }.font(.headline)
                    }.buttonStyle(.plain)
                        .accessibilityValue(camerasExpanded ? "Expanded" : "Collapsed")
                    Spacer()
                    Button("Add Camera", systemImage: "plus") {
                        #if os(iOS)
                        pendingMobileCameraAction = .add; showingMobileCameras = false
                        #else
                        choosingCamera = true
                        #endif
                    }
                        .buttonStyle(.borderedProminent)
                        .disabled(availableFeeds.isEmpty || gatewayID.isEmpty || busy)
                }
                if camerasExpanded {
                    ScrollView {
                        cameraControls(current)
                            .fixedSize(horizontal: false, vertical: true)
                            .onGeometryChange(for: CGFloat.self) { $0.size.height } action: { cameraListHeight = $0 }
                    }
                    .frame(height: min(cameraListHeight ?? CGFloat(max(1, current.site.cameras.count)) * 100,
                                       max(75, min(240, size.height * 0.3))))
                    .transition(.opacity)
                }
                Text("Cones show viewing direction, not measured coverage.")
                    .font(.caption2).foregroundStyle(.secondary)
            }
        }
        #if os(macOS)
        .animation(reduceMotion ? nil : .easeInOut(duration: 0.22), value: camerasExpanded)
        .padding(14)
        .frame(width: min(380, max(0, size.width - 32)), alignment: .leading)
        .background(.regularMaterial, in: RoundedRectangle(cornerRadius: 16))
        #else
        .frame(maxWidth: .infinity, alignment: .leading)
        .transaction { $0.animation = nil }
        #endif
    }

    private var availableFeeds: [SiteFeed] {
        feeds.filter { feed in
            !(current?.site.cameras.contains { onGateway($0) && $0.sourceID == feed.id } ?? false)
        }
    }

    private func cameraControls(_ current: StoredSite) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            TimelineView(.periodic(from: .now, by: 1)) { context in
                VStack(alignment: .leading, spacing: 8) {
                    ForEach(current.site.cameras) { camera in
                        cameraRow(camera, now: context.date)
                    }
                }
            }
            if current.site.cameras.isEmpty {
                Text(feeds.isEmpty ? (notices.isEmpty ? "No cameras available. Use Pair Cameras to connect one." : "Camera alignment is unavailable. See the gateway notice above.") : "Add a camera to locate it automatically using its live view and the room scan.")
                    .font(.callout).foregroundStyle(.secondary)
                    .frame(maxWidth: .infinity, alignment: .leading).padding(.vertical, 8)
            }
        }
    }

    private func cameraRow(_ camera: SiteCamera, now: Date) -> some View {
        let feed = onGateway(camera) ? feeds.first { $0.id == camera.sourceID } : nil
        let label = SiteCameraLabel(name: camera.name, sourceID: camera.sourceID)
        let hasPose = current.map { cameraMetadata(camera)?.pose(in: $0.site, now: now) != nil } ?? false
        let status = camera.position == nil && !hasPose ? "Not positioned" : (feed == nil ? "Unavailable on this gateway" : (feed?.isReceiving(at: now) == true ? "Receiving video" : "Ready to preview"))
        return HStack(spacing: 10) {
            Button { selectedCamera = camera.id; camerasExpanded = false } label: {
                HStack(spacing: 10) {
                    Image(systemName: "video").frame(width: 26)
                    VStack(alignment: .leading, spacing: 4) {
                        Text(label.title).font(.subheadline.weight(.medium)).lineLimit(2)
                        Text(status).font(.caption).foregroundStyle(.secondary)
                        if let alignmentStatus = alignmentStatuses[camera.sourceID] {
                            Text(alignmentStatus).font(.caption).foregroundStyle(.secondary)
                                .fixedSize(horizontal: false, vertical: true)
                        }
                        Text(geometryDescription(camera, now: now)).font(.caption).foregroundStyle(.secondary)
                            .fixedSize(horizontal: false, vertical: true)
                    }.frame(maxWidth: .infinity, alignment: .leading)
                }.contentShape(Rectangle())
            }.buttonStyle(.plain).disabled(feed == nil)
            Menu {
                if calibrate != nil {
                    Button("Calibrate and Align", systemImage: "viewfinder") { beginAlignment(camera) }
                }
                Button(camera.position == nil ? "Place Manually" : "Move Manual Marker", systemImage: "move.3d") {
                    #if os(iOS)
                    pendingMobileCameraAction = .place(camera.id); showingMobileCameras = false
                    #else
                    pendingCamera = nil; placingCamera = camera.id
                    #endif
                }.disabled(hasPose)
                Button("Remove from Model", systemImage: "minus.circle", role: .destructive) {
                    actions.submit {
                        if await edit({ $0.cameras.removeAll { $0.id == camera.id } }), selectedCamera == camera.id {
                            selectedCamera = nil
                        }
                    }
                }
            } label: { Image(systemName: "ellipsis") }
                .buttonStyle(.plain).fixedSize().disabled(busy)
                .accessibilityLabel("Actions for \(label.title)")
        }.padding(10).background(.quaternary.opacity(0.4), in: RoundedRectangle(cornerRadius: 10))
    }

    private func beginAlignment(_ camera: SiteCamera) {
        guard let current else { return }
        #if os(iOS)
        if showingMobileCameras {
            pendingMobileCameraAction = .align(camera)
            showingMobileCameras = false
            return
        }
        if onGateway(camera), current.visualMapURL == nil, SiteRoomCapture.isSupported {
            alignmentScanCamera = camera
            scanning = true
            return
        }
        #endif
        calibrate?(current, camera)
    }

    private func cameraMetadata(_ camera: SiteCamera) -> SiteCameraMetadata? {
        guard onGateway(camera) else { return nil }
        let matches = metadata.filter { $0.sourceID == camera.sourceID }
        return matches.count == 1 ? matches.first : nil
    }

    private func cameraGeometry(_ site: Site) -> [UUID: SiteCameraGeometry] {
        var result: [UUID: SiteCameraGeometry] = [:]
        for camera in site.cameras {
            guard let record = cameraMetadata(camera), let pose = record.pose(in: site, now: Date()) else { continue }
            result[camera.id] = SiteCameraGeometry(pose: pose, lens: record.lens)
        }
        return result
    }

    private func geometryDescription(_ camera: SiteCamera, now: Date) -> String {
        guard let site = current?.site, let record = cameraMetadata(camera) else { return "Needs camera alignment and lens calibration" }
        var parts: [String] = []
        if let lens = record.lens, let horizontal = lens.horizontalFOV, let vertical = lens.verticalFOV {
            parts.append(String(format: "%@ FOV %.0f° × %.0f°", lens.measured ? "Calibrated" : "Estimated", horizontal, vertical))
        } else { parts.append("Lens calibration needed for FOV") }
        if let pose = record.pose(in: site, now: now) {
            parts.append(String(format: "%@Yaw %.0f° · Pitch %.0f°", record.lens?.measured == false ? "Estimated " : "", pose.yaw, pose.pitch))
        } else { parts.append("Not aligned to this model") }
        return parts.joined(separator: "\n")
    }

    private var removedCaptures: some View {
        NavigationStack {
            List {
                ForEach(removedSites) { capture in
                    HStack {
                        VStack(alignment: .leading, spacing: 4) {
                            Text(capture.site.name).font(.headline)
                            Text("\(capture.site.cameras.count) camera placements").font(.caption).foregroundStyle(.secondary)
                        }
                        Spacer()
                        Button("Restore") { actions.submit { await restoreCapture(capture.id) } }.disabled(busy)
                    }.padding(.vertical, 6)
                }
            }
            .overlay { if removedSites.isEmpty { ContentUnavailableView("No removed captures", systemImage: "trash") } }
            .navigationTitle("Recently Removed")
            .toolbar { ToolbarItem(placement: .confirmationAction) { Button("Done") { showingRemoved = false } } }
        }
        #if os(macOS)
        .frame(width: 480, height: 380)
        #endif
    }

    private func removeCapture(_ capture: StoredSite) async {
        guard !busy, let store else { return }
        busy = true
        defer { busy = false }
        do {
            try await store.remove(id: capture.id)
            sites.removeAll { $0.id == capture.id }
            removedSites = try await store.removed()
            if selectedSite == capture.id {
                selectedSite = sites.first?.id
                selectedCamera = nil; placingCamera = nil; pendingCamera = nil; setupPreviewID = nil
            }
        } catch { report(error, message: "The capture could not be removed. Try again.") }
    }

    private func restoreCapture(_ id: UUID) async {
        guard !busy, let store else { return }
        busy = true
        defer { busy = false }
        do {
            let restored = try await store.restore(id: id)
            sites.append(restored)
            removedSites.removeAll { $0.id == id }
            selectedSite = id
            selectedCamera = nil; placingCamera = nil; pendingCamera = nil
            showingRemoved = false
        } catch { report(error, message: "The capture could not be restored. Try again.") }
    }

    private func load() async {
        loading = true
        loadError = false
        defer { loading = false }
        do {
            let opened = try SiteStore.applicationStore()
            sites = try await opened.list()
            removedSites = try await opened.removed()
            store = opened
            if selectedSite == nil { selectedSite = sites.first?.id }
        } catch {
            loadError = true
            report(
                error,
                message: "Your saved sites could not be read. Try opening them again.",
                loading: true
            )
        }
    }

    private func importModel(_ url: URL, scoped: Bool, alignmentCamera: SiteCamera? = nil) async {
        guard !busy, let store else { return }
        busy = true
        let access = scoped && url.startAccessingSecurityScopedResource()
        defer {
            if access { url.stopAccessingSecurityScopedResource() }
            busy = false
        }
        do {
            var created: StoredSite
            if url.pathExtension.lowercased() == "wendysite" {
                let values = try url.resourceValues(forKeys: [
                    .fileSizeKey, .isRegularFileKey, .isSymbolicLinkKey,
                ])
                guard values.isRegularFile == true, values.isSymbolicLink != true,
                    let size = values.fileSize, size <= SiteArchive.maximumBytes
                else { throw SiteError.invalidSite }
                let archive = try SiteArchive(data: Data(contentsOf: url))
                let temporary = FileManager.default.temporaryDirectory.appendingPathComponent(
                    UUID().uuidString + ".usdz"
                )
                defer { try? FileManager.default.removeItem(at: temporary) }
                try archive.model.write(to: temporary)
                _ = try SCNScene(
                    url: temporary,
                    options: [
                        .checkConsistency: true, .convertToYUp: true, .convertUnitsToMeters: 1.0,
                    ]
                )
                created = try await store.importArchive(archive)
            } else {
                // Fail before saving if the USDZ is not a renderable scene.
                try SiteStore.validateModel(url)
                _ = try SCNScene(
                    url: url,
                    options: [
                        .checkConsistency: true, .convertToYUp: true, .convertUnitsToMeters: 1.0,
                    ]
                )
                created = try await store.create(
                    name: url.deletingPathExtension().lastPathComponent,
                    model: url
                )
            }
            if let camera = alignmentCamera, created.visualMapURL != nil {
                var placed = camera
                placed.position = nil
                created.site.cameras.append(placed)
                try await store.save(created.site)
                alignmentScanCamera = nil
                if scanPresentationActive { pendingAlignment = (created, placed) }
                else { calibrate?(created, placed) }
            }
            sites.append(created)
            selectedSite = created.id
            selectedCamera = nil
            placingCamera = nil
        } catch {
            report(
                error,
                message: (error as? SiteError)?.errorDescription
                    ?? "The model could not be imported. Choose a valid USDZ file and try again."
            )
        }
    }

    private func exportSite(_ id: UUID) async {
        guard !busy, let store else { return }
        busy = true
        defer { busy = false }
        do {
            document = SiteFileDocument(data: try await store.archive(id: id).data())
            exporting = true
        } catch {
            report(error, message: "The site could not be exported. Try opening the site again.")
        }
    }

    @discardableResult
    private func edit(_ change: (inout Site) -> Void) async -> Bool {
        guard !busy, let store, var site = current?.site else { return false }
        busy = true
        defer { busy = false }
        change(&site)
        do {
            try await store.save(site)
            if let index = sites.firstIndex(where: { $0.id == site.id }) {
                sites[index].site = site
            }
            return true
        } catch {
            report(error, message: "Your changes could not be saved. Try again.")
            return false
        }
    }
    private func report(_ cause: any Error, message: String, loading: Bool = false) {
        if cause is CancellationError { return }
        underlyingError = cause
        error = message
        if let onFailure {
            onFailure(cause, loading)
        } else {
            Logger(subsystem: "dev.wendy.SiteTwin", category: "Sites").error(
                "Site operation failed: \(String(describing: cause), privacy: .private)"
            )
        }
    }

}

private struct SiteFileDocument: FileDocument {
    static var readableContentTypes: [UTType] { [.data] }
    var data: Data
    init(data: Data) { self.data = data }
    init(configuration: ReadConfiguration) throws {
        guard let contents = configuration.file.regularFileContents,
            contents.count <= SiteArchive.maximumBytes
        else { throw SiteError.invalidSite }
        _ = try SiteArchive(data: contents)
        data = contents
    }
    func fileWrapper(configuration: WriteConfiguration) throws -> FileWrapper {
        FileWrapper(regularFileWithContents: data)
    }
}
