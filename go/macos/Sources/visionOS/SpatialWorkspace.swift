import SwiftUI
import UniformTypeIdentifiers
import QuartzCore

private enum SpatialSection: String, CaseIterable {
    case overview = "Overview", devices = "Devices", space = "Space", replay = "Replay"
    var symbol: String {
        switch self { case .overview: "square.grid.2x2"; case .devices: "cpu"; case .space: "cube.transparent"; case .replay: "clock.arrow.circlepath" }
    }
}

struct SpatialWorkspace: View {
    @Bindable var model: SpatialModel
    @State private var actions = AsyncActions()
    @State private var section = SpatialSection.overview
    @State private var sheet: SpatialSheet?
    @State private var pendingImport: URL?
    @State private var address = ""
    @State private var search = ""
    @State private var openingAR = false
    @State private var preparingImport = false
    @State private var scanName = "My room"
    @State private var scanAfterOpeningAR = false
    @Environment(\.openWindow) private var openWindow
    @Environment(\.openImmersiveSpace) private var openImmersiveSpace
    @Environment(\.dismissImmersiveSpace) private var dismissImmersiveSpace
    @Environment(\.openURL) private var openURL
    @Environment(\.scenePhase) private var scenePhase

    var body: some View {
        VStack(spacing: 0) {
            header
            if let error = model.error {
                HStack(alignment: .top, spacing: 16) {
                    Image(systemName: "exclamationmark.circle").foregroundStyle(.orange)
                    ScrollView { Text(error).font(.subheadline).textSelection(.enabled).frame(maxWidth: .infinity, alignment: .leading) }
                        .frame(maxHeight: 110)
                    if model.connectionNeedsLogin {
                        Button("Sign in") { actions.submit { await model.beginLogin() } }
                    }
                    Button { model.error = nil } label: { Image(systemName: "xmark") }.accessibilityLabel("Dismiss message")
                }.padding(18).background(.orange.opacity(0.08), in: RoundedRectangle(cornerRadius: 20))
                    .padding(.horizontal, 28).padding(.bottom, 12)
            }
            if let operation = model.roomOperation {
                HStack { ProgressView(); Text(operation); Spacer() }
                    .padding(.horizontal, 28).padding(.bottom, 12)
            } else if let status = model.roomStatus {
                HStack {
                    Text(status).font(.callout)
                    Spacer()
                    Button("Dismiss") { model.roomStatus = nil }
                }.padding(.horizontal, 28).padding(.bottom, 12)
            }
            TabView(selection: $section) {
                page { overview }.tabItem { Label("Overview", systemImage: SpatialSection.overview.symbol) }.tag(SpatialSection.overview)
                page { devices }.tabItem { Label("Devices", systemImage: SpatialSection.devices.symbol) }.tag(SpatialSection.devices)
                page { space }.tabItem { Label("Space", systemImage: SpatialSection.space.symbol) }.tag(SpatialSection.space)
                page { replay }.tabItem { Label("Replay", systemImage: SpatialSection.replay.symbol) }.tag(SpatialSection.replay)
            }
            if model.preview != nil { callConfirmation.padding(.horizontal, 28).padding(.bottom, 18) }
            if model.selectingAlignment || model.registrationStep != nil { alignmentGuide.padding(.horizontal, 28).padding(.bottom, 18) }
        }
        .tint(SpatialStyle.amber)
        .frame(minWidth: 880, minHeight: 650)
        .ornament(attachmentAnchor: .scene(.bottom)) { controlBar }
        .sheet(item: $sheet, onDismiss: {
            if let url = pendingImport {
                pendingImport = nil
                actions.submit { await importSelected(url) }
            }
        }) { presentation in
            switch presentation.kind {
            case .connection: connectionSheet
            case .importFile:
                SpatialDocumentSheet(exportURL: nil, preparing: preparingImport) { url in
                    guard sheet?.id == presentation.id else { return }
                    guard let url else { sheet = nil; return }
                    preparingImport = true
                    actions.submit {
                        defer { preparingImport = false }
                        do {
                            let staged = try await SpatialClient.work { try SpatialFileImport.stage(url) }
                            guard !Task.isCancelled, sheet?.id == presentation.id else {
                                SpatialFileImport.discardStaged(staged); return
                            }
                            pendingImport = staged; sheet = nil
                        } catch { model.error = error.localizedDescription; sheet = nil }
                    }
                }
            case .exportTwin(let archive):
                SpatialDocumentSheet(exportURL: archive) { url in
                    guard sheet?.id == presentation.id else { return }
                    if url != nil { model.roomStatus = "Twin exported to Files." }
                    sheet = nil
                }
            }
        }
        .onOpenURL { url in
            if sheet != nil { pendingImport = url; sheet = nil }
            else { actions.submit { await importSelected(url) } }
        }
        .task { await actions.run() }
        .task { await model.boot() }
        .task(id: model.site?.modelURL) { await model.loadSiteGeometry() }
        .task { await SpatialAssets.load() }
        .task(id: "\(model.handle)-\(model.active)") { await model.pollRobot(model.handle) }
        .task(id: "search-\(model.handle)-\(model.active)") { await model.robotSearch.run(model, owned: model.handle) }
        .modifier(RobotSearchPrompt(presented: Binding(
            get: { model.robotSearch.showPrompt }, set: { model.robotSearch.showPrompt = $0 }
        ), canStart: model.robotSearch.canStart(model), start: { model.robotSearch.start(model) },
           dismiss: model.robotSearch.dismiss))
        .task(id: "\(model.session)-\(model.active)") { await model.pollTelemetry(model.session) }
        .task(id: model.initialized) {
            guard model.initialized else { return }
            while !Task.isCancelled {
                if let events = try? await SpatialClient.events() {
                    for (kind, text) in events {
                        if kind == 1, let url = URL(string: text), url.scheme == "https" { model.loginURL = url; openURL(url) }
                        if kind == 2 { await model.loginCompleted(); await model.discover(cloud: true) }
                        if kind == 3 { model.error = text }
                    }
                }
                do { try await Task.sleep(for: .milliseconds(100)) } catch { break }
            }
        }
        .onChange(of: scenePhase) { _, phase in
            model.active = phase == .active
            if phase != .active { actions.submit { await model.deactivate() } }
            else { actions.submit { await model.resumeAfterLogin() } }
        }
        .onChange(of: model.tracking.tracked) { _, tracked in
            if tracked && scanAfterOpeningAR { scanAfterOpeningAR = false; actions.submit { await model.startScan() } }
        }
        .onChange(of: model.tracking.alignmentInvalidated) { _, _ in
            model.alignedFrame = nil; model.preview = nil; model.registration = nil; model.registrationStep = nil
            actions.submit { await model.invalidateAlignment() }
        }
        .onAppear { model.active = scenePhase == .active }
        .onDisappear { model.active = false }
    }

