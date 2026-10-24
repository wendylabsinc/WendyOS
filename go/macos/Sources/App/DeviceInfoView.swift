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
                return DeviceUpdateSnapshot(installed: Self.string(v.installed), latest: Self.string(v.latest), phase: Self.string(v.phase), detail: Self.string(v.detail), available: v.available != 0)
            }
        }
    }
}
struct DeviceInfoView: View {
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
    var body: some View {
        Form {
            Section("Device") {
                LabeledContent("Name", value: deviceName)
                if let info {
                    row("Model", info.board)
                    row("Serial number", info.serial)
                    row("Architecture", info.architecture)
                    if info.cores > 0 { LabeledContent("CPU cores", value: info.cores.formatted()) }
                    if info.memory > 0 { LabeledContent("Memory", value: ByteCountFormatter.string(fromByteCount: info.memory, countStyle: .memory)) }
                } else if error == nil { ProgressView("Loading device info…") }
                if let error { FailureNote(message: error) }
            }
            if let info {
                Section("Software") {
                    row("Wendy Agent", info.version)
                    row("Operating system", info.os)
                    row("OS version", info.osVersion)
                    row("Kernel", info.kernel)
                    row("JetPack", info.jetpack)
                    row("CUDA", info.cuda)
                    row("L4T", info.l4t)
                }
            }
            Section {
                if checking { HStack { ProgressView(); Text("Checking for updates…") } }
                else if starting { HStack { ProgressView(); Text("Starting update…") } }
                else if waiting || update?.busy == true {
                    HStack { ProgressView(); Text(waiting ? "Waiting for device to restart…" : phaseTitle) }
                } else if verified { Label("Updated successfully", systemImage: "checkmark.circle").foregroundStyle(.green) }
                else if let update, update.phase == "failed" { FailureNote(message: update.detail) }
                else if let update, update.available {
                    Label("Update available!", systemImage: "arrow.down.circle.fill").foregroundStyle(Color.accentColor)
                    LabeledContent("Version", value: update.latest)
                    Button("Update Wendy Agent") { Task { await start() } }
                } else if let update, update.phase == "current" {
                    Label(update.installed == "dev" || update.installed.hasSuffix("-dev") ? "Development build" : "Up to date", systemImage: "checkmark.circle")
                    LabeledContent("Latest release", value: update.latest)
                }
                if let updateError { FailureNote(message: updateError) }
                if !checking && !starting && !waiting && update?.busy != true {
                    Button("Check again") { Task { await refresh() } }
                }
            } header: { Text("Agent update") } footer: { Text("The device downloads and installs the update. Its connection will briefly restart.") }
        }.formStyle(.grouped).navigationTitle("Device info")
            .task(id: session) { await refresh() }
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
        checking = true; updateError = nil; defer { checking = false }
        do { info = try await GoClient.shared.deviceInfo(session: session); error = nil }
        catch { self.error = error.localizedDescription }
        do {
            let status = try await GoClient.shared.onlineUpdate(session: session, action: 0)
            if status.busy { expected = status.latest; update = status }
            else { update = try await GoClient.shared.onlineUpdate(session: session, action: 1) }
        } catch { updateError = error.localizedDescription }
    }
    private func start() async {
        guard let update, update.available, !starting else { return }
        starting = true; updateError = nil; verified = false
        defer { starting = false }
        do {
            expected = update.latest
            self.update = try await GoClient.shared.onlineUpdate(session: session, action: 2, version: expected)
        } catch { updateError = error.localizedDescription }
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
                    if status.installed == expected {
                        verified = true; update = status
                        info = try await GoClient.shared.deviceInfo(session: session)
                    } else {
                        update = status; updateError = "The device is reachable, but the new version has not been verified. Check again."
                    }
                    return
                }
                update = status
            } catch { waiting = true }
        }
        if !Task.isCancelled { updateError = "The update has not been verified. Reopen Device info when the device reconnects."; waiting = false; update = nil }
    }
}
