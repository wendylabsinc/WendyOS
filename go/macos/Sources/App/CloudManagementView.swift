import SwiftUI

struct CloudManagementView: View {
    @EnvironmentObject private var app: AppModel
    @StateObject private var model = CloudManagementModel()
    @State private var invitationID: String?
    @State private var memberID: String?
    @State private var formSubmitting = false
    @State private var form: CloudForm?
    @State private var removal: CloudRemoval?
    @State private var email = ""
    @State private var role = ""
    @State private var user = ""
    @State private var device = ""
    @State private var deny = false
    @State private var expires = Date().addingTimeInterval(7 * 86400)
    @State private var invitationLink = ""
    @State private var details: String?
    @State private var initialLoadComplete = false
    private enum CloudForm: String, Identifiable {
        case teammate = "Add teammate"
        case invitation = "Create invitation"
        case invitations = "Invitations"
        case access = "Add device access"
        case role = "Change role"
        var id: String { rawValue }
    }
    private enum CloudRemoval: Identifiable {
        case member(CloudMember)
        case access(CloudAccessRule)
        case invite(CloudInvitation)
        var id: String {
            switch self {
            case .member(let x): x.id
            case .access(let x): x.id
            case .invite(let x): x.id
            }
        }
        var title: String {
            switch self {
            case .member: "Remove teammate?"
            case .access: "Remove access rule?"
            case .invite: "Revoke invitation?"
            }
        }
        var message: String {
            switch self {
            case .member(let x):
                "Remove \(x.title) from this organization and remove their direct device access rules."
            case .access(let x):
                "Remove the \(x.effectName.lowercased()) rule for \(x.principal) on \(x.target). Other rules still apply."
            case .invite: "This invitation link will no longer let people join the organization."
            }
        }
    }

    var body: some View {
        Group {
            #if os(iOS)
            mobileContent
            #else
            desktopContent
            #endif
        }
            .focusedSceneValue(\.organizationRefresh, PaneRefreshAction(
                enabled: !model.loading && !model.saving,
                perform: { Task { await model.loadAccounts() } }
            ))
            #if os(iOS)
            .task(id: app.mobileCloudSelectionKey) {
                model.accounts = app.mobileCloudAccounts
                if model.account != app.mobileCloudAccount || model.organization != app.mobileCloudOrganization {
                    model.account = app.mobileCloudAccount
                    model.organization = app.mobileCloudOrganization
                    await model.refresh(clear: true)
                } else if model.snapshot == nil { await model.refresh() }
                initialLoadComplete = true
            }
            #else
            .task { await model.loadAccounts(); initialLoadComplete = true }
            .onChange(of: app.signingIn) { old, new in
                if old && !new { Task { await model.loadAccounts() } }
            }
            #endif
            .sheet(item: $form) { kind in
                if kind == .invitations { invitationsSheet } else { formView(kind) }
            }
            .onChange(of: form) { _, _ in model.actionError = nil }
            .alert(
                removal?.title ?? "Confirm",
                isPresented: Binding(
                    get: { removal != nil && form != .invitations },
                    set: { if !$0 { removal = nil } }
                )
            ) {
                Button("Cancel", role: .cancel) { removal = nil }
                Button("Confirm", role: .destructive) {
                    if let action = removal {
                        removal = nil
                        Task { await remove(action) }
                    }
                }
            } message: {
                Text(removal?.message ?? "")
            }
            .alert(
                "Could not complete the action",
                isPresented: Binding(
                    get: { model.actionError != nil && form == nil },
                    set: { if !$0 { model.actionError = nil } }
                )
            ) {
                Button("OK") { model.actionError = nil }
                Button("Technical details") {
                    details = model.actionError
                    model.actionError = nil
                }
            } message: {
                Text(PresentedFailure(model.actionError ?? "").message)
            }
            .sheet(isPresented: Binding(get: { details != nil }, set: { if !$0 { details = nil } }))
        {
            VStack(alignment: .leading, spacing: 16) {
                Text("Technical details").font(.title2)
                ScrollView {
                    Text(details ?? "").font(.system(.callout, design: .monospaced)).textSelection(
                        .enabled
                    )
                }
                Button("Done") { details = nil }.keyboardShortcut(.defaultAction)
            }.padding(24).desktopFrame(width: 600, height: 320)
        }
    }

