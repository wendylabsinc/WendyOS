import SwiftUI

struct PairingView: View {
    @State private var actions = AsyncActions()
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
        actionContent.task { await actions.run() }
    }

    @ViewBuilder private var actionContent: some View {
        Group {
            #if os(iOS)
            mobileList
                .focusedSceneValue(\.devicePaneRefresh, PaneRefreshAction(enabled: !loading && busy == nil, perform: { actions.submit { await refresh() } }))
            #else
            VStack(spacing: 0) {
                if kind == .unitree {
                    Picker("Pair", selection: $kind) { ForEach(PairingKind.allCases) { Text($0.rawValue).tag($0) } }
                        .pickerStyle(.segmented).fixedSize().padding(.top, 16)
                    UnitreePairingView(session: session, deviceName: deviceName).id(session)
                } else {
                    desktopContent
                        .focusedSceneValue(\.devicePaneRefresh, PaneRefreshAction(enabled: !loading && busy == nil, perform: { actions.submit { await refresh() } }))
                }
            }
            #endif
        }
            .task(id: kind) { devices = []; await refresh() }
            .sheet(item: $login) { item in
                CameraLoginSheet(item: item, deviceName: deviceName, session: session) { actions.submit { await refresh() } }
            }
            .alert("Forget pairing?", isPresented: Binding(get: { forgetting != nil }, set: { if !$0 { forgetting = nil } }), presenting: forgetting) { item in
                Button("Cancel", role: .cancel) {}
                Button("Forget", role: .destructive) { actions.submit { await forget(item) } }
            } message: { item in Text("Remove \(item.name) from \(deviceName)? Its sensors will no longer be available to apps on this device.") }
    }
    #if os(iOS)
    private var mobileList: some View {
        List {
            if !warning.isEmpty { Section { Label(warning, systemImage: "exclamationmark.triangle").font(.callout).foregroundStyle(.orange) } }
            if let error { Section { FailureNote(message: error) } }
            Section {
                ForEach(devices) { item in
                    HStack(alignment: .center, spacing: 12) {
                        Image(systemName: kind == .lite ? "sensor" : "web.camera")
                            .foregroundStyle(.secondary).frame(width: 24)
                        VStack(alignment: .leading, spacing: 5) {
                            Text(item.name).font(.body.weight(.medium)).foregroundStyle(.primary)
                            Text(item.address).font(.caption).foregroundStyle(.secondary)
                            if !item.canPair && !item.paired {
                                Text(item.detail).font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                            } else {
                                Text(item.paired ? (item.connected ? "Connected" : "Paired") : "Not paired")
                                    .font(.caption).foregroundStyle(item.connected ? Color.green : .secondary)
                            }
                        }.frame(maxWidth: .infinity, alignment: .leading)
                        if busy == item.id { ProgressView() }
                        else if item.paired {
                            Menu {
                                if kind == .camera { Button("Edit login", systemImage: "key") { login = item } }
                                Button("Forget pairing", systemImage: "trash", role: .destructive) { forgetting = item }
                            } label: { Image(systemName: "ellipsis").frame(width: 44, height: 44) }
                                .accessibilityLabel("Options for \(item.name)")
                                .disabled(busy != nil || loading)
                        } else if item.canPair {
                            Button("Pair") {
                                if kind == .camera { login = item } else { actions.submit { await pair(item) } }
                            }.buttonStyle(.bordered).fixedSize().disabled(busy != nil || loading)
                        }
                    }.padding(.vertical, 6)
                }
            } footer: {
                if !devices.isEmpty { Text("Sources on \(deviceName)’s network.") }
            }
        }
        .safeAreaInset(edge: .top) {
            Picker("Source type", selection: $kind) {
                ForEach(PairingKind.allCases) { Text($0.rawValue).tag($0) }
            }.pickerStyle(.segmented).padding().background(Color(uiColor: .systemGroupedBackground))
                .disabled(busy != nil || loading)
        }
        .overlay {
            if devices.isEmpty && error == nil && warning.isEmpty {
                if loading { ProgressView("Finding sources…") }
                else { ContentUnavailableView("No devices found", systemImage: kind == .lite ? "sensor" : "web.camera", description: Text("Connect a source to the same network, then pull down to scan again.")) }
            }
        }
        .refreshable { if !loading && busy == nil { await refresh() } }
    }
    #endif
    private var desktopContent: some View {
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
                        Button("Scan again") { actions.submit { await refresh() } }.padding(.top, 4)
                    }.frame(maxWidth: 420)
                        .frame(maxWidth: .infinity, maxHeight: .infinity)
                        .mobileRefreshablePlaceholder { if !loading && busy == nil { await refresh() } }
                } else {
                    ScrollView {
                        LazyVStack(spacing: 0) {
                            ForEach(Array(devices.enumerated()), id: \.element.id) { index, item in
                                AdaptiveStack(spacing: 10) {
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
                                                else { actions.submit { await pair(item) } }
                                            }.disabled(busy != nil || loading)
                                        }
                                        if item.paired {
                                            Menu { Button("Forget pairing", role: .destructive) { forgetting = item } } label: { Image(systemName: "ellipsis") }
                                                .menuStyle(.automatic).fixedSize().disabled(busy != nil || loading)
                                        }
                                    }.desktopActionWidth(128)
                                }.padding(.horizontal, 16).frame(minHeight: 76)
                                    .background(index.isMultiple(of: 2) ? Color.clear : Color.primary.opacity(0.035), in: RoundedRectangle(cornerRadius: 8))
                            }
                        }
                    }.mobileRefreshable { if !loading && busy == nil { await refresh() } }
                }
            }.padding(.horizontal, 28).padding(.vertical, 16)
                .frame(width: geometry.size.width, height: geometry.size.height, alignment: .topLeading)
        }
    }
    private func refresh() async {
        #if os(macOS)
        guard kind != .unitree else { return }
        #endif
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
        catch { if !Task.isCancelled { self.error = error.localizedDescription } }
    }
    private func forget(_ item: PairingDevice) async {
        busy = item.id; error = nil; defer { busy = nil }
        do {
            if kind == .lite { try await GoClient.shared.pairSensor(session: session, id: item.id, forget: true) }
            else { try await GoClient.shared.forgetCamera(session: session, id: item.cameraID) }
            await refresh()
        } catch { if !Task.isCancelled { self.error = error.localizedDescription } }
    }
}

