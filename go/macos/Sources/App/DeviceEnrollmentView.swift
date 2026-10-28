import SwiftUI

enum DeviceEnrollmentForm {
    static func suggestedName(_ displayName: String) -> String {
        let folded = displayName.folding(options: [.diacriticInsensitive, .caseInsensitive], locale: Locale(identifier: "en_US_POSIX")).lowercased()
        var name = folded.components(separatedBy: CharacterSet(charactersIn: "abcdefghijklmnopqrstuvwxyz0123456789").inverted)
            .filter { !$0.isEmpty }.joined(separator: "-")
        if name.isEmpty { name = "wendy-device" }
        if let first = name.first, first.isNumber { name = "device-" + name }
        return String(name.prefix(63)).trimmingCharacters(in: CharacterSet(charactersIn: "-"))
    }

    static func validName(_ name: String) -> Bool {
        name.range(of: "^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$", options: .regularExpression) == name.startIndex..<name.endIndex
    }

    // The Go bridge preserves error text, not gRPC status. Only these explicit
    // pre-mint refusals prove that retrying cannot spend a second credential.
    static func canRetryEnrollment(_ message: String) -> Bool {
        let prefix = "Cloud refused this enrollment before minting anything, so no credential was spent: rpc error: code = "
        return ["AlreadyExists", "InvalidArgument"].contains {
            message.hasPrefix(prefix + $0 + " desc = ")
        }
    }

    // These failures come from the device after Cloud has reserved the name.
    // Connecting Wi-Fi must not start another Cloud registration.
    static func needsWiFiRecovery(_ message: String) -> Bool {
        let message = message.lowercased()
        return message.contains("device pki enrollment failed") && [
            "network is unreachable", "no route to host", "network is down",
            "temporary failure in name resolution", "no such host", "i/o timeout"
        ].contains(where: message.contains)
    }
}

struct DeviceEnrollmentSnapshot {
    let enrolled: Bool
    let organizationID, organizationName: String
    var title: String {
        guard enrolled else { return "Not enrolled in Wendy Cloud" }
        let organization = organizationName.isEmpty ? organizationID : organizationName
        return organization.isEmpty ? "Enrolled in Wendy Cloud" : "Enrolled in \(organization)"
    }
}

struct EnrollmentOrganization: Identifiable {
    let account, organizationID, name, email: String
    var id: String { account }
    var title: String { "\(name) · \(email.isEmpty ? account : email)" }
}

extension GoClient {
    func deviceEnrollment(session: UInt64) async throws -> DeviceEnrollmentSnapshot {
        try await perform {
            guard let p = WDDeviceEnrollment(session) else { throw WendyError(message: "Missing enrollment status.") }
            defer { WDEnrollmentFree(p) }
            try Self.check(p.pointee.status)
            return DeviceEnrollmentSnapshot(enrolled: p.pointee.enrolled != 0,
                organizationID: Self.string(p.pointee.organization_id), organizationName: Self.string(p.pointee.organization_name))
        }
    }
    func enrollmentOrganizations() async throws -> [EnrollmentOrganization] {
        try await perform {
            guard let p = WDEnrollmentOrganizations() else { throw WendyError(message: "Missing organizations.") }
            defer { WDEnrollmentOrganizationsFree(p) }
            try Self.check(p.pointee.status)
            return UnsafeBufferPointer(start: p.pointee.items, count: p.pointee.count).map {
                EnrollmentOrganization(account: Self.string($0.account), organizationID: Self.string($0.id),
                    name: Self.string($0.name), email: Self.string($0.email))
            }
        }
    }
    func enrollDevice(session: UInt64, organization: EnrollmentOrganization, name: String) async throws {
        try await perform {
            try Self.withCString(organization.account) { account in
                try Self.withCString(organization.organizationID) { org in
                    try Self.withCString(name) { try Self.finish(WDEnrollDevice(session, account, org, $0)) }
                }
            }
        }
    }
}

struct DeviceEnrollmentSettings: View {
    @State private var actions = AsyncActions()
    @EnvironmentObject private var app: AppModel
    @Environment(\.scenePhase) private var scenePhase
    let session: UInt64
    let deviceName: String
    let lite: Bool
    @State private var snapshot: DeviceEnrollmentSnapshot?
    @State private var error: String?
    @State private var loading = false
    @State private var showingEnrollment = false
    @State private var signInAfterDismiss = false

    var body: some View {
        actionContent.task { await actions.run() }
    }

