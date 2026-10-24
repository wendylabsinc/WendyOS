import SwiftUI

struct DeviceWiFiNetwork: Identifiable {
    let ssid: String
    let security: Int32
    let signal: Int?
    let known, connected: Bool
    let priority: Int
    var id: String { "\(ssid)|\(security)" }
    var securityLabel: String {
        switch security {
        case 1: "Open"
        case 2: "WEP"
        case 3: "WPA"
        case 4: "WPA2"
        case 5: "WPA3"
        case 6: "Enterprise"
        default: "Unknown security"
        }
    }
}
struct DeviceNetworkSettings {
    let topology: DeviceMeshTopology
    let wifiAvailable, wifiConnected: Bool
    let ssid, wifiError, scanError, knownError, meshError: String
    let networks, known: [DeviceWiFiNetwork]
    let meshAvailable, runtimeAvailable: Bool
    let meshConfig: UInt32
    let peers: UInt32
    let meshState, meshDetail, gateway: String
}
extension GoClient {
    func networkSettings(session: UInt64) async throws -> DeviceNetworkSettings {
        try await perform {
            guard let p = WDNetworkSettings(session) else { throw WendyError(message: "Missing device settings.") }
            defer { WDNetworkSettingsFree(p) }
            let value = p.pointee
            try Self.check(value.status)
            func networks(_ pointer: UnsafeMutablePointer<WDWiFiNetwork>?, _ count: Int) -> [DeviceWiFiNetwork] {
                let rows = UnsafeBufferPointer(start: pointer, count: count).map {
                    DeviceWiFiNetwork(ssid: Self.string($0.ssid), security: $0.security,
                                      signal: $0.has_signal != 0 ? Int($0.signal) : nil,
                                      known: $0.known != 0, connected: $0.connected != 0, priority: Int($0.priority))
                }
                // An SSID may be reported by several access points. Keep the strongest observation.
                return Dictionary(grouping: rows.filter { !$0.ssid.isEmpty }, by: \.id).compactMap { _, values in
                    values.sorted { ($0.signal ?? -1) > ($1.signal ?? -1) }.first
                }.sorted {
                    if $0.connected != $1.connected { return $0.connected }
                    if ($0.signal ?? -1) != ($1.signal ?? -1) { return ($0.signal ?? -1) > ($1.signal ?? -1) }
                    return $0.ssid.localizedStandardCompare($1.ssid) == .orderedAscending
                }
            }
            let topology = DeviceMeshTopology(
                available: value.topology_available != 0, runtime: value.topology_runtime != 0,
                truncated: value.topology_truncated != 0,
                captured: value.topology_captured_ms > 0 ? Date(timeIntervalSince1970: Double(value.topology_captured_ms) / 1000) : nil,
                error: Self.string(value.topology_error), principal: Self.string(value.self_principal), address: Self.string(value.self_address),
                links: UnsafeBufferPointer(start: value.mesh_links, count: value.mesh_link_count).map {
                    DeviceMeshPeer(id: $0.link_id, principal: Self.string($0.principal), deviceID: Self.string($0.device_id),
                                   address: Self.string($0.address), transport: Self.string($0.transport), name: Self.string($0.name), cost: $0.cost)
                },
                routes: UnsafeBufferPointer(start: value.mesh_routes, count: value.mesh_route_count).enumerated().map { index, route in
                    DeviceMeshRoute(id: index, prefix: Self.string(route.prefix), origin: Self.string(route.origin), nextHop: Self.string(route.next_hop),
                                    metric: route.metric, local: route.local != 0, unreachable: route.unreachable != 0, authorized: route.authorized != 0)
                })
            return DeviceNetworkSettings(topology: topology, wifiAvailable: value.wifi_available != 0, wifiConnected: value.wifi_connected != 0,
                                         ssid: Self.string(value.ssid), wifiError: Self.string(value.wifi_error), scanError: Self.string(value.scan_error),
                                         knownError: Self.string(value.known_error), meshError: Self.string(value.mesh_error),
                                         networks: networks(value.networks, value.network_count), known: networks(value.known, value.known_count),
                                         meshAvailable: value.mesh_available != 0, runtimeAvailable: value.runtime_available != 0,
                                         meshConfig: value.mesh_config, peers: value.peers, meshState: Self.string(value.mesh_state),
                                         meshDetail: Self.string(value.mesh_detail), gateway: Self.string(value.gateway))
        }
    }
    func joinWiFi(session: UInt64, ssid: String, password: String, security: Int32, hidden: Bool) async throws {
        try await perform {
            try Self.withCString(ssid) { name in
                try Self.withCString(password) { secret in
                    try Self.finish(WDJoinWiFi(session, name, secret, security, hidden ? 1 : 0))
                }
            }
        }
    }
    func disconnectWiFi(session: UInt64) async throws { try await perform { try Self.finish(WDDisconnectWiFi(session)) } }
    func forgetWiFi(session: UInt64, ssid: String) async throws {
        try await perform { try Self.withCString(ssid) { try Self.finish(WDForgetWiFi(session, $0)) } }
    }
    func configureMesh(session: UInt64, changed: UInt32, enabled: UInt32) async throws {
        try await perform { try Self.finish(WDConfigureMesh(session, changed, enabled)) }
    }
}

