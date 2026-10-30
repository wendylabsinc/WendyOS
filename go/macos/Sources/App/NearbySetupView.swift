import SwiftUI

/// The sheet is owned by an explicit selection, not by a changing scan result.
struct NearbySetupButton: View {
    @EnvironmentObject private var app: AppModel
    @State private var showing = false
    @State private var signInAfterDismiss = false
    @State private var actions = AsyncActions()

    var body: some View {
        Button { showing = true } label: { Label("Set up nearby device", systemImage: "plus.circle") }
            .sheet(isPresented: $showing, onDismiss: {
                actions.submit {
                    if signInAfterDismiss { signInAfterDismiss = false; await app.signIn() }
                    await app.refreshNearby()
                }
            }) { NearbySetupView { signInAfterDismiss = true } }
            .task { await actions.run() }
    }
}

struct NearbySetupView: View {
    @Environment(\.dismiss) private var dismiss
    @Environment(\.scenePhase) private var scenePhase
    let signIn: () -> Void
    var initialDevice: NearbySetupDevice? = nil
    @State private var actions = AsyncActions()
    @State private var devices: [NearbySetupDevice] = []
    @State private var selected: NearbySetupDevice?
    @State private var state = NearbySetupSnapshot()
    @State private var organizations: [EnrollmentOrganization] = []
    @State private var organization = ""
    @State private var name = ""
    @State private var ssid = ""
    @State private var password = ""
    @State private var wifiExpanded = false
    @State private var busy = false
    @State private var progress = ""
    @State private var error: String?
    @State private var lastStatus = Date.distantPast
    @State private var lastWiFiScan = Date.distantPast
    @State private var automatic = false
    @State private var observationError: String?
    @State private var nextReconnect = Date.distantPast
    @State private var reconnectDelay: TimeInterval = 3
    private var editingLocked: Bool { busy && !automatic }
    private var choice: EnrollmentOrganization? { organizations.first { $0.id == organization } }
    private var nameValid: Bool { name.range(of: "^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$", options: .regularExpression) != nil }

    var body: some View {
        NavigationStack {
            Form {
                if let selected {
                    Section {
                        Label(selected.title, systemImage: "server.rack").font(.headline)
                        if state.completed {
                            Label("Enrolled in Wendy Cloud", systemImage: "checkmark.circle.fill").foregroundStyle(.green)
                        } else if state.pending {
                            ProgressView("Finishing setup…")
                            Text("We’ll confirm automatically when your device comes online. Keep it powered on and connected to the internet.")
                                .font(.callout).foregroundStyle(.secondary)
                        } else if state.attempted {
                            Text("Cloud enrollment could not be confirmed. Check the selected organization's device list before starting another enrollment.")
                                .foregroundStyle(.secondary)
                        } else if state.enrolled {
                            Label("This device is already enrolled", systemImage: "checkmark.shield")
                        } else if state.connected {
                            Label("Ready to set up", systemImage: "checkmark.circle").foregroundStyle(.secondary)
                        } else {
                            ProgressView("Connecting to your device…")
                            Text("Keep your device nearby. We’ll reconnect automatically.")
                                .font(.callout).foregroundStyle(.secondary)
                        }
                    }
                    if !state.completed && !state.enrolled && !state.attempted {
                        Section("Add to Wendy Cloud") {
                            if organizations.isEmpty {
                                Text("Sign in to choose an organization.").foregroundStyle(.secondary)
                                Button("Sign in…") { signIn(); dismiss() }.disabled(editingLocked)
                            } else {
                                Picker("Organization", selection: $organization) {
                                    Text("Choose an organization").tag("")
                                    ForEach(organizations) { Text($0.title).tag($0.id) }
                                }
                                TextField("Device name", text: $name).autocorrectionDisabled()
                                Text("Start with a lowercase letter. Use letters, numbers and hyphens.").font(.caption).foregroundStyle(.secondary)
                                Button("Add device") { submit(.enroll) }
                                    .buttonStyle(.borderedProminent)
                                    .disabled(!state.connected || choice == nil || !nameValid || editingLocked)
                            }
                        }.disabled(editingLocked)
                    }
                    if !state.completed && !state.enrolled && !state.attempted {
                        Section {
                            DisclosureGroup("Wi-Fi · Optional", isExpanded: $wifiExpanded) {
                                if state.wifiConnected { Label("Connected to \(state.ssid)", systemImage: "wifi").foregroundStyle(.green) }
                                if !state.networks.isEmpty {
                                    Picker("Network", selection: $ssid) {
                                        Text("Choose network").tag("")
                                        if !ssid.isEmpty && !state.networks.contains(ssid) { Text(ssid).tag(ssid) }
                                        ForEach(Array(Set(state.networks)).sorted(), id: \.self) { Text($0).tag($0) }
                                    }
                                }
                                TextField("Network name", text: $ssid).autocorrectionDisabled()
                                SecureField("Password, if required", text: $password)
                                Button("Connect to Wi-Fi") { submit(.connectWiFi) }.disabled(ssid.isEmpty)
                                if automatic && progress == "Finding networks…" { ProgressView("Finding networks…") }
                            }.disabled(editingLocked || !state.connected)
                        } footer: {
                            Text("Wi-Fi is optional. Enrollment can start over Bluetooth; cloud confirmation needs a working network connection.")
                        }
                    }
                } else {
                    Section {
                        Text("Keep your unregistered Wendy device nearby and powered on.").foregroundStyle(.secondary)
                        ForEach(devices) { device in
                            Button { actions.submit { await choose(device) } } label: {
                                Label(device.title, systemImage: "server.rack")
                            }.disabled(editingLocked)
                        }
                        ProgressView("Looking for nearby devices…")
                        if devices.isEmpty {
                            Text("Devices will appear here automatically. Keep Bluetooth on and your device nearby.").font(.callout).foregroundStyle(.secondary)
                        }
                    }
                }
                if editingLocked || error != nil {
                    Section {
                        if editingLocked { ProgressView(progress) }
                        if let error { Text(error).foregroundStyle(.orange).textSelection(.enabled) }
                    }
                }
                if let observationError, !state.completed {
                    Section {
                        DisclosureGroup("Connection details") {
                            Text(observationError).font(.caption).foregroundStyle(.secondary).textSelection(.enabled)
                        }
                    }
                }
            }
            .formStyle(.grouped)
            .navigationTitle(state.completed ? "You’re all set" : "Set up device")
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar { ToolbarItem(placement: .cancellationAction) { Button(state.completed ? "Done" : "Close") { dismiss() }.disabled(editingLocked) } }
        }
        #if os(macOS)
        .frame(width: 520, height: 620)
        #endif
        .interactiveDismissDisabled(editingLocked)
        .task {
            // Join all started C calls before closing the exact session they own.
            await withTaskGroup(of: Void.self) { group in
                group.addTask { await actions.run() }
                group.addTask { await watch() }
                await group.waitForAll()
            }
            password = ""
            await GoClient.shared.closeNearbySetup(state.session)
        }
    }

