import SwiftUI

private enum DeviceSegment: String, CaseIterable { case apps = "Apps", cameras = "Cameras", data = "Data", location = "Location", settings = "Settings" }
private enum DeviceTool: String, Identifiable { case metrics = "App activity", twin = "Digital Twin", campaigns = "Campaigns", update = "Agent update", management = "Device management"
    var id: Self { self }
}

struct MobileDeviceView: View {
    @EnvironmentObject private var model: AppModel
    let device: Device
    let openRecordings: () -> Void
    @State private var showingAppearance = false
    @State private var segment = DeviceSegment.apps
    @State private var update: DeviceUpdateSnapshot?
    @State private var showLogs = false
    @State private var showEvents = false
    @State private var tool: DeviceTool?
    @State private var clip: String?
    private var ready: Bool { model.mobileScopeIsCurrent && model.selectedDeviceReady && model.connected?.id == device.id }
    private var site: String? { model.mobilePresence?.0.devices.first { $0.selector == device.selector }?.site.nonEmpty }
    var body: some View {
        Group {
            if ready {
                if model.connectedLite { LiteDeviceView(model: model).id(model.deviceSession).modifier(MobileFloatingChatScrollMargins()) }
                else if segment == .cameras || segment == .settings || segment == .apps {
                    VStack(spacing: 0) {
                        header.padding(.horizontal, 16).padding(.vertical, 8)
                        if segment == .cameras { CameraWallView(model: model).modifier(MobileFloatingChatScrollMargins()) }
                        else if segment == .apps { MobileAppsList(model: model, device: device).refreshable { await model.refreshDetails() } }
                        else { DeviceSettingsView(session: model.deviceSession, deviceName: device.title).id(model.deviceSession).modifier(MobileFloatingChatScrollMargins()) }
                    }
                } else {
                    ScrollView {
                        VStack(alignment: .leading, spacing: 18) {
                            header
                            switch segment {
                            case .apps: MobileAppsList(model: model, device: device)
                            case .data:
                                MobileRecordingSection(model: model, recent: true)
                                Button(action: openRecordings) {
                                    LensRow(title: "All recordings", subtitle: "", symbol: "cylinder.split.1x2")
                                }.buttonStyle(.plain).background(MobileLensStyle.card, in: .rect(cornerRadius: 22))
                            case .location:
                                LensSection(title: "Location") {
                                    LensRow(title: "Home place", subtitle: site ?? "Unassigned", symbol: "building.2", navigates: false)
                                    Divider().padding(.leading, 64)
                                    LensRow(title: "Current place", subtitle: "Not reported by this device", symbol: "mappin", color: .pink, navigates: false)
                                }
                                Text(device.transport == .cloud ? "Remote: reached through Wendy Cloud." : "Nearby: this iPhone can reach the device directly.")
                                    .font(.caption).foregroundStyle(.secondary).padding(.horizontal, 12)
                            case .cameras, .settings: EmptyView()
                            }
                        }.padding(.horizontal, 16).padding(.vertical, 8).frame(maxWidth: 700).frame(maxWidth: .infinity)
                    }.modifier(MobileFloatingChatScrollMargins())
                        .refreshable { await model.refreshDetails() }
                }
            } else { connectionState }
        }.background(MobileLensStyle.background)
            .navigationTitle(device.title).navigationBarTitleDisplayMode(.inline)
            .sheet(isPresented: $showingAppearance) {
                let key = DeviceAppearanceStore.key(account: model.mobileCloudAccount, organization: model.mobileCloudOrganization, device: device)
                MobileDeviceAppearanceEditor(device: device, key: key).id(key)
            }
            .toolbar {
                ToolbarItem(placement: .topBarTrailing) {
                    if ready && !model.connectedLite {
                        CampaignEventButton(session: model.deviceSession, deviceID: device.id, deviceName: device.title, showingLog: $showEvents)
                            .id(model.deviceSession)
                    } else if ready {
                        Button("Logs", systemImage: "text.alignleft", action: openLogs).accessibilityIdentifier("devices.detail.logs")
                    }
                }
                ToolbarItem(placement: .topBarTrailing) {
                    Menu("More device tools", systemImage: "ellipsis.circle") {
                        Button("Customize appearance", systemImage: "paintpalette") { showingAppearance = true }
                            .accessibilityIdentifier("devices.appearance")
                        if ready && !model.connectedLite {
                            Button("Logs", systemImage: "text.alignleft", action: openLogs)
                                .accessibilityIdentifier("devices.detail.logs")
                            Button(updateTitle, systemImage: "info.circle") { tool = .update }
                            Button("App activity", systemImage: "waveform.path.ecg") { tool = .metrics }
                            Button("Digital Twin", systemImage: "cube.transparent") { tool = .twin }
                            Button("Campaigns", systemImage: "record.circle") { tool = .campaigns }
                            Button("Device management", systemImage: "gearshape") { tool = .management }
                        }
                    }
                }
            }
            .task(id: ready ? model.deviceSession : 0) {
                update = nil
                guard ready && !model.connectedLite else { return }
                let captured = model.deviceSession
                repeat {
                    let software = try? await GoClient.shared.onlineUpdate(session: captured, action: 0)
                    guard !Task.isCancelled, ready, captured == model.deviceSession else { return }
                    update = software
                    do { try await Task.sleep(for: .seconds(software?.busy == true ? 5 : 30)) } catch { return }
                } while !Task.isCancelled
            }
            .navigationDestination(isPresented: $showLogs) {
                if ready { DeviceLogsPage(model: model).id(model.deviceSession).modifier(MobileFloatingChatScrollMargins()).accessibilityIdentifier("devices.detail.logPage") }
                else { connectionState.navigationTitle("Logs") }
            }
            .navigationDestination(item: $tool) { destination in
                if ready {
                    switch destination {
                    case .metrics: ActivityView(model: model)
                    case .twin: NativeSiteView(model: model)
                    case .campaigns: CampaignsView(session: model.deviceSession, deviceName: device.title, openClip: { clip = $0 }, showEventLog: { showEvents = true })
                    case .management: DeviceManagementView(session: model.deviceSession, deviceName: device.title)
                    case .update: DeviceInfoView(session: model.deviceSession, deviceName: device.title)
                    }
                } else { connectionState }
            }
            .navigationDestination(item: $clip) { id in
                if ready { DataVisualizerView(session: model.deviceSession, deviceName: device.title, initialEpisode: id) }
            }
    }
    @ViewBuilder private var connectionState: some View {
        if model.mobileScopeIsCurrent && model.selection?.id == device.id {
            DeviceConnectionView(model: model, device: device)
        } else { ProgressView("Connecting to \(device.title)…").frame(maxWidth: .infinity, maxHeight: .infinity) }
    }
    private var updateTitle: String {
        guard let update else { return "Device info" }
        if update.busy {
            if update.phase == "downloading", let fraction = update.downloadFraction { return "Downloading \(Int(fraction * 100))%" }
            return update.phase == "installing" ? "Installing…" : "Reconnecting…"
        }
        return update.available ? "Update" : "Device info"
    }
    private func openLogs() { model.logApp = ""; showLogs = true }
    private var header: some View {
        Picker("Device section", selection: $segment) {
            ForEach(DeviceSegment.allCases, id: \.self) { item in
                Text(item.rawValue).tag(item)
                    .accessibilityIdentifier("device.segment." + item.rawValue)
            }
        }.pickerStyle(.segmented).accessibilityIdentifier("device.sections")
    }
}