    private var desktopContent: some View {
        VStack(alignment: .leading, spacing: 20) {
            HStack {
                VStack(alignment: .leading, spacing: 5) {
                    Text("Wendy Cloud").font(.largeTitle.bold())
                    Text("Your organization, teammates, and device access.").foregroundStyle(
                        .secondary
                    )
                }
                Spacer()
                if model.loading || model.saving { ProgressView().controlSize(.small) }
            }.padding(.horizontal, 28).padding(.top, 28)
            if model.accounts.isEmpty {
                ContentUnavailableView {
                    Label("Connect to Wendy Cloud", systemImage: "cloud")
                } description: {
                    Text("Sign in to manage your organization.")
                } actions: {
                    Button(app.signingIn ? "Signing in…" : "Sign in") {
                        Task { await app.signIn() }
                    }.disabled(app.signingIn)
                    if let url = app.loginURL { Link("Continue in browser", destination: url) }
                }
                .frame(maxWidth: .infinity, maxHeight: .infinity)
            } else {
                ViewThatFits(in: .horizontal) {
                    HStack(alignment: .firstTextBaseline, spacing: 12) {
                        accountMenu
                        organizationPicker
                        teammateCount
                        Spacer(minLength: 16)
                        teammateMenu
                    }
                    VStack(alignment: .leading, spacing: 10) {
                        HStack(spacing: 12) {
                            accountMenu
                            organizationPicker
                            Spacer(minLength: 0)
                        }
                        HStack {
                            teammateCount
                            Spacer()
                            teammateMenu
                        }
                    }
                }.disabled(model.saving).padding(.horizontal, 28)
                if let error = model.error {
                    if model.snapshot == nil {
                        FailureView(title: "Could not load organization", error: error) {
                            Task { await model.loadAccounts() }
                        }.padding(.horizontal, 28)
                    } else {
                        FailureNote(message: error).padding(.horizontal, 28)
                    }
                }
                if let snapshot = model.snapshot {
                    if !snapshot.permissionsError.isEmpty {
                        FailureNote(message: snapshot.permissionsError).padding(.horizontal, 28)
                    }
                    team(snapshot)
                } else if model.loading {
                    Spacer()
                    HStack {
                        Spacer()
                        ProgressView("Loading organization…")
                        Spacer()
                    }
                    Spacer()
                } else if model.error == nil {
                    ContentUnavailableView(
                        "No organizations",
                        systemImage: "person.2",
                        description: Text(
                            "Sign in with an account that belongs to an organization."
                        )
                    )
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
                }
            }
        }.frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
    }
    #if os(iOS)
    @ViewBuilder private var mobileContent: some View {
        if !initialLoadComplete {
            ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
        } else {
            Form {
                if model.accounts.isEmpty {
                    ContentUnavailableView { Label("Connect to Wendy Cloud", systemImage: "cloud") }
                        description: { Text("Sign in to manage your organization.") }
                        actions: { Button("Sign in") { Task { await app.signIn() } } }
                } else {
                    if let error = model.error { FailureNote(message: error) }
                    if let snapshot = model.snapshot {
                        if !snapshot.permissionsError.isEmpty { FailureNote(message: snapshot.permissionsError) }
                        if !snapshot.teamError.isEmpty { FailureNote(message: snapshot.teamError) }
                        else {
                            Section {
                                ForEach(snapshot.members) { member in
                                    NavigationLink(value: member.id) {
                                        VStack(alignment: .leading, spacing: 5) {
                                            Text(member.title).font(.body.weight(.medium))
                                            if !member.email.isEmpty && member.email != member.title {
                                                Text(member.email).font(.caption).foregroundStyle(.secondary)
                                            }
                                            Text(roleName(member.roleID, snapshot)).font(.caption).foregroundStyle(.secondary)
                                        }.padding(.vertical, 4)
                                    }
                                }
                            } header: { Text("Teammates") }
                        }
                    } else if model.loading { ProgressView().frame(maxWidth: .infinity) }
                    else if model.error == nil { Text("No organizations").foregroundStyle(.secondary) }
                }
            }
            .refreshable { await model.refresh() }
            .toolbar { ToolbarItem(placement: .topBarTrailing) { teammateMenu.labelStyle(.iconOnly).disabled(model.snapshot == nil) } }
            .navigationDestination(for: String.self) { id in
                if let snapshot = model.snapshot, let member = snapshot.members.first(where: { $0.id == id }) {
                    mobileMemberDetails(member, snapshot)
                        .navigationTitle(member.title).navigationBarTitleDisplayMode(.inline)
                        .refreshable { await model.refresh() }
                }
            }
        }
    }
    private func mobileMemberDetails(_ member: CloudMember, _ snapshot: CloudOrganizationSnapshot) -> some View {
        Form {
            Section {
                if !member.email.isEmpty { LabeledContent("Email", value: member.email).textSelection(.enabled) }
                if !member.profileError.isEmpty { FailureNote(message: member.profileError) }
            }
            Section("Role") {
                LabeledContent("Assigned role", value: roleName(member.roleID, snapshot))
                Button("Change role") {
                    user = member.id; role = member.roleID; form = .role
                }.disabled(!model.can("member:set-role") || !snapshot.rolesError.isEmpty)
                if let assigned = snapshot.roles.first(where: { $0.id == member.roleID }) {
                    NavigationLink("Role permissions") {
                        List(assigned.permissions.sorted(), id: \.self) { permission in
                            Text(permission).font(.callout.monospaced())
                        }.navigationTitle("Role permissions").navigationBarTitleDisplayMode(.inline)
                    }
                }
                if !snapshot.rolesError.isEmpty { FailureNote(message: snapshot.rolesError) }
            }
            Section {
                if !snapshot.accessError.isEmpty { FailureNote(message: snapshot.accessError) }
                else {
                    let rules = snapshot.rules.filter { $0.principalType == WD_PRINCIPAL_USER && $0.principalID == member.id }
                    if rules.isEmpty { Text("No direct rules").foregroundStyle(.secondary) }
                    ForEach(rules) { rule in ruleRow(rule) }
                }
                Button("Add device access", systemImage: "plus") {
                    user = member.id; device = ""; deny = false; form = .access
                }.disabled(!model.can("device-access:create"))
            } header: { Text("Direct device access") }
                footer: { Text("Group rules may also apply. A deny overrides an allow. Device access also requires the appropriate role permissions.") }
            Section {
                Button("Remove teammate", role: .destructive) { removal = .member(member) }
                    .disabled(!model.can("member:remove"))
            }
        }
    }
    #endif

