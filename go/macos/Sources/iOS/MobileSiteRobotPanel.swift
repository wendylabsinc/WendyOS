#if os(iOS)
import SwiftUI

struct MobileSiteRobotPanel: View {
    @ObservedObject var model: SiteRobotMapModel
    let session: UInt64
    let gatewayName: String
    let site: Site
    let modelURL: URL
    @Binding var isPresented: Bool
    @Environment(\.scenePhase) private var scenePhase
    @State private var pairing = false
    @State private var expanded = false

    // Keep the session owner on the map. Dismissing the controls must not tear
    // down telemetry, discard localization, or restart the robot connection.
    var body: some View {
        HStack {
            Button("Go2", systemImage: "pawprint") {
                model.selected = true
                isPresented = true
            }.buttonStyle(.bordered)
            Spacer(minLength: 8)
            if !model.robots.isEmpty {
                Button("Stop", systemImage: "stop.fill") { model.stop() }
                    .buttonStyle(.borderedProminent).tint(.red)
            }
        }
        .background {
            // Visual localization still needs fresh frames while the map is
            // visible. Only one camera consumer is mounted at a time.
            if !isPresented, !model.serial.isEmpty {
                SiteRobotCameraView(handle: model.handle, onFrame: model.acceptCameraFrame, onDecoder: model.acceptCameraDecoder)
                    .frame(width: 1, height: 1).hidden().accessibilityHidden(true)
            }
        }
        .task { await model.actions.run() }
        .task(id: session) { await model.load(session: session) }
        .task(id: "\(session)/\(model.serial)/\(site.id)/\(site.modelRevision)/\(scenePhase)/\(model.reconnectGeneration)") {
            await model.observe(session: session, site: site, modelURL: modelURL, active: scenePhase == .active)
        }
        .sheet(isPresented: $isPresented) {
            NavigationStack {
                ScrollView { controlsContent.padding(20) }
                    .navigationTitle("Go2")
                    .navigationBarTitleDisplayMode(.inline)
                    .toolbar {
                        ToolbarItem(placement: .cancellationAction) {
                            Button("Stop", systemImage: "stop.fill") { model.stop() }
                                .buttonStyle(.borderedProminent).tint(.red)
                                .disabled(model.robots.isEmpty)
                        }
                        ToolbarItem(placement: .confirmationAction) {
                            Button("Done") { isPresented = false }
                        }
                    }
            }
            .presentationDetents([.medium, .large])
            .presentationDragIndicator(.visible)
            .presentationBackgroundInteraction(.enabled(upThrough: .medium))
            .sheet(isPresented: $pairing, onDismiss: { model.actions.submit { await model.load(session: session) } }) {
                MobileGo2PairingView(session: session, gatewayName: gatewayName)
            }
        }
    }

