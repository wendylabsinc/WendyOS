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
                ForEach(["Cameras", "Device", "Wi-Fi", "Logs", "Settings"], id: \.self) { Text($0) }
            }.pickerStyle(.segmented).padding(.horizontal)
            switch page {
            case "Cameras": CameraWallView(model: model)
            case "Logs": DeviceLogsPage(model: model)
            case "Wi-Fi": wifi
            case "Settings":
                Form {
                    Section("Enrollment") {
                        DeviceEnrollmentSettings(session: model.deviceSession, deviceName: model.connected?.title ?? "", lite: true)
                    }
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
