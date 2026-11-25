import SwiftUI

/// Wi-Fi for an already-enrolled device over its authenticated Bluetooth
/// management door. The section owns its backend session: it connects while
/// visible and the app is active, recovers lost links with backoff, and
/// releases the connection when it leaves. Credentials are sent only on an
/// explicit action and confirmed by reading the device's Wi-Fi status back.
struct NearbyWiFiSection: View {
    @EnvironmentObject private var app: AppModel
    @Environment(\.scenePhase) private var scenePhase
    let device: NearbySetupDevice
    @Binding var deviceID: String
    @State private var actions = AsyncActions()
    @State private var state = NearbyWiFiSnapshot()
    @State private var ssid = ""
    @State private var password = ""
    @State private var busy = false
    @State private var automatic = false
    @State private var progress = ""
    @State private var error: String?
    @State private var observationError: String?
    @State private var notice: String?
    @State private var canReconnect = true
    @State private var confirmingForget = false
    @State private var lastStatus = Date.distantPast
    @State private var lastWiFiScan = Date.distantPast
    @State private var nextReconnect = Date.distantPast
    @State private var reconnectDelay: TimeInterval = 3
    private var editingLocked: Bool { busy && !automatic }
    private var account: String {
        #if os(iOS)
        app.mobileCloudAccount
        #else
        app.account?.context ?? ""
        #endif
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Label(state.connected ? "Bluetooth connected" : canReconnect ? "Connecting over Bluetooth…" : "Not connected",
                  systemImage: state.connected ? "lock.shield" : "antenna.radiowaves.left.and.right")
                .font(.subheadline.weight(.semibold))
            if state.connected {
                controls
            } else if let observationError {
                Text(PresentedFailure(observationError).message).font(.callout).foregroundStyle(.secondary)
                if PresentedFailure(observationError).offersDeviceSignIn {
                    Text("The device accepts operator certificates of its organization only.").font(.caption).foregroundStyle(.secondary)
                }
                Button("Retry connection") { canReconnect = true; nextReconnect = .distantPast; actions.submit { await connect() } }
                    .disabled(busy)
            } else {
                ProgressView().controlSize(.small)
            }
            if let detail = observationError ?? error {
                DisclosureGroup("Details") {
                    Text(PresentedFailure(detail).details).font(.caption.monospaced())
                        .foregroundStyle(.secondary).textSelection(.enabled)
                }.font(.caption).tint(.secondary)
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(16).background(.quaternary.opacity(0.5), in: RoundedRectangle(cornerRadius: 20))
        .confirmationDialog("Forget \(state.ssid)?", isPresented: $confirmingForget, titleVisibility: .visible) {
            Button("Forget network", role: .destructive) { submit(.forgetWiFi, ssid: state.ssid) }
        } message: {
            Text("\(device.title) will no longer reconnect to this network automatically.")
        }
        .task {
            await withTaskGroup(of: Void.self) { group in
                group.addTask { await actions.run() }
                group.addTask { await watch() }
                await group.waitForAll()
            }
            password = ""
            await GoClient.shared.closeNearbyWiFi(state.session)
        }
    }

    @ViewBuilder private var controls: some View {
        if state.wifiConnected {
            Label(state.ssid, systemImage: "wifi").foregroundStyle(.secondary)
        } else {
            Label("Not connected to Wi-Fi", systemImage: "wifi.slash").foregroundStyle(.secondary)
        }
        if !state.networks.isEmpty {
            Picker("Network", selection: $ssid) {
                Text("Choose network").tag("")
                if !ssid.isEmpty && !state.networks.contains(ssid) { Text(ssid).tag(ssid) }
                ForEach(Array(Set(state.networks)).sorted(), id: \.self) { Text($0).tag($0) }
            }
        } else if lastWiFiScan == .distantPast { ProgressView("Finding networks…") }
        else { Text("No networks found. Enter a network name or scan again.").font(.caption).foregroundStyle(.secondary) }
        Button("Scan networks") { submit(.scanWiFi) }.disabled(editingLocked)
        TextField("Network name", text: $ssid).textFieldStyle(.roundedBorder).autocorrectionDisabled()
            #if os(iOS)
            .textInputAutocapitalization(.never)
            #endif
        SecureField("Password", text: $password).textFieldStyle(.roundedBorder)
        HStack(spacing: 12) {
            Button("Connect") { submit(.connectWiFi) }.buttonStyle(.borderedProminent)
                .disabled(!NearbySetupFlow.validWiFi(ssid: ssid, password: password) || editingLocked)
            if state.wifiConnected {
                Button("Disconnect") { submit(.disconnectWiFi) }.disabled(editingLocked)
                Button("Forget…") { confirmingForget = true }.disabled(editingLocked)
            }
        }
        Text("Credentials are sent over the device's authenticated Bluetooth connection. Its connection to Wendy may drop while switching networks.")
            .font(.caption).foregroundStyle(.secondary)
        if editingLocked { ProgressView(progress).controlSize(.small) }
        if let notice { Text(notice).font(.callout).foregroundStyle(.secondary) }
        if let error { Text(PresentedFailure(error).message).font(.callout).foregroundStyle(.secondary) }
    }

    private func connect() async {
        guard !busy else { return }
        busy = true; automatic = true
        defer { busy = false; automatic = false }
        do {
            let result = try await GoClient.shared.openNearbyWiFi(address: device.id, name: device.name, account: account)
            guard !Task.isCancelled else { await GoClient.shared.closeNearbyWiFi(result.session); return }
            state = result
            observationError = state.error
            lastStatus = .now; lastWiFiScan = .distantPast
            if !result.deviceID.isEmpty { deviceID = result.deviceID }
        } catch { if !Task.isCancelled { observationError = error.localizedDescription } }
        // A refusal (sign-in, identity, organization) is not fixed by retrying.
        if let observationError, !state.connected, !ConnectionRecovery.canRetry(observationError) { canReconnect = false }
        nextReconnect = .now.addingTimeInterval(reconnectDelay)
        reconnectDelay = state.connected ? 3 : min(reconnectDelay * 2, 30)
    }

    private func submit(_ action: NearbyWiFiAction, ssid: String? = nil) {
        actions.submit { await perform(action, ssid: ssid) }
    }

    private func perform(_ action: NearbyWiFiAction, ssid requested: String? = nil, automatically: Bool = false) async {
        // A tap during a background read must not be silently discarded.
        while busy && !automatically {
            do { try await Task.sleep(for: .milliseconds(100)) } catch { return }
        }
        guard !busy, !Task.isCancelled, state.session != 0, state.connected else { return }
        busy = true; automatic = automatically
        if !automatically { error = nil; notice = nil }
        let network = requested ?? ssid
        switch action {
        case .status: progress = "Checking connection…"
        case .scanWiFi: progress = "Finding networks…"
        case .connectWiFi: progress = "Connecting to Wi-Fi…"
        case .disconnectWiFi: progress = "Disconnecting…"
        case .forgetWiFi: progress = "Forgetting network…"
        }
        defer {
            busy = false; automatic = false
            if action == .connectWiFi { password = "" }
            if action == .scanWiFi { lastWiFiScan = .now } else { lastStatus = .now }
        }
        do {
            let result = try await GoClient.shared.nearbyWiFiAction(action, session: state.session, ssid: network, password: password)
            guard !Task.isCancelled else { return }
            state = result
            if automatically { observationError = state.error }
            else if let detail = state.error, ConnectionRecovery.canRetry(detail) { observationError = detail }
            else { error = state.error }
            if state.error == nil {
                observationError = nil
                switch action {
                case .connectWiFi: notice = "\(device.title) is connected to \(network)."
                case .disconnectWiFi: notice = "\(device.title) disconnected from Wi-Fi."
                case .forgetWiFi: notice = "\(device.title) forgot \(network)."
                default: break
                }
            }
        } catch {
            if !Task.isCancelled {
                if automatically || ConnectionRecovery.canRetry(error.localizedDescription) { observationError = error.localizedDescription }
                else { self.error = error.localizedDescription }
            }
        }
    }

    private func watch() async {
        while !Task.isCancelled {
            if !busy {
                if scenePhase != .active {
                    if state.connected { await GoClient.shared.closeNearbyWiFi(state.session); state.connected = false }
                } else {
                    switch state.observation(canReconnect: canReconnect, statusAge: Date.now.timeIntervalSince(lastStatus),
                                             scanAge: Date.now.timeIntervalSince(lastWiFiScan)) {
                    case .reconnect: if Date.now >= nextReconnect { await connect() }
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
