#if os(macOS)
import SwiftUI

struct UnitreeControlSession { let handle: UInt64; let name: String }

struct UnitreePairingView: View {
    let session: UInt64
    let deviceName: String
    var initialRobot: UnitreeRobot?
    @State private var robots: [UnitreeRobot] = []
    @State private var loading = false
    @State private var error: String?
    @State private var warning = ""
    @State private var editing: UnitreeRobot?
    @State private var forgetting: UnitreeRobot?
    @State private var control: UnitreeControlSession?
    var body: some View {
        VStack(alignment: .leading, spacing: 18) {
            if let editing {
                UnitreeConnectionForm(session: session, deviceName: deviceName, robot: editing, cancel: { self.editing = nil }) { handle, name in
                    self.editing = nil
                    if handle != 0 { control = UnitreeControlSession(handle: handle, name: name) }
                    else { Task { await refresh() } }
                }.id(editing.id)
            } else if let control {
                UnitreeRemoteControlView(connection: control) { self.control = nil }
            } else {
                VStack(alignment: .leading, spacing: 16) {
                    VStack(alignment: .leading, spacing: 6) {
                        Text("Unitree Go2").font(.title2.bold())
                        Text(session == 0 ? "Connect directly from this Mac to a Go2 on the same network." : "Pair a Go2 on \(deviceName)'s network. Its robot key is saved on that device.")
                            .foregroundStyle(.secondary)
                    }
                    HStack {
                        Button("Enter IP address") { editing = .manual }
                        Button("Scan again") { Task { await refresh() } }.disabled(loading)
                        if loading { ProgressView().controlSize(.small) }
                    }
                }
                if let error { FailureNote(message: error) }
                if !warning.isEmpty { Label(warning, systemImage: "exclamationmark.triangle").foregroundStyle(.orange) }
                if robots.isEmpty && !loading {
                    ContentUnavailableView("No Go2 robots found", systemImage: "network", description: Text("Connect your Go2 to the same network, then scan again.\nYou can also enter its serial number and IPv4 address."))
                        .frame(maxWidth: .infinity, maxHeight: .infinity)
                } else {
                    List(robots) { robot in
                        HStack(spacing: 14) {
                            RobotIcon().font(.title2).foregroundStyle(.secondary)
                            VStack(alignment: .leading, spacing: 4) {
                                Text(robot.name).fontWeight(.medium)
                                Text("\(robot.serial) · \(robot.address)").font(.caption).foregroundStyle(.secondary)
                            }
                            Spacer()
                            if robot.paired { Text(robot.connected ? "Connected" : "Paired").foregroundStyle(robot.connected ? .green : .secondary) }
                            Button(session == 0 ? "Remote control" : robot.paired ? "Reconnect" : "Pair") { editing = robot }
                            if robot.paired {
                                Button("Forget", role: .destructive) { forgetting = robot }
                            }
                        }.padding(.vertical, 8)
                    }.listStyle(.plain)
                }
                Text("Direct LAN control supports Go2. G1 devices use the Wendy agent installation flow.").font(.caption).foregroundStyle(.secondary)
            }
        }
        .padding(24)
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
        .task(id: session) { if let initialRobot { robots = [initialRobot]; editing = initialRobot }; await refresh() }
        .focusedSceneValue(\.devicePaneRefresh, PaneRefreshAction(enabled: !loading && control == nil && editing == nil, perform: { Task { await refresh() } }))
        .alert("Forget Unitree pairing?", isPresented: Binding(get: { forgetting != nil }, set: { if !$0 { forgetting = nil } }), presenting: forgetting) { robot in
            Button("Cancel", role: .cancel) {}
            Button("Forget", role: .destructive) {
                Task {
                    do { try await UnitreeClient.shared.forget(session: session, serial: robot.serial); await refresh() }
                    catch { self.error = error.localizedDescription }
                }
            }
        } message: { robot in Text("Remove \(robot.name) and its saved robot key from \(deviceName)?") }
    }
    private func refresh() async {
        guard !loading else { return }
        loading = true; error = nil
        defer { loading = false }
        do {
            let result = try await UnitreeClient.shared.robots(session: session)
            guard !Task.isCancelled else { return }
            robots = result.robots; warning = result.warning
            if let initialRobot, !robots.contains(where: { $0.id == initialRobot.id }) { robots.insert(initialRobot, at: 0) }
        } catch { if !Task.isCancelled { self.error = error.localizedDescription } }
    }
}