    private var header: some View {
        HStack(spacing: 14) {
            Text("w").font(.system(size: 31, weight: .semibold, design: .rounded)).foregroundStyle(SpatialStyle.amber)
                .frame(width: 46, height: 46).background(SpatialStyle.amber.opacity(0.12), in: RoundedRectangle(cornerRadius: 15))
            Text("Wendy").font(.system(size: 23, weight: .semibold, design: .rounded))
            Text("SPATIAL").font(.system(size: 10, weight: .medium)).tracking(3).foregroundStyle(.secondary).padding(.leading, 4)
            Spacer()
            if model.busy { ProgressView().controlSize(.small) }
            SpatialStatus(title: model.replay != nil ? "Replay" : model.connected != nil ? "Device connected" : "\(model.devices.count) discovered",
                          color: model.replay != nil ? SpatialStyle.amber : model.connected != nil ? SpatialStyle.cyan : .secondary)
            Button { presentSheet(.connection) } label: { Image(systemName: "plus").frame(width: 44, height: 44) }
                .buttonStyle(.borderless).accessibilityLabel("Add a device")
        }.padding(.horizontal, 30).padding(.top, 22).padding(.bottom, 12)
    }
    private func page<Content: View>(@ViewBuilder content: () -> Content) -> some View {
        ScrollView { VStack(alignment: .leading, spacing: 22) { content() }.padding(28).padding(.bottom, 12) }
    }
    private var overview: some View {
        VStack(alignment: .leading, spacing: 22) {
            HStack(spacing: 16) {
                VStack(alignment: .leading, spacing: 18) {
                    SpatialEyebrow(title: model.robot == nil ? "A new perspective" : "Your robot")
                    Text(model.robot?.name.isEmpty == false ? model.robot!.name : "A place for\nevery device.")
                        .font(.system(size: 43, weight: .medium, design: .rounded)).tracking(-1.5).fixedSize(horizontal: false, vertical: true)
                    Text(model.robot == nil ? "Bring your devices into the room.\nSee their state, ask a question, and act." : "See what Go2 sees. Understand its route.\nCall it over when you're ready.")
                        .font(.system(size: 16)).foregroundStyle(.secondary).lineSpacing(5)
                    Button(model.connected == nil ? "Explore devices" : "Ask about this device", systemImage: model.connected == nil ? "arrow.right" : "waveform") {
                        if model.connected == nil { section = .devices } else { ask("Give me a concise summary of this device and the current spatial observations.") }
                    }.buttonStyle(.borderedProminent).controlSize(.large)
                }.frame(maxWidth: .infinity, alignment: .leading)
                VStack(spacing: 0) {
                    SpatialRobotPreview().frame(width: 370, height: 235)
                    Text("UNITREE GO2  /  MODEL PREVIEW").font(.system(size: 9, weight: .medium)).tracking(2).foregroundStyle(.tertiary)
                }.frame(width: 380)
            }.padding(28)
                .background(RadialGradient(colors: [SpatialStyle.amber.opacity(0.13), .clear], center: .trailing, startRadius: 20, endRadius: 520), in: RoundedRectangle(cornerRadius: 32))
                .overlay(RoundedRectangle(cornerRadius: 32).strokeBorder(.white.opacity(0.08)))

            HStack(alignment: .top, spacing: 18) {
                SpatialCard(accent: SpatialStyle.cyan) {
                    HStack { SpatialEyebrow(title: "Connected device"); Spacer(); Image(systemName: "cpu").foregroundStyle(SpatialStyle.cyan) }
                    Text(model.connected?.name ?? "Choose a gateway").font(.title3.weight(.medium))
                    TimelineView(.periodic(from: .now, by: 1)) { _ in
                        Text(model.connected == nil ? "Your device is the source of live state and local AI." : CACurrentMediaTime() - model.telemetryAt < 8 ? model.telemetry : "Waiting for fresh measurements")
                            .font(.caption).foregroundStyle(.secondary).lineLimit(3).frame(minHeight: 36, alignment: .top)
                    }
                }
                SpatialCard(accent: SpatialStyle.amber) {
                    HStack { SpatialEyebrow(title: "Spatial awareness"); Spacer(); Image(systemName: "viewfinder").foregroundStyle(SpatialStyle.amber) }
                    Text(model.tracking.tracked ? "Your room is tracked" : "Step into your space").font(.title3.weight(.medium))
                    Text(model.tracking.tracked ? "Place device labels or align a twin to locate yourself." : "Open AR to anchor devices and robot observations around you.")
                        .font(.caption).foregroundStyle(.secondary).frame(minHeight: 36, alignment: .top)
                }
            }
            if model.connected != nil { robotPanel }
            if model.handle != 0, model.replay == nil { SpatialCard { SpatialVideoView(model: model) } }
            HStack(spacing: 18) {
                SpatialActionTile(title: "Ask Wendy", detail: "A conversation with spatial context", symbol: "waveform") { ask("") }
                SpatialActionTile(title: "Your digital twin", detail: model.site?.name ?? "Import a scan of your space", symbol: "cube.transparent", color: SpatialStyle.cyan) { section = .space }
            }
        }
    }
    private var devices: some View {
        VStack(alignment: .leading, spacing: 24) {
            HStack {
                SpatialSectionTitle(title: "Your devices", subtitle: "Select a gateway, or pin a device in the room.")
                Spacer()
                if !model.labels.isEmpty {
                    Button("Clear pins", systemImage: "pin.slash", role: .destructive) { model.labels.removeAll() }
                }
                Button("Refresh", systemImage: "arrow.clockwise") { actions.submit { await model.discover(cloud: false) } }.disabled(model.busy)
                Button("Cloud", systemImage: "cloud") { actions.submit { await model.discover(cloud: true) } }.disabled(model.busy)
            }
            TextField("Find a device", text: $search).textFieldStyle(.roundedBorder)
            if model.devices.isEmpty {
                SpatialCard {
                    Image(systemName: "dot.radiowaves.left.and.right").font(.system(size: 42, weight: .ultraLight)).foregroundStyle(SpatialStyle.amber)
                    Text(model.busy ? "Looking for your devices" : "Let's connect your first device").font(.title2)
                    Text("Find devices on your network, or sign in to Wendy Cloud.").foregroundStyle(.secondary)
                    Button("Connect a device") { presentSheet(.connection) }.buttonStyle(.borderedProminent)
                }
            }
            LazyVGrid(columns: [GridItem(.adaptive(minimum: 250), spacing: 18)], spacing: 18) {
                ForEach(model.devices.filter { search.isEmpty || $0.name.localizedCaseInsensitiveContains(search) }) { device in
                    SpatialCard(accent: model.connected?.id == device.id ? SpatialStyle.cyan : .white) {
                        HStack {
                            Image(systemName: device.lite ? "cpu" : "server.rack").font(.system(size: 28, weight: .light))
                                .foregroundStyle(model.connected?.id == device.id ? SpatialStyle.cyan : SpatialStyle.amber)
                            Spacer()
                            if model.connected?.id == device.id { SpatialStatus(title: "Connected", color: SpatialStyle.cyan) }
                        }
                        VStack(alignment: .leading, spacing: 5) {
                            Text(device.name).font(.headline).lineLimit(2).frame(minHeight: 44, alignment: .top)
                            Text(device.lite ? "Wendy Lite" : "Wendy gateway").font(.caption).foregroundStyle(.secondary)
                        }
                        HStack(spacing: 14) {
                            Button(model.connected?.id == device.id ? "Inspect" : "Connect") {
                                if model.connected?.id == device.id { section = .overview }
                                else { actions.submit { await model.connect(device); if model.connected?.id == device.id { section = .overview } } }
                            }.buttonStyle(.borderedProminent).disabled(model.busy)
                            Button("Pin", systemImage: "pin") { model.placeLabel(device) }.disabled(!model.tracking.tracked)
                        }
                    }
                }
            }
        }
    }
    private var robotPanel: some View {
        SpatialCard {
            HStack {
                SpatialSectionTitle(title: model.robot?.name ?? "Robot dogs", subtitle: model.robot == nil ? "Paired robots on your gateway" : "Live observations and supervised movement")
                Spacer()
                if model.handle != 0 { Button("Ask why", systemImage: "waveform") { ask("Explain the current robot state. If it cannot move, explain what evidence is missing or blocking it.") } }
            }
            if model.robots.isEmpty { Text("No paired Go2 found. Pair a robot in Wendy on Mac or iPhone.").foregroundStyle(.secondary) }
            ForEach(model.robots) { robot in
                HStack(spacing: 18) {
                    Image(systemName: "pawprint.fill").foregroundStyle(SpatialStyle.amber)
                    Text(robot.name.isEmpty ? robot.id : robot.name).font(.headline)
                    Spacer()
                    SpatialStatus(title: robot.connected ? "Available" : "Offline", color: robot.connected ? SpatialStyle.cyan : .secondary)
                    Button(model.robot?.id == robot.id ? "Reconnect" : "Select") { actions.submit { await model.selectRobot(robot) } }
                        .disabled(model.busy || !robot.connected)
                }
            }
            if let reading = model.reading {
                Label(reading.message, systemImage: reading.ready ? "checkmark.circle" : "info.circle").foregroundStyle(.secondary)
                if !reading.warning.isEmpty { Label(reading.warning, systemImage: "exclamationmark.triangle").foregroundStyle(.orange) }
                if let timing = reading.timing {
                    Text(timing.supervisionMessage).font(.caption).foregroundStyle(.secondary)
                }
                if let message = reading.floorPolicy?.supervisionMessage {
                    Text(message).font(.caption).foregroundStyle(.secondary)
                }
                SiteRobotSupervisionControls(timing: reading.timing, floorPolicy: reading.floorPolicy) { setting, enabled in
                    actions.submit { await model.setRobotSetting(setting, enabled: enabled) }
                }.disabled(model.busy || model.replay != nil || reading.disconnected)
                if model.alignedFrame != model.tracking.frameID {
                    Text(model.robotSearch.message).font(.callout).foregroundStyle(.secondary)
                    RobotRotationProgress(state: model.robotSearch.state, canRetry: model.robotSearch.canStart(model),
                        canPlace: model.alignmentBlocker == nil,
                        retry: { model.robotSearch.request(model) },
                        place: { actions.submit { await model.beginAlignment() } },
                        stop: { actions.submit { await model.stop() } })
                    if model.robotSearch.candidate != nil {
                        HStack {
                            Button("Confirm robot position") { actions.submit { await model.robotSearch.confirm(model) } }
                                .buttonStyle(.borderedProminent).disabled(model.robotSearch.working)
                            Button("Place manually") { actions.submit { await model.beginAlignment() } }
                        }
                    } else {
                        Button("Find Go2 in this room", systemImage: "arrow.trianglehead.2.clockwise.rotate.90") {
                            model.robotSearch.request(model)
                        }.disabled(!model.robotSearch.canStart(model))
                    }
                }
                if let blocker = model.alignmentBlocker {
                    Label(blocker, systemImage: "scope").font(.callout).foregroundStyle(.secondary)
                }
                HStack(spacing: 18) {
                    Button("Align in AR", systemImage: "scope") { actions.submit { await model.beginAlignment() } }
                        .disabled(model.busy || model.alignmentBlocker != nil)
                    Button("Come here", systemImage: "figure.stand") { actions.submit { await model.callRobot() } }
                        .buttonStyle(.borderedProminent).disabled(model.busy || !model.canPreview)
                    Text("Preview first. Stops short of you.").font(.caption).foregroundStyle(.secondary)
                }
            }
        }
    }
    private var space: some View {
        VStack(alignment: .leading, spacing: 22) {
            HStack {
                SpatialSectionTitle(title: "Your space", subtitle: "A room-scale view, a tabletop twin, and your place within it.")
                Spacer()
                Button("Import", systemImage: "square.and.arrow.down") { presentSheet(.importFile) }
            }
            SpatialCard(accent: SpatialStyle.cyan) {
                SpatialEyebrow(title: "Scan with Vision Pro")
                TextField("Room name", text: $scanName).font(.title3).textFieldStyle(.roundedBorder)
                Text(model.tracking.scan.message).foregroundStyle(.secondary)
                HStack(spacing: 18) {
                    if model.tracking.scan.capturing {
                        Button("Pause scan", systemImage: "pause") { model.tracking.scan.pause() }
                    } else {
                        Button("Scan room", systemImage: "viewfinder") {
                            if model.tracking.tracked { actions.submit { await model.startScan() } }
                            else if !model.immersive { scanAfterOpeningAR = true; toggleAR() }
                            else { model.error = model.tracking.message }
                        }.buttonStyle(.borderedProminent).disabled(model.tracking.scan.saving)
                    }
                    Button("Save twin", systemImage: "square.and.arrow.down") {
                        actions.submit { if await model.saveScan(name: scanName) { openWindow(id: "tabletop") } }
                    }.disabled(!model.tracking.scan.canSave || model.changingSite)
                    if model.tracking.scan.saving { ProgressView().controlSize(.small) }
                    Text("\(model.tracking.scan.triangleCount.formatted()) triangles").font(.caption.monospacedDigit())
                    if let url = model.scanExportURL { ShareLink("Share twin", item: url) }
                }
                Text("Captures visible geometry, not photo textures. Scan coverage is not a guarantee of safe robot clearance.")
                    .font(.caption).foregroundStyle(.secondary)
            }
            SpatialCard(accent: SpatialStyle.cyan) {
                HStack(alignment: .top, spacing: 24) {
                    Image(systemName: "square.3.layers.3d").font(.system(size: 54, weight: .ultraLight)).foregroundStyle(SpatialStyle.cyan).frame(width: 90, height: 90)
                    VStack(alignment: .leading, spacing: 12) {
                        SpatialEyebrow(title: "Digital twin")
                        Text(model.site?.name ?? "Bring your room into view").font(.system(size: 30, weight: .medium, design: .rounded))
                        Text(model.site == nil ? "Import a Wendy site or USDZ scan. Explore it at tabletop scale, then align it to show your live position." : "Match two floor points to connect this model to your surroundings. Your position and facing direction then follow inside-out tracking.")
                            .foregroundStyle(.secondary).lineSpacing(4)
                        HStack(spacing: 18) {
                            if model.site == nil { Button("Import a room") { presentSheet(.importFile) }.buttonStyle(.borderedProminent) }
                            else {
                                Button("Open tabletop") { openWindow(id: "tabletop") }.buttonStyle(.borderedProminent)
                                Button("Export to Files", systemImage: "square.and.arrow.up") {
                                    actions.submit {
                                        if let url = await model.prepareTwinExport() { presentSheet(.exportTwin(url)) }
                                    }
                                }.disabled(model.changingSite || openingAR)
                                Button(model.tracking.tracked ? "Locate me" : "Open AR to locate me", systemImage: "location") {
                                    if model.tracking.tracked { model.beginRegistration(); openWindow(id: "tabletop") }
                                    else if !model.immersive { toggleAR() }
                                    else { model.error = model.tracking.message }
                                }
                                if !model.tracking.tracked {
                                    Text(model.tracking.message).font(.caption).foregroundStyle(.secondary).frame(maxWidth: 230)
                                }
                            }
                        }
                    }
                }
                if model.registeredWearer != nil { SpatialStatus(title: "Your position is live · Manually aligned", color: SpatialStyle.cyan) }
            }
            HStack(spacing: 18) {
                SpatialActionTile(title: model.immersive ? "AR is open" : "Open AR overlays", detail: "Anchor devices in your surroundings", symbol: "viewfinder") { toggleAR() }
                SpatialActionTile(title: "Ask about your space", detail: "Share the current alignment and state", symbol: "waveform", color: SpatialStyle.cyan) { ask("Explain the current room alignment and what I can inspect in this spatial workspace.") }
            }
            if model.labels.isEmpty {
                Text("To add a label, open AR and choose Pin on a device. Drag its marker into position.").font(.subheadline).foregroundStyle(.secondary).padding(6)
            } else {
                SpatialCard {
                    SpatialEyebrow(title: "Placed in this room")
                    ForEach(model.labels) { label in
                        HStack(spacing: 18) {
                            Image(systemName: label.lite ? "cpu" : "server.rack").foregroundStyle(SpatialStyle.cyan)
                            VStack(alignment: .leading) { Text(label.name).font(.headline); Text("Manual placement · This AR session").font(.caption).foregroundStyle(.secondary) }
                            Spacer()
                            Button("Remove", systemImage: "xmark", role: .destructive) { model.labels.removeAll { $0.id == label.id } }.labelStyle(.iconOnly)
                        }
                    }
                }
            }
        }
    }
    private var replay: some View {
        VStack(alignment: .leading, spacing: 22) {
            HStack {
                SpatialSectionTitle(title: "Understand the moment", subtitle: "Scrub through robot observations and inspect what changed.")
                Spacer()
                Button("Import", systemImage: "square.and.arrow.down") { presentSheet(.importFile) }
            }
            SpatialCard(accent: SpatialStyle.amber) {
                HStack {
                    VStack(alignment: .leading, spacing: 6) {
                        SpatialEyebrow(title: model.recording ? "Recording" : model.replay == nil ? "Observation capture" : "Playback")
                        Text(model.replay?.robot ?? model.robot?.name ?? "Record a robot session").font(.title2.weight(.medium))
                    }
                    Spacer()
                    SpatialStatus(title: "\(model.replay?.samples.count ?? model.samples.count) samples", color: model.recording ? .red : SpatialStyle.amber)
                }
                SpatialTimeline(samples: model.replay?.samples ?? model.samples, index: model.replayIndex).frame(height: 90)
                if let replay = model.replay {
                    Slider(value: $model.replayIndex, in: 0...Double(max(1, replay.samples.count - 1)), step: 1)
                        .accessibilityLabel("Replay position")
                    HStack {
                        Text(String(format: "%.1f seconds", replay.samples[min(Int(model.replayIndex), replay.samples.count - 1)].elapsed)).monospacedDigit()
                        Spacer()
                        Button("Ask about this moment", systemImage: "waveform") { ask("Explain this recorded moment using the captured status, route and obstacle observations. Distinguish recorded evidence from current device state.") }
                    }
                    Text(model.displayedReading?.message ?? "").foregroundStyle(.secondary)
                } else {
                    Text("Captures pose, routes, obstacles and controller messages for up to three minutes. Video is not recorded.").font(.subheadline).foregroundStyle(.secondary)
                }
                HStack(spacing: 18) {
                    Button(model.recording ? "Finish capture" : "Record", systemImage: model.recording ? "stop.circle" : "record.circle") {
                        if model.recording { model.recording = false } else { model.startRecording() }
                    }.buttonStyle(.borderedProminent).disabled(model.handle == 0 || model.replay != nil)
                    Button("Replay", systemImage: "play") { actions.submit { await model.replayCapture(); openWindow(id: "tabletop") } }.disabled(model.samples.isEmpty)
                    Menu {
                        Button("Prepare export") { actions.submit { await model.exportCapture() } }
                        if let url = model.exportURL { ShareLink(item: url) }
                    } label: { Label("Export", systemImage: "square.and.arrow.up") }.disabled(model.samples.isEmpty && model.replay == nil)
                    if model.replay != nil { Button("Return to live") { model.replay = nil; model.reading = nil } }
                }
                if let url = model.exportURL { ShareLink("Share recording", item: url) }
            }
        }
    }
    private var controlBar: some View {
        HStack(spacing: 8) {
            Button { toggleAR() } label: { Label(model.immersive ? "Close AR" : "Enter AR", systemImage: "viewfinder").padding(.horizontal, 8).frame(height: 44) }.disabled(openingAR)
            Button { openWindow(id: "tabletop") } label: { Label("Tabletop", systemImage: "cube.transparent").padding(.horizontal, 8).frame(height: 44) }
            Divider().frame(height: 26).padding(.horizontal, 4)
            Button { ask("") } label: { Label("Ask Wendy", systemImage: "waveform").foregroundStyle(SpatialStyle.amber).padding(.horizontal, 8).frame(height: 44) }
            if model.handle != 0 {
                Divider().frame(height: 26).padding(.horizontal, 4)
                Button(role: .destructive) { actions.submit { await model.stop() } } label: { Label("Stop", systemImage: "stop.fill").padding(.horizontal, 8).frame(height: 44) }.tint(.red)
            }
        }.buttonStyle(.borderless).padding(10).glassBackgroundEffect(in: Capsule())
    }
    private var callConfirmation: some View {
        TimelineView(.periodic(from: .now, by: 0.2)) { _ in
            HStack(spacing: 20) {
                Image(systemName: "figure.stand").font(.title).foregroundStyle(SpatialStyle.amber)
                VStack(alignment: .leading, spacing: 4) {
                    Text(model.canMove ? "Your route is ready" : "This preview has expired").font(.headline)
                    Text("Stops 1.5 m short of you. Review the route and stay in place.").font(.caption).foregroundStyle(.secondary)
                }
                Spacer()
                Button("Cancel") { actions.submit { await model.stop() } }
                Button("Confirm call") { actions.submit { await model.confirmCall() } }.buttonStyle(.borderedProminent).disabled(!model.canMove || model.busy)
            }.padding(20).background(SpatialStyle.amber.opacity(0.12), in: RoundedRectangle(cornerRadius: 24))
        }
    }
    private var alignmentGuide: some View {
        HStack(spacing: 18) {
            Image(systemName: "scope").font(.title2).foregroundStyle(SpatialStyle.cyan)
            VStack(alignment: .leading, spacing: 4) {
                Text(model.selectingAlignment ? "Align your robot" : "Align your room").font(.headline)
                Text(model.notice).font(.caption).foregroundStyle(.secondary)
                if model.registrationStep == 1 || model.registrationStep == 3 || model.selectingAlignment {
                    Text(model.tracking.floorSelectionMessage).font(.caption)
                        .foregroundStyle(model.tracking.floorTargetCount == 0 ? Color.orange : SpatialStyle.cyan)
                }
            }
            Spacer(minLength: 10)
            if model.selectingAlignment, model.front != nil {
                Button("Confirm") { actions.submit { await model.confirmAlignment() } }.buttonStyle(.borderedProminent).disabled(model.busy)
            }
            if model.registrationStep == 4 { Button("Confirm") { model.confirmRegistration() }.buttonStyle(.borderedProminent) }
            Button("Cancel") { model.registrationStep = nil; model.selectingAlignment = false }
        }.padding(20).background(SpatialStyle.cyan.opacity(0.08), in: RoundedRectangle(cornerRadius: 24))
    }
    private var connectionSheet: some View {
        VStack(alignment: .leading, spacing: 24) {
            HStack { SpatialSectionTitle(title: "Connect your world", subtitle: "Find a device or choose a gateway address."); Spacer(); Button("Done") { sheet = nil } }
            SpatialActionTile(title: "Nearby devices", detail: "Discover Wendy devices on this network", symbol: "dot.radiowaves.left.and.right") {
                sheet = nil; section = .devices; actions.submit { await model.discover(cloud: false) }
            }
            SpatialActionTile(title: model.signedIn ? "Wendy account" : "Sign in to Wendy", detail: "Authenticate with enrolled devices, nearby or in the cloud", symbol: "person.crop.circle", color: SpatialStyle.cyan) {
                sheet = nil
                actions.submit { await model.beginLogin() }
            }
            SharedLoginView(onImport: { await model.loginCompleted() })
            Divider()
            TextField("Gateway address or selector", text: $address).textInputAutocapitalization(.never).autocorrectionDisabled().textFieldStyle(.roundedBorder)
            Button("Connect") {
                let selector = address.trimmingCharacters(in: .whitespacesAndNewlines)
                actions.submit { await model.connect(SpatialDevice(id: selector, name: selector, selector: selector, lite: false)); sheet = nil; section = .overview }
            }.buttonStyle(.borderedProminent).disabled(model.busy || !model.initialized || address.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
        }.padding(32).frame(width: 620)
    }
    private func importSelected(_ url: URL) async {
        defer { SpatialFileImport.discardStaged(url) }
        switch await model.importFile(url) {
        case .room: section = .space; openWindow(id: "tabletop")
        case .recording: section = .replay
        case nil: break
        }
    }
    private func presentSheet(_ kind: SpatialSheet.Kind) {
        guard sheet == nil, !openingAR, !model.changingSite else { return }
        if case .connection = kind { sheet = SpatialSheet(kind: kind); return }
        openingAR = true
        actions.submit {
            defer { openingAR = false }
            // Files is a system presentation. Leave our Full Space first so
            // its browser is not hidden behind the immersive scene.
            if model.immersive {
                await model.invalidateAlignment()
                await dismissImmersiveSpace()
                model.immersive = false
            }
            guard !Task.isCancelled else { return }
            sheet = SpatialSheet(kind: kind)
        }
    }
    private func ask(_ question: String) {
        if !question.isEmpty { model.assistant.prefill(question, session: model.session) }
        openWindow(id: "assistant")
    }
    private func toggleAR() {
        guard !openingAR else { return }
        openingAR = true
        actions.submit {
            defer { openingAR = false }
            if model.immersive { await model.invalidateAlignment(); await dismissImmersiveSpace() }
            else {
                switch await openImmersiveSpace(id: "overlays") {
                case .opened: model.immersive = true
                case .error: scanAfterOpeningAR = false; model.error = "Could not open AR overlays."
                case .userCancelled: scanAfterOpeningAR = false
                @unknown default: scanAfterOpeningAR = false
                }
            }
        }
    }
}

struct SpatialTimeline: View {
    let samples: [SpatialSample]
    let index: Double
    var body: some View {
        Canvas { context, size in
            guard !samples.isEmpty else {
                let line = Path(CGRect(x: 0, y: size.height / 2, width: size.width, height: 1))
                context.fill(line, with: .color(.white.opacity(0.12)))
                return
            }
            let count = min(100, samples.count)
            let stride = size.width / Double(count)
            for column in 0..<count {
                let sample = samples[min(samples.count - 1, column * samples.count / count)]
                let height = sample.reading.moving ? size.height * 0.78 : sample.reading.ready ? size.height * 0.42 : size.height * 0.22
                let rect = CGRect(x: Double(column) * stride, y: (size.height - height) / 2, width: max(2, stride - 3), height: height)
                context.fill(Path(roundedRect: rect, cornerRadius: 3), with: .color(sample.reading.moving ? SpatialStyle.cyan : SpatialStyle.amber.opacity(0.45)))
            }
            let x = size.width * index / Double(max(1, samples.count - 1))
            context.fill(Path(CGRect(x: x, y: 0, width: 2, height: size.height)), with: .color(.white))
        }.accessibilityLabel("Observation timeline. Taller bars indicate movement.")
    }
}
