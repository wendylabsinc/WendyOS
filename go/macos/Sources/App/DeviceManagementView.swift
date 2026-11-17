import SwiftUI

extension GoClient {
    func manageDevice(session: UInt64, account: String, organization: String, device: String, name: String, remove: Bool) async throws {
        try await perform {
            try Self.withCString(account) { account in
                try Self.withCString(organization) { organization in
                    try Self.withCString(device) { device in
                        try Self.withCString(name) { name in
                            try Self.finish(WDManageDevice(session, account, organization, device, name, remove ? 1 : 0))
                        }
                    }
                }
            }
        }
    }
}

struct DeviceManagementView: View {
    @State private var actions = AsyncActions()
    let session: UInt64
    let deviceName: String
    @State private var identity: DeviceEnrollmentSnapshot?
    @State private var accounts: [EnrollmentOrganization] = []
    @State private var account = ""
    @State private var name = ""
    @State private var error: String?
    @State private var message: String?
    @State private var busy = false
    @State private var removed = false
    @State private var confirmation: ManagementAction?
    private enum ManagementAction: String, Identifiable { case rename, remove; var id: String { rawValue } }
    private var canManage: Bool { identity?.deviceID.isEmpty == false && !account.isEmpty && !busy && !removed }
    var body: some View {
        Form {
            Section("Device") {
                LabeledContent("Name", value: deviceName)
                if let identity {
                    Text(identity.title)
                    if !identity.deviceID.isEmpty { LabeledContent("Device ID", value: identity.deviceID).font(.caption).textSelection(.enabled) }
                } else if error == nil { ProgressView("Checking device identity…") }
                if !accounts.isEmpty {
                    Picker("Signed-in account", selection: $account) {
                        Text("Choose an account").tag("")
                        ForEach(accounts) { Text($0.title).tag($0.account) }
                    }.disabled(busy || removed)
                }
                if identity != nil && accounts.isEmpty { Text("Sign in to this device’s organization to manage it.").foregroundStyle(.secondary) }
            }
            Section {
                TextField("New device name", text: $name).autocorrectionDisabled().disabled(busy || removed)
                Button("Rename device") { confirmation = .rename }.disabled(!canManage || name.isEmpty)
            } header: { Text("Rename") } footer: {
                Text("Use lowercase letters, numbers and hyphens, starting with a letter. This changes its Cloud name and local hostname; reconnect from Devices afterward.")
            }
            Section {
                Button("Remove from Wendy Cloud", role: .destructive) { confirmation = .remove }.disabled(!canManage)
            } footer: {
                Text("Revokes Cloud access and removes the device from this organization. This does not reset its local enrollment, apps or Wi-Fi. Local recovery is required before enrolling it again.")
            }
            Section {
                NavigationLink("Reset local enrollment") {
                    DeviceLocalResetView(session: session, deviceName: deviceName, account: account,
                        organization: identity?.organizationID ?? "", deviceID: identity?.deviceID ?? "")
                }.disabled(!canManage)
            }
            Section("Software and power") {
                NavigationLink("Agent update and device info") { DeviceInfoView(session: session, deviceName: deviceName) }
                NavigationLink("OS update") { DeviceOSUpdateView(session: session, deviceName: deviceName) }
                Text("Remote restart is not supported yet.").font(.footnote).foregroundStyle(.secondary)
            }
            if busy { ProgressView("Applying to \(deviceName)…") }
            if let message { Text(message).foregroundStyle(.secondary) }
            if let error { FailureNote(message: error) }
            if !busy && !removed { Button("Refresh identity") { actions.submit { await refresh() } } }
        }
        .formStyle(.grouped).navigationTitle("Device management")
        .task { await actions.run() }
        .task(id: session) { name = deviceName; await refresh() }
        .confirmationDialog(confirmation == .remove ? "Remove \(deviceName) from Wendy Cloud?" : "Rename \(deviceName) to \(name)?", isPresented: Binding(get: { confirmation != nil }, set: { if !$0 { confirmation = nil } }), titleVisibility: .visible) {
            if confirmation == .remove {
                Button("Revoke access and remove", role: .destructive) { actions.submit { await apply(remove: true) } }
            } else {
                Button("Rename device") { actions.submit { await apply(remove: false) } }
            }
            Button("Cancel", role: .cancel) { }
        } message: {
            Text(confirmation == .remove ? "Cloud connections will stop. The device is not factory reset. Local recovery will be needed to enroll again." : "The Cloud name and local hostname will both change. Reconnect from the device list afterward.")
        }
    }
    private func refresh() async {
        busy = true; error = nil; identity = nil; accounts = []; account = ""
        defer { busy = false }
        do {
            let current = try await GoClient.shared.deviceEnrollment(session: session)
            let saved = try await GoClient.shared.enrollmentOrganizations()
            guard !Task.isCancelled else { return }
            identity = current; accounts = saved.filter { $0.organizationID == current.organizationID }
            if accounts.count == 1 { account = accounts[0].account }
        } catch { if !Task.isCancelled { self.error = error.localizedDescription } }
    }
    private func apply(remove: Bool) async {
        guard canManage, let identity else { return }
        busy = true; error = nil; message = nil
        defer { busy = false }
        do {
            try await GoClient.shared.manageDevice(session: session, account: account, organization: identity.organizationID, device: identity.deviceID, name: name, remove: remove)
            guard !Task.isCancelled else { return }
            removed = remove
            message = remove ? "Cloud access removed. Local enrollment was not reset. Return to Devices and refresh." : "Renamed to \(name). Return to Devices and refresh before reconnecting."
        } catch { if !Task.isCancelled { self.error = error.localizedDescription } }
    }
}
