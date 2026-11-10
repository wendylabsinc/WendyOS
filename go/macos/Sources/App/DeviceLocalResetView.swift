import SwiftUI

struct LocalResetPlan: Equatable, Sendable {
    let principal: String
    let fingerprint: String
}
struct LocalResetOutcome: Sendable {
    let completed: Bool
    let warning: String?
}
extension GoClient {
    func prepareLocalReset(session: UInt64, account: String, organization: String, device: String) async throws -> LocalResetPlan {
        try await perform {
            let strings = CStringStorage()
            defer { withExtendedLifetime(strings) {} }
            guard let result = WDPrepareLocalReset(session, strings.add(account), strings.add(organization), strings.add(device)) else {
                throw WendyError(message: "Could not check reset support.")
            }
            defer { WDLocalResetFree(result) }
            try Self.check(result.pointee.status)
            return LocalResetPlan(principal: Self.string(result.pointee.principal), fingerprint: Self.string(result.pointee.fingerprint))
        }
    }
    func resetLocalEnrollment(session: UInt64, account: String, organization: String, device: String, plan: LocalResetPlan) async throws -> LocalResetOutcome {
        try await perform {
            let strings = CStringStorage()
            defer { withExtendedLifetime(strings) {} }
            guard let result = WDResetLocalEnrollment(session, strings.add(account), strings.add(organization), strings.add(device), strings.add(plan.principal), strings.add(plan.fingerprint)) else {
                throw WendyError(message: "Reset was not confirmed. Check nearby setup before trying again.")
            }
            defer { WDLocalResetFree(result) }
            let message = Self.string(result.pointee.status.message)
            return LocalResetOutcome(completed: result.pointee.completed != 0, warning: message.isEmpty ? nil : message)
        }
    }
}

struct DeviceLocalResetView: View {
    let session: UInt64
    let deviceName, account, organization, deviceID: String
    @State private var actions = AsyncActions()
    @State private var plan: LocalResetPlan?
    @State private var confirmedPlan: LocalResetPlan?
    @State private var busy = false
    @State private var attempted = false
    @State private var outcome: LocalResetOutcome?
    @State private var error: String?

    var body: some View {
        Form {
            Section("Device") {
                LabeledContent("Name", value: deviceName)
                LabeledContent("Device ID", value: deviceID).font(.caption).textSelection(.enabled)
            }
            Section {
                Text("Clears enrollment credentials on this device and restarts its Agent. You will need to enroll it again.")
                Text("Cloud records and certificate revocation are unchanged. This is not a factory reset: apps, recordings, and saved Wi-Fi are not erased.")
                Text("Active connections will end. Mesh access and Unitree pairings are cleared.")
            } header: { Text("What changes") }
            if busy { ProgressView(attempted ? "Resetting enrollment…" : "Checking reset support…") }
            if let outcome {
                Section(outcome.completed ? "Enrollment cleared" : "Completion not confirmed") {
                    Text(outcome.completed
                        ? "The Agent confirmed local reset. Return to Devices and use nearby setup to enroll it again. Reconnection has not been verified."
                        : "The device may already be unenrolled. Check nearby setup before attempting another reset.")
                    if let warning = outcome.warning { FailureNote(message: warning) }
                }
            } else if let error { FailureNote(message: error) }
            if !attempted {
                Section {
                    Button("Reset local enrollment", role: .destructive) { confirmedPlan = plan }
                        .disabled(busy || plan == nil)
                    if !busy && plan == nil { Button("Check again") { actions.submit { await prepare() } } }
                }
            }
        }.formStyle(.grouped).navigationTitle("Reset enrollment")
            .interactiveDismissDisabled(busy)
            .task { await actions.run() }
            .task(id: session) { await prepare() }
            .confirmationDialog("Reset enrollment on \(deviceName)?", isPresented: Binding(get: { confirmedPlan != nil }, set: { if !$0 { confirmedPlan = nil } }), titleVisibility: .visible, presenting: confirmedPlan) { confirmed in
                Button("Reset local enrollment", role: .destructive) { actions.submit { await reset(confirmed) } }
                Button("Cancel", role: .cancel) {}
            } message: { _ in
                Text("The device will disconnect and require enrollment again. Cloud records and certificate revocation will not change. Apps, recordings, and Wi-Fi stay on the device.")
            }
    }
    private func prepare() async {
        guard !attempted, !busy else { return }
        busy = true; plan = nil; error = nil
        defer { busy = false }
        do {
            let value = try await GoClient.shared.prepareLocalReset(session: session, account: account, organization: organization, device: deviceID)
            if !Task.isCancelled { plan = value }
        } catch { if !Task.isCancelled { self.error = error.localizedDescription } }
    }
    private func reset(_ confirmed: LocalResetPlan) async {
        guard !attempted, !busy, plan == confirmed else { return }
        attempted = true; busy = true; error = nil; plan = nil
        defer { busy = false }
        do {
            outcome = try await GoClient.shared.resetLocalEnrollment(session: session, account: account, organization: organization, device: deviceID, plan: confirmed)
        } catch {
            outcome = LocalResetOutcome(completed: false, warning: error.localizedDescription)
        }
    }
}