private struct WiFiJoinTarget: Identifiable {
    let id = UUID()
    var network: DeviceWiFiNetwork?
}
struct DeviceSettingsView: View {
    @EnvironmentObject private var model: AppModel
    let session: UInt64
    let deviceName: String
    @State private var snapshot: DeviceNetworkSettings?
    @State private var draft: UInt32 = 0
    @State private var loading = false
    @State private var saving = false
    @State private var error: String?
    @State private var notice: String?
    @State private var joining: WiFiJoinTarget?
    @State private var disconnecting = false
    @State private var showingMeshGraph = false
    @State private var forgetting: DeviceWiFiNetwork?
    private var dirty: Bool { snapshot.map { $0.meshConfig != draft } ?? false }
    private var participates: Bool { draft & 1 != 0 }

    private enum Page: String, Hashable {
        case info = "Device info", wifi = "Wi-Fi", mesh = "Wendy Mesh", bluetooth = "Bluetooth", audio = "Audio", pairing = "Pairing", logs = "Logs", notifications = "Notifications"
    }
    @State private var selectedPage: Page?

    var body: some View {
        Group {
            #if os(macOS)
            VStack(alignment: .leading, spacing: 0) {
                if let page = selectedPage {
                    HStack(spacing: 12) {
                        Button { selectedPage = nil } label: { Label("Settings", systemImage: "chevron.left") }
                            .buttonStyle(.borderless)
                        Divider().frame(height: 16)
                        Text(page.rawValue).font(.headline)
                        Spacer()
                    }.padding(.horizontal, 28).padding(.vertical, 12)
                    settingsPage(page)
                } else {
                    settingsMenu
                }
            }.frame(maxWidth: .infinity, maxHeight: .infinity)
            .navigationTitle("Wendy")
            .focusedSceneValue(\.desktopBack, selectedPage == nil ? nil : { selectedPage = nil })
            #else
            settingsMenu
            #endif
        }
        .task(id: session) { await refresh() }
        .focusedSceneValue(\.devicePaneRefresh, PaneRefreshAction(enabled: !loading && !saving, perform: { Task { await refresh() } }))
        .sheet(item: $joining) { target in
            WiFiJoinSheet(session: session, deviceName: deviceName, network: target.network) {
                notice = "Wi-Fi connection requested. Checking the device’s connection…"
                Task { await refresh() }
            }
        }
        .confirmationDialog("Disconnect from Wi-Fi?", isPresented: $disconnecting, titleVisibility: .visible) {
            Button("Disconnect", role: .destructive) { Task { await perform { try await GoClient.shared.disconnectWiFi(session: session) } } }
        } message: { Text("\(deviceName) may become unreachable until it connects through another network.") }
        .confirmationDialog("Forget \(forgetting?.ssid ?? "network")?", isPresented: Binding(get: { forgetting != nil }, set: { if !$0 { forgetting = nil } }), titleVisibility: .visible) {
            if let network = forgetting {
                Button("Forget network", role: .destructive) {
                    forgetting = nil
                    Task { await perform { try await GoClient.shared.forgetWiFi(session: session, ssid: network.ssid) } }
                }
            }
        } message: { Text("The saved Wi-Fi profile will be removed from \(deviceName).") }
    }
    private var settingsMenu: some View {
        Form {
            Section("Enrollment") {
                DeviceEnrollmentSettings(session: session, deviceName: deviceName, lite: false)
            }
            Section {
                settingsLink(.info, symbol: "info.circle")
                settingsLink(.wifi, symbol: "wifi", value: snapshot.map { $0.wifiConnected ? $0.ssid : $0.wifiError.isEmpty ? "Not connected" : "Unavailable" } ?? "")
                settingsLink(.bluetooth, symbol: "antenna.radiowaves.left.and.right")
                settingsLink(.audio, symbol: "speaker.wave.2")
                settingsLink(.mesh, symbol: "network", value: snapshot?.meshState.capitalized ?? "")
                settingsLink(.notifications, symbol: "bell.badge")
            }
            Section {
                settingsLink(.pairing, symbol: "sensor")
                settingsLink(.logs, symbol: "doc.text")
            }
        }.formStyle(.grouped).mobileRefreshable { await refresh() }
    }
    @ViewBuilder private func settingsLink(_ page: Page, symbol: String, value: String = "") -> some View {
        #if os(macOS)
        Button { selectedPage = page } label: {
            HStack {
                settingsLabel(page.rawValue, symbol: symbol, value: value)
                Image(systemName: "chevron.right").font(.caption).foregroundStyle(.tertiary)
            }.contentShape(Rectangle())
        }.buttonStyle(.plain)
        #else
        NavigationLink { settingsPage(page) } label: {
            settingsLabel(page.rawValue, symbol: symbol, value: value)
        }
        #endif
    }
    private func settingsPage(_ page: Page) -> some View {
            Group {
                switch page {
                case .notifications: DeviceNotificationsView(session: session)
                case .info: DeviceInfoView(session: session, deviceName: deviceName)
                case .wifi: if let snapshot { wifiPage(snapshot) } else { settingsLoading }
                case .mesh: if let snapshot { meshPage(snapshot) } else { settingsLoading }
                case .audio: DeviceAudioView(session: session, deviceName: deviceName)
                case .bluetooth: DeviceBluetoothView(session: session, deviceName: deviceName)
                case .pairing: PairingView(session: session, deviceName: deviceName).navigationTitle("Pairing")
                case .logs: DeviceLogsPage(model: model)
                }
            }
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
    }
    @ViewBuilder private var settingsLoading: some View {
        if let error { FailureView(title: "Could not load settings", error: error) { Task { await refresh() } } }
        else { ProgressView("Loading settings…").frame(maxWidth: .infinity, maxHeight: .infinity) }
    }
    private func settingsLabel(_ title: String, symbol: String, value: String) -> some View {
        HStack(spacing: 12) {
            Image(systemName: symbol).frame(width: 24).foregroundStyle(Color.accentColor)
            Text(title)
            Spacer()
            Text(value).foregroundStyle(.secondary).lineLimit(1)
        }.padding(.vertical, 4)
    }
    @ViewBuilder private var notices: some View {
        if let error { Section { FailureNote(message: error) } }
        if let notice { Section { Text(notice).font(.callout).foregroundStyle(.secondary) } }
    }
    private func visibleNetworks(_ snapshot: DeviceNetworkSettings) -> [DeviceWiFiNetwork] {
        var networks = snapshot.networks.map { network in
            DeviceWiFiNetwork(ssid: network.ssid, security: network.security, signal: network.signal,
                              known: network.known || snapshot.known.contains { $0.ssid == network.ssid },
                              connected: network.connected || (snapshot.wifiConnected && snapshot.ssid == network.ssid), priority: network.priority)
        }
        if snapshot.wifiConnected && !networks.contains(where: { $0.ssid == snapshot.ssid }) {
            networks.insert(DeviceWiFiNetwork(ssid: snapshot.ssid, security: snapshot.known.first { $0.ssid == snapshot.ssid }?.security ?? 0,
                                             signal: nil, known: snapshot.known.contains { $0.ssid == snapshot.ssid }, connected: true, priority: 0), at: 0)
        }
        return networks.sorted { $0.connected && !$1.connected }
    }
    private func wifiPage(_ snapshot: DeviceNetworkSettings) -> some View {
        let available = visibleNetworks(snapshot)
        let saved = snapshot.known.filter { network in !available.contains { $0.ssid == network.ssid } }
        return Form {
            notices
            Section {
                if !snapshot.wifiError.isEmpty { FailureNote(message: snapshot.wifiError) }
                if !snapshot.scanError.isEmpty { FailureNote(message: snapshot.scanError) }
                if available.isEmpty && snapshot.scanError.isEmpty { Text("No networks found").foregroundStyle(.secondary) }
                ForEach(available) { network in
                    HStack {
                        if network.connected { networkLabel(network) }
                        else {
                            Button { joining = WiFiJoinTarget(network: network) } label: { networkLabel(network) }.buttonStyle(.plain)
                        }
                        if network.connected || network.known {
                            Menu {
                                if network.connected { Button("Disconnect", role: .destructive) { disconnecting = true } }
                                if network.known { Button("Forget network", role: .destructive) { forgetting = network } }
                            } label: { Image(systemName: "ellipsis").frame(minWidth: 32, minHeight: 32) }
                                .accessibilityLabel("Actions for \(network.ssid)")
                        }
                    }.disabled(saving)
                }
                Button("Join another network…", systemImage: "plus") { joining = WiFiJoinTarget() }.disabled(saving)
            } header: { Text("Available networks") }
            footer: { Text("Changes apply to \(deviceName) and may interrupt its connection to Wendy.") }
            if !saved.isEmpty || !snapshot.knownError.isEmpty {
                Section("Saved networks") {
                    if !snapshot.knownError.isEmpty { FailureNote(message: snapshot.knownError) }
                    ForEach(saved) { network in
                        HStack {
                            Button { joining = WiFiJoinTarget(network: network) } label: { networkLabel(network) }.buttonStyle(.plain)
                            Menu { Button("Forget network", role: .destructive) { forgetting = network } }
                                label: { Image(systemName: "ellipsis").frame(minWidth: 32, minHeight: 32) }
                                .accessibilityLabel("Actions for \(network.ssid)")
                        }.disabled(saving)
                    }
                }
            }
        }.formStyle(.grouped).navigationTitle("Wi-Fi")
            .mobileRefreshable { await refresh() }
            #if os(macOS)
            .toolbar { ToolbarItem(placement: .primaryAction) { Button { Task { await refresh() } } label: { Image(systemName: "arrow.clockwise") }.disabled(loading || saving).accessibilityLabel("Scan for Wi-Fi networks") } }
            #endif
    }
    private func meshPage(_ snapshot: DeviceNetworkSettings) -> some View {
        Form {
            notices
            Section {
                Button("Show mesh graph", systemImage: "point.3.connected.trianglepath.dotted") { showingMeshGraph = true }
                LabeledContent("Runtime", value: snapshot.runtimeAvailable ? (snapshot.meshState.isEmpty ? "Available" : snapshot.meshState.capitalized) : "Unavailable")
                if snapshot.runtimeAvailable { LabeledContent("Authenticated peers", value: snapshot.peers.formatted()) }
                if !snapshot.gateway.isEmpty { LabeledContent("Internet gateway", value: snapshot.gateway) }
                if !snapshot.meshDetail.isEmpty { Text(snapshot.meshDetail).font(.caption).foregroundStyle(.secondary) }
                if !snapshot.meshError.isEmpty { FailureNote(message: snapshot.meshError) }
            }
            if snapshot.meshAvailable {
                Section {
                    Toggle("Participate in mesh", isOn: setting(0))
                    Toggle("Use mesh internet when offline", isOn: setting(1)).disabled(!participates)
                    Toggle("Share internet connection", isOn: setting(2)).disabled(!participates)
                } footer: { Text("Participation lets this device join and relay the mesh. Internet sharing offers its own uplink to other mesh devices.") }
                    .disabled(saving)
                Section("Transports") {
                    Toggle("Ethernet", isOn: setting(5))
                    Toggle("Wi-Fi network", isOn: setting(6))
                    Toggle("Wi-Fi Aware", isOn: setting(3))
                    Toggle("Bluetooth", isOn: setting(4))
                }.disabled(saving || !participates)
                if dirty || saving {
                    Section {
                        Button("Save changes") { Task { await saveMesh() } }.disabled(saving || loading)
                        Button("Discard changes", role: .cancel) { draft = snapshot.meshConfig }.disabled(saving)
                        if saving { ProgressView() }
                    }
                }
            }
        }.formStyle(.grouped).navigationTitle("Wendy Mesh")
            .mobileRefreshable { await refresh() }
            .sheet(isPresented: $showingMeshGraph) {
                MeshGraphView(topology: snapshot.topology, deviceName: deviceName)
            }
    }
    private func networkLabel(_ network: DeviceWiFiNetwork) -> some View {
        HStack(spacing: 12) {
            Image(systemName: network.connected ? "checkmark" : "wifi").frame(width: 22).foregroundStyle(network.connected ? Color.green : .secondary)
            VStack(alignment: .leading, spacing: 3) {
                Text(network.ssid).foregroundStyle(.primary)
                Text(network.securityLabel + (network.connected ? " · Connected" : network.known ? " · Saved" : "")).font(.caption).foregroundStyle(.secondary)
            }.frame(maxWidth: .infinity, alignment: .leading)
            if let signal = network.signal { Text("\(signal)%").font(.caption).foregroundStyle(.secondary) }
        }.padding(.vertical, 3).contentShape(Rectangle())
    }
    private func setting(_ bit: UInt32) -> Binding<Bool> {
        Binding(get: { draft & (1 << bit) != 0 }, set: { value in
            if value { draft |= 1 << bit } else { draft &= ~(1 << bit) }
        })
    }
    private func refresh() async {
        guard !loading else { return }
        loading = true; defer { loading = false }
        do {
            let value = try await GoClient.shared.networkSettings(session: session)
            guard !Task.isCancelled else { return }
            if !dirty { draft = value.meshConfig }
            snapshot = value; error = nil
            if notice != nil, value.wifiAvailable { notice = value.wifiConnected ? "Connected to \(value.ssid)." : nil }
        } catch { if !Task.isCancelled { self.error = error.localizedDescription } }
    }
    private func perform(_ work: () async throws -> Void) async {
        guard !saving else { return }
        saving = true; error = nil; notice = nil
        defer { saving = false }
        do { try await work(); await refresh() }
        catch { self.error = error.localizedDescription }
    }
    private func saveMesh() async {
        guard let snapshot else { return }
        let changed = snapshot.meshConfig ^ draft
        let enabled = draft
        await perform { try await GoClient.shared.configureMesh(session: session, changed: changed, enabled: enabled) }
    }
}