    private var accountMenu: some View {
        Menu {
            Picker(
                "Account",
                selection: Binding(
                    get: { model.account },
                    set: { value in
                        model.account = value
                        memberID = nil
                        Task { await model.loadOrganizations() }
                    }
                )
            ) {
                ForEach(model.accounts) { account in
                    Text(
                        (account.account.email.isEmpty
                            ? account.id : account.account.email) + " · " + account.id
                    )
                    .tag(account.id)
                }
            }
            Divider()
            Button("Sign in", systemImage: "person.crop.circle.badge.plus") {
                Task { await app.signIn() }
            }.disabled(app.signingIn)
        } label: {
            Label(selectedAccountTitle, systemImage: "person.crop.circle")
                .lineLimit(1).truncationMode(.middle)
                .frame(maxWidth: 240, alignment: .leading)
        }.fixedSize(horizontal: true, vertical: false).help("Cloud account")
    }

    private var organizationPicker: some View {
        Picker(
            "Organization",
            selection: Binding(
                get: { model.organization },
                set: { value in
                    model.organization = value
                    memberID = nil
                    Task { await model.refresh(clear: true) }
                }
            )
        ) {
            ForEach(model.organizations) { Text($0.name).tag($0.id) }
        }.labelsHidden().accessibilityLabel("Organization").fixedSize(horizontal: true, vertical: false)
    }

