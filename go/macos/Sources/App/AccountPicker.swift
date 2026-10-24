#if os(macOS)
import SwiftUI

struct AccountChoice: Identifiable {
    let id: String
    let email: String
    let cloud: String
    let organizationID: String
    var organizationName: String
    let state: WDAccountState
    let active: Bool
    var accountKey: String { cloud + "\n" + (email.isEmpty ? id : email) }
    var organizationTitle: String { organizationName.isEmpty ? id : organizationName }
    var accountTitle: String { email.isEmpty ? "Saved account" : email }
}

extension GoClient {
    func accountChoices() async throws -> [AccountChoice] {
        try await perform {
            guard let p = WDAccountChoices() else { throw WendyError(message: "Could not load saved accounts.") }
            defer { WDAccountChoicesFree(p) }
            try Self.check(p.pointee.status)
            return UnsafeBufferPointer(start: p.pointee.items, count: p.pointee.count).map {
                AccountChoice(id: Self.string($0.context), email: Self.string($0.email), cloud: Self.string($0.cloud),
                    organizationID: Self.string($0.organization_id), organizationName: Self.string($0.organization_name),
                    state: $0.state, active: $0.active != 0)
            }
        }
    }
    func useAccount(_ context: String) async throws {
        try await perform { try Self.withCString(context) { try Self.finish(WDUseAccount($0)) } }
    }
}

@MainActor final class AccountPickerModel: ObservableObject {
    @Published var choices: [AccountChoice] = []
    @Published var loading = false
    @Published var switching = false
    @Published var error: String?
    private var generation = 0
    var active: AccountChoice? { choices.first { $0.active } }
    var groups: [[AccountChoice]] {
        Dictionary(grouping: choices, by: \.accountKey).values.map {
            $0.sorted { a, b in
                if a.active != b.active { return a.active }
                return a.organizationTitle.localizedStandardCompare(b.organizationTitle) == .orderedAscending
            }
        }.sorted { a, b in
            let activeA = a.contains { $0.active }, activeB = b.contains { $0.active }
            if activeA != activeB { return activeA }
            return (a.first?.accountKey ?? "") < (b.first?.accountKey ?? "")
        }
    }
    func load() async {
        generation += 1
        let request = generation
        loading = true; error = nil
        defer { if request == generation { loading = false } }
        do {
            let values = try await GoClient.shared.accountChoices()
            guard request == generation, !Task.isCancelled else { return }
            choices = values
            // Saved context IDs remain the selection authority. Cloud membership
            // only supplies display names; it never creates selectable sessions.
            let unresolved = groups.compactMap { group in group.first(where: { $0.organizationName.isEmpty && !$0.organizationID.isEmpty }) }
            for account in unresolved {
                guard request == generation, !Task.isCancelled else { return }
                do {
                    let organizations = try await CloudManagementClient().organizations(account: account.id)
                    guard request == generation, !Task.isCancelled else { return }
                    for i in choices.indices where choices[i].accountKey == account.accountKey {
                        if let organization = organizations.first(where: { $0.id == choices[i].organizationID }) {
                            choices[i].organizationName = organization.name
                        }
                    }
                } catch { /* Local context names remain usable when Cloud is offline. */ }
            }
        } catch { if request == generation { self.error = error.localizedDescription } }
    }
    func select(_ choice: AccountChoice, app: AppModel) async -> Bool {
        guard !switching else { return false }
        if choice.active { return true }
        return await select(context: choice.id, app: app)
    }
    func select(context: String, app: AppModel) async -> Bool {
        guard !switching else { return false }
        switching = true; error = nil
        defer { switching = false }
        do {
            try await GoClient.shared.useAccount(context)
            app.cloud = []; app.nearby = []
            await app.refreshAccount()
            await load()
            await app.refreshNearby()
            await app.refreshCloud()
            return true
        } catch { self.error = error.localizedDescription; return false }
    }
}

