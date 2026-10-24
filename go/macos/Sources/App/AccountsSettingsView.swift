import SwiftUI

struct AccountsSettingsView: View {
    @ObservedObject var model: AppModel
    @State private var accounts: [SavedAccount] = []
    @State private var error: String?
    @State private var loading = true

    var body: some View {
        TabView {
            VStack(alignment: .leading, spacing: 16) {
                HStack {
                    Text("Accounts").font(.title2.weight(.semibold))
                    Spacer()
                    Button("Refresh", systemImage: "arrow.clockwise") { Task { await refresh() } }.disabled(loading)
                }
                if loading && accounts.isEmpty { ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity) }
                else if let error, accounts.isEmpty {
                    FailureView(title: "Could not load accounts", error: error) { Task { await refresh() } }
                } else if accounts.isEmpty {
                    ContentUnavailableView("No Accounts", systemImage: "person.crop.circle", description: Text("Sign in to add your Wendy Cloud account."))
                } else {
                    if let error { FailureNote(message: error) }
                    List(accounts) { entry in
                        HStack(alignment: .top, spacing: 12) {
                            Image(systemName: "person.crop.circle").font(.title2).foregroundStyle(.secondary)
                            VStack(alignment: .leading, spacing: 6) {
                                Text(entry.account.email.isEmpty ? "Email unavailable" : entry.account.email).font(.headline).textSelection(.enabled)
                                Text(entry.account.cloud).font(.callout).foregroundStyle(.secondary).textSelection(.enabled)
                                Text(entry.account.title + " · Context: " + entry.account.context).font(.caption).foregroundStyle(.secondary)
                            }
                            Spacer()
                            if entry.active { Label("Active", systemImage: "checkmark.circle.fill").font(.caption).foregroundStyle(.green) }
                        }.padding(.vertical, 10)
                    }.listStyle(.inset)
                }
                HStack {
                    Menu("Add Account") {
                        Button("Wendy Cloud") { Task { await model.signIn() } }
                        Button("Development Cloud") { Task { await model.signIn(development: true) } }
                    }.disabled(model.signingIn)
                    if model.signingIn {
                        ProgressView().controlSize(.small)
                        Text("Complete sign-in in your browser.").font(.caption).foregroundStyle(.secondary)
                        if let url = model.loginURL { Link("Open", destination: url) }
                    }
                }
            }.padding(24)
                .tabItem { Label("Accounts", systemImage: "person.crop.circle") }
        }.frame(width: 620, height: 450)
            .task { await refresh() }
            .onChange(of: model.signingIn) { _, signingIn in if !signingIn { Task { await refresh() } } }
    }
    private func refresh() async {
        loading = true
        defer { loading = false }
        do { accounts = try await model.client.accounts(); error = nil }
        catch { self.error = error.localizedDescription }
    }
}
