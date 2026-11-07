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
    @EnvironmentObject private var app: AppModel
    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    @Environment(\.dismiss) private var dismiss
    @Environment(\.scenePhase) private var scenePhase
    let signIn: () -> Void
    var initialDevice: NearbySetupDevice? = nil
    @State private var actions = AsyncActions()
    @State private var devices: [NearbySetupDevice] = []
    @State private var candidate: NearbySetupDevice?
    @State private var flow = NearbySetupFlow()
    @State private var scanned = false
    @State private var closing = false
    @State private var selected: NearbySetupDevice?
    @State private var state = NearbySetupSnapshot()
    @State private var organizations: [EnrollmentOrganization] = []
    @State private var organization = ""
    @State private var name = ""
    @State private var ssid = ""
    @State private var password = ""
    @State private var wifiExpanded = false
    @State private var organizationsLoaded = false
    @State private var organizationError: String?
    @State private var busy = false
    @State private var opening = false
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

    private var finished: Bool { state.completed || state.enrolled && !state.attempted }
    private var stage: String {
        if finished { return "done" }
        if !state.failure.isEmpty && !editingLocked { return "attention" }
        if state.pending || state.attempted || editingLocked { return "working" }
        if selected == nil { return "discovering" }
        return state.connected && organizationsLoaded ? "ready" : "connecting"
    }

    var body: some View {
        NavigationStack {
            ScrollView {
                VStack(spacing: 28) {
                    if !finished { stepIndicator }
                    DeviceEmblem(lite: state.lite, size: 104, completed: finished)
                        .padding(.top, 32)
                    VStack(spacing: 10) {
                        Text(headline).font(.largeTitle.weight(.semibold)).multilineTextAlignment(.center)
                        if let selected {
                            Text(name.isEmpty ? selected.title : name).font(.headline).foregroundStyle(.secondary)
                                .multilineTextAlignment(.center)
                        }
                        Text(explanation).font(.body).foregroundStyle(.secondary)
                            .multilineTextAlignment(.center).fixedSize(horizontal: false, vertical: true)
                    }
                    Group {
                        switch stage {
                        case "done":
                            if opening {
                                ProgressView("Opening device…").controlSize(.large)
                            } else {
                                primaryButton("Open device", symbol: "arrow.right") { actions.submit { await finish() } }
                            }
                        case "discovering": discovery
                        case "connecting":
                            ProgressView().controlSize(.large)
                            if !busy {
                                if let selected {
                                    Button("Retry connection") { actions.submit { await connect(selected) } }
                                }
                                Button("Choose another device") { actions.submit { await chooseAnother() } }
                            }
                        case "attention":
                            if state.canResume {
                                primaryButton("Continue setup", symbol: "arrow.right") { submit(.enroll) }
                                    .disabled(!state.connected || editingLocked)
                            } else {
                                if opening {
                                    ProgressView("Opening device…").controlSize(.large)
                                } else {
                                    primaryButton("View device", symbol: "arrow.right") { actions.submit { await finish() } }
                                }
                            }
                            wifiControls
                        case "working":
                            ProgressView().controlSize(.large)
                            if !editingLocked { wifiControls }
                        default:
                            if flow.step == .network { networkStep }
                            else { confirmation }
                        }
                    }
                    if let error, stage == "ready" || stage == "done" || stage == "attention" {
                        Text(PresentedFailure(error).message).font(.callout).foregroundStyle(.secondary)
                            .multilineTextAlignment(.center)
                    }
                    if let detail = observationError ?? error, !finished {
                        DisclosureGroup("Details") {
                            Text(PresentedFailure(detail).details).font(.caption.monospaced())
                                .foregroundStyle(.secondary).textSelection(.enabled)
                        }.font(.caption).tint(.secondary)
                    }
                }
                .frame(maxWidth: 440).padding(.horizontal, 28).padding(.bottom, 32)
                .frame(maxWidth: .infinity)
            }
            .scrollBounceBehavior(.basedOnSize)
            .animation(reduceMotion ? nil : .smooth(duration: 0.3), value: stage)
            .sensoryFeedback(.success, trigger: state.completed)
            .navigationTitle("")
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button {
                        if editingLocked || state.pending { closing = true } else { dismiss() }
                    } label: { Image(systemName: "xmark") }
                        .accessibilityLabel("Close setup")
                }
            }
        }
        #if os(macOS)
        .frame(width: 500, height: 640)
        #endif
        .interactiveDismissDisabled(editingLocked || state.pending)
        .confirmationDialog("Close setup?", isPresented: $closing, titleVisibility: .visible) {
            Button("Close setup", role: .destructive) { password = ""; dismiss() }
            Button("Keep checking", role: .cancel) { }
        } message: {
            Text("A request already sent to the device may still finish. Closing stops checks here; it does not undo enrollment or Wi-Fi changes. Reopen setup to check the same device.")
        }
        #if os(iOS)
        .onChange(of: app.mobileCloudSelectionKey) { _, _ in password = ""; dismiss() }
        #endif
        .task {
            app.setupSessions += 1
            defer { app.setupSessions -= 1 }
            await withTaskGroup(of: Void.self) { group in
                group.addTask { await actions.run() }
                group.addTask { await watch() }
                await group.waitForAll()
            }
            password = ""
            await GoClient.shared.closeNearbySetup(state.session)
        }
    }

    private var headline: String {
        switch stage {
        case "attention": return "Setup needs attention."
        case "done": return state.completed ? "You’re all set." : "Already part of Wendy."
        case "discovering": return observationError == nil ? "Choose a device" : "Bluetooth unavailable"
        case "connecting": return "Connecting to your Wendy…"
        case "working":
            if !state.attempted && !state.pending { return progress }
            return state.attempted && !editingLocked ? "Checking enrollment…" : "Making it yours…"
        default: return flow.step == .network ? "Connect to Wi-Fi" : "Name & organization"
        }
    }

    private var explanation: String {
        switch stage {
        case "attention": return state.failure
        case "done": return "Your device is enrolled and ready to open."
        case "discovering": return "Power on your device and keep it nearby. We’ll find it automatically."
        case "connecting": return "Keep your device nearby. We’re taking care of the connection."
        case "working":
            if !state.attempted && !state.pending && progress == "Connecting to Wi-Fi…" { return "We’ll continue when your device is connected." }
            if state.attempted && !state.pending && !editingLocked {
                return "We haven’t received confirmation yet. Your enrollment won’t be sent twice."
            }
            return state.wifiConnected
                ? "Your device is joining your organization. We’ll keep checking automatically."
                : "Your device needs internet to finish. Connect it to Wi-Fi below, or plug in Ethernet. We’ll keep checking automatically."
        default:
            if flow.step == .network { return "Credentials are sent over Bluetooth. You can also use an Ethernet connection." }
            if let choice { return "Add this device to \(choice.name)." }
            return organizations.isEmpty ? "Sign in to add this device to your organization." : "Choose the organization this device belongs to."
        }
    }

    @ViewBuilder private var confirmation: some View {
        VStack(spacing: 20) {
            if let organizationError {
                Text(organizationError).foregroundStyle(.secondary)
                Button("Retry organizations") { actions.submit { await loadOrganizations() } }
            } else if organizations.isEmpty {
                primaryButton("Sign in", symbol: "arrow.right") { signIn(); dismiss() }
            } else {
                TextField("Device name", text: $name).textFieldStyle(.roundedBorder).autocorrectionDisabled()
                    #if os(iOS)
                    .textInputAutocapitalization(.never)
                    #endif
                if !nameValid { Text("Use a lowercase name with letters, numbers, and hyphens.").font(.caption).foregroundStyle(.secondary) }
                organizationPicker
                primaryButton(choice.map { "Enroll in \($0.name)" } ?? "Enroll device", symbol: "arrow.right") { submit(.enroll) }
                    .disabled(choice == nil || !nameValid || editingLocked)
                Text("Home place assignment is not available during setup yet.").font(.caption).foregroundStyle(.secondary)

            }
            Button("Back to connection") { flow.backToNetwork(); wifiExpanded = true }
                .disabled(editingLocked)
        }.tint(.accentColor)
    }

    private var stepIndicator: some View {
        let step = state.pending || state.attempted || editingLocked && progress == "Preparing enrollment…" ? NearbySetupFlow.Step.enroll : selected == nil ? .device : flow.step
        return VStack(spacing: 8) {
            Text("Step \(step.rawValue) of 4 · \(step.title)").font(.subheadline.weight(.semibold))
            HStack(spacing: 6) {
                ForEach(1...4, id: \.self) { number in
                    Capsule().fill(number <= step.rawValue ? Color.accentColor : Color.secondary.opacity(0.18)).frame(height: 4)
                }
            }.accessibilityHidden(true)
        }.accessibilityIdentifier("setup.steps")
    }

    private var discovery: some View {
        VStack(spacing: 16) {
            if !scanned { ProgressView("Scanning over Bluetooth…") }
            if let observationError {
                Text(PresentedFailure(observationError).message).foregroundStyle(.secondary)
                #if os(iOS)
                Button("Open Settings") {
                    if let url = URL(string: UIApplication.openSettingsURLString) { UIApplication.shared.open(url) }
                }
                #endif
            } else if scanned && devices.isEmpty {
                Text("No nearby devices found. Keep the device powered on and within a few metres, then scan again.").foregroundStyle(.secondary)
            }
            ForEach(devices) { device in
                Button { candidate = device } label: {
                    HStack(spacing: 14) {
                        DeviceEmblem(size: 40)
                        VStack(alignment: .leading, spacing: 4) {
                            Text(device.title).font(.headline)
                            Text(device.rssi < 0 ? "Bluetooth signal \(device.rssi) dBm" : "Bluetooth signal unavailable")
                                .font(.caption).foregroundStyle(.secondary)
                        }
                        Spacer()
                        Image(systemName: candidate?.id == device.id ? "checkmark.circle.fill" : "circle")
                    }.padding(16).background(.quaternary.opacity(0.5), in: RoundedRectangle(cornerRadius: 20))
                }.buttonStyle(.plain)
            }
            Button("Scan again") { actions.submit { await discover() } }.disabled(busy)
            primaryButton("Continue", symbol: "arrow.right") {
                if let candidate { actions.submit { await choose(candidate) } }
            }.disabled(candidate == nil || busy || !devices.contains { $0.id == candidate?.id })
        }
    }

    private var networkStep: some View {
        VStack(spacing: 20) {
            wifiControls
            if state.wifiConnected {
                primaryButton("Continue", symbol: "arrow.right") { flow.continueFromNetwork(connected: true) }
            } else {
                Button("Use Ethernet instead") { flow.continueFromNetwork(connected: false, useEthernet: true) }
                Text("Ethernet connectivity will be checked when the device enrolls.").font(.caption).foregroundStyle(.secondary)
            }
            Button("Choose another device") { actions.submit { await chooseAnother() } }.disabled(editingLocked)
        }
    }

    private func chooseAnother() async {
        guard !busy, !state.attempted, !state.pending, !editingLocked else { return }
        busy = true
        defer { busy = false }
        await GoClient.shared.closeNearbySetup(state.session)
        guard !Task.isCancelled else { return }
        selected = nil; candidate = nil; state = NearbySetupSnapshot(); flow = NearbySetupFlow()
        password = ""; ssid = ""; error = nil; observationError = nil
        nextReconnect = .distantPast; reconnectDelay = 3
    }

    private var wifiControls: some View {
        DisclosureGroup(state.wifiConnected ? "Wi-Fi connected" : "Connect to Wi-Fi", isExpanded: $wifiExpanded) {
            VStack(alignment: .leading, spacing: 14) {
                if state.wifiConnected { Label(state.ssid, systemImage: "wifi").foregroundStyle(.secondary) }
                if !state.networks.isEmpty {
                    Picker("Network", selection: $ssid) {
                        Text("Choose network").tag("")
                        if !ssid.isEmpty && !state.networks.contains(ssid) { Text(ssid).tag(ssid) }
                        ForEach(Array(Set(state.networks)).sorted(), id: \.self) { Text($0).tag($0) }
                    }
                } else if lastWiFiScan == .distantPast { ProgressView("Finding networks…") }
                else { Text("No networks found. Enter a network name or scan again.").font(.caption).foregroundStyle(.secondary) }
                Button("Scan networks") { submit(.scanWiFi) }.disabled(!state.connected || editingLocked)
                TextField("Network name", text: $ssid).textFieldStyle(.roundedBorder).autocorrectionDisabled()
                    #if os(iOS)
                    .textInputAutocapitalization(.never)
                    #endif
                SecureField("Password", text: $password).textFieldStyle(.roundedBorder)
                Button("Connect") { submit(.connectWiFi) }.buttonStyle(.borderedProminent).disabled(!NearbySetupFlow.validWiFi(ssid: ssid, password: password) || !state.connected || editingLocked)
            }.padding(.top, 12)
        }
        .font(.subheadline).disabled(editingLocked)
    }

    private var organizationPicker: some View {
        Picker("Organization", selection: $organization) {
            Text("Choose organization").tag("")
            ForEach(organizations) { Text($0.title).tag($0.id) }
        }.pickerStyle(.menu)
    }

    private func primaryButton(_ title: String, symbol: String, action: @escaping () -> Void) -> some View {
        Button(action: action) {
            HStack { Spacer(); Text(title).fontWeight(.semibold); Image(systemName: symbol); Spacer() }
                .padding(.vertical, 10)
        }.buttonStyle(.borderedProminent).controlSize(.large)
    }

    private func finish() async {
        guard !opening else { return }
        opening = true
        error = nil
        defer { opening = false }
        // Show feedback before waiting for a background observation to finish.
        while busy {
            do { try await Task.sleep(for: .milliseconds(100)) } catch { return }
        }
        guard !Task.isCancelled else { return }
        busy = true; automatic = true
        defer { busy = false; automatic = false }
        // Match the identifier, never a potentially duplicated display name.
        if openKnownDevice() { return }
        await app.refreshCloud()
        if openKnownDevice() { return }
        await app.refreshNearby()
        if openKnownDevice() { return }
        error = "Your device is not in the device list yet. Keep it online, then open it again."
    }

    private func openKnownDevice() -> Bool {
        guard !state.deviceID.isEmpty, let device = (app.cloud + app.nearby).first(where: {
            $0.id == state.deviceID || ($0.selector.hasPrefix("cloud://") && URL(string: $0.selector)?.lastPathComponent == state.deviceID)
        }) else { return false }
        app.requestedDevice = device
        dismiss()
        return true
    }

    private func discover() async {
        guard !busy else { return }
        busy = true; automatic = true
        defer { busy = false; automatic = false }
        do {
            let found = try await GoClient.shared.discoverSetupDevices()
            guard !Task.isCancelled else { return }
            devices = found; scanned = true
            if let candidate, !found.contains(where: { $0.id == candidate.id }) { self.candidate = nil }
            observationError = nil
        } catch { if !Task.isCancelled { devices = []; candidate = nil; scanned = true; observationError = error.localizedDescription } }
    }

    private func choose(_ device: NearbySetupDevice) async {
        while busy {
            do { try await Task.sleep(for: .milliseconds(100)) } catch { return }
        }
        guard selected == nil, !Task.isCancelled, devices.contains(where: { $0.id == device.id }) else { return }
        name = DeviceEnrollmentForm.suggestedName(device.title)
        flow.choseDevice(); wifiExpanded = true
        await connect(device)
    }

    private func connect(_ device: NearbySetupDevice) async {
        guard !busy else { return }
        busy = true; automatic = true; selected = device
        defer { busy = false; automatic = false }
        do {
            let result = try await GoClient.shared.openNearbySetup(device.id)
            guard !Task.isCancelled else { await GoClient.shared.closeNearbySetup(result.session); return }
            state = result
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
            let result = try await GoClient.shared.nearbySetupAction(action, session: state.session, ssid: ssid, password: password,
                account: choice?.account ?? "", organization: choice?.organizationID ?? "", name: name)
            guard !Task.isCancelled else { return }
            state = result
            if automatically || state.pending { observationError = state.error }
            else if let detail = state.error, ConnectionRecovery.canRetry(detail) { observationError = detail }
            else {
                error = state.error
            }
            if (state.pending || state.canResume) && !state.wifiConnected && action == .enroll { wifiExpanded = true }
            if state.completed { error = nil; observationError = nil }
        } catch {
            if !Task.isCancelled {
                if automatically { observationError = error.localizedDescription }
                else if ConnectionRecovery.canRetry(error.localizedDescription) { observationError = error.localizedDescription }
                else { self.error = error.localizedDescription }
            }
        }
    }

    private func loadOrganizations() async {
        do {
            let loaded = try await GoClient.shared.enrollmentOrganizations()
            guard !Task.isCancelled else { return }
            organizations = loaded
            organizationError = nil
            #if os(iOS)
            let current = app.mobileCloudAccount
            #else
            let current = app.account?.context ?? ""
            #endif
            organization = DeviceEnrollmentForm.defaultOrganization(organizations, currentAccount: current)
        }
        catch { if !Task.isCancelled { organizationError = error.localizedDescription } }
        organizationsLoaded = true
    }

    private func watch() async {
        if let initialDevice {
            name = DeviceEnrollmentForm.suggestedName(initialDevice.title)
            selected = initialDevice
            flow.choseDevice(); wifiExpanded = true
        }
        await loadOrganizations()
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
