import SwiftUI

struct DeviceInfoSnapshot {
    let version, os, osVersion, architecture, board, serial, kernel, jetpack, cuda, l4t: String
    let uptime: UInt64
    let memory: Int64
    let cores: UInt32
}
struct DeviceUpdateSnapshot {
    let installed, latest, phase, detail: String
    let available: Bool
    var downloaded: Int64 = 0
    var total: Int64 = 0
    var downloadFraction: Double? { total > 0 && downloaded >= 0 ? min(1, Double(downloaded) / Double(total)) : nil }
    var busy: Bool { ["downloading", "installing", "restarting"].contains(phase) }
}
extension GoClient {
    func deviceInfo(session: UInt64) async throws -> DeviceInfoSnapshot {
        try await perform {
            guard let p = WDDeviceInfo(session) else { throw WendyError(message: "Missing device information.") }
            defer { WDDeviceInfoFree(p) }; try Self.check(p.pointee.status)
            let v = p.pointee
            return DeviceInfoSnapshot(version: Self.string(v.version), os: Self.string(v.os), osVersion: Self.string(v.os_version), architecture: Self.string(v.architecture), board: Self.string(v.board), serial: Self.string(v.serial), kernel: Self.string(v.kernel), jetpack: Self.string(v.jetpack), cuda: Self.string(v.cuda), l4t: Self.string(v.l4t), uptime: v.uptime, memory: v.memory, cores: v.cores)
        }
    }
    func onlineUpdate(session: UInt64, action: Int32, version: String = "") async throws -> DeviceUpdateSnapshot {
        try await perform {
            try Self.withCString(version) {
                guard let p = WDOnlineUpdate(session, action, $0) else { throw WendyError(message: "Missing update status.") }
                defer { WDOnlineUpdateFree(p) }; try Self.check(p.pointee.status)
                let v = p.pointee
                return DeviceUpdateSnapshot(installed: Self.string(v.installed), latest: Self.string(v.latest), phase: Self.string(v.phase), detail: Self.string(v.detail), available: v.available != 0, downloaded: v.downloaded, total: v.total)
            }
        }
    }
}
struct DeviceInfoView: View {
    let session: UInt64
    let deviceName: String
    @State private var info: DeviceInfoSnapshot?
    @State private var error: String?

    var body: some View {
        Form {
            Section {
                VStack(alignment: .leading, spacing: 6) {
                    Text(deviceName).font(.headline).textSelection(.enabled)
                    if let info {
                        if !info.board.isEmpty {
                            Text(info.board).font(.subheadline).foregroundStyle(.secondary)
                                .textSelection(.enabled)
                        }
                        let specs = hardwareSummary(info)
                        if !specs.isEmpty {
                            Text(specs).font(.subheadline).foregroundStyle(.secondary)
                        }
                    }
                }.padding(.vertical, 4)
                if info == nil && error == nil { ProgressView("Loading device info…") }
                if let error { FailureNote(message: error) }
            }
            if let info {
                Section("Operating system") {
                    row("Installed", info.osVersion.isEmpty ? info.os : info.osVersion)
                    NavigationLink("OS update") {
                        DeviceOSUpdateView(session: session, deviceName: deviceName)
                    }
                }
                Section {
                    NavigationLink("Advanced") {
                        Form {
                            Section("Hardware") {
                                row("Serial number", info.serial)
                                row("Architecture", info.architecture)
                            }
                            Section("Software") {
                                row("Operating system", info.os)
                                row("Kernel", info.kernel)
                                row("JetPack", info.jetpack)
                                row("CUDA", info.cuda)
                                row("L4T", info.l4t)
                                NavigationLink("Wendy Agent") {
                                    DeviceAgentUpdateView(session: session, deviceName: deviceName)
                                }
                            }
                        }
                        .formStyle(.grouped)
                        .navigationTitle("Advanced")
                    }
                }
            }
        }
        .formStyle(.grouped)
        .navigationTitle("Device info")
        .task(id: session) { await refresh() }
        .mobileRefreshable { await refresh() }
    }

    private func refresh() async {
        do {
            let snapshot = try await GoClient.shared.deviceInfo(session: session)
            guard !Task.isCancelled else { return }
            info = snapshot
            error = nil
        } catch {
            if !Task.isCancelled { self.error = error.localizedDescription }
        }
    }
    private func hardwareSummary(_ info: DeviceInfoSnapshot) -> String {
        var parts: [String] = []
        if info.cores > 0 { parts.append("\(info.cores.formatted()) CPU cores") }
        if info.memory > 0 {
            parts.append("\(ByteCountFormatter.string(fromByteCount: info.memory, countStyle: .memory)) memory")
        }
        return parts.joined(separator: " · ")
    }

    @ViewBuilder private func row(_ title: String, _ value: String) -> some View {
        if !value.isEmpty {
            ViewThatFits(in: .horizontal) {
                HStack(alignment: .firstTextBaseline) {
                    Text(title).fixedSize()
                    Spacer(minLength: 16)
                    Text(value).foregroundStyle(.secondary).fixedSize()
                }
                VStack(alignment: .leading, spacing: 4) {
                    Text(title)
                    Text(value).foregroundStyle(.secondary)
                        .fixedSize(horizontal: false, vertical: true)
                }
            }
            .textSelection(.enabled)
            .accessibilityElement(children: .combine)
        }
    }
}