private struct CameraLoginSheet: View {
    @State private var actions = AsyncActions()
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
        actionContent.task { await actions.run() }
    }

    @ViewBuilder private var actionContent: some View {
        Group {
            #if os(iOS)
            NavigationStack {
                Form {
                    Section {
                        TextField("Username", text: $username).textInputAutocapitalization(.never).autocorrectionDisabled()
                        SecureField("Password", text: $password)
                    } header: { Text(item.name) } footer: { Text("The camera login is saved on \(deviceName).") }
                    if let error { Section { FailureNote(message: error) } }
                }
                .navigationTitle("Camera login").navigationBarTitleDisplayMode(.inline)
                .toolbar {
                    ToolbarItem(placement: .cancellationAction) { Button("Cancel") { password = ""; dismiss() }.disabled(saving) }
                    ToolbarItem(placement: .confirmationAction) { Button("Save", action: save).disabled(saving || username.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty) }
                }
                .overlay { if saving { ProgressView() } }
            }.presentationBackground(Color(uiColor: .systemGroupedBackground))
            #else
            desktopForm
            #endif
        }.interactiveDismissDisabled(saving).onDisappear { password = "" }
    }
    private func save() {
        saving = true
        actions.submit {
            do {
                try await GoClient.shared.pairCamera(session: session, id: item.cameraID, username: username, password: password)
                password = ""; saved(); dismiss()
            } catch { if !Task.isCancelled { self.error = error.localizedDescription }; password = "" }
            saving = false
        }
    }
    private var desktopForm: some View {
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
                    actions.submit {
                        do {
                            try await GoClient.shared.pairCamera(session: session, id: item.cameraID, username: username, password: password)
                            password = ""; saved(); dismiss()
                        } catch { if !Task.isCancelled { self.error = error.localizedDescription }; password = "" }
                        saving = false
                    }
                }.keyboardShortcut(.defaultAction).disabled(saving || username.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
            }
        }.padding(24).desktopFrame(width: 440).interactiveDismissDisabled(saving).onDisappear { password = "" }
    }
}
