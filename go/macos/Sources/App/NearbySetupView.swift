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
    @State private var checkingUntil = Date.distantPast
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
                            Label("Waiting for cloud confirmation", systemImage: "clock")
                            Text("The device needs a working network connection to finish. You can check again without restarting enrollment.")
                                .font(.callout).foregroundStyle(.secondary)
                            Button("Check enrollment status") { submit(.checkEnrollment) }.disabled(busy)
                        } else if state.attempted {
                            Text("Cloud enrollment could not be confirmed. Check the selected organization's device list before starting another enrollment.")
                                .foregroundStyle(.secondary)
                        } else if state.enrolled {
                            Label("This device is already enrolled", systemImage: "checkmark.shield")
                        } else if state.connected {
                            Label("Ready to set up", systemImage: "checkmark.circle").foregroundStyle(.secondary)
                        } else if !busy {
                            Label("Bluetooth disconnected", systemImage: "antenna.radiowaves.left.and.right.slash")
                        }
                        if !state.connected && !state.completed && !state.enrolled {
                            Button("Retry connection") { actions.submit { await connect(selected) } }.disabled(busy)
                        }
                    }
                    if !state.completed && !state.enrolled && !state.attempted {
                        Section("Add to Wendy Cloud") {
                            if organizations.isEmpty {
                                Text("Sign in to choose an organization.").foregroundStyle(.secondary)
                                Button("Sign in…") { signIn(); dismiss() }.disabled(busy)
                            } else {
                                Picker("Organization", selection: $organization) {
                                    Text("Choose an organization").tag("")
                                    ForEach(organizations) { Text($0.title).tag($0.id) }
                                }
                                TextField("Device name", text: $name).autocorrectionDisabled()
                                Text("Start with a lowercase letter. Use letters, numbers and hyphens.").font(.caption).foregroundStyle(.secondary)
                                Button("Add device") { submit(.enroll) }
                                    .buttonStyle(.borderedProminent)
                                    .disabled(!state.connected || choice == nil || !nameValid || busy)
                            }
                        }.disabled(busy)
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
                                Button("Find networks") { submit(.scanWiFi) }
                            }.disabled(busy || !state.connected)
                        } footer: {
                            Text("Wi-Fi is optional. Enrollment can start over Bluetooth; cloud confirmation needs a working network connection.")
                        }
                    }
                } else {
                    Section {
                        Text("Keep your unregistered Wendy device nearby and powered on.").foregroundStyle(.secondary)
                        ForEach(devices) { device in
                            Button { actions.submit { name = DeviceEnrollmentForm.suggestedName(device.title); await connect(device) } } label: {
                                Label(device.title, systemImage: "server.rack")
                            }.disabled(busy)
                        }
                        if devices.isEmpty && !busy { Text("No setup devices found. Check that Bluetooth is enabled and the device is ready for setup.").foregroundStyle(.secondary) }
                        Button("Scan again") { actions.submit { await discover() } }.disabled(busy)
                    }
                }
                if busy || error != nil {
                    Section {
                        if busy { ProgressView(progress) }
                        if let error { Text(error).foregroundStyle(.orange).textSelection(.enabled) }
                    }
                }
            }
            .formStyle(.grouped)
            .navigationTitle("Set up nearby device")
            .toolbar { ToolbarItem(placement: .cancellationAction) { Button(state.completed ? "Done" : "Close") { dismiss() }.disabled(busy) } }
        }
        #if os(macOS)
        .frame(width: 520, height: 620)
        #endif
        .interactiveDismissDisabled(busy)
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
        busy = true; progress = "Looking for devices…"; error = nil
        defer { busy = false }
        do { devices = try await GoClient.shared.discoverSetupDevices() }
        catch { if !Task.isCancelled { self.error = error.localizedDescription } }
    }

    private func connect(_ device: NearbySetupDevice) async {
        guard !busy else { return }
        busy = true; progress = "Connecting…"; error = nil; selected = device
        defer { busy = false }
        do {
            state = try await GoClient.shared.openNearbySetup(device.id)
            error = state.error; lastStatus = .now
        } catch { if !Task.isCancelled { self.error = error.localizedDescription } }
    }

    private func submit(_ action: NearbySetupAction) { actions.submit { await perform(action) } }
    private func perform(_ action: NearbySetupAction) async {
        guard !busy, state.session != 0 else { return }
        busy = true; error = nil
        switch action {
        case .status: progress = "Checking connection…"
        case .scanWiFi: progress = "Finding networks…"
        case .connectWiFi: progress = "Connecting to Wi-Fi…"
        case .enroll: progress = "Preparing enrollment…"
        case .checkEnrollment: progress = "Checking enrollment…"
        }
        defer { busy = false; if action == .connectWiFi { password = "" } }
        do {
            state = try await GoClient.shared.nearbySetupAction(action, session: state.session, ssid: ssid, password: password,
                account: choice?.account ?? "", organization: choice?.organizationID ?? "", name: name)
            error = state.error; lastStatus = .now
            if action == .enroll && state.pending { checkingUntil = .now.addingTimeInterval(180) }
            if action == .checkEnrollment && !state.completed && state.error == nil { error = "Still waiting for the device to appear online in Wendy Cloud." }
        } catch { if !Task.isCancelled { self.error = error.localizedDescription } }
    }

    private func watch() async {
        if let initialDevice {
            name = DeviceEnrollmentForm.suggestedName(initialDevice.title)
            await connect(initialDevice)
        } else {
            await discover()
        }
        do { organizations = try await GoClient.shared.enrollmentOrganizations() }
        catch { if !Task.isCancelled { self.error = error.localizedDescription } }
        while !Task.isCancelled {
            do { try await Task.sleep(for: .seconds(3)) } catch { break }
            guard !busy else { continue }
            if scenePhase != .active {
                if state.connected { await GoClient.shared.closeNearbySetup(state.session); state.connected = false }
                continue
            }
            if state.pending && !state.completed && Date.now < checkingUntil { await perform(.checkEnrollment) }
            else if state.connected && !state.completed && Date.now.timeIntervalSince(lastStatus) > 25 { await perform(.status) }
        }
    }
}