private struct WiFiJoinSheet: View {
    let session: UInt64
    let deviceName: String
    let network: DeviceWiFiNetwork?
    let joined: () -> Void
    @Environment(\.dismiss) private var dismiss
    @State private var ssid = ""
    @State private var password = ""
    @State private var security: Int32 = 4
    @State private var hidden = false
    @State private var saving = false
    @State private var error: String?
    var body: some View {
        VStack(spacing: 0) {
            HStack {
                Button("Cancel") { password = ""; dismiss() }.keyboardShortcut(.cancelAction)
                Spacer()
                Text("Join Wi-Fi").font(.headline)
                Spacer()
                Button("Join") { Task { await join() } }.keyboardShortcut(.defaultAction)
                    .disabled(ssid.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty || security == 6)
            }.padding().disabled(saving)
            Form {
                Section {
                    TextField("Network name", text: $ssid).autocorrectionDisabled()
                        #if os(iOS)
                        .textInputAutocapitalization(.never)
                        #endif
                    if network == nil {
                        Picker("Security", selection: $security) {
                            Text("Open").tag(Int32(1)); Text("WPA2").tag(Int32(4)); Text("WPA3").tag(Int32(5))
                        }
                        Toggle("Hidden network", isOn: $hidden)
                    } else { LabeledContent("Security", value: network?.securityLabel ?? "") }
                    if security != 1 && security != 6 { SecureField(network?.known == true ? "Password (leave blank to use saved)" : "Password", text: $password) }
                    if security == 6 { Text("Enterprise authentication is not supported by this connection form.").foregroundStyle(.secondary) }
                } footer: { Text("Connects \(deviceName) to this network. The device may disconnect from Wendy while switching networks.") }
                if let error { FailureNote(message: error) }
                if saving { ProgressView("Connecting…") }
            }.formStyle(.grouped).disabled(saving)
        }
        .desktopFrame(width: 460, height: 380)
        .interactiveDismissDisabled(saving)
        .onAppear { ssid = network?.ssid ?? ""; security = network?.security ?? 4 }
        .onDisappear { password = "" }
        #if os(iOS)
        .presentationDetents([.medium, .large])
        .presentationBackground(Color(uiColor: .systemGroupedBackground))
        #endif
    }
    private func join() async {
        saving = true; error = nil; defer { saving = false }
        do {
            try await GoClient.shared.joinWiFi(session: session, ssid: ssid, password: password, security: security, hidden: hidden)
            password = ""; joined(); dismiss()
        } catch { password = ""; self.error = error.localizedDescription }
    }
}