    private func discover() async {
        guard !busy else { return }
        busy = true; automatic = true
        defer { busy = false; automatic = false }
        do {
            devices = try await GoClient.shared.discoverSetupDevices()
            observationError = nil
        } catch { if !Task.isCancelled { observationError = error.localizedDescription } }
    }

    private func choose(_ device: NearbySetupDevice) async {
        while busy {
            do { try await Task.sleep(for: .milliseconds(100)) } catch { return }
        }
        guard selected == nil, !Task.isCancelled else { return }
        name = DeviceEnrollmentForm.suggestedName(device.title)
        await connect(device)
    }

    private func connect(_ device: NearbySetupDevice) async {
        guard !busy else { return }
        busy = true; automatic = true; selected = device
        defer { busy = false; automatic = false }
        do {
            state = try await GoClient.shared.openNearbySetup(device.id)
            observationError = state.error; lastStatus = .now
        } catch { if !Task.isCancelled { observationError = error.localizedDescription } }
        nextReconnect = .now.addingTimeInterval(reconnectDelay)
        reconnectDelay = state.connected ? 3 : min(reconnectDelay * 2, 30)
    }

    private func submit(_ action: NearbySetupAction) { actions.submit { await perform(action) } }
    private func perform(_ action: NearbySetupAction, automatically: Bool = false) async {
        // A tap during a background read must not be silently discarded.
        while busy && !automatically {
            do { try await Task.sleep(for: .milliseconds(100)) } catch { return }
        }
        guard !busy, !Task.isCancelled, state.session != 0 else { return }
        busy = true; automatic = automatically
        if !automatically { error = nil }
        switch action {
        case .status: progress = "Checking connection…"
        case .scanWiFi: progress = "Finding networks…"
        case .connectWiFi: progress = "Connecting to Wi-Fi…"
        case .enroll: progress = "Preparing enrollment…"
        case .checkEnrollment: progress = "Checking enrollment…"
        }
        defer {
            busy = false; automatic = false
            if action == .connectWiFi { password = "" }
            if action == .scanWiFi { lastWiFiScan = .now }
            if action == .status { lastStatus = .now }
        }
        do {
            state = try await GoClient.shared.nearbySetupAction(action, session: state.session, ssid: ssid, password: password,
                account: choice?.account ?? "", organization: choice?.organizationID ?? "", name: name)
            if automatically || state.pending { observationError = state.error }
            else { error = state.error }
            if state.completed { error = nil; observationError = nil }
        } catch {
            if !Task.isCancelled {
                if automatically { observationError = error.localizedDescription }
                else { self.error = error.localizedDescription }
            }
        }
    }

    private func watch() async {
        if let initialDevice {
            name = DeviceEnrollmentForm.suggestedName(initialDevice.title)
            selected = initialDevice
        }
        do { organizations = try await GoClient.shared.enrollmentOrganizations() }
        catch { if !Task.isCancelled { self.error = error.localizedDescription } }
        while !Task.isCancelled {
            if !busy {
                if scenePhase != .active {
                    if state.connected { await GoClient.shared.closeNearbySetup(state.session); state.connected = false }
                } else {
                    switch state.observation(hasSelection: selected != nil, wifiExpanded: wifiExpanded,
                                             statusAge: Date.now.timeIntervalSince(lastStatus),
                                             scanAge: Date.now.timeIntervalSince(lastWiFiScan)) {
                    case .discover: await discover()
                    case .reconnect:
                        if let selected, Date.now >= nextReconnect { await connect(selected) }
                    case .checkEnrollment: await perform(.checkEnrollment, automatically: true)
                    case .status: await perform(.status, automatically: true)
                    case .scanWiFi: await perform(.scanWiFi, automatically: true)
                    case .idle: break
                    }
                }
            }
            do { try await Task.sleep(for: .seconds(5)) } catch { break }
        }
    }
}