    @ViewBuilder private var teammateCount: some View {
        if let snapshot = model.snapshot, snapshot.teamError.isEmpty {
            Text("\(snapshot.members.count) teammates")
                .foregroundStyle(.secondary).lineLimit(1).fixedSize()
        }
    }

    private var selectedAccountTitle: String {
        guard let account = model.accounts.first(where: { $0.id == model.account }) else {
            return "Account"
        }
        return account.account.email.isEmpty ? account.id : account.account.email
    }

    private var teammateMenu: some View {
        Menu {
            Button("Add by email", systemImage: "person.badge.plus") {
                email = ""
                role = ""
                form = .teammate
            }.disabled(!model.can("member:add"))
            Button("Create invite link", systemImage: "link") {
                role = ""
                invitationLink = ""
                form = .invitation
            }.disabled(!model.can("invite:create"))
            Divider()
            Button("Manage invitations", systemImage: "envelope") { form = .invitations }
        } label: {
            Label("Add teammate", systemImage: "person.badge.plus")
        }.disabled(model.saving)
    }

    private func roleName(_ id: String, _ s: CloudOrganizationSnapshot) -> String {
        s.roles.first(where: { $0.id == id })?.name ?? (id.isEmpty ? "No assigned role" : id)
    }
    @ViewBuilder private func team(_ s: CloudOrganizationSnapshot) -> some View {
        if !s.teamError.isEmpty {
            FailureView(title: "Could not load teammates", error: s.teamError) {
                Task { await model.refresh() }
            }
        } else {
            HSplitView {
                Table(s.members, selection: $memberID) {
                    TableColumn("Teammate") { m in
                        VStack(alignment: .leading, spacing: 3) {
                            Text(m.title).fontWeight(.medium)
                            if !m.email.isEmpty && m.email != m.title {
                                Text(m.email).font(.caption).foregroundStyle(.secondary)
                            }
                        }.padding(.vertical, 5)
                    }
                    TableColumn("Role") { m in
                        Text(roleName(m.roleID, s)).foregroundStyle(.secondary)
                    }.width(min: 140, ideal: 180, max: 240)
                }
                    #if os(macOS)
                    .tableStyle(.bordered)
                    #endif
                    .mobileListHeight(280).desktopMinimumWidth(330)
                    .keyboardRowSelection(s.members.map(\.id), selection: $memberID)
                ScrollView {
                    if let member = s.members.first(where: { $0.id == memberID }) {
                        memberDetails(member, s)
                    } else {
                        Text("Select a teammate").foregroundStyle(.secondary).padding(18)
                    }
                }.desktopWidth(min: 280, ideal: 320, max: 400)
            }
        }
    }
    private func memberDetails(_ member: CloudMember, _ s: CloudOrganizationSnapshot) -> some View {
                        VStack(alignment: .leading, spacing: 16) {
                            Text(member.title).font(.title3.bold()).textSelection(.enabled)
                            if !member.email.isEmpty {
                                Text(member.email).foregroundStyle(.secondary).textSelection(
                                    .enabled
                                )
                            }
                            if !member.profileError.isEmpty {
                                FailureNote(message: member.profileError)
                            }
                            LabeledContent("Role", value: roleName(member.roleID, s))
                            HStack {
                                Button("Change role") {
                                    user = member.id
                                    role = member.roleID
                                    form = .role
                                }.disabled(!model.can("member:set-role") || !s.rolesError.isEmpty)
                                Button("Remove", role: .destructive) { removal = .member(member) }
                                    .disabled(!model.can("member:remove"))
                            }
                            Divider()
                            Text("Direct device access").font(.headline)
                            Text(
                                "Group rules may also apply. A deny overrides an allow. Device access also requires the appropriate role permissions."
                            ).font(.caption).foregroundStyle(.secondary)
                            if !s.accessError.isEmpty {
                                FailureNote(message: s.accessError)
                            } else {
                                let entries = s.rules.filter {
                                    $0.principalType == WD_PRINCIPAL_USER
                                        && $0.principalID == member.id
                                }
                                if entries.isEmpty {
                                    Text("No direct rules").foregroundStyle(.secondary)
                                }
                                ForEach(entries) { rule in ruleRow(rule) }
                            }
                            Button("Add device access") {
                                user = member.id
                                device = ""
                                deny = false
                                form = .access
                            }.disabled(!model.can("device-access:create"))
                            if let assigned = s.roles.first(where: { $0.id == member.roleID }) {
                                DisclosureGroup("Assigned role permissions") {
                                    ForEach(assigned.permissions.sorted(), id: \.self) {
                                        Text($0).font(.caption.monospaced()).frame(
                                            maxWidth: .infinity,
                                            alignment: .leading
                                        )
                                    }
                                }
                            }
                            if !s.rolesError.isEmpty { FailureNote(message: s.rolesError) }
                        }.padding(18)
    }
    private func ruleRow(_ r: CloudAccessRule) -> some View {
        HStack {
            Circle().fill(r.effect == WD_ACCESS_ALLOW ? .green : .red).frame(width: 7, height: 7)
            VStack(alignment: .leading, spacing: 3) {
                Text(r.target)
                Text("\(r.effectName) · \(r.scope)").font(.caption).foregroundStyle(.secondary)
            }
            Spacer()
            Button {
                removal = .access(r)
            } label: {
                Image(systemName: "minus.circle")
            }.buttonStyle(.borderless).help("Remove rule").disabled(
                !model.can("device-access:delete")
            )
        }.padding(.vertical, 5)
    }
    private var invitationsSheet: some View {
        VStack(alignment: .leading, spacing: 0) {
            VStack(alignment: .leading, spacing: 20) {
                HStack {
                    Text("Invitations").font(.title2.bold())
                    Spacer()
                    if model.saving || model.loading { ProgressView().controlSize(.small) }
                    Button("Done") { form = nil }.keyboardShortcut(.cancelAction).disabled(model.saving)
                }
                if let error = model.actionError { FailureNote(message: error) }
            }.padding(24)
            if let snapshot = model.snapshot { invitations(snapshot) }
        }
        .desktopFrame(width: 720, height: 460)
        .interactiveDismissDisabled(model.saving)
        .alert(
            "Revoke invitation?",
            isPresented: Binding(
                get: {
                    if case .invite = removal { return true }
                    return false
                },
                set: { if !$0 { removal = nil } }
            )
        ) {
            Button("Cancel", role: .cancel) { removal = nil }
            Button("Revoke", role: .destructive) {
                if let action = removal {
                    removal = nil
                    Task { await remove(action) }
                }
            }
        } message: {
            Text("This invitation link will no longer let people join the organization.")
        }
    }

