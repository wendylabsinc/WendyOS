import SwiftUI

struct LiteDeviceView: View {
    @State private var actions = AsyncActions()
    @ObservedObject var model: AppModel
    @State private var page = "Cameras"
    @State private var info: DeviceInfoSnapshot?
    @State private var error: String?
    @State private var ssid = ""
    @State private var password = ""
    @State private var saving = false
    @State private var notice: String?
    @State private var confirmWiFi = false

    var body: some View {
        actionContent.task { await actions.run() }
    }

    @ViewBuilder private var actionContent: some View {
        VStack(spacing: 16) {
            Picker("Wendy Lite view", selection: $page) {
                ForEach(["Cameras", "Sensors", "Device", "Wi-Fi", "Logs", "Settings"], id: \.self) { Text($0) }
            }.pickerStyle(.segmented).padding(.horizontal)
            switch page {
            case "Cameras": CameraWallView(model: model)
            case "Sensors": LiteTemperatureView(session: model.deviceSession)
            case "Logs": DeviceLogsPage(model: model)
            case "Wi-Fi": wifi
            case "Settings":
                Form {
                    Section("Enrollment") {
                        DeviceEnrollmentSettings(session: model.deviceSession, deviceName: model.connected?.title ?? "", lite: true)
                    }
                    LiteMeshSettings(session: model.deviceSession).id(model.deviceSession)
                }.formStyle(.grouped)
            default: device
            }
        }
        .task(id: model.deviceSession) { await refreshInfo() }
        .focusedSceneValue(\.devicePaneRefresh, PaneRefreshAction(enabled: !saving, perform: {
            actions.submit { await refreshInfo(); await model.refreshDetails() }
        }))
        .confirmationDialog("Save Wi-Fi and restart?", isPresented: $confirmWiFi, titleVisibility: .visible) {
            Button("Save and restart") { actions.submit { await saveWiFi() } }
        } message: {
            Text("This replaces the device's saved Wi-Fi networks with \(ssid) and restarts it. Camera and console streams will disconnect.")
        }
    }

    private var device: some View {
        Form {
            Section("Wendy Lite") {
                LabeledContent("Name", value: model.connected?.title ?? "")
                if let info {
                    LabeledContent("Board", value: info.board)
                    LabeledContent("Architecture", value: info.architecture)
                    LabeledContent("Firmware", value: info.osVersion)
                } else if error == nil { ProgressView("Loading device information…") }
                if let error { FailureNote(message: error) }
                Button("Refresh") { actions.submit { await refreshInfo() } }
                Button("Reconnect") { reconnect() }
            }
            Section {
                Text("View SensorLink cameras, read the device console, and configure Wi-Fi. For containers, campaigns, and recorded data, pair this source with a WendyOS device.")
                    .foregroundStyle(.secondary)
            }
        }.formStyle(.grouped)
    }

    private var wifi: some View {
        Form {
            Section("Wi-Fi network") {
                TextField("Network name", text: $ssid)
                SecureField("Password", text: $password)
                Text("Leave the password empty for an open network.").font(.caption).foregroundStyle(.secondary)
                Button("Save and restart…") { confirmWiFi = true }
                    .disabled(saving || ssid.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
                if saving { ProgressView("Saving Wi-Fi…") }
            }
            if let notice { Section { Text(notice); Button("Reconnect") { reconnect() } } }
            if let error { Section { FailureNote(message: error) } }
        }.formStyle(.grouped)
    }

    private func reconnect() {
        guard let device = model.connected else { return }
        actions.submit { await model.connect(device, force: true) }
    }
    private func refreshInfo() async {
        do { info = try await GoClient.shared.deviceInfo(session: model.deviceSession); error = nil }
        catch { if !Task.isCancelled { self.error = error.localizedDescription } }
    }
    private func saveWiFi() async {
        saving = true; error = nil; notice = nil
        defer { saving = false }
        do {
            try await GoClient.shared.joinWiFi(session: model.deviceSession, ssid: ssid, password: password, security: 0, hidden: false)
            password = ""
            notice = "Wi-Fi configuration sent. Reconnect after the device restarts."
        } catch { if !Task.isCancelled { self.error = error.localizedDescription } }
    }
}

private struct LiteMeshSettings: View {
    let session: UInt64
    @State private var actions = AsyncActions()
    @State private var snapshot: DeviceNetworkSettings?
    @State private var loading = false
    @State private var saving = false
    @State private var error: String?
    @State private var notice: String?

    var body: some View {
        Section {
            if let snapshot {
                let enabled = snapshot.meshConfig & 1 != 0
                LabeledContent("Participation", value: enabled ? "Enabled" : "Disabled")
                LabeledContent("Status", value: snapshot.meshState.capitalized)
                LabeledContent("Active links", value: snapshot.peers.formatted())
                if !snapshot.meshDetail.isEmpty {
                    Text(snapshot.meshDetail).font(.caption).foregroundStyle(.secondary)
                }
                Button(enabled ? "Disable mesh participation" : "Enable mesh participation", systemImage: "network") {
                    actions.submit { await setParticipation(!enabled) }
                }.disabled(loading || saving || !snapshot.meshAvailable)
            }
            if saving { ProgressView("Saving mesh settings…") }
            else if loading { ProgressView("Reading mesh settings…") }
            if let notice { Text(notice).font(.callout).foregroundStyle(.secondary) }
            if let error { FailureNote(message: error) }
            Button("Refresh mesh status") { actions.submit { await refresh() } }
                .disabled(loading || saving)
        } header: {
            Text("Wendy Mesh")
        } footer: {
            Text("Participation lets this device join the mesh using its configured Bluetooth and Wi-Fi connections. Disabling it can disconnect a device reached through the mesh.")
        }
        .task { await actions.run() }
        .task(id: session) {
            snapshot = nil
            notice = nil
            await refresh()
        }
    }

    private func refresh() async {
        guard !loading else { return }
        loading = true
        error = nil
        defer { loading = false }
        do {
            let value = try await GoClient.shared.networkSettings(session: session)
            guard !Task.isCancelled else { return }
            snapshot = value
        } catch {
            guard !Task.isCancelled else { return }
            snapshot = nil
            self.error = error.localizedDescription
        }
    }

    private func setParticipation(_ enabled: Bool) async {
        guard !saving else { return }
        saving = true
        error = nil
        notice = nil
        defer { saving = false }
        do {
            try await GoClient.shared.configureMesh(session: session, changed: 1, enabled: enabled ? 1 : 0)
            guard !Task.isCancelled else { return }
            notice = "Mesh participation \(enabled ? "enabled" : "disabled"). Active links show whether the device has connected to peers."
            await refresh()
        } catch {
            guard !Task.isCancelled else { return }
            snapshot = nil
            self.error = "Could not confirm the change. Refresh or reconnect to check the device. \(error.localizedDescription)"
        }
    }
}