struct AccountPicker: View {
    @ObservedObject var app: AppModel
    @StateObject private var picker = AccountPickerModel()
    @State private var presented = false
    @State private var hovering = false
    private var title: String {
        if app.signingIn { return "Signing in…" }
        if app.account?.signedIn == true { return picker.active?.organizationTitle ?? "Wendy Cloud" }
        return app.account?.state == WD_ACCOUNT_NEEDS_LOGIN ? "Sign in again" : "Sign in to Wendy"
    }
    private var subtitle: String {
        if app.signingIn { return "Continue in your browser" }
        return app.account?.email.isEmpty == false ? app.account!.email : "Accounts & organizations"
    }
    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            Button { presented.toggle() } label: {
                HStack(spacing: 10) {
                    avatar(title, signedIn: app.account?.signedIn == true)
                    VStack(alignment: .leading, spacing: 3) {
                        Text(title).font(.system(size: 13, weight: .semibold)).lineLimit(1)
                        Text(subtitle).font(.system(size: 11)).foregroundStyle(.secondary).lineLimit(1).truncationMode(.middle)
                    }
                    Spacer(minLength: 4)
                    if app.signingIn || picker.switching { ProgressView().controlSize(.mini) }
                    else { Image(systemName: "chevron.up.chevron.down").font(.system(size: 9, weight: .semibold)).foregroundStyle(.secondary) }
                }
                .padding(.horizontal, 10).padding(.vertical, 9)
                .frame(maxWidth: .infinity, alignment: .leading)
                .background(hovering || presented ? Color.primary.opacity(0.055) : .clear, in: RoundedRectangle(cornerRadius: 9))
                .contentShape(RoundedRectangle(cornerRadius: 9))
            }
            .buttonStyle(.plain)
            .onHover { hovering = $0 }
            .help("Switch account or organization")
            .accessibilityLabel("Account and organization")
            .accessibilityValue(title + ", " + subtitle)
            .popover(isPresented: $presented, arrowEdge: .bottom) { popover }
            if let error = app.accountError { FailureNote(message: error).padding(.horizontal, 10) }
        }
        .padding(8)
        .task(id: app.account?.context) { await picker.load() }
        .onChange(of: app.signingIn) { _, value in if !value { Task { await picker.load() } } }
    }
    private func avatar(_ name: String, signedIn: Bool) -> some View {
        ZStack {
            RoundedRectangle(cornerRadius: 8).fill(Color.accentColor.opacity(0.14))
            if signedIn {
                Text(String(name.prefix(1)).uppercased()).font(.system(size: 15, weight: .semibold)).foregroundStyle(Color.accentColor)
            } else { Image(systemName: "person.crop.circle").font(.system(size: 19)).foregroundStyle(.secondary) }
        }.frame(width: 34, height: 34)
    }
    private var popover: some View {
        VStack(spacing: 0) {
            HStack {
                Text("Accounts & organizations").font(.system(size: 12, weight: .semibold)).foregroundStyle(.secondary)
                Spacer()
                if picker.switching { ProgressView().controlSize(.mini) }
                Button { Task { await picker.load() } } label: {
                    Image(systemName: "arrow.clockwise").font(.system(size: 11, weight: .medium))
                }.buttonStyle(.plain).help("Refresh accounts").accessibilityLabel("Refresh accounts").disabled(picker.loading || picker.switching)
            }.padding(.horizontal, 16).padding(.top, 16).padding(.bottom, 10)
            if picker.loading && picker.choices.isEmpty { ProgressView().controlSize(.small).padding(20) }
            else if picker.choices.isEmpty {
                VStack(alignment: .leading, spacing: 5) {
                    Text("No saved accounts").font(.system(size: 13, weight: .medium))
                    Text("Sign in to access your organizations and cloud devices.").font(.callout).foregroundStyle(.secondary)
                }.frame(maxWidth: .infinity, alignment: .leading).padding(16)
            } else {
                ScrollView {
                    VStack(alignment: .leading, spacing: 14) {
                        ForEach(picker.groups, id: \.first!.accountKey) { group in
                            VStack(alignment: .leading, spacing: 5) {
                                if let first = group.first {
                                    VStack(alignment: .leading, spacing: 2) {
                                        Text(first.accountTitle).font(.system(size: 12, weight: .medium)).lineLimit(1).truncationMode(.middle)
                                        if first.cloud != "api.wendy.dev:443" { Text(first.cloud).font(.system(size: 10)).foregroundStyle(.tertiary).lineLimit(1) }
                                    }.padding(.horizontal, 10).padding(.bottom, 3)
                                }
                                ForEach(group) { choice in
                                    Button {
                                        Task { if await picker.select(choice, app: app) { presented = false } }
                                    } label: {
                                        HStack(spacing: 10) {
                                            Image(systemName: "building.2").foregroundStyle(choice.active ? Color.accentColor : .secondary).frame(width: 18)
                                            VStack(alignment: .leading, spacing: 2) {
                                                Text(choice.organizationTitle).font(.system(size: 13, weight: choice.active ? .semibold : .regular)).lineLimit(1)
                                                if choice.state != WD_ACCOUNT_SIGNED_IN { Text("Sign-in required").font(.caption).foregroundStyle(.orange) }
                                            }
                                            Spacer()
                                            if choice.active { Image(systemName: "checkmark").font(.system(size: 11, weight: .semibold)).foregroundStyle(Color.accentColor) }
                                        }.padding(.horizontal, 10).padding(.vertical, 9)
                                            .background(choice.active ? Color.accentColor.opacity(0.09) : .clear, in: RoundedRectangle(cornerRadius: 7))
                                            .contentShape(RoundedRectangle(cornerRadius: 7))
                                    }.buttonStyle(.plain).disabled(picker.switching || app.signingIn)
                                        .accessibilityAddTraits(choice.active ? [.isSelected] : [])
                                }
                            }
                        }
                    }.padding(.horizontal, 8).padding(.vertical, 6)
                }.frame(maxHeight: 320)
            }
            if let error = picker.error { FailureNote(message: error).padding(12) }
            Divider()
            VStack(alignment: .leading, spacing: 2) {
                Menu {
                    Button("Wendy Cloud") { presented = false; Task { await app.signIn() } }
                    Button("Development Cloud") { presented = false; Task { await app.signIn(development: true) } }
                } label: {
                    Label("Add account…", systemImage: "plus").frame(maxWidth: .infinity, alignment: .leading).padding(8)
                }.menuStyle(.borderlessButton).disabled(app.signingIn || picker.switching)
                SettingsLink {
                    Label("Manage accounts…", systemImage: "person.crop.circle").frame(maxWidth: .infinity, alignment: .leading).padding(8)
                }.buttonStyle(.plain)
                if app.signingIn {
                    if let url = app.loginURL { Link("Continue in browser", destination: url).padding(8) }
                    Button("Cancel sign-in") { Task { await app.cancelLogin() } }.buttonStyle(.plain).padding(8)
                }
            }.font(.system(size: 12)).padding(8)
        }.frame(width: 320).fixedSize(horizontal: false, vertical: true)
    }
}
#endif
