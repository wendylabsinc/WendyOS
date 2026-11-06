#if os(macOS)
import SwiftUI

/// Settings → Accounts: one entry per person, with their signed-in
/// organizations under it. Each organization can be used or signed out of,
/// and each person can be signed out of as a whole.
struct AccountsSettingsView: View {
    @State private var actions = AsyncActions()
    @ObservedObject var model: AppModel
    @StateObject private var picker = AccountPickerModel()
    @State private var switchingContext: String?
    @State private var pendingSignOut: AccountSignOut?
    @State private var error: String?
    @State private var loading = true
    private var busy: Bool { loading || picker.switching || model.signingIn || model.signingOut }

    var body: some View {
        actionContent.task { await actions.run() }
    }

    @ViewBuilder private var actionContent: some View {
        TabView {
            VStack(alignment: .leading, spacing: 16) {
                HStack {
                    Text("Accounts").font(.title2.weight(.semibold))
                    Spacer()
                    if model.signingOut { ProgressView().controlSize(.small); Text("Signing out…").font(.caption).foregroundStyle(.secondary) }
                    Button("Refresh", systemImage: "arrow.clockwise") { actions.submit { await refresh() } }.disabled(busy)
                }
                if loading && picker.choices.isEmpty { ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity) }
                else if let error, picker.choices.isEmpty {
                    FailureView(title: "Could not load accounts", error: error) { actions.submit { await refresh() } }
                } else if picker.choices.isEmpty {
                    ContentUnavailableView("No Accounts", systemImage: "person.crop.circle", description: Text("Sign in to add your Wendy Cloud account."))
                        .frame(maxWidth: .infinity, maxHeight: .infinity)
                } else {
                    if let error { FailureNote(message: error) }
                    List {
                        ForEach(picker.groups, id: \.first!.accountKey) { group in
                            let person = AccountPickerModel.person(group)
                            Section {
                                ForEach(group) { choice in
                                    organizationRow(choice, person: person)
                                        .alignmentGuide(.listRowSeparatorLeading) { _ in 0 }
                                        .alignmentGuide(.listRowSeparatorTrailing) { dimensions in dimensions.width }
                                }
                            } header: { personHeader(person) }
                        }
                    }.listStyle(.inset)
                }
                HStack {
                    Menu("Add Account") {
                        Button("Wendy Cloud") { actions.submit { await model.signIn() } }
                        Button("Development Cloud") { actions.submit { await model.signIn(development: true) } }
                    }.disabled(busy)
                        .help("Sign in to another account or organization")
                    if model.signingIn {
                        ProgressView().controlSize(.small)
                        Text("Complete sign-in in your browser.").font(.caption).foregroundStyle(.secondary)
                        if let url = model.loginURL { Link("Open", destination: url) }
                    }
                }
            }.padding(24)
                .tabItem { Label("Accounts", systemImage: "person.crop.circle") }
        }.desktopFrame(width: 620, height: 450)
            .task(id: "\(model.account?.context ?? ""):\(model.signingIn):\(model.accountsRevision)") {
                if !model.signingIn { await refresh() }
            }
            .accountSignOutConfirmation($pendingSignOut, app: model)
    }

    private func personHeader(_ person: AccountPerson) -> some View {
        HStack(spacing: 10) {
            Image(systemName: "person.crop.circle").font(.title2).foregroundStyle(.secondary)
            VStack(alignment: .leading, spacing: 2) {
                Text(person.title).font(.headline).foregroundStyle(.primary)
                Text(person.cloud).font(.caption).foregroundStyle(.secondary)
            }
            Spacer()
            Button("Sign Out…") { pendingSignOut = AccountSignOut.person(person) }
                .disabled(busy)
                .help("Sign out of every organization of \(person.title) on this Mac")
                .accessibilityLabel("Sign out of " + person.title)
        }
        .padding(.vertical, 6)
        .contextMenu {
            Button("Sign Out of \(person.title)…") { pendingSignOut = AccountSignOut.person(person) }.disabled(busy)
        }
    }

    private func organizationRow(_ choice: AccountChoice, person: AccountPerson) -> some View {
        HStack(spacing: 12) {
            Button { actions.submit { await select(choice) } } label: {
                HStack(spacing: 12) {
                    Image(systemName: "building.2").foregroundStyle(choice.active ? Color.accentColor : .secondary).frame(width: 18)
                    VStack(alignment: .leading, spacing: 3) {
                        Text(choice.organizationTitle).font(.body.weight(choice.active ? .semibold : .regular))
                        Text(choice.saved.account.title).font(.caption).foregroundStyle(choice.state == WD_ACCOUNT_SIGNED_IN ? .secondary : Color.orange)
                    }
                    Spacer()
                    if choice.active {
                        Label("Active", systemImage: "checkmark.circle.fill").font(.caption).foregroundStyle(.green)
                    } else if switchingContext == choice.id {
                        ProgressView().controlSize(.small)
                    } else {
                        Text("Use").font(.caption).foregroundStyle(Color.accentColor)
                    }
                }.contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .disabled(busy || choice.active)
            .accessibilityLabel("Use organization " + choice.organizationTitle + ", " + person.title)
            .accessibilityAddTraits(choice.active ? [.isSelected] : [])
            Button("Sign Out…") { pendingSignOut = AccountSignOut.organization(choice.saved, person: person) }
                .disabled(busy)
                .help("Sign out of \(choice.organizationTitle) on this Mac")
                .accessibilityLabel("Sign out of " + choice.organizationTitle)
        }
        .padding(.vertical, 8).padding(.leading, 28)
        .frame(maxWidth: .infinity, alignment: .leading)
        .contextMenu {
            if !choice.active { Button("Use \(choice.organizationTitle)") { actions.submit { await select(choice) } }.disabled(busy) }
            Button("Sign Out of \(choice.organizationTitle)…") { pendingSignOut = AccountSignOut.organization(choice.saved, person: person) }.disabled(busy)
        }
    }

    private func select(_ choice: AccountChoice) async {
        guard !busy, !choice.active else { return }
        switchingContext = choice.id
        error = nil
        defer { switchingContext = nil }
        if await picker.select(context: choice.id, app: model) {
            await refresh()
        } else {
            error = picker.error
        }
    }
    private func refresh() async {
        loading = true
        defer { loading = false }
        await picker.load()
        error = picker.error
    }
}
#endif