    @ViewBuilder private var actionContent: some View {
        VStack(alignment: .leading, spacing: 6) {
            if let snapshot {
                Label(snapshot.title, systemImage: snapshot.enrolled ? "checkmark.shield" : "exclamationmark.triangle.fill")
                    .font(.subheadline.weight(.medium))
                    .foregroundStyle(snapshot.enrolled ? Color.secondary : Color.orange)
                if !snapshot.enrolled {
                    if lite {
                        Text("Enroll this Wendy Lite device over Bluetooth.")
                            .font(.caption).foregroundStyle(.secondary)
                        NearbySetupButton()
                    } else {
                        Text("Choose an organization to manage this device in Wendy Cloud.")
                            .font(.caption).foregroundStyle(.secondary)
                        Button("Enroll device…") { showingEnrollment = true }
                    }
                }
            } else if loading {
                ProgressView("Checking enrollment…").controlSize(.small)
            } else {
                Label("Enrollment status unavailable", systemImage: "questionmark.circle").font(.subheadline)
                if let error { Text(error).font(.caption).foregroundStyle(.secondary).textSelection(.enabled) }
            }
            if !loading && snapshot?.enrolled != true {
                Button("Check enrollment again") { actions.submit { await refresh() } }.font(.caption)
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .task(id: session) { snapshot = nil; await refresh() }
        .task(id: scenePhase) { if scenePhase == .active { await refresh() } }
        .sheet(isPresented: $showingEnrollment, onDismiss: {
            actions.submit {
                if signInAfterDismiss { signInAfterDismiss = false; await app.signIn() }
                await refresh()
            }
        }) {
            DeviceEnrollmentSheet(session: session, deviceName: deviceName, lite: lite) {
                signInAfterDismiss = true
            }
            .environmentObject(app)
        }
    }
    private func refresh() async {
        guard !loading else { return }
        loading = true
        defer { loading = false }
        do {
            let value = try await GoClient.shared.deviceEnrollment(session: session)
            guard !Task.isCancelled else { return }
            snapshot = value; error = nil
        } catch {
            guard !Task.isCancelled else { return }
            snapshot = nil; if !Task.isCancelled { self.error = error.localizedDescription }
        }
    }
}

private struct DeviceEnrollmentSheet: View {
    @State private var actions = AsyncActions()
    @Environment(\.dismiss) private var dismiss
    @EnvironmentObject private var app: AppModel
    let session: UInt64
    let deviceName: String
    let lite: Bool
    let signIn: () -> Void
    @State private var organizations: [EnrollmentOrganization] = []
    @State private var selected = ""
    @State private var name = ""
    @State private var loading = false
    @State private var enrolling = false
    @State private var completed = false
    @State private var error: String?
    @State private var canRetry = true
    @State private var showingWiFi = false
    @State private var resumeAfterWiFi = false
    @State private var wifiNetworks: [DeviceWiFiNetwork] = []
    @State private var notice: String?
    private var trimmedName: String { name.trimmingCharacters(in: .whitespacesAndNewlines) }
    private var choice: EnrollmentOrganization? { organizations.first { $0.id == selected } }

    var body: some View {
        actionContent.task { await actions.run() }
    }

    @ViewBuilder private var actionContent: some View {
        NavigationStack {
            Form {
                Section {
                    Text(deviceName).font(.headline)
                    if lite {
                        Text("Connect the board to your Mac over USB, then run this command in Terminal. Choose this device and the organization you want to enroll it in.")
                        Text("wendy device enroll").font(.body.monospaced()).textSelection(.enabled)
                        Text("After enrollment finishes, reconnect to the device in the app.").foregroundStyle(.secondary)
                    } else if completed {
                        Label("Enrolled in \(choice?.name ?? "Wendy Cloud")", systemImage: "checkmark.circle.fill").foregroundStyle(.green)
                    } else {
                        Text("Enroll this device in an organization to access it through Wendy Cloud.")
                        if loading { ProgressView("Loading organizations…") }
                        else if organizations.isEmpty {
                            Text("Sign in to the organization you want to use.").foregroundStyle(.secondary)
                        } else {
                            Picker("Organization", selection: $selected) {
                                Text("Choose an organization").tag("")
                                ForEach(organizations) { Text($0.title).tag($0.id) }
                            }
                            TextField("Device name", text: $name).autocorrectionDisabled()
                                #if os(iOS)
                                .textInputAutocapitalization(.never)
                                #endif
                            if !DeviceEnrollmentForm.validName(trimmedName) {
                                Text("Use 1–63 lowercase letters, numbers or hyphens. Start with a letter and end with a letter or number.")
                                    .font(.caption).foregroundStyle(.secondary)
                            }
                        }
                        Button(organizations.isEmpty ? "Sign in…" : "Sign in to another organization…") { signIn(); dismiss() }
                        Button("Refresh organizations") { actions.submit { await loadOrganizations() } }
                        if enrolling { ProgressView("Preparing enrollment…") }
                        if let notice { Text(notice).foregroundStyle(.secondary) }
                        if !canRetry && notice != nil {
                            Button("Connect to Wi-Fi…") { showingWiFi = true }
                        }
                        if let error { Text(error).foregroundStyle(.red).textSelection(.enabled) }
                        if !canRetry && notice == nil { Text("Close this sheet and check the device's enrollment status before trying again.").foregroundStyle(.secondary) }
                    }
                }.disabled(enrolling)
            }
            .formStyle(.grouped)
            .navigationTitle("Enroll device")
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button(completed || lite ? "Done" : "Cancel") { dismiss() }.disabled(enrolling)
                }
                if !lite && !completed {
                    ToolbarItem(placement: .confirmationAction) {
                        Button("Enroll") { actions.submit { await enroll() } }
                            .disabled(loading || enrolling || choice == nil || !DeviceEnrollmentForm.validName(trimmedName) || !canRetry || app.deviceSession != session)
                    }
                }
            }
        }
        #if os(macOS)
        .frame(width: 520, height: 440)
        #endif
        .interactiveDismissDisabled(enrolling)
        .onChange(of: name) { _, _ in if canRetry { error = nil } }
        .onChange(of: selected) { _, _ in if canRetry { error = nil } }
        .sheet(isPresented: $showingWiFi, onDismiss: {
            if resumeAfterWiFi {
                resumeAfterWiFi = false
                actions.submit { await enroll(checkWiFi: false) }
            }
        }) {
            WiFiJoinSheet(session: session, deviceName: deviceName, network: nil,
                          networks: wifiNetworks,
                          explanation: canRetry
                            ? "Connect this device to the internet before enrolling in Wendy Cloud. If it already has internet through Ethernet or another connection, you can continue."
                            : "The device could not reach Wendy Cloud. Connect it to Wi-Fi, then check its enrollment status. Its Cloud name has already been reserved.",
                          skip: canRetry ? { resumeAfterWiFi = true; showingWiFi = false } : nil) {
                resumeAfterWiFi = canRetry
            }
        }
        .task { name = DeviceEnrollmentForm.suggestedName(deviceName); if !lite { await loadOrganizations() } }
    }
    private func loadOrganizations() async {
        loading = true; error = nil
        defer { loading = false }
        do {
            organizations = try await GoClient.shared.enrollmentOrganizations()
            if !organizations.contains(where: { $0.id == selected }) { selected = "" }
        } catch { if !Task.isCancelled { self.error = error.localizedDescription } }
    }
    private func enroll(checkWiFi: Bool = true) async {
        guard let choice, !enrolling, canRetry, DeviceEnrollmentForm.validName(trimmedName), app.deviceSession == session else { return }
        enrolling = true; error = nil
        defer { enrolling = false }
        if checkWiFi {
            do {
                let settings = try await GoClient.shared.networkSettings(session: session)
                guard !Task.isCancelled, app.deviceSession == session else { return }
                wifiNetworks = settings.networks
                if settings.wifiAvailable && settings.wifiError.isEmpty && !settings.wifiConnected {
                    showingWiFi = true
                    return
                }
            } catch {
                if !Task.isCancelled { self.error = error.localizedDescription }
                return
            }
        }
        guard !Task.isCancelled, app.deviceSession == session else { return }
        do {
            try await GoClient.shared.enrollDevice(session: session, organization: choice, name: trimmedName)
            completed = true
        } catch {
            // Unknown outcomes remain blocked; a confirmed pre-mint refusal is safe to retry.
            canRetry = DeviceEnrollmentForm.canRetryEnrollment(error.localizedDescription)
            guard !Task.isCancelled else { return }
            if DeviceEnrollmentForm.needsWiFiRecovery(error.localizedDescription) {
                notice = "Connect the device to the internet, then close this sheet and check enrollment again. The device name is already reserved in Wendy Cloud."
                showingWiFi = true
            } else {
                self.error = error.localizedDescription
            }
        }
    }
}