private extension String {
    var nonEmpty: String? { isEmpty ? nil : self }
}

private struct LensHero<Actions: View>: View {
    let icon: String
    let color: Color
    let kicker: String
    let title: String
    let subtitle: String
    let chips: [String]
    var device: Device? = nil
    var customize: (() -> Void)? = nil
    @ViewBuilder let actions: Actions
    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            HStack(spacing: 14) {
                if let device, let customize {
                    Button(action: customize) { MobileDeviceAvatar(device: device, size: 54) }
                        .buttonStyle(.plain).accessibilityLabel("Customize device appearance").accessibilityIdentifier("devices.appearance.avatar")
                } else { LensIcon(symbol: icon, color: color, size: 54) }
                VStack(alignment: .leading, spacing: 3) {
                    Text(kicker).font(.caption2.bold()).tracking(0.4).foregroundStyle(color).lineLimit(1)
                    Text(title).font(.title2.bold()).lineLimit(2)
                    Text(subtitle).font(.subheadline).foregroundStyle(.secondary).lineLimit(2)
                }
                Spacer(minLength: 0)
            }
            if !chips.isEmpty {
                ViewThatFits(in: .horizontal) {
                    chipRow
                    ScrollView(.horizontal) { chipRow }.scrollIndicators(.hidden)
                }
            }
            HStack(spacing: 8) { actions }
        }.padding(18).frame(maxWidth: .infinity, alignment: .leading).background(MobileLensStyle.card, in: .rect(cornerRadius: 26))
    }
    private var chipRow: some View {
        HStack(spacing: 6) {
            ForEach(chips, id: \.self) { value in
                Text(value).font(.caption.weight(.semibold)).lineLimit(1).padding(.horizontal, 9).padding(.vertical, 5)
                    .background(.quaternary, in: .capsule)
            }
        }
    }
}

