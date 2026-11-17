import SwiftUI

struct OSUpdateSnapshot: Decodable, Sendable {
    struct Install: Decodable, Sendable {
        var active: Bool
        var phase: String
        var percent: Int
        var rebootRequired: Bool
        var error: String
        var baseline: Int64
        var fraction: Double? { (0...100).contains(percent) ? Double(percent) / 100 : nil }
    }
    struct Status: Decodable, Sendable {
        struct Engine: Decodable, Sendable {
            struct Pending: Decodable, Sendable { var artifactVersion: String?; var phase: String? }
            var connector: String?
            var currentSlot: String?
            var pending: Pending?
        }
        struct Service: Decodable, Sendable { var unit: String?; var status: String?; var reason: String? }
        var hasResult: Bool?
        var outcome: String?
        var oldOsVersion: String?
        var newOsVersion: String?
        var createdAtUnix: String?
        var finalizedAtUnix: String?
        var rollbackError: String?
        var note: String?
        var engineStatus: Engine?
        var services: [Service]?
        var timestamp: Int64 { Int64(createdAtUnix ?? "") ?? 0 }
        var title: String {
            switch outcome {
            case "OUTCOME_COMMITTED": "Update committed"
            case "OUTCOME_ROLLED_BACK": (Int64(finalizedAtUnix ?? "") ?? 0) > 0 ? "Previous OS restored" : "Rollback awaiting verification"
            case "OUTCOME_ROLLBACK_FAILED": "Rollback failed"
            case "OUTCOME_COMMIT_FAILED": "Update could not be committed"
            default: "Outcome not yet verified"
            }
        }
        func verifies(_ install: Install, installedVersion: String?) -> Bool {
            hasResult == true && timestamp > install.baseline && outcome == "OUTCOME_COMMITTED" &&
                newOsVersion?.isEmpty == false && newOsVersion == installedVersion && engineStatus?.pending == nil
        }
    }
    var status: Status
    var install: Install
}
extension GoClient {
    func osUpdate(session: UInt64, artifact: String = "", start: Bool = false) async throws -> OSUpdateSnapshot {
        try await perform {
            try Self.withCString(artifact) { artifact in
                guard let result = WDOSUpdate(session, artifact, start ? 1 : 0) else { throw WendyError(message: "Missing OS update status.") }
                defer { WDOSUpdateFree(result) }
                try Self.check(result.pointee.status)
                return try JSONDecoder().decode(OSUpdateSnapshot.self, from: Data(Self.string(result.pointee.json).utf8))
            }
        }
    }
}

