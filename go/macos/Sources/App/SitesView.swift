import SwiftUI

struct FleetDevice: Identifiable {
    let id, name, selector, siteID, site, hardware, address: String
    let online: Bool
    let lastSeen: Date?
    var siteKey: String { siteID.isEmpty ? site : siteID }
    var device: Device { Device(id: selector, name: name, selector: selector, transport: .cloud, address: address) }
}
struct FleetSnapshot { let devices: [FleetDevice]; let presenceError: String }
extension GoClient {
    func fleet(account: String, org: String) async throws -> FleetSnapshot {
        try await perform {
            let strings = CStringStorage(); defer { withExtendedLifetime(strings) {} }
            guard let p = WDFleet(strings.add(account), strings.add(org)) else { throw WendyError(message: "Missing fleet result.") }
            defer { WDFleetFree(p) }; try Self.check(p.pointee.status)
            let devices: [FleetDevice] = UnsafeBufferPointer(start: p.pointee.items, count: p.pointee.count).map { d in
                FleetDevice(id: Self.string(d.id), name: Self.string(d.name), selector: Self.string(d.selector), siteID: Self.string(d.site_id), site: Self.string(d.site),
                            hardware: Self.string(d.hardware), address: Self.string(d.address), online: d.online != 0,
                            lastSeen: d.last_seen > 0 ? Date(timeIntervalSince1970: Double(d.last_seen)) : nil)
            }
            return FleetSnapshot(devices: devices, presenceError: Self.string(p.pointee.presence_error))
        }
    }
}

@MainActor private final class SitesModel: ObservableObject {
    @Published var accounts: [SavedAccount] = []
    @Published var account = ""
    @Published var organizations: [CloudOrganization] = []
    @Published var organization = ""
    @Published var snapshot: FleetSnapshot?
    @Published var loading = false
    @Published var error: String?
    @Published var updated: Date?
    private var generation = 0
    func load() async {
        do {
            accounts = try await GoClient.shared.accounts()
            if !accounts.contains(where: { $0.id == account }) { account = accounts.first(where: { $0.active })?.id ?? accounts.first?.id ?? "" }
            await loadOrganizations()
        } catch { self.error = error.localizedDescription }
    }
    func loadOrganizations() async {
        generation += 1; let request = generation
        organizations = []; organization = ""; snapshot = nil; updated = nil; error = nil
        guard !account.isEmpty else { return }
        loading = true
        do {
            let items = try await CloudManagementClient().organizations(account: account)
            guard generation == request else { return }
            organizations = items; organization = items.first?.id ?? ""
            await refresh()
        } catch { if generation == request { self.error = error.localizedDescription; loading = false } }
    }
    func refresh() async {
        generation += 1; let request = generation
        guard !account.isEmpty, !organization.isEmpty else { loading = false; return }
        loading = true; error = nil
        do {
            let value = try await GoClient.shared.fleet(account: account, org: organization)
            guard request == generation else { return }
            snapshot = value; updated = Date()
        } catch { if request == generation { self.error = error.localizedDescription } }
        if request == generation { loading = false }
    }
    func changeOrganization() async { snapshot = nil; updated = nil; await refresh() }
}