private struct UnitreeConnectionForm: View {
    @EnvironmentObject private var app: AppModel
    let session: UInt64
    let deviceName: String
    let robot: UnitreeRobot
    let cancel: () -> Void
    let saved: (UInt64, String) -> Void
    private enum Field: Hashable { case serial, address, email, password }
    @FocusState private var focusedField: Field?
    @State private var serial = ""
    @State private var address = ""
    @State private var email = ""
    @State private var password = ""
    @State private var region = "global"
    @State private var saving = false
    @State private var attempt: UUID?
    @State private var visible = false
    @State private var error: String?
    var body: some View {
        VStack(alignment: .leading, spacing: 18) {
            Text(session == 0 ? "Connect to Go2" : "Pair Go2 with \(deviceName)").font(.title2.bold())
            Form {
                TextField("Robot serial number", text: $serial).disabled(!robot.serial.isEmpty)
                    .focused($focusedField, equals: .serial)
                TextField("Robot IPv4 address", text: $address)
                    .focused($focusedField, equals: .address)
                Picker("Unitree account region", selection: $region) {
                    Text("Global").tag("global"); Text("China").tag("cn")
                }
                TextField("Unitree email", text: $email)
                    .focused($focusedField, equals: .email)
                SecureField("Unitree password", text: $password)
                    .focused($focusedField, equals: .password)
            }.textFieldStyle(.roundedBorder).disabled(saving)
            Text("Use the account bound to this robot in the Unitree Go app. Leave login blank to try an existing pairing or firmware that does not require a key.")
                .font(.callout).foregroundStyle(.secondary)
            if let error {
                Label(error, systemImage: "exclamationmark.triangle")
                    .font(.callout).foregroundStyle(.red)
                    .fixedSize(horizontal: false, vertical: true)
                    .textSelection(.enabled)
            }
            HStack {
                if saving { ProgressView(session == 0 ? "Connecting to Go2…" : "Pairing Go2…").controlSize(.small) }
                Spacer()
                Button("Cancel") {
                    if saving { attempt = nil; saving = false }
                    else { cancel() }
                }.keyboardShortcut(.cancelAction)
                Button(session == 0 ? "Connect" : "Pair") { save() }
                    .keyboardShortcut(.defaultAction).disabled(saving || serial.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty || address.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
            }
        }
        .frame(maxWidth: 520, alignment: .leading)
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .top)
        .interactiveDismissDisabled(saving)
        .onAppear { visible = true; serial = robot.serial; address = robot.address }
        .task { focusedField = robot.serial.isEmpty ? .serial : .address }
        .task(id: attempt) {
            if let attempt { await connect(attempt: attempt) }
        }
        .onDisappear { visible = false; password = "" }
    }
    private func save() {
        saving = true; error = nil
        attempt = UUID()
    }
    private func connect(attempt currentAttempt: UUID) async {
        defer { if attempt == currentAttempt { saving = false } }
        do {
            let handle = try await UnitreeClient.shared.connect(session: session, serial: serial.trimmingCharacters(in: .whitespacesAndNewlines), name: robot.name, address: address.trimmingCharacters(in: .whitespacesAndNewlines), region: region, email: email.trimmingCharacters(in: .whitespacesAndNewlines), password: password)
            guard visible, !Task.isCancelled, attempt == currentAttempt else { if handle != 0 { try? await UnitreeClient.shared.close(handle: handle) }; return }
            if handle != 0 {
                app.rememberUnitree(UnitreeRobot(serial: serial.trimmingCharacters(in: .whitespacesAndNewlines), name: robot.name, address: address.trimmingCharacters(in: .whitespacesAndNewlines), paired: false, connected: false))
            }
            password = ""
            saved(handle, robot.name)
        } catch {
            if visible && !Task.isCancelled && attempt == currentAttempt { self.error = error.localizedDescription }
        }
    }
}

