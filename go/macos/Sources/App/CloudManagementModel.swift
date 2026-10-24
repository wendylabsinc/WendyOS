import SwiftUI

@MainActor final class CloudManagementModel: ObservableObject {
    let client = CloudManagementClient()
    @Published var accounts: [SavedAccount] = []
    @Published var account = ""
    @Published var organizations: [CloudOrganization] = []
    @Published var organization = ""
    @Published var revision = 0
    @Published var snapshot: CloudOrganizationSnapshot?
    @Published var loading = false
    @Published var saving = false
    @Published var error: String?
    @Published var actionError: String?
    private var generation = 0

    func loadAccounts() async {
        do {
            accounts = try await GoClient.shared.accounts()
            if !accounts.contains(where: { $0.id == account }) {
                account = accounts.first(where: { $0.active })?.id ?? accounts.first?.id ?? ""
            }
            await loadOrganizations()
        } catch { if !Task.isCancelled { self.error = error.localizedDescription } }
    }
    func loadOrganizations() async {
        generation += 1
        let request = generation
        snapshot = nil
        organizations = []
        organization = ""
        error = nil
        guard !account.isEmpty else {
            loading = false
            return
        }
        loading = true
        do {
            let values = try await client.organizations(account: account)
            guard request == generation else { return }
            organizations = values
            if !values.contains(where: { $0.id == organization }) {
                organization = values.first?.id ?? ""
            }
            loading = false
            await refresh()
        } catch {
            if request == generation {
                if !Task.isCancelled { self.error = error.localizedDescription }
                loading = false
            }
        }
    }
    func refresh(clear: Bool = false) async {
        generation += 1
        let request = generation
        if clear { snapshot = nil }
        guard !account.isEmpty, !organization.isEmpty else {
            loading = false
            return
        }
        loading = true
        error = nil
        do {
            let value = try await client.overview(account: account, org: organization)
            guard request == generation else { return }
            snapshot = value
            revision += 1
        } catch { if request == generation { if !Task.isCancelled { self.error = error.localizedDescription } } }
        if request == generation { loading = false }
    }
    func can(_ permission: String) -> Bool {
        snapshot?.permissions.contains(permission) == true && !saving && !loading
    }
    func save(_ work: () async throws -> Void, onSuccess: () -> Void = {}) async -> Bool {
        guard !saving else { return false }
        saving = true
        actionError = nil
        defer { saving = false }
        do {
            try await work()
            // Dismiss the submitted sheet before refreshing the surrounding UI.
            // A slow refresh must not leave a completed form available to save.
            onSuccess()
            await refresh()
            return true
        } catch {
            if !Task.isCancelled { actionError = error.localizedDescription }
            return false
        }
    }
}