    @ViewBuilder private func invitations(_ s: CloudOrganizationSnapshot) -> some View {
        HStack {
            Text("Create a link to share with a teammate. Links are shown only once.")
                .foregroundStyle(.secondary)
            Spacer()
            Button("Create invitation") {
                role = ""
                invitationLink = ""
                form = .invitation
            }.disabled(!model.can("invite:create"))
        }.padding(.horizontal, 24).padding(.bottom, 20)
        if !s.invitesError.isEmpty {
            FailureView(title: "Could not load invitations", error: s.invitesError) {
                Task { await model.refresh() }
            }
        } else if s.invitations.isEmpty {
            ContentUnavailableView("No invitations", systemImage: "envelope")
                .frame(maxWidth: .infinity, maxHeight: .infinity)
        } else {
            Table(s.invitations, selection: $invitationID) {
                TableColumn("Role") {
                    Text($0.role.isEmpty ? "Organization default" : $0.role.capitalized)
                }
                TableColumn("Status") { i in
                    Label(i.state, systemImage: "circle.fill").foregroundStyle(
                        i.state == "Active" ? .green : .secondary
                    )
                }
                TableColumn("Expires") { Text($0.expires, style: .date) }
                TableColumn("") { i in
                    Button("Revoke") { removal = .invite(i) }.disabled(
                        !model.can("invite:revoke") || i.state != "Active"
                    )
                }.width(100)
            }
                #if os(macOS)
                .tableStyle(.bordered)
                #endif
                .keyboardRowSelection(s.invitations.map(\.id), selection: $invitationID)
        }
    }
    @ViewBuilder private func formView(_ kind: CloudForm) -> some View {
        #if os(iOS)
        NavigationStack {
            Form {
                if let error = model.actionError {
                    Section { FailureNote(message: error) }
                }
                if !invitationLink.isEmpty && kind == .invitation {
                    Section {
                        Text(invitationLink).font(.callout.monospaced()).textSelection(.enabled)
                        Button("Copy link") { PlatformClipboard.copy(invitationLink) }
                    } footer: {
                        Text("Copy this link before closing. Anyone with the link can join with the selected role until it expires.")
                    }
                } else {
                    Section {
                        formFields(kind)
                    } footer: {
                        Text(model.snapshot?.name ?? "")
                    }
                    .disabled(model.saving || formSubmitting)
                }
            }
            .navigationTitle(kind.rawValue)
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button(invitationLink.isEmpty ? "Cancel" : "Close") { form = nil }
                        .disabled(model.saving || formSubmitting)
                }
                if invitationLink.isEmpty || kind != .invitation {
                    ToolbarItem(placement: .confirmationAction) {
                        Button(kind == .invitation ? "Create link" : "Save") { submitForm(kind) }
                            .disabled(formInvalid(kind))
                    }
                }
            }
            .overlay {
                if model.saving || formSubmitting { ProgressView().controlSize(.large) }
            }
        }
        .presentationBackground(Color(uiColor: .systemGroupedBackground))
        .interactiveDismissDisabled(model.saving || formSubmitting)
        #else
        VStack(alignment: .leading, spacing: 20) {
            Text(kind.rawValue).font(.title2.bold())
            Text(model.snapshot?.name ?? "").foregroundStyle(.secondary)
            if let error = model.actionError { FailureNote(message: error) }
            if !invitationLink.isEmpty && kind == .invitation {
                Text(
                    "Copy this link before closing. Anyone with the link can join with the selected role until it expires."
                )
                Text(invitationLink).font(.callout.monospaced()).textSelection(.enabled)
                HStack {
                    Button("Copy link") {
                        PlatformClipboard.copy(invitationLink)
                    }
                    Spacer()
                    Button("Done") { form = nil }.keyboardShortcut(.defaultAction)
                }
            } else {
                Form {
                    formFields(kind)
                }.formStyle(.grouped).fixedSize(horizontal: false, vertical: true)
                    .disabled(model.saving || formSubmitting)
                HStack {
                    Button("Cancel") { form = nil }.keyboardShortcut(.cancelAction).disabled(
                        model.saving || formSubmitting
                    )
                    Spacer()
                    if model.saving || formSubmitting { ProgressView().controlSize(.small) }
                    Button(kind == .invitation ? "Create link" : "Save") {
                        guard !formSubmitting, !model.saving else { return }
                        formSubmitting = true
                        Task {
                            defer { formSubmitting = false }
                            await submit(kind)
                        }
                    }.keyboardShortcut(.defaultAction)
                        .disabled(
                            model.saving || formSubmitting
                                || (kind == .teammate
                                    && email.trimmingCharacters(in: .whitespaces).isEmpty)
                                || (kind == .role && role.isEmpty)
                                || (kind == .access && user.isEmpty)
                        )
                }
            }
        }.padding(24).desktopFrame(width: 520).interactiveDismissDisabled(model.saving || formSubmitting)
        #endif
    }
    @ViewBuilder private func formFields(_ kind: CloudForm) -> some View {
        switch kind {
        case .invitations: EmptyView()
        case .teammate:
            TextField("Email", text: $email)
            Text(
                "Adds someone who already has a Wendy account. For a new teammate, create an invitation link."
            ).font(.caption).foregroundStyle(.secondary)
            rolePicker
        case .role:
            rolePicker
                #if os(iOS)
                .pickerStyle(.inline)
                #endif
            if let error = model.snapshot?.rolesError, !error.isEmpty { FailureNote(message: error) }
        case .invitation:
            Picker("Role", selection: $role) {
                Text("Organization default").tag("")
                Text("Viewer").tag("viewer")
                Text("Member").tag("member")
                Text("Admin").tag("admin")
            }
            DatePicker(
                "Expires",
                selection: $expires,
                in: Date()...,
                displayedComponents: [.date, .hourAndMinute]
            )
        case .access:
            Picker("Teammate", selection: $user) {
                ForEach(model.snapshot?.members ?? []) { Text($0.title).tag($0.id) }
            }
            Picker("Devices", selection: $device) {
                Text("All devices").tag("")
                ForEach(model.snapshot?.devices ?? []) { Text($0.name).tag($0.id) }
            }
            Picker("Access", selection: $deny) {
                Text("Allow").tag(false)
                Text("Deny").tag(true)
            }.pickerStyle(.segmented)
            Text(
                deny
                    ? "Deny blocks access even if another rule allows it."
                    : "An allow still requires role permissions and cannot override a deny."
            ).font(.caption).foregroundStyle(.secondary)
            if let e = model.snapshot?.devicesError, !e.isEmpty {
                FailureNote(message: e)
            }
        }
    }
    private func formInvalid(_ kind: CloudForm) -> Bool {
        model.saving || formSubmitting
            || (kind == .teammate && email.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
            || (kind == .role && role.isEmpty)
            || (kind == .access && user.isEmpty)
    }
    private func submitForm(_ kind: CloudForm) {
        guard !formInvalid(kind) else { return }
        formSubmitting = true
        Task {
            defer { formSubmitting = false }
            await submit(kind)
        }
    }
    private var rolePicker: some View {
        Picker("Role", selection: $role) {
            Text("Organization default").tag("")
            ForEach(model.snapshot?.roles ?? []) { Text($0.name).tag($0.id) }
        }
    }
    private func submit(_ kind: CloudForm) async {
        let account = model.account
        let org = model.organization
        _ = await model.save(
            {
                switch kind {
                case .invitations: return
                case .teammate:
                    try await model.client.addMember(
                        account: account,
                        org: org,
                        email: email.trimmingCharacters(in: .whitespacesAndNewlines),
                        role: role
                    )
                case .role:
                    try await model.client.setRole(
                        account: account,
                        org: org,
                        user: user,
                        role: role
                    )
                case .access:
                    try await model.client.addAccess(
                        account: account,
                        org: org,
                        user: user,
                        device: device,
                        effect: deny ? WD_ACCESS_DENY : WD_ACCESS_ALLOW
                    )
                case .invitation:
                    invitationLink = try await model.client.invite(
                        account: account,
                        org: org,
                        role: role,
                        expires: expires
                    )
                }
            },
            onSuccess: {
                if kind != .invitation { form = nil }
            }
        )
    }
    private func remove(_ action: CloudRemoval) async {
        let account = model.account
        let org = model.organization
        _ = await model.save {
            switch action {
            case .member(let x):
                try await model.client.removeMember(account: account, org: org, user: x.id)
            case .access(let x):
                try await model.client.removeAccess(account: account, org: org, id: x.id)
            case .invite(let x):
                try await model.client.revokeInvite(account: account, org: org, id: x.id)
            }
        }
    }
}