private struct UnitreeRemoteControlView: View {
    let connection: UnitreeControlSession
    let disconnected: () -> Void
    @Environment(\.scenePhase) private var scenePhase
    @StateObject private var motion: UnitreeMotionDriver
    @State private var closing = false
    init(connection: UnitreeControlSession, disconnected: @escaping () -> Void) {
        self.connection = connection
        self.disconnected = disconnected
        _motion = StateObject(wrappedValue: UnitreeMotionDriver(handle: connection.handle))
    }
    var body: some View {
        VStack(spacing: 12) {
            HStack {
                VStack(alignment: .leading, spacing: 6) {
                    Text(connection.name).font(.title2.bold())
                    Text("Remote control from this Mac").foregroundStyle(.secondary)
                }
                Spacer()
            }
            UnitreeCameraView(handle: connection.handle).frame(minHeight: 160, idealHeight: 220, maxHeight: 220)
            Text("Stand Go2 with its remote first. Keep it in sight and the remote at hand.")
                .foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            Toggle("Enable movement", isOn: $motion.enabled).toggleStyle(.switch)
                .onChange(of: motion.enabled) { _, value in if value { motion.error = nil } else { stop() } }
            VStack(spacing: 8) {
                hold("Forward", icon: "arrow.up", forward: 0.6)
                HStack(spacing: 8) {
                    hold("Left", icon: "arrow.left", lateral: 0.3)
                    Button("Stop") { motion.stop(disable: true) }.buttonStyle(.borderedProminent).tint(.red)
                        .keyboardShortcut(.space, modifiers: []).frame(width: 100, height: 34)
                    hold("Right", icon: "arrow.right", lateral: -0.3)
                }
                hold("Backward", icon: "arrow.down", forward: -0.6)
                HStack(spacing: 8) {
                    hold("Turn left", icon: "arrow.counterclockwise", yaw: 0.5)
                    hold("Turn right", icon: "arrow.clockwise", yaw: -0.5)
                }
            }.disabled(closing)
            Text("Hold WASD / arrows to move · Q/E to turn · Space to stop").font(.caption).foregroundStyle(.secondary)
            if let error = motion.error {
                Label(error, systemImage: "exclamationmark.triangle").font(.callout).foregroundStyle(.red)
                    .fixedSize(horizontal: false, vertical: true)
            }
            Spacer(minLength: 0)
            HStack {
                Spacer()
                Button("Disconnect") { close() }.disabled(closing)
            }
        }
        .background(UnitreeKeyboard(enabled: motion.enabled && !closing, changed: { motion in
            if motion == UnitreeMotion() { stop() }
            else { startMovement("Keyboard", motion: motion) }
        }, stop: { motion.stop(disable: true) }, suspend: { stop() }).frame(width: 0, height: 0))
        .task { await motion.run() }
        .onChange(of: scenePhase) { _, phase in if phase != .active { stop() } }
        .onDisappear { motion.stop(); Task { try? await UnitreeClient.shared.close(handle: connection.handle) } }
    }
    private func hold(_ title: String, icon: String, forward: Double = 0, lateral: Double = 0, yaw: Double = 0) -> some View {
        Label(title, systemImage: icon).frame(width: 110, height: 34)
            .background(motion.direction == title ? Color.accentColor.opacity(0.25) : Color.primary.opacity(0.07), in: RoundedRectangle(cornerRadius: 10))
            .opacity(motion.enabled ? 1 : 0.4)
            .accessibilityLabel("Hold to move \(title.lowercased())")
            .gesture(DragGesture(minimumDistance: 0)
                .onChanged { value in
                    guard motion.enabled, !closing else { return }
                    guard (0...110).contains(value.location.x), (0...34).contains(value.location.y) else { stop(); return }
                    guard motion.direction != title else { return }
                    startMovement(title, motion: UnitreeMotion(forward: forward, lateral: lateral, yaw: yaw))
                }
                .onEnded { _ in stop() })
    }
    private func startMovement(_ title: String, motion desired: UnitreeMotion) {
        guard !closing else { return }
        motion.update(desired, direction: title)
    }
    private func stop() { motion.stop() }
    private func close() {
        closing = true; motion.stop(disable: true)
        Task {
            do { try await UnitreeClient.shared.close(handle: connection.handle); disconnected() }
            catch { motion.error = "Disconnected, but Stop could not be confirmed. Use the physical remote."; closing = false }
        }
    }
}
#endif