private struct CampaignNotificationPermission: Identifiable {
    let name: String
    var enabled: Bool
    let registered, requestsNotifications: Bool
    var id: String { name }
}
private struct NotificationPermissionChange {
    let campaign: CampaignNotificationPermission
    let enabled: Bool
}
extension GoClient {
    fileprivate func campaignNotifications(session: UInt64) async throws -> [CampaignNotificationPermission] {
        try await perform {
            guard let p = WDCampaignNotifications(session) else { throw WendyError(message: "Missing notification permissions.") }
            defer { WDCampaignNotificationsFree(p) }
            try Self.check(p.pointee.status)
            return UnsafeBufferPointer(start: p.pointee.items, count: p.pointee.count).map {
                CampaignNotificationPermission(name: Self.string($0.name), enabled: $0.enabled != 0,
                                               registered: $0.registered != 0, requestsNotifications: $0.requests_notifications != 0)
            }
        }
    }
    func setCampaignNotifications(session: UInt64, name: String, enabled: Bool) async throws {
        try await perform { try Self.withCString(name) { try Self.finish(WDSetCampaignNotifications(session, $0, enabled ? 1 : 0)) } }
    }
}
private struct DeviceNotificationsView: View {
    let session: UInt64
    @State private var permissions: [CampaignNotificationPermission] = []
    @State private var loading = false
    @State private var loaded = false
    @State private var saving = false
    @State private var error: String?
    @State private var pending: NotificationPermissionChange?
    var body: some View {
        permissionForm
        #if os(iOS)
        .navigationTitle("Notifications")
        #endif
        .task(id: session) { await refresh() }
        .mobileRefreshable { await refresh() }
        .focusedSceneValue(\.devicePaneRefresh, PaneRefreshAction(enabled: !loading && !saving, perform: { Task { await refresh() } }))
        .alert(pending?.enabled == true ? "Allow notifications?" : "Turn off notifications?", isPresented: Binding(get: { pending != nil }, set: { if !$0 { pending = nil } }), presenting: pending) { change in
            Button("Cancel", role: .cancel) { pending = nil }
            Button(change.enabled ? "Allow notifications" : "Turn off", role: change.enabled ? nil : .destructive) {
                pending = nil
                Task { await save(change) }
            }
        } message: { change in
            Text("This changes the Cloud notification permission for “\(change.campaign.name)” across all devices using that campaign in this organization.")
        }
    }
    private var permissionForm: some View {
        Form {
            Section("Receive notifications") { NotificationReceivingView() }
            if (!loaded || loading) && permissions.isEmpty && error == nil {
                Section("Campaign notifications") {
                    HStack { ProgressView().controlSize(.small); Text("Loading permissions…").foregroundStyle(.secondary) }
                }
            } else if permissions.isEmpty && error == nil {
                Section("Campaign notifications") {
                    Text("Create a campaign in chat to choose which alerts to send.").foregroundStyle(.secondary)
                }
            }
            if let error {
                Section {
                    FailureNote(message: error)
                    Button("Try again") { Task { await refresh() } }.disabled(loading || saving)
                }
            }
            if !permissions.isEmpty {
                Section {
                    ForEach(permissions) { permission in
                        Toggle(isOn: Binding(get: { permission.enabled }, set: { enabled in
                            pending = NotificationPermissionChange(campaign: permission, enabled: enabled)
                        })) {
                            VStack(alignment: .leading, spacing: 4) {
                                Text(permission.name)
                                if !permission.registered { Text("Cloud registration will be created after approval").font(.caption).foregroundStyle(.secondary) }
                                else if !permission.requestsNotifications { Text("Notifications are off in the campaign’s settings").font(.caption).foregroundStyle(.secondary) }
                            }
                        }
                        .disabled(loading || saving || (!permission.requestsNotifications && !permission.enabled))
                    }
                } header: { Text("Campaign notifications") } footer: {
                    Text("Cloud permissions apply across devices using the same campaign in this organization. Changing them requires an owner or admin. Recordings are not uploaded by this permission.")
                }
                if saving { Section { ProgressView("Saving permission…") } }
            }
        }
        .formStyle(.grouped)
    }
    private func refresh() async {
        guard !loading, !saving else { return }
        loading = true
        defer { loading = false; loaded = true }
        do {
            let result = try await GoClient.shared.campaignNotifications(session: session)
            guard !Task.isCancelled else { return }
            permissions = result; error = nil
        } catch { if !Task.isCancelled { self.error = error.localizedDescription } }
    }
    private func save(_ change: NotificationPermissionChange) async {
        saving = true; error = nil
        do {
            try await GoClient.shared.setCampaignNotifications(session: session, name: change.campaign.name, enabled: change.enabled)
            if let index = permissions.firstIndex(where: { $0.name == change.campaign.name }) { permissions[index].enabled = change.enabled }
        } catch { self.error = error.localizedDescription }
        saving = false
    }
}
