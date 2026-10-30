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
    @State private var selected: NearbySetupDevice?
    @State private var state = NearbySetupSnapshot()
    @State private var organizations: [EnrollmentOrganization] = []
    @State private var organization = ""
    @State private var name = ""
    @State private var ssid = ""
    @State private var password = ""
    @State private var wifiExpanded = false
    @State private var customizing = false
    @State private var organizationsLoaded = false
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
                        case "discovering":
                            ProgressView().controlSize(.large)
                            ForEach(devices) { device in
                                Button { actions.submit { await choose(device) } } label: {
                                    HStack(spacing: 14) {
                                        DeviceEmblem(size: 40)
                                        Text(device.title).font(.headline)
                                        Spacer()
                                        Image(systemName: "chevron.right").font(.caption.weight(.semibold))
                                    }.padding(16).background(.quaternary.opacity(0.5), in: RoundedRectangle(cornerRadius: 20))
                                }.buttonStyle(.plain)
                            }
                        case "connecting": ProgressView().controlSize(.large)
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
                        default: confirmation
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
                    Button { dismiss() } label: { Image(systemName: "xmark") }
                        .accessibilityLabel("Close setup").disabled(editingLocked)
                }
            }
        }
        #if os(macOS)
        .frame(width: 500, height: 640)
        #endif
        .interactiveDismissDisabled(editingLocked)
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
        case "discovering": return "Find your Wendy."
        case "connecting": return "Connecting to your Wendy…"
        case "working":
            if !state.attempted && !state.pending { return progress }
            return state.attempted && !editingLocked ? "Checking enrollment…" : "Making it yours…"
        default: return "Make it yours."
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
            if let choice { return "Add this device to \(choice.name)." }
            return organizations.isEmpty ? "Sign in to add this device to your organization." : "Choose the organization this device belongs to."
        }
    }

    @ViewBuilder private var confirmation: some View {
        VStack(spacing: 20) {
            if organizations.isEmpty {
                primaryButton("Sign in", symbol: "arrow.right") { signIn(); dismiss() }
            } else {
                if choice == nil { organizationPicker }
                primaryButton(choice.map { "Add to \($0.name)" } ?? "Add device", symbol: "arrow.right") { submit(.enroll) }
                    .disabled(choice == nil || !nameValid || editingLocked)
                DisclosureGroup("Name & organization", isExpanded: $customizing) {
                    VStack(alignment: .leading, spacing: 16) {
                        TextField("Device name", text: $name).textFieldStyle(.roundedBorder).autocorrectionDisabled()
                        if !nameValid { Text("Use a lowercase name with letters, numbers, and hyphens.").font(.caption).foregroundStyle(.secondary) }
                        organizationPicker
                    }.padding(.top, 12)
                }
            }
            wifiControls
        }.tint(.accentColor)
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
                } else { ProgressView("Finding networks…") }
                TextField("Network name", text: $ssid).textFieldStyle(.roundedBorder).autocorrectionDisabled()
                SecureField("Password", text: $password).textFieldStyle(.roundedBorder)
                Button("Connect") { submit(.connectWiFi) }.buttonStyle(.borderedProminent).disabled(ssid.isEmpty || editingLocked)
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
            else if let detail = state.error, ConnectionRecovery.canRetry(detail) { observationError = detail }
            else {
                error = state.error
                if state.error?.contains("name is already in use") == true { customizing = true }
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

    private func watch() async {
        if let initialDevice {
            name = DeviceEnrollmentForm.suggestedName(initialDevice.title)
            selected = initialDevice
        }
        do {
            organizations = try await GoClient.shared.enrollmentOrganizations()
            #if os(iOS)
            let current = app.mobileCloudAccount
            #else
            let current = app.account?.context ?? ""
            #endif
            organization = DeviceEnrollmentForm.defaultOrganization(organizations, currentAccount: current)
        }
        catch { if !Task.isCancelled { self.error = error.localizedDescription } }
        organizationsLoaded = true
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