struct DeviceAgentUpdateView: View {
    @State private var actions = AsyncActions()
    let session: UInt64
    let deviceName: String
    @State private var info: DeviceInfoSnapshot?
    @State private var update: DeviceUpdateSnapshot?
    @State private var error: String?
    @State private var updateError: String?
    @State private var checking = false
    @State private var starting = false
    @State private var expected = ""
    @State private var verified = false
    @State private var waiting = false
    @State private var confirmVersion: String?
    var body: some View {
        actionContent.task { await actions.run() }
    }

    @ViewBuilder private var actionContent: some View {
        Form {
            Section {
                row("Device", deviceName)
                if let info { row("Installed version", info.version) }
                if let error { FailureNote(message: error) }
            }
            Section {
                if checking { HStack { ProgressView(); Text("Checking for updates…") } }
                else if starting { HStack { ProgressView(); Text("Starting update…") } }
                else if waiting || update?.busy == true {
                    HStack { ProgressView(); Text(waiting ? "Waiting for device to reconnect…" : phaseTitle) }
                    if !waiting, let update, update.phase == "downloading", let fraction = update.downloadFraction {
                        ProgressView(value: fraction)
                        Text("\(ByteCountFormatter.string(fromByteCount: update.downloaded, countStyle: .file)) of \(ByteCountFormatter.string(fromByteCount: update.total, countStyle: .file))").font(.caption).foregroundStyle(.secondary)
                    }
                } else if verified { Label("Updated successfully", systemImage: "checkmark.circle").foregroundStyle(.green) }
                else if let update, update.phase == "failed" { FailureNote(message: update.detail) }
                else if let update, update.available {
                    Label("Update available!", systemImage: "arrow.down.circle.fill").foregroundStyle(Color.accentColor)
                    LabeledContent("Version", value: update.latest)
                    Button("Update Wendy Agent") { confirmVersion = update.latest }
                } else if let update, update.phase == "current" {
                    Label(update.installed == "dev" || update.installed.hasSuffix("-dev") ? "Development build" : "Up to date", systemImage: "checkmark.circle")
                    LabeledContent("Latest release", value: update.latest)
                }
                if let updateError { FailureNote(message: updateError) }
                if !checking && !starting && !waiting && update?.busy != true {
                    Button("Check for agent updates") { actions.submit { await refresh() } }
                }
            } header: { Text("Agent update") } footer: { Text("The device downloads and installs the update. Its connection will briefly restart.") }
        }.formStyle(.grouped).navigationTitle("Wendy Agent")
            .task(id: session) { await refresh() }
            .confirmationDialog("Update Wendy Agent on \(deviceName)?", isPresented: Binding(get: { confirmVersion != nil }, set: { if !$0 { confirmVersion = nil } }), titleVisibility: .visible) {
                if let target = confirmVersion {
                    Button("Install \(target)") { actions.submit { await start(version: target) } }
                }
                Button("Cancel", role: .cancel) { }
            } message: { Text("The Agent connection will restart. Keep the device powered on; installed version will be checked before reporting success.") }
            .task(id: update?.busy == true) { if update?.busy == true { await monitor() } }
            .mobileRefreshable { if update?.busy != true && !starting { await refresh() } }
    }
    @ViewBuilder private func row(_ title: String, _ value: String) -> some View {
        if !value.isEmpty { LabeledContent(title) { Text(value).textSelection(.enabled) } }
    }
    private var phaseTitle: String {
        switch update?.phase { case "downloading": "Downloading on device…"; case "installing": "Installing on device…"; default: "Restarting agent…" }
    }
    private func refresh() async {
        guard !checking else { return }
        checking = true; updateError = nil; verified = false; defer { checking = false }
        do { info = try await GoClient.shared.deviceInfo(session: session); error = nil }
        catch { if !Task.isCancelled { self.error = error.localizedDescription } }
        do {
            let status = try await GoClient.shared.onlineUpdate(session: session, action: 0)
            guard !Task.isCancelled else { return }
            if status.busy { expected = status.latest; update = status }
            else { update = try await GoClient.shared.onlineUpdate(session: session, action: 1) }
        } catch { if !Task.isCancelled { updateError = error.localizedDescription } }
    }
    private func start(version: String) async {
        guard let update, update.available, update.latest == version, !starting else { return }
        starting = true; updateError = nil; verified = false
        defer { starting = false }
        do {
            expected = version
            self.update = try await GoClient.shared.onlineUpdate(session: session, action: 2, version: expected)
        } catch { if !Task.isCancelled { updateError = error.localizedDescription } }
    }
    private func monitor() async {
        let deadline = Date().addingTimeInterval(12 * 60)
        defer { waiting = false }
        while !Task.isCancelled && Date() < deadline {
            do { try await Task.sleep(for: .seconds(2)) } catch { return }
            do {
                let status = try await GoClient.shared.onlineUpdate(session: session, action: 0)
                guard !Task.isCancelled else { return }
                waiting = false
                if status.phase == "failed" { update = status; return }
                if !status.busy {
                    if !expected.isEmpty && status.installed == expected {
                        let refreshed = try await GoClient.shared.deviceInfo(session: session)
                        guard !Task.isCancelled else { return }
                        guard refreshed.version == expected else {
                            update = status; updateError = "The device reported a different installed version. Update has not been verified."; return
                        }
                        info = refreshed; verified = true; update = status
                    } else {
                        update = status; updateError = "The device is reachable, but the new version has not been verified. Check again."
                    }
                    return
                }
                update = status
            } catch { waiting = true }
        }
        if !Task.isCancelled { updateError = "The update has not been verified. Reopen Wendy Agent when the device reconnects."; waiting = false; update = nil }
    }
}
