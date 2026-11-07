import SwiftUI

private enum DeviceSegment: String, CaseIterable { case apps = "Apps", cameras = "Cameras", data = "Data", location = "Location", settings = "Settings" }
private enum DeviceTool: String, Identifiable { case metrics = "App activity", twin = "Digital Twin", campaigns = "Campaigns", update = "Agent & OS update"
    var id: Self { self }
}

struct MobileDeviceView: View {
    @EnvironmentObject private var model: AppModel
    let device: Device
    let openRecordings: () -> Void
    @State private var showingAppearance = false
    @State private var segment = DeviceSegment.apps
    @State private var info: DeviceInfoSnapshot?
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
                if model.connectedLite { LiteDeviceView(model: model).id(model.deviceSession) }
                else if segment == .cameras || segment == .settings {
                    VStack(spacing: 0) {
                        header.padding(.horizontal, 16).padding(.vertical, 8)
                        if segment == .cameras { CameraWallView(model: model) }
                        else { DeviceSettingsView(session: model.deviceSession, deviceName: device.title).id(model.deviceSession) }
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
                                    LensRow(title: "All recordings", subtitle: "Open Data for " + device.title, symbol: "cylinder.split.1x2")
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
                    }.refreshable { await model.refreshDetails() }
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
                            Button("App activity", systemImage: "waveform.path.ecg") { tool = .metrics }
                            Button("Digital Twin", systemImage: "cube.transparent") { tool = .twin }
                            Button("Campaigns", systemImage: "record.circle") { tool = .campaigns }
                        }
                    }
                }
            }
            .task(id: ready ? model.deviceSession : 0) {
                info = nil; update = nil
                guard ready && !model.connectedLite else { return }
                let captured = model.deviceSession
                let details = try? await GoClient.shared.deviceInfo(session: captured)
                guard !Task.isCancelled, ready, captured == model.deviceSession else { return }
                info = details
                let software = try? await GoClient.shared.onlineUpdate(session: captured, action: 0)
                if !Task.isCancelled, ready, captured == model.deviceSession { update = software }
            }
            .navigationDestination(isPresented: $showLogs) {
                if ready { DeviceLogsPage(model: model).id(model.deviceSession).accessibilityIdentifier("devices.detail.logPage") }
                else { connectionState.navigationTitle("Logs") }
            }
            .navigationDestination(item: $tool) { destination in
                if ready {
                    switch destination {
                    case .metrics: ActivityView(model: model)
                    case .twin: NativeSiteView(model: model)
                    case .campaigns: CampaignsView(session: model.deviceSession, deviceName: device.title, openClip: { clip = $0 }, showEventLog: { showEvents = true })
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
    private func openLogs() { model.logApp = ""; showLogs = true }
    private var header: some View {
        VStack(alignment: .leading, spacing: 18) {
            LensHero(icon: "cpu", color: MobileLensStyle.green, kicker: ["DEVICE", info?.board.nonEmpty?.uppercased()].compactMap { $0 }.joined(separator: " · "),
                     title: device.title, subtitle: ["Online", "\(model.apps.count) apps", site].compactMap { $0 }.joined(separator: " · "),
                     chips: ["Online", device.transport == .cloud ? "Cloud" : "Local",
                             update?.available == true ? "Update available" : info?.version.nonEmpty.map { "Agent \($0)" },
                             model.camerasError == nil ? "\(model.cameras.count) \(model.cameras.count == 1 ? "camera" : "cameras")" : nil].compactMap { $0 }, device: device, customize: { showingAppearance = true }) {
                Button("Logs", action: openLogs).buttonStyle(LensPillStyle()).accessibilityIdentifier("devices.detail.logs")
                Button(update?.available == true ? "Update" : "Device info") { tool = .update }.buttonStyle(LensPillStyle(primary: true))
            }
            ViewThatFits(in: .horizontal) {
                segments
                ScrollView(.horizontal) { segments }.scrollIndicators(.hidden)
            }
        }
    }
    private var segments: some View {
        HStack(spacing: 0) {
            ForEach(DeviceSegment.allCases, id: \.self) { item in
                Button { segment = item } label: {
                    Text(item.rawValue).font(.caption.weight(.semibold)).lineLimit(1).fixedSize()
                        .padding(.horizontal, 5).frame(maxWidth: .infinity, minHeight: 32)
                        .foregroundStyle(item == segment ? Color.white : .primary)
                        .background(item == segment ? MobileLensStyle.green : .clear, in: .capsule)
                        .frame(minHeight: 44).contentShape(.rect)
                }.buttonStyle(.plain).accessibilityAddTraits(item == segment ? .isSelected : [])
                    .accessibilityIdentifier("device.segment." + item.rawValue)
            }
        }.padding(.horizontal, 2).background { Capsule().fill(.quaternary).frame(height: 36) }
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
                    Text(kicker).font(.caption2.bold()).tracking(0.4).foregroundStyle(color)
                    Text(title).font(.title2.bold())
                    Text(subtitle).font(.subheadline).foregroundStyle(.secondary)
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
                Text(value).font(.caption.weight(.semibold)).fixedSize().padding(.horizontal, 9).padding(.vertical, 5)
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
        LensSection(title: "Apps") {
            ForEach(model.visibleApps) { app in
                NavigationLink {
                    MobileAppDetail(model: model, device: device, name: app.appName)
                } label: {
                    HStack(spacing: 12) {
                        LensIcon(symbol: "square.grid.2x2", color: app.runningState == .running ? .green : .gray)
                        VStack(alignment: .leading, spacing: 2) {
                            // Prefer wrapping bundle identifiers at a dot instead of splitting a word or version.
                            Text(verbatim: app.appName.replacingOccurrences(of: ".", with: ".\u{200B}"))
                                .font(.body.weight(.semibold)).lineLimit(textSize.isAccessibilitySize ? nil : 2)
                                .truncationMode(.middle).accessibilityLabel(app.appName)
                            Text(app.terminationReason?.nonEmpty ?? app.runningState.title).font(.caption).foregroundStyle(.secondary)
                            if textSize.isAccessibilitySize { metadata(for: app) }
                        }.frame(maxWidth: .infinity, alignment: .leading)
                        if !textSize.isAccessibilitySize { metadata(for: app) }
                        Image(systemName: "chevron.right").font(.caption.bold()).foregroundStyle(.tertiary)
                    }.padding(.horizontal, 16).padding(.vertical, 10).contentShape(.rect)
                }.buttonStyle(.plain).accessibilityIdentifier("device.apps.row." + app.appName)
                if app.id != model.visibleApps.last?.id { Divider().padding(.leading, 64) }
            }
            if let error = model.appsError { FailureNote(message: error).padding(16) }
            else if model.visibleApps.isEmpty { Text("No visible apps on this device.").foregroundStyle(.secondary).padding(16) }
            if model.hiddenAppCount > 0 { HiddenAppsFilter(model: model).padding(12) }
        }
        Text("WEB means the app serves a page you can open.").font(.caption).foregroundStyle(.secondary).padding(.horizontal, 12)
        LensSection(title: "Add") {
            Button { deploying = true } label: {
                LensRow(title: "Deploy an app", subtitle: "From a Wendy project", symbol: "square.grid.2x2")
            }.buttonStyle(.plain)
        }.sheet(isPresented: $deploying) {
            NavigationStack {
                VStack(alignment: .leading, spacing: 12) {
                    Text("Run this command in your project on your developer machine.")
                    Text(verbatim: "wendy run --device '\(device.selector.replacingOccurrences(of: "'", with: "'\\''"))'").font(.body.monospaced()).textSelection(.enabled)
                }.padding().navigationTitle("Deploy an app").toolbar { Button("Done") { deploying = false } }
            }.presentationDetents([.medium])
        }
    }

    private func metadata(for app: DeviceApp) -> some View {
        let badge = app.hasWebpage && app.runningState == .running ? "WEB" : app.state.replacingOccurrences(of: "_", with: " ")
        return HStack(spacing: 6) {
            Text(badge).font(.caption2.bold()).foregroundStyle(app.runningState.color).fixedSize()
                .padding(.horizontal, 7).padding(.vertical, 4).background(app.runningState.color.opacity(0.12), in: .capsule)
            if app.hasWebpage {
                Text(verbatim: ":\(app.httpPort)").font(.caption).foregroundStyle(.secondary).fixedSize()
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
                                 chips: app.hasWebpage ? [":\(app.httpPort) · http"] : []) {
                            Button(app.runningState == .running ? "Stop" : "Start") {
                                actions.submit { await model.setAppRunning(name, running: app.runningState != .running, expectedSession: session) }
                            }.buttonStyle(LensPillStyle()).disabled(model.appActions.contains(name) || app.runningState == .unknown)
                                .accessibilityIdentifier("app.toggle")
                            if app.hasWebpage {
                                Button("Open") { actions.submit { await model.openApp(app, expectedSession: session) } }
                                    .buttonStyle(LensPillStyle(primary: true)).disabled(app.runningState != .running || model.appActions.contains(name))
                                    .accessibilityIdentifier("app.open")
                            } else { Button("Logs", action: openLogs).buttonStyle(LensPillStyle(primary: true)) }
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
                            Text(device.transport == .cloud ? "Remote: opens through a secure Wendy Cloud tunnel." :
                                 device.transport == .mesh ? "Opens through the Wendy mesh connection." : "Nearby: opens directly on this network.")
                                .font(.caption).foregroundStyle(.secondary).padding(.horizontal, 12)
                        }
                        LensSection(title: "App") {
                            Button(action: openLogs) { LensRow(title: "Logs", subtitle: "Follow live output", symbol: "clock", color: .gray) }
                                .buttonStyle(.plain).accessibilityIdentifier("app.logs")
                            Divider().padding(.leading, 64)
                            LensRow(title: "Version", subtitle: app.appVersion?.nonEmpty ?? "Not reported", symbol: "square.grid.2x2", color: .gray, navigates: false)
                            Divider().padding(.leading, 64)
                            Button { actions.submit { await model.restartApp(name, expectedSession: session) } } label: {
                                LensRow(title: "Restart", subtitle: "Stops and starts the container", symbol: "arrow.clockwise", color: .orange)
                            }.buttonStyle(.plain).disabled(app.runningState != .running || model.appActions.contains(name))
                                .accessibilityIdentifier("app.restart")
                        }
                    }.padding(.horizontal, 16).padding(.vertical, 8).frame(maxWidth: 700).frame(maxWidth: .infinity)
                }.refreshable { await model.refreshDetails() }
            } else {
                ContentUnavailableView(ready ? "App unavailable" : "Device disconnected", systemImage: "square.grid.2x2")
            }
        }.background(MobileLensStyle.background).navigationTitle(name).navigationBarTitleDisplayMode(.inline)
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
                    DeviceLogsPage(model: model, fixedApp: name).id(model.deviceSession)
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
