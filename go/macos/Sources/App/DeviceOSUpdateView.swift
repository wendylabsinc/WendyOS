import SwiftUI

struct OSReleaseSnapshot: Decodable, Sendable {
    var deviceType, installed, latest, artifactURL, blockedReason: String
    var available, current: Bool
}

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
        func hasFinalResult(after baseline: Int64) -> Bool {
            hasResult == true && timestamp > baseline &&
                (Int64(finalizedAtUnix ?? "") ?? 0) > 0 &&
                ["OUTCOME_COMMITTED", "OUTCOME_ROLLED_BACK", "OUTCOME_ROLLBACK_FAILED", "OUTCOME_COMMIT_FAILED"].contains(outcome ?? "")
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
    func restartAfterOSUpdate(session: UInt64) async throws {
        try await perform { try Self.finish(WDOSRestart(session)) }
    }

    func checkOSRelease(session: UInt64) async throws -> OSReleaseSnapshot {
        try await perform {
            guard let result = WDOSRelease(session) else { throw WendyError(message: "Missing OS release information.") }
            defer { WDOSUpdateFree(result) }
            try Self.check(result.pointee.status)
            return try JSONDecoder().decode(OSReleaseSnapshot.self, from: Data(Self.string(result.pointee.json).utf8))
        }
    }

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
    #if os(iOS)
    @ObservedObject private var backgroundUpdate = MobileOSUpdateTask.shared
    @EnvironmentObject private var model: AppModel
    @Environment(\.scenePhase) private var scenePhase
    #endif
    @State private var actions = AsyncActions()
    @State private var snapshot: OSUpdateSnapshot?
    @State private var info: DeviceInfoSnapshot?
    @State private var release: OSReleaseSnapshot?
    @State private var releaseError: String?
    @State private var customArtifact: String?
    @State private var confirmedArtifact: String?
    @State private var confirmRestart = false
    @State private var promptedRestart = false
    @State private var waitingForRestart = false
    @State private var restartBaseline: Int64 = 0
    @State private var busy = false
    @State private var error: String?
    @State private var refreshID = 0
    @State private var checked: Date?
    @State private var loadedSession: UInt64?
    private var pending: Bool {
        guard let snapshot else { return false }
        return snapshot.status.engineStatus?.pending != nil ||
            snapshot.install.rebootRequired && snapshot.status.timestamp <= snapshot.install.baseline
    }
    private var running: Bool { snapshot?.install.active == true }
    private var backgroundBusy: Bool {
        #if os(iOS)
        backgroundUpdate.active
        #else
        false
        #endif
    }
    private var artifact: String { customArtifact ?? release?.artifactURL ?? "" }
    private var canStart: Bool {
        !busy && !running && !pending && !backgroundBusy && error == nil && checked.map { Date().timeIntervalSince($0) < 60 } == true &&
            snapshot?.status.engineStatus?.connector?.isEmpty == false && !artifact.isEmpty &&
            (customArtifact != nil || release?.available == true && release?.blockedReason.isEmpty == true)
    }
    var body: some View {
        Form {
            Section("Operating system") {
                LabeledContent("Device", value: deviceName)
                if let info { LabeledContent("Installed", value: info.osVersion.isEmpty ? info.os : info.osVersion) }
                else if let release, !release.installed.isEmpty { LabeledContent("Installed", value: release.installed) }
            }
            if !running && !pending {
                Section("OS update") {
                    if let customArtifact {
                        Text("Custom artifact").font(.headline)
                        Text(customArtifact).font(.caption).foregroundStyle(.secondary).textSelection(.enabled)
                        Button("Install custom update") { confirmedArtifact = artifact }.disabled(!canStart)
                        Button("Use latest release") { self.customArtifact = nil }
                    } else if let release {
                        LabeledContent("Latest release", value: release.latest)
                        if release.available {
                            Button("Update to \(release.latest)") { confirmedArtifact = artifact }.disabled(!canStart)
                        } else if release.current {
                            Label("OS is up to date", systemImage: "checkmark.circle")
                        }
                        if !release.blockedReason.isEmpty { FailureNote(message: release.blockedReason) }
                    }
                    if let releaseError { FailureNote(message: "Could not check OS releases. \(releaseError)") }
                    if backgroundBusy { ProgressView("Preparing OS update…") }
                    else if busy { ProgressView("Checking for OS updates…") }
                    Button("Check for updates") { refreshID += 1 }.disabled(busy)
                }
            }
            if let snapshot {
                if running {
                    Section("Updating") {
                        Text(snapshot.install.phase.capitalized)
                        if let fraction = snapshot.install.fraction { ProgressView(value: fraction) }
                        else { ProgressView() }
                        Text("Keep the device powered on and connected. You can use other apps during installation. Force-quitting Wendy or switching devices interrupts the update.").font(.footnote).foregroundStyle(.secondary)
                    }
                } else if pending {
                    Section("Restart required") {
                        Text("Restart the device to finish installing the OS update. Wendy will check its health after it reconnects.")
                        if waitingForRestart { ProgressView("Waiting for device to restart…") }
                        else { Button("Restart device") { confirmRestart = true }.disabled(busy) }
                    }
                }
                if waitingForRestart && !pending {
                    Section("Verifying update") { ProgressView("Waiting for device health checks…") }
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
                    if snapshot.status.engineStatus?.connector?.isEmpty != false {
                        Text("This device has not reported a supported OS updater. Refresh or check its Agent version.").font(.footnote).foregroundStyle(.secondary)
                    }
                }
            }
            if !running && !pending {
                Section {
                    NavigationLink("Advanced") {
                        CustomOSArtifactView(initialURL: customArtifact ?? "") { customArtifact = $0 }
                    }
                }
            }
            if let error { FailureNote(message: error) }
            if running || pending { Button("Refresh status") { refreshID += 1 }.disabled(busy) }
        }
        .navigationTitle("OS update").formStyle(.grouped)
        .task { await actions.run() }
        #if os(iOS)
        .onReceive(backgroundUpdate.$snapshot) { value in
            if backgroundUpdate.session == session, let value { snapshot = value; checked = Date() }
        }
        .onReceive(backgroundUpdate.$error) { value in
            if backgroundUpdate.session == session, let value { error = value }
        }
        .onChange(of: scenePhase) { _, phase in if phase == .active { refreshID += 1 } }
        #endif
        .task(id: "\(session):\(refreshID)") { await refresh() }
        .task(id: running) { if running { await monitor() } }
        .task(id: waitingForRestart) { if waitingForRestart { await monitorRestart() } }
        .onChange(of: pending) { _, needsRestart in
            if needsRestart && !promptedRestart { promptedRestart = true; confirmRestart = true }
        }
        .confirmationDialog("Restart \(deviceName)?", isPresented: $confirmRestart, titleVisibility: .visible) {
            Button("Restart device", role: .destructive) { actions.submit { await restart() } }
            Button("Later", role: .cancel) { }
        } message: {
            Text("Running apps will stop and the device will briefly disconnect. Keep power connected while the update finishes.")
        }
        .confirmationDialog("Install an OS update on \(deviceName)?", isPresented: Binding(get: { confirmedArtifact != nil }, set: { if !$0 { confirmedArtifact = nil } }), titleVisibility: .visible) {
            if let target = confirmedArtifact {
                Button("Install OS update", role: .destructive) { actions.submit { await start(artifact: target) } }
            }
            Button("Cancel", role: .cancel) { }
        } message: {
            Text("The operating system will be replaced. Keep power connected and plan for downtime while the device restarts. The update is only verified after post-boot health checks. Your selected artifact: \(confirmedArtifact ?? "")")
        }
    }
    private func refresh() async {
        if loadedSession != session {
            snapshot = nil; info = nil; customArtifact = nil; confirmedArtifact = nil; loadedSession = session
            promptedRestart = false; waitingForRestart = false
        }
        busy = true; defer { busy = false }; error = nil; releaseError = nil; release = nil; checked = nil
        do {
            let value = try await GoClient.shared.osUpdate(session: session)
            let deviceInfo = try await GoClient.shared.deviceInfo(session: session)
            guard !Task.isCancelled else { return }
            snapshot = value; info = deviceInfo; checked = Date()
        } catch { if !Task.isCancelled { self.error = "\(error.localizedDescription) Reconnect to this device and reopen OS update to check its result." } }
        guard !Task.isCancelled, !running, !pending else { return }
        do {
            let value = try await GoClient.shared.checkOSRelease(session: session)
            guard !Task.isCancelled else { return }
            release = value
        } catch { if !Task.isCancelled { releaseError = error.localizedDescription } }
    }
    private func start(artifact target: String) async {
        guard canStart, target == artifact else { return }
        busy = true; error = nil; defer { busy = false }
        do {
            #if os(iOS)
            try backgroundUpdate.start(session: session, scope: model.mobileCloudSelectionKey, name: deviceName, artifact: target)
            #else
            let value = try await GoClient.shared.osUpdate(session: session, artifact: target, start: true)
            guard !Task.isCancelled else { return }
            snapshot = value
            #endif
        } catch { if !Task.isCancelled { self.error = error.localizedDescription } }
    }
    private func restart() async {
        guard pending, !running, !busy else { return }
        busy = true; error = nil; defer { busy = false }
        do {
            restartBaseline = snapshot?.status.timestamp ?? 0
            try await GoClient.shared.restartAfterOSUpdate(session: session)
            guard !Task.isCancelled else { return }
            waitingForRestart = true
        } catch { if !Task.isCancelled { self.error = error.localizedDescription } }
    }
    private func monitorRestart() async {
        let deadline = ContinuousClock.now + .seconds(300)
        while !Task.isCancelled && ContinuousClock.now < deadline {
            do {
                try await Task.sleep(for: .seconds(5))
                let value = try await GoClient.shared.osUpdate(session: session)
                let deviceInfo = try await GoClient.shared.deviceInfo(session: session)
                guard !Task.isCancelled else { return }
                snapshot = value; info = deviceInfo; checked = Date()
                if !pending && value.status.hasFinalResult(after: restartBaseline) { waitingForRestart = false; return }
            } catch { if Task.isCancelled { return } }
        }
        if !Task.isCancelled {
            waitingForRestart = false
            error = "The device has not reported its update result yet. Reconnect and refresh status."
        }
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

private struct CustomOSArtifactView: View {
    @Environment(\.dismiss) private var dismiss
    @State private var url: String
    let select: (String) -> Void

    init(initialURL: String, select: @escaping (String) -> Void) {
        _url = State(initialValue: initialURL)
        self.select = select
    }

    var body: some View {
        Form {
            Section {
                TextField("OS artifact URL", text: $url).autocorrectionDisabled()
                Button("Use this artifact") {
                    select(url.trimmingCharacters(in: .whitespacesAndNewlines))
                    dismiss()
                }.disabled(url.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
            } header: { Text("Custom OS update") } footer: {
                Text("Override the release catalog with an artifact for this device’s hardware. You will confirm installation on the OS update screen.")
            }
        }.formStyle(.grouped).navigationTitle("Advanced")
    }
}