private struct MobileAppsList: View {
    @Environment(\.dynamicTypeSize) private var textSize
    @ObservedObject var model: AppModel
    let device: Device
    @State private var deploying = false
    var body: some View {
        List {
        Section {
            ForEach(model.visibleApps) { app in
                NavigationLink {
                    MobileAppDetail(model: model, device: device, name: app.appName)
                } label: {
                    HStack(spacing: 12) {
                        LensIcon(symbol: "square.grid.2x2", color: app.runningState == .running ? .green : .gray)
                        VStack(alignment: .leading, spacing: 2) {
                            // Prefer wrapping bundle identifiers at a dot instead of splitting a word or version.
                            Text(verbatim: app.appName.replacingOccurrences(of: ".", with: ".\u{200B}"))
                                .font(.body.weight(.semibold)).lineLimit(textSize.isAccessibilitySize ? 3 : 2)
                                .truncationMode(.middle).accessibilityLabel(app.appName)
                            if let reason = app.terminationReason?.nonEmpty, reason.localizedCaseInsensitiveCompare(app.runningState.title) != .orderedSame {
                                Text(reason).font(.caption).foregroundStyle(.secondary).lineLimit(2)
                            }
                            if textSize.isAccessibilitySize { metadata(for: app) }
                        }.frame(maxWidth: .infinity, alignment: .leading)
                        if !textSize.isAccessibilitySize { metadata(for: app) }
                    }.padding(.vertical, 4).contentShape(.rect)
                }.buttonStyle(.plain).accessibilityIdentifier("device.apps.row." + app.appName)
            }
            if let error = model.appsErrorFailure { FailureNote(message: error).padding(16) }
            else if model.visibleApps.isEmpty && model.appsInitialLoading {
                MobileLoadingPlaceholder(label: "Loading apps…").padding(16)
            }
            else if model.visibleApps.isEmpty { Text("No visible apps on this device.").foregroundStyle(.secondary).padding(16) }
            if model.hiddenAppCount > 0 { HiddenAppsFilter(model: model).padding(12) }
        } header: {
            HStack {
                Text("Apps")
                Spacer()
                Button("Add", systemImage: "plus") { deploying = true }
                    .font(.subheadline.weight(.semibold))
                    .textCase(nil)
                    .accessibilityLabel("Deploy an app")
                    .accessibilityIdentifier("device.apps.add")
            }
        }
        }.listStyle(.insetGrouped)
        .modifier(MobileFloatingChatScrollMargins())
        .scrollContentBackground(.hidden)
        .background(MobileLensStyle.background)
        .sheet(isPresented: $deploying) {
            NavigationStack {
                VStack(alignment: .leading, spacing: 12) {
                    Text("Run this command in your project on your developer machine.")
                    Text(verbatim: "wendy run --device '\(device.selector.replacingOccurrences(of: "'", with: "'\\''"))'").font(.body.monospaced()).textSelection(.enabled)
                }.padding().navigationTitle("Deploy an app").toolbar { Button("Done") { deploying = false } }
            }.presentationDetents([.medium])
        }
    }

    private func metadata(for app: DeviceApp) -> some View {
        let badge = app.state.replacingOccurrences(of: "_", with: " ")
        return HStack(spacing: 6) {
            Text(badge).font(.caption2.bold()).foregroundStyle(app.runningState.color).fixedSize()
                .padding(.horizontal, 7).padding(.vertical, 4).background(app.runningState.color.opacity(0.12), in: .capsule)
            if app.hasWebpage {
                Image(systemName: "globe").font(.caption).foregroundStyle(.secondary)
                    .accessibilityLabel("Has a web page")
            }
        }
    }
}

