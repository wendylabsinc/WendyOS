import SwiftUI

struct AccountsSettingsView: View {
    @State private var actions = AsyncActions()
    @ObservedObject var model: AppModel
    @State private var accounts: [SavedAccount] = []
    @State private var error: String?
    @State private var loading = true
    #if os(macOS)
    @StateObject private var picker = AccountPickerModel()
    @State private var switchingContext: String?
    #endif
    private var busy: Bool {
        #if os(macOS)
        return loading || picker.switching || model.signingIn
        #else
        return loading || model.signingIn
        #endif
    }

    var body: some View {
        actionContent.task { await actions.run() }
    }

    @ViewBuilder private var actionContent: some View {
        TabView {
            VStack(alignment: .leading, spacing: 16) {
                HStack {
                    Text("Accounts").font(.title2.weight(.semibold))
                    Spacer()
                    Button("Refresh", systemImage: "arrow.clockwise") { actions.submit { await refresh() } }.disabled(busy)
                }
                if loading && accounts.isEmpty { ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity) }
                else if let error, accounts.isEmpty {
                    FailureView(title: "Could not load accounts", error: error) { actions.submit { await refresh() } }
                } else if accounts.isEmpty {
                    ContentUnavailableView("No Accounts", systemImage: "person.crop.circle", description: Text("Sign in to add your Wendy Cloud account."))
                        .frame(maxWidth: .infinity, maxHeight: .infinity)
                } else {
                    if let error { FailureNote(message: error) }
                    List(accounts) { entry in
                        Group {
                            #if os(macOS)
                            Button { actions.submit { await select(entry) } } label: {
                                accountRow(entry)
                            }
                            .buttonStyle(.plain)
                            .disabled(busy || entry.active)
                            .accessibilityLabel("Use account " + accountTitle(entry) + ", " + (organizationTitle(entry) ?? "Saved organization"))
                            .accessibilityAddTraits(entry.active ? [.isSelected] : [])
                            #else
                            accountRow(entry)
                            #endif
                        }
                        .alignmentGuide(.listRowSeparatorLeading) { _ in 0 }
                        .alignmentGuide(.listRowSeparatorTrailing) { dimensions in dimensions.width }
                    }.listStyle(.inset)
                }
                HStack {
                    Menu("Add Account") {
                        Button("Wendy Cloud") { actions.submit { await model.signIn() } }
                        Button("Development Cloud") { actions.submit { await model.signIn(development: true) } }
                    }.disabled(busy)
                    if model.signingIn {
                        ProgressView().controlSize(.small)
                        Text("Complete sign-in in your browser.").font(.caption).foregroundStyle(.secondary)
                        if let url = model.loginURL { Link("Open", destination: url) }
                    }
                }
            }.padding(24)
                .tabItem { Label("Accounts", systemImage: "person.crop.circle") }
        }.desktopFrame(width: 620, height: 450)
            .task(id: "\(model.account?.context ?? ""):\(model.signingIn)") {
                if !model.signingIn { await refresh() }
            }
    }
    private func accountRow(_ entry: SavedAccount) -> some View {
        HStack(alignment: .top, spacing: 12) {
            Image(systemName: "person.crop.circle").font(.title2).foregroundStyle(.secondary)
            VStack(alignment: .leading, spacing: 6) {
                Text(accountTitle(entry)).font(.headline)
                if let organization = organizationTitle(entry) {
                    Text(organization).font(.callout)
                }
                Text(entry.account.cloud).font(.callout).foregroundStyle(.secondary)
                Text(entry.account.title).font(.caption).foregroundStyle(.secondary)
            }
            Spacer()
            if entry.active {
                Label("Active", systemImage: "checkmark.circle.fill").font(.caption).foregroundStyle(.green)
            }
            #if os(macOS)
            if !entry.active {
                if switchingContext == entry.id {
                    ProgressView().controlSize(.small)
                } else {
                    Text("Use account").font(.caption).foregroundStyle(Color.accentColor)
                }
            }
            #endif
        }
        .padding(.vertical, 10)
        .frame(maxWidth: .infinity, alignment: .leading)
        .contentShape(Rectangle())
    }
    private func accountTitle(_ entry: SavedAccount) -> String {
        entry.account.email.isEmpty ? "Saved account" : entry.account.email
    }
    private func organizationTitle(_ entry: SavedAccount) -> String? {
        #if os(macOS)
        return picker.choices.first { $0.id == entry.id }?.organizationTitle
        #else
        return nil
        #endif
    }
    #if os(macOS)
    private func select(_ entry: SavedAccount) async {
        guard !busy, !entry.active else { return }
        switchingContext = entry.id
        error = nil
        defer { switchingContext = nil }
        if await picker.select(context: entry.account.context, app: model) {
            await refresh()
        } else {
            error = picker.error
        }
    }
    #endif
    private func refresh() async {
        loading = true
        defer { loading = false }
        do {
            accounts = try await model.client.accounts()
            error = nil
            #if os(macOS)
            await picker.load()
            #endif
        }
        catch { if !Task.isCancelled { self.error = error.localizedDescription } }
    }
}