    private var controlsContent: some View {
        VStack(alignment: .leading, spacing: 10) {
            if model.robots.isEmpty {
                Text("Pair Go2 with \(gatewayName) to locate it on this floor.").font(.caption).foregroundStyle(.secondary)
                Button("Pair Go2", systemImage: "link") { pairing = true }
                    .buttonStyle(.borderedProminent).disabled(session == 0)
            } else {
                SiteRobotCameraView(handle: model.handle, onFrame: model.acceptCameraFrame, onDecoder: model.acceptCameraDecoder)
                    .frame(height: 180)
                if model.robots.count > 1 {
                    Picker("Robot", selection: $model.serial) {
                        ForEach(model.robots) { Text($0.name.isEmpty ? $0.serial : $0.name).tag($0.serial) }
                    }
                }
                SiteRobotStatus(model: model)
                if model.placing {
                    HStack {
                        Text(model.anchor == nil ? "Tap the floor beneath Go2." : "Tap directly ahead to set its heading.")
                            .font(.callout)
                        Button("Cancel") { model.placing = false; model.anchor = nil }.buttonStyle(.bordered)
                    }
                } else {
                    HStack {
                        if model.candidates.isEmpty {
                            Button("Locate Go2", systemImage: "scope") { model.findPosition() }
                                .disabled(model.reading?.pose != true || model.localizing || model.reading?.moving == true || model.lensCalibrating)
                        }
                        if model.destination != nil {
                            Button("Move here", systemImage: "location.fill") { model.command(move: true) }
                                .buttonStyle(.borderedProminent).tint(.green)
                                .disabled(model.reading?.ready != true || model.reading?.route.isEmpty != false)
                        }
                    }.buttonStyle(.bordered)
                }
                SiteRobotLocalizationControls(model: model)
                RobotLocalizationDiagnostics(model: model)
                SiteRobotSupervisionSummary(model: model)
                DisclosureGroup("Settings and help", isExpanded: $expanded) {
                    settingsContent
                }
            }
            if let error = model.error, error != model.reading?.message {
                Text(error).font(.caption).foregroundStyle(.orange).fixedSize(horizontal: false, vertical: true)
            }
        }
        .transaction { $0.animation = nil }
    }
    private var settingsContent: some View {
        VStack(alignment: .leading, spacing: 12) {
            HStack(spacing: 12) {
                Go2Preview().frame(width: 100, height: 90).clipShape(RoundedRectangle(cornerRadius: 10))
                VStack(alignment: .leading, spacing: 8) {
                    Text(gatewayName).font(.caption).foregroundStyle(.secondary)
                    Button("Manage pairing") {
                        model.stop(); pairing = true
                    }.disabled(session == 0)
                    Button("Reconnect") { model.reconnectGeneration += 1 }
                        .disabled(model.connecting || model.reading?.moving == true)
                }
            }
            Button("Place Manually") { model.beginAlignment(); isPresented = false }.disabled(model.reading?.pose != true)
            SiteRobotTimingControls(model: model)
            Text("Tap the floor to preview a route, then Move here. Position follows aligned odometry; LiDAR can retain old observations.")
                .font(.caption2).foregroundStyle(.secondary)
        }
        .buttonStyle(.bordered)
        .padding(.top, 8)
    }

}

private struct MobileGo2PairingView: View {
    let session: UInt64
    let gatewayName: String
    @Environment(\.dismiss) private var dismiss
    @State private var serial = ""
    @State private var address = ""
    @State private var email = ""
    @State private var password = ""
    @State private var region = "global"
    @State private var robots: [UnitreeRobot] = []
    @State private var attempt: UUID?
    @State private var saving = false
    @State private var error: String?

    var body: some View {
        NavigationStack {
            Form {
                Section("Pair with \(gatewayName)") {
                    if !robots.isEmpty {
                        ForEach(robots) { robot in
                            Button("\(robot.name) · \(robot.serial)") { serial = robot.serial; address = robot.address }
                        }
                    }
                    TextField("Robot serial number", text: $serial)
                    // IPv4 separators must remain available in comma-decimal locales.
                    TextField("Robot IPv4 address", text: $address).keyboardType(.numbersAndPunctuation)
                }
                Section {
                    Picker("Unitree region", selection: $region) { Text("Global").tag("global"); Text("China").tag("cn") }
                    TextField("Unitree email", text: $email).keyboardType(.emailAddress)
                    SecureField("Unitree password", text: $password)
                } footer: {
                    Text("Use the account bound to this Go2. Leave login blank to try an existing pairing. The robot key stays on the gateway.")
                }
                if let error { Section { Text(error).foregroundStyle(.red) } }
                Section {
                    Button("Pair Go2") { saving = true; error = nil; attempt = UUID() }
                        .disabled(saving || session == 0 || serial.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty || address.isEmpty)
                    if saving { ProgressView("Pairing…") }
                }
            }
            .textInputAutocapitalization(.never).autocorrectionDisabled()
            .navigationTitle("Pair Go2").navigationBarTitleDisplayMode(.inline)
            .toolbar { ToolbarItem(placement: .cancellationAction) { Button("Cancel") { dismiss() } } }
        }
        .task {
            do { robots = try await UnitreeClient.shared.robots(session: session).robots }
            catch { if !Task.isCancelled { self.error = error.localizedDescription } }
        }
        .task(id: attempt) {
            guard attempt != nil, session != 0 else { return }
            defer { saving = false; password = "" }
            do {
                _ = try await UnitreeClient.shared.connect(session: session,
                    serial: serial.trimmingCharacters(in: .whitespacesAndNewlines), name: "Unitree Go2",
                    address: address.trimmingCharacters(in: .whitespacesAndNewlines), region: region,
                    email: email.trimmingCharacters(in: .whitespacesAndNewlines), password: password)
                if !Task.isCancelled { dismiss() }
            } catch { if !Task.isCancelled { self.error = error.localizedDescription } }
        }
        .onDisappear { password = "" }
    }
}
#endif