struct MobileAppDetail: View {
    @ObservedObject var model: AppModel
    let device: Device
    let name: String
    @State private var actions = AsyncActions()
    @State private var showingLogs = false
    @State private var ports: [ResourcePort] = []
    @State private var portError: String?
    @State private var showEventLog = false
    private var ready: Bool { model.mobileScopeIsCurrent && model.selectedDeviceReady && model.connected?.id == device.id }
    private var app: DeviceApp? { ready ? model.apps.first { $0.appName == name } : nil }
    var body: some View {
        Group {
            if let app {
                let session = model.deviceSession
                ScrollView {
                    VStack(alignment: .leading, spacing: 18) {
                        LensHero(icon: "square.grid.2x2", color: app.runningState == .running ? .green : .gray, kicker: "APP · \(device.title.uppercased())",
                                 title: name, subtitle: app.terminationReason?.nonEmpty ?? app.runningState.title,
                                 chips: []) {
                            Button(app.runningState == .running ? "Stop" : "Start") {
                                actions.submit { await model.setAppRunning(name, running: app.runningState != .running, expectedSession: session) }
                            }.mobileActionStyle().disabled(model.appActions.contains(name) || app.runningState == .unknown)
                                .accessibilityIdentifier("app.toggle")
                            if app.hasWebpage {
                                Button("Open") { actions.submit { await model.openApp(app, expectedSession: session) } }
                                    .mobileActionStyle(primary: true).disabled(app.runningState != .running || model.appActions.contains(name))
                                    .accessibilityIdentifier("app.open")
                            } else { Button("Logs", action: openLogs).mobileActionStyle(primary: true) }
                        }
                        if model.appActions.contains(name) { ProgressView("Updating app…").font(.caption) }
                        if app.hasWebpage || !ports.isEmpty || portError != nil {
                            LensSection(title: "Ports") {
                                if app.hasWebpage {
                                    Button { actions.submit { await model.openApp(app, expectedSession: session) } } label: {
                                        LensRow(title: "Dashboard", subtitle: endpoint(for: app),
                                                symbol: "globe", trailing: ":\(app.httpPort)")
                                    }.buttonStyle(.plain).disabled(app.runningState != .running)
                                }
                                ForEach(ports.filter { $0.port != app.httpPort }) { port in
                                    LensRow(title: port.protocolName.uppercased(), subtitle: port.label, symbol: "network", navigates: false)
                                }
                                if let portError { FailureNote(message: portError).padding(12) }
                            }
                        }
                        LensSection(title: "App") {
                            Button(action: openLogs) { LensRow(title: "Logs", subtitle: "Follow live output", symbol: "clock", color: .gray) }
                                .buttonStyle(.plain).accessibilityIdentifier("app.logs")
                            Divider().padding(.leading, 64)
                            LensRow(title: "Version", subtitle: app.appVersion?.nonEmpty ?? "Not reported", symbol: "square.grid.2x2", color: .gray, navigates: false)
                            Divider().padding(.leading, 64)
                            Button { actions.submit { await model.restartApp(name, expectedSession: session) } } label: {
                                LensRow(title: "Restart", subtitle: "Stops and starts the app", symbol: "arrow.clockwise", color: .orange)
                            }.buttonStyle(.plain).disabled(app.runningState != .running || model.appActions.contains(name))
                                .accessibilityIdentifier("app.restart")
                        }
                    }.padding(.horizontal, 16).padding(.vertical, 8).frame(maxWidth: 700).frame(maxWidth: .infinity)
                }.refreshable { await model.refreshDetails() }
            } else {
                ContentUnavailableView(ready ? "App unavailable" : "Device disconnected", systemImage: "square.grid.2x2")
            }
        }.modifier(MobileFloatingChatScrollMargins())
            .background(MobileLensStyle.background).navigationTitle(name).navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .topBarTrailing) {
                    if ready {
                        CampaignEventButton(session: model.deviceSession, deviceID: device.id, deviceName: device.title,
                                            showingLog: $showEventLog).id(model.deviceSession)
                    }
                }
            }
            .task { await actions.run() }
            .task(id: "\(ready ? model.deviceSession : 0)/\(app?.state ?? "")") {
                ports = []; portError = nil
                guard ready, app?.runningState == .running else { return }
                let session = model.deviceSession
                do {
                    let values = try await GoClient.shared.ports(app: name, session: session)
                    if !Task.isCancelled, ready, model.deviceSession == session { ports = values }
                } catch { if !Task.isCancelled, ready, model.deviceSession == session { portError = error.localizedDescription } }
            }
            .navigationDestination(isPresented: $showingLogs) {
                if app != nil {
                    DeviceLogsPage(model: model, fixedApp: name).id(model.deviceSession).modifier(MobileFloatingChatScrollMargins())
                } else { ContentUnavailableView("Device or app unavailable", systemImage: "bolt.slash") }
            }
    }
    private func openLogs() {
        guard app != nil else { return }
        model.logApp = name
        showingLogs = true
    }
    private func endpoint(for app: DeviceApp) -> String {
        if device.transport == .cloud { return "Wendy Cloud tunnel" }
        if device.transport == .mesh { return "Wendy mesh tunnel" }
        guard var address = URLComponents(string: "http://" + device.selector), address.host != nil else {
            return "Published HTTP endpoint"
        }
        address.port = Int(app.httpPort)
        return address.url?.absoluteString ?? "Published HTTP endpoint"
    }
}
