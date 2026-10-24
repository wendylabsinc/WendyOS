import SwiftUI

struct PairingView: View {
    let session: UInt64
    let deviceName: String
    @State private var kind: PairingKind = .lite
    @State private var devices: [PairingDevice] = []
    @State private var loading = false
    @State private var busy: String?
    @State private var error: String?
    @State private var warning = ""
    @State private var login: PairingDevice?
    @State private var forgetting: PairingDevice?

    var body: some View {
        GeometryReader { geometry in
            VStack(alignment: .leading, spacing: 18) {
                HStack {
                    Picker("Pair", selection: $kind) { ForEach(PairingKind.allCases) { Text($0.rawValue).tag($0) } }
                        .pickerStyle(.segmented).labelsHidden().fixedSize().disabled(busy != nil || loading)
                    Spacer()
                    if loading { ProgressView().controlSize(.small) }
                }
                Text("Discover and pair sources on \(deviceName)’s network.").foregroundStyle(.secondary)
                if !warning.isEmpty { Label(warning, systemImage: "exclamationmark.triangle").font(.callout).foregroundStyle(.orange) }
                if let error { FailureNote(message: error) }
                if devices.isEmpty && !loading {
                    VStack(spacing: 12) {
                        Image(systemName: kind == .lite ? "sensor" : "web.camera")
                            .font(.system(size: 30)).foregroundStyle(.tertiary)
                        Text(error == nil ? "No devices found" : "Could not load pairings")
                            .font(.headline)
                        Text(kind == .lite ? "Power on your Wendy Lite device and connect it to the same network as \(deviceName). Enroll it in your organization before pairing."
                             : "Connect an ONVIF IP camera to the same network as \(deviceName), then scan again.")
                            .font(.callout).foregroundStyle(.secondary)
                            .multilineTextAlignment(.center).fixedSize(horizontal: false, vertical: true)
                        Button("Scan again") { Task { await refresh() } }.padding(.top, 4)
                    }.frame(maxWidth: 420)
                        .frame(maxWidth: .infinity, maxHeight: .infinity)
                } else {
                    ScrollView {
                        LazyVStack(spacing: 0) {
                            ForEach(Array(devices.enumerated()), id: \.element.id) { index, item in
                                HStack(spacing: 14) {
                                    Image(systemName: kind == .lite ? "sensor" : "web.camera").font(.title2).foregroundStyle(.secondary).frame(width: 30)
                                    VStack(alignment: .leading, spacing: 5) {
                                        Text(item.name).fontWeight(.medium)
                                        Text([item.detail, item.address].filter { !$0.isEmpty }.joined(separator: " · ")).font(.caption).foregroundStyle(.secondary).lineLimit(2)
                                    }
                                    Spacer()
                                    if busy == item.id { ProgressView().controlSize(.small) }
                                    Label(item.paired ? (item.connected ? "Connected" : "Paired") : "Not paired", systemImage: "circle.fill")
                                        .font(.caption).foregroundStyle(item.connected ? Color.green : .secondary).frame(width: 100, alignment: .leading)
                                    HStack(spacing: 8) {
                                        if item.canPair && (kind == .camera || !item.paired) {
                                            Button(item.paired ? "Edit login" : "Pair") {
                                                if kind == .camera { login = item }
                                                else { Task { await pair(item) } }
                                            }.disabled(busy != nil || loading)
                                        }
                                        if item.paired {
                                            Menu { Button("Forget pairing", role: .destructive) { forgetting = item } } label: { Image(systemName: "ellipsis") }
                                                .menuStyle(.borderlessButton).fixedSize().disabled(busy != nil || loading)
                                        }
                                    }.frame(width: 128, alignment: .trailing)
                                }.padding(.horizontal, 16).frame(height: 76)
                                    .background(index.isMultiple(of: 2) ? Color.clear : Color.primary.opacity(0.035), in: RoundedRectangle(cornerRadius: 8))
                            }
                        }
                    }
                }
            }.padding(.horizontal, 28).padding(.vertical, 16)
                .frame(width: geometry.size.width, height: geometry.size.height, alignment: .topLeading)
        }
            .task(id: kind) { devices = []; await refresh() }
            .focusedSceneValue(\.devicePaneRefresh, PaneRefreshAction(enabled: !loading && busy == nil, perform: { Task { await refresh() } }))
            .sheet(item: $login) { item in
                CameraLoginSheet(item: item, deviceName: deviceName, session: session) { Task { await refresh() } }
            }
            .alert("Forget pairing?", isPresented: Binding(get: { forgetting != nil }, set: { if !$0 { forgetting = nil } }), presenting: forgetting) { item in
                Button("Cancel", role: .cancel) {}
                Button("Forget", role: .destructive) { Task { await forget(item) } }
            } message: { item in Text("Remove \(item.name) from \(deviceName)? Its sensors will no longer be available to apps on this device.") }
    }
    private func refresh() async {
        loading = true; error = nil
        defer { loading = false }
        do {
            let result = try await GoClient.shared.pairings(session: session, kind: kind)
            guard !Task.isCancelled else { return }
            devices = result.devices; warning = result.warning
        } catch { if !Task.isCancelled { self.error = error.localizedDescription } }
    }
    private func pair(_ item: PairingDevice) async {
        busy = item.id; error = nil; defer { busy = nil }
        do { try await GoClient.shared.pairSensor(session: session, id: item.id); await refresh() }
        catch { self.error = error.localizedDescription }
    }
    private func forget(_ item: PairingDevice) async {
        busy = item.id; error = nil; defer { busy = nil }
        do {
            if kind == .lite { try await GoClient.shared.pairSensor(session: session, id: item.id, forget: true) }
            else { try await GoClient.shared.forgetCamera(session: session, id: item.cameraID) }
            await refresh()
        } catch { self.error = error.localizedDescription }
    }
}

private struct CameraLoginSheet: View {
    let item: PairingDevice
    let deviceName: String
    let session: UInt64
    let saved: () -> Void
    @Environment(\.dismiss) private var dismiss
    @State private var username = "admin"
    @State private var password = ""
    @State private var saving = false
    @State private var error: String?
    var body: some View {
        VStack(alignment: .leading, spacing: 20) {
            Label(item.name, systemImage: "web.camera").font(.title2.bold())
            Text("The camera login is saved on \(deviceName).").foregroundStyle(.secondary)
            Form {
                TextField("Username", text: $username)
                SecureField("Password", text: $password)
            }.textFieldStyle(.roundedBorder)
            if let error { FailureNote(message: error) }
            HStack {
                if saving { ProgressView().controlSize(.small) }
                Spacer()
                Button("Cancel") { password = ""; dismiss() }.keyboardShortcut(.cancelAction).disabled(saving)
                Button("Save login") {
                    saving = true
                    Task {
                        do {
                            try await GoClient.shared.pairCamera(session: session, id: item.cameraID, username: username, password: password)
                            password = ""; saved(); dismiss()
                        } catch { self.error = error.localizedDescription; password = "" }
                        saving = false
                    }
                }.keyboardShortcut(.defaultAction).disabled(saving || username.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
            }
        }.padding(24).frame(width: 440).interactiveDismissDisabled(saving).onDisappear { password = "" }
    }
}