struct DeviceOSUpdateView: View {
    let session: UInt64
    let deviceName: String
    @State private var actions = AsyncActions()
    @State private var snapshot: OSUpdateSnapshot?
    @State private var info: DeviceInfoSnapshot?
    @State private var artifact = ""
    @State private var confirm = false
    @State private var busy = false
    @State private var error: String?
    @State private var refreshID = 0
    @State private var checked: Date?
    private var pending: Bool { snapshot?.status.engineStatus?.pending != nil }
    private var running: Bool { snapshot?.install.active == true }
    private var canStart: Bool {
        !busy && !running && !pending && error == nil && checked.map { Date().timeIntervalSince($0) < 60 } == true &&
            snapshot?.status.engineStatus?.connector?.isEmpty == false && !artifact.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
    }
    var body: some View {
        Form {
            Section("Operating system") {
                LabeledContent("Device", value: deviceName)
                if let info { LabeledContent("Installed", value: info.osVersion.isEmpty ? info.os : info.osVersion) }
                if let checked { Text("Checked \(checked.formatted(date: .omitted, time: .shortened))").font(.caption).foregroundStyle(.secondary) }
            }
            if let snapshot {
                if running {
                    Section("Updating") {
                        Text(snapshot.install.phase.capitalized)
                        if let fraction = snapshot.install.fraction { ProgressView(value: fraction) }
                        else { ProgressView() }
                        Text("Keep the device powered on and Wendy open on this device until installation finishes. Switching devices or backgrounding the app interrupts the connection.").font(.footnote).foregroundStyle(.secondary)
                    }
                } else if pending || snapshot.install.rebootRequired && snapshot.status.timestamp <= snapshot.install.baseline {
                    Section("Restart required") {
                        Text("The OS is installed but has not been verified. Restart the device using its local controls or Wendy CLI device shell, then reconnect and check the update result.")
                        Text("This app cannot restart the device yet.").font(.footnote).foregroundStyle(.secondary)
                    }
                }
                if !snapshot.install.error.isEmpty { FailureNote(message: snapshot.install.error) }
                if snapshot.status.hasResult == true {
                    Section("Most recent result") {
                        Text(snapshot.status.title).font(.headline)
                        if let version = snapshot.status.newOsVersion { LabeledContent("Target version", value: version) }
                        if snapshot.status.timestamp > 0 {
                            Text(Date(timeIntervalSince1970: Double(snapshot.status.timestamp)).formatted(date: .abbreviated, time: .shortened)).font(.caption).foregroundStyle(.secondary)
                        }
                        if snapshot.status.verifies(snapshot.install, installedVersion: info?.osVersion) {
                            Label("Installed version verified", systemImage: "checkmark.circle")
                        } else if snapshot.status.outcome == "OUTCOME_COMMITTED" {
                            Text("Check the installed version above before considering this update verified.").font(.footnote).foregroundStyle(.secondary)
                        }
                        if let note = snapshot.status.note, !note.isEmpty { Text(note).font(.footnote) }
                        if let failure = snapshot.status.rollbackError, !failure.isEmpty { FailureNote(message: failure) }
                        let failures = snapshot.status.services?.filter { $0.status == "STATUS_FAILED" } ?? []
                        if !failures.isEmpty {
                            DisclosureGroup("Health check failures") {
                                ForEach(Array(failures.enumerated()), id: \.offset) { _, service in
                                    Text([service.unit, service.reason].compactMap { $0 }.joined(separator: ": ")).font(.caption).textSelection(.enabled)
                                }
                            }
                        }
                    }
                }
                if !running && !pending {
                    Section {
                        TextField("OS artifact URL", text: $artifact).autocorrectionDisabled()
                        Button("Install OS update") { confirm = true }.disabled(!canStart)
                    } header: { Text("Install update") } footer: {
                        Text(snapshot.status.engineStatus?.connector?.isEmpty == false ?
                             "Use the OS artifact for this device’s hardware. The Agent checks compatibility before installation. Restart and post-boot health checks are required." :
                             "This device has not reported a supported OS updater. Refresh or check its Agent version.")
                    }
                }
            }
            if busy { ProgressView("Checking device…") }
            if let error { FailureNote(message: error) }
            Button("Refresh status") { refreshID += 1 }.disabled(busy)
        }
        .navigationTitle("OS update").formStyle(.grouped)
        .task { await actions.run() }
        #if os(iOS)
        .onChange(of: running) { _, active in UIApplication.shared.isIdleTimerDisabled = active }
        .onDisappear { UIApplication.shared.isIdleTimerDisabled = false }
        #endif
        .task(id: "\(session):\(refreshID)") { snapshot = nil; info = nil; checked = nil; await refresh() }
        .task(id: running) { if running { await monitor() } }
        .confirmationDialog("Install an OS update on \(deviceName)?", isPresented: $confirm, titleVisibility: .visible) {
            Button("Install OS update", role: .destructive) { actions.submit { await start() } }
            Button("Cancel", role: .cancel) { }
        } message: {
            Text("The operating system will be replaced. Keep power connected and plan for downtime while the device restarts. The update is only verified after post-boot health checks. Your selected artifact: \(artifact)")
        }
    }
    private func refresh() async {
        busy = true; defer { busy = false }; error = nil
        do {
            let value = try await GoClient.shared.osUpdate(session: session)
            let deviceInfo = try await GoClient.shared.deviceInfo(session: session)
            guard !Task.isCancelled else { return }
            snapshot = value; info = deviceInfo; checked = Date()
        } catch { if !Task.isCancelled { self.error = "\(error.localizedDescription) Reconnect to this device and reopen OS update to check its result." } }
    }
    private func start() async {
        guard canStart else { return }
        busy = true; error = nil; defer { busy = false }
        do {
            let value = try await GoClient.shared.osUpdate(session: session, artifact: artifact.trimmingCharacters(in: .whitespacesAndNewlines), start: true)
            guard !Task.isCancelled else { return }
            snapshot = value
        } catch { if !Task.isCancelled { self.error = error.localizedDescription } }
    }
    private func monitor() async {
        let deadline = Date().addingTimeInterval(45 * 60)
        while !Task.isCancelled && Date() < deadline {
            do {
                try await Task.sleep(for: .seconds(3))
                let value = try await GoClient.shared.osUpdate(session: session)
                guard !Task.isCancelled else { return }
                snapshot = value; checked = Date()
                if !value.install.active { return }
            } catch {
                if !Task.isCancelled { self.error = "Connection interrupted. Reconnect to this device and reopen OS update to verify its result." }
                return
            }
        }
    }
}
