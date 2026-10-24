import SwiftUI

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
                        Text("Enroll this Wendy Lite device in an organization using a USB connection to your Mac.")
                            .font(.caption).foregroundStyle(.secondary)
                        Button("How to enroll…") { showingEnrollment = true }
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
                            TextField("Device name", text: $name)
                        }
                        Button(organizations.isEmpty ? "Sign in…" : "Sign in to another organization…") { signIn(); dismiss() }
                        Button("Refresh organizations") { actions.submit { await loadOrganizations() } }
                        if enrolling { ProgressView("Enrolling device…") }
                        if let error { Text(error).foregroundStyle(.red).textSelection(.enabled) }
                        if !canRetry { Text("Close this sheet and check the device's enrollment status before trying again.").foregroundStyle(.secondary) }
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
                            .disabled(loading || enrolling || choice == nil || name.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty || !canRetry || app.deviceSession != session)
                    }
                }
            }
        }
        #if os(macOS)
        .frame(width: 520, height: 440)
        #endif
        .interactiveDismissDisabled(enrolling)
        .task { name = deviceName; if !lite { await loadOrganizations() } }
    }
    private func loadOrganizations() async {
        loading = true; error = nil
        defer { loading = false }
        do {
            organizations = try await GoClient.shared.enrollmentOrganizations()
            if !organizations.contains(where: { $0.id == selected }) { selected = "" }
        } catch { if !Task.isCancelled { self.error = error.localizedDescription } }
    }
    private func enroll() async {
        guard let choice, !enrolling, app.deviceSession == session else { return }
        enrolling = true; error = nil
        defer { enrolling = false }
        do {
            try await GoClient.shared.enrollDevice(session: session, organization: choice, name: name.trimmingCharacters(in: .whitespacesAndNewlines))
            completed = true
        } catch {
            if !Task.isCancelled { self.error = error.localizedDescription }
            // A failed reply may follow a successful cloud mutation. Require a fresh status read.
            canRetry = false
        }
    }
}