struct SitesView: View {
    @EnvironmentObject private var app: AppModel
    @StateObject private var model = SitesModel()
    @State private var search = ""
    #if os(iOS)
    @AppStorage("sites.onlineOnly") private var onlineOnly = true
    #else
    @State private var onlineOnly = false
    #endif
    let connect: (Device) -> Void
    private var devices: [FleetDevice] {
        (model.snapshot?.devices ?? []).filter {
            (!onlineOnly || !presenceKnown || $0.online) && (search.isEmpty || [$0.name, $0.site, $0.hardware].contains { $0.localizedCaseInsensitiveContains(search) })
        }.sorted { a, b in a.name == b.name ? a.id < b.id : a.name.localizedStandardCompare(b.name) == .orderedAscending }
    }
    private var groups: [(key: String, name: String, devices: [FleetDevice])] {
        Dictionary(grouping: devices, by: \.siteKey).map { key, values in
            (key: key, name: values.first?.site.isEmpty == false ? values[0].site : "No site reported", devices: values)
        }.sorted { $0.name.localizedStandardCompare($1.name) == .orderedAscending }
    }
    private var presenceKnown: Bool { model.snapshot?.presenceError.isEmpty == true }
    var body: some View {
        Group {
            #if os(iOS)
            mobileContent
            #else
            desktopContent
            #endif
        }
        #if os(iOS)
        .task(id: app.mobileCloudSelectionKey) {
            model.accounts = app.mobileCloudAccounts
            if model.account != app.mobileCloudAccount || model.organization != app.mobileCloudOrganization {
                model.account = app.mobileCloudAccount
                model.organization = app.mobileCloudOrganization
                await model.changeOrganization()
            } else if model.snapshot == nil { await model.refresh() }
        }
        #else
        .task { await model.load() }
        #endif
        #if os(macOS)
        .task(id: app.account?.signedIn) { if app.account?.signedIn == true { await model.load() } }
        #endif
        .focusedSceneValue(\.fleetRefresh, PaneRefreshAction(enabled: !model.loading, perform: { Task { await model.load() } }))
    }
    #if os(iOS)
    private var mobileContent: some View {
        List {
            if let snapshot = model.snapshot {
                Section {
                    HStack {
                        metric("Devices", count: snapshot.devices.count, color: .primary).frame(maxWidth: .infinity, alignment: .leading)
                        metric("Sites", count: Set(snapshot.devices.filter { !$0.siteKey.isEmpty }.map(\.siteKey)).count, color: .blue).frame(maxWidth: .infinity, alignment: .leading)
                        if presenceKnown { metric("Online", count: snapshot.devices.filter(\.online).count, color: .green).frame(maxWidth: .infinity, alignment: .leading) }
                    }.padding(.vertical, 4)
                    Toggle("Online only", isOn: $onlineOnly).disabled(!presenceKnown)
                }
            }
            if let error = model.error { FailureNote(message: error) }
            if let snapshot = model.snapshot, !snapshot.presenceError.isEmpty {
                FailureNote(message: "Device presence is unavailable. " + snapshot.presenceError)
            }
            if model.accounts.isEmpty && !model.loading {
                ContentUnavailableView { Label("Sign in to view your fleet", systemImage: "building.2") }
                    actions: { Button("Sign in") { Task { await app.signIn() } } }
            } else if devices.isEmpty && !model.loading && model.error == nil {
                ContentUnavailableView.search(text: search)
            }
            ForEach(groups, id: \.key) { group in
                Section {
                    ForEach(group.devices) { device in
                        Button { connect(device.device) } label: {
                            HStack(spacing: 12) {
                                Image(systemName: "desktopcomputer").foregroundStyle(.secondary).frame(width: 24)
                                VStack(alignment: .leading, spacing: 5) {
                                    Text(device.name).font(.subheadline.weight(.semibold)).foregroundStyle(.primary)
                                    Text(device.hardware.isEmpty ? "Wendy device" : device.hardware).font(.caption).foregroundStyle(.secondary)
                                    Text(presenceKnown ? (device.online ? "Online" : "Offline") : "Status unknown")
                                        .font(.caption).foregroundStyle(presenceKnown && device.online ? Color.green : .secondary)
                                }.frame(maxWidth: .infinity, alignment: .leading)
                                if app.connectingDevice?.id == device.selector { ProgressView() }
                                else { Image(systemName: "chevron.right").font(.caption.weight(.semibold)).foregroundStyle(.tertiary) }
                            }.padding(.vertical, 5).contentShape(Rectangle())
                        }.buttonStyle(.plain)
                    }
                } header: { Text("\(group.name) · \(group.devices.count)") }
            }
            if let updated = model.updated {
                Text("Updated \(updated.formatted(date: .omitted, time: .shortened))").font(.caption).foregroundStyle(.secondary)
                    .listRowBackground(Color.clear)
            }
        }
        .searchable(text: $search, prompt: "Filter fleet")
        .refreshable { await model.refresh() }
        .overlay { if model.loading && model.snapshot == nil { ProgressView() } }
    }
    #endif
    private var desktopContent: some View {
        VStack(alignment: .leading, spacing: 22) {
            HStack {
                VStack(alignment: .leading, spacing: 6) {
                    Text("Sites").font(.largeTitle.bold())
                    Text("Your fleet, across every site in your organization.").foregroundStyle(.secondary)
                }
                Spacer()
                if model.loading { ProgressView().controlSize(.small) }
            }
            AdaptiveStack(spacing: 12) {
                Picker("Account", selection: Binding(get: { model.account }, set: { model.account = $0; Task { await model.loadOrganizations() } })) {
                    ForEach(model.accounts) { Text($0.account.email.isEmpty ? $0.id : $0.account.email).tag($0.id) }
                }
                Picker("Organization", selection: Binding(get: { model.organization }, set: { model.organization = $0; Task { await model.changeOrganization() } })) {
                    ForEach(model.organizations) { Text($0.name).tag($0.id) }
                }
            }.disabled(model.loading)
            if let error = model.error { FailureNote(message: error) }
            if let snapshot = model.snapshot, !snapshot.presenceError.isEmpty { FailureNote(message: "Device presence is unavailable. " + snapshot.presenceError) }
            if let snapshot = model.snapshot {
                AdaptiveStack(spacing: 12) {
                    metric("Devices", count: snapshot.devices.count, color: .primary)
                    metric("Sites", count: Set(snapshot.devices.filter { !$0.siteKey.isEmpty }.map(\.siteKey)).count, color: .blue)
                    if presenceKnown { metric("Online", count: snapshot.devices.filter(\.online).count, color: .green) }
                    Spacer()
                    TextField("Filter fleet", text: $search).textFieldStyle(.roundedBorder).frame(maxWidth: 220)
                    Toggle("Online only", isOn: $onlineOnly).disabled(!presenceKnown)
                }
            }
            if model.accounts.isEmpty && !model.loading {
                ContentUnavailableView { Label("Sign in to view your fleet", systemImage: "building.2") }
                    actions: { Button("Sign in") { Task { await app.signIn() } } }
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
            } else if devices.isEmpty && !model.loading && model.error == nil {
                ContentUnavailableView("No matching devices", systemImage: "desktopcomputer", description: Text("Devices enrolled in this organization appear here, including offline devices."))
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
            } else {
                ScrollView {
                    LazyVStack(alignment: .leading, spacing: 24) {
                        ForEach(groups, id: \.key) { group in
                            VStack(alignment: .leading, spacing: 12) {
                                HStack { Label(group.name, systemImage: "building.2").font(.headline); Text("\(group.devices.count)").foregroundStyle(.secondary) }
                                LazyVGrid(columns: [GridItem(.adaptive(minimum: 230, maximum: 340), spacing: 14)], spacing: 14) {
                                    ForEach(group.devices) { device in deviceTile(device) }
                                }
                            }
                        }
                    }.padding(.bottom, 12)
                }
            }
            if let updated = model.updated { Text("Updated \(updated.formatted(date: .omitted, time: .shortened))").font(.caption).foregroundStyle(.secondary) }
        }.padding(28)
    }
    private func metric(_ title: String, count: Int, color: Color) -> some View {
        VStack(alignment: .leading, spacing: 4) { Text(count.formatted()).font(.title2.bold()).foregroundStyle(color); Text(title).font(.caption).foregroundStyle(.secondary) }
    }
    private func deviceTile(_ device: FleetDevice) -> some View {
        Button { connect(device.device) } label: {
            VStack(alignment: .leading, spacing: 12) {
                HStack {
                    Image(systemName: "desktopcomputer").font(.title2).foregroundStyle(.secondary)
                    Spacer()
                    if app.connectingDevice?.id == device.selector { ProgressView().controlSize(.small) }
                    else { Circle().fill(presenceKnown && device.online ? Color.green : .gray).frame(width: 8, height: 8) }
                }
                Text(device.name).fontWeight(.semibold).lineLimit(1)
                Text(device.hardware.isEmpty ? "Wendy device" : device.hardware).font(.caption).foregroundStyle(.secondary).lineLimit(1)
                HStack {
                    Text(presenceKnown ? (device.online ? "Online" : "Offline") : "Status unknown").font(.caption)
                    Spacer()
                    if !device.online, let date = device.lastSeen { Text(date, style: .relative).font(.caption).foregroundStyle(.secondary).help("Last seen \(date.formatted())") }
                }
            }.padding(16).frame(maxWidth: .infinity, minHeight: 142, alignment: .leading)
                .background(Color.primary.opacity(0.035), in: RoundedRectangle(cornerRadius: 12))
                .overlay(RoundedRectangle(cornerRadius: 12).strokeBorder(Color.primary.opacity(0.08)))
                .contentShape(RoundedRectangle(cornerRadius: 12))
        }.buttonStyle(.plain).help("Connect to \(device.name)")
    }
}
