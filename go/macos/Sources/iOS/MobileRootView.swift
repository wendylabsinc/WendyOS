import SwiftUI

private enum MobileDestination: Hashable { case organization, sites, device(String) }
struct MobileRootView: View {
    @EnvironmentObject private var model: AppModel
    @Environment(\.scenePhase) private var scenePhase
    @State private var destination: MobileDestination?
    @State private var preferredColumn = NavigationSplitViewColumn.sidebar
    @State private var source = "nearby"
    @State private var search = ""
    @State private var address = ""
    @State private var adding = false
    @State private var accounts = false
    @State private var details: String?
    private var devices: [Device] {
        (source == "nearby" ? model.nearby : model.cloud).filter { search.isEmpty || $0.title.localizedCaseInsensitiveContains(search) }
    }
    var body: some View {
        NavigationSplitView(preferredCompactColumn: $preferredColumn) {
            List(selection: $destination) {
                if model.account?.signedIn == true {
                    Section {
                        HStack(spacing: 16) {
                            Button {
                                destination = .organization; preferredColumn = .detail
                            } label: {
                                Label("Organization", systemImage: "person.2").frame(maxWidth: .infinity)
                            }
                            Divider()
                            Button {
                                destination = .sites; preferredColumn = .detail
                            } label: {
                                Label("Sites", systemImage: "building.2").frame(maxWidth: .infinity)
                            }
                        }.font(.subheadline).lineLimit(1).buttonStyle(.borderless)
                            .padding(.vertical, 4)
                    }
                }
                Section {
                    ForEach(devices) { device in
                        NavigationLink(value: MobileDestination.device(device.id)) {
                            HStack {
                                Label(device.title, systemImage: device.transport == .cloud ? "cloud" : "network")
                                Spacer()
                                if model.connectingDevice?.id == device.id { ProgressView() }
                                else if model.connected?.id == device.id { Image(systemName: "checkmark.circle.fill").foregroundStyle(.green) }
                            }.padding(.vertical, 4)
                        }
                    }
                    if devices.isEmpty && !model.discovering {
                        if source == "cloud" && model.account?.signedIn != true {
                            Button("Sign in to Wendy Cloud") { Task { await model.signIn() } }
                        } else { Text("No devices found").foregroundStyle(.secondary) }
                    }
                    if let error = source == "nearby" ? model.nearbyError : model.cloudError { FailureNote(message: error) }
                }
            }
            .safeAreaInset(edge: .top, spacing: 0) {
                Picker("Device source", selection: $source) {
                    Text("Nearby").tag("nearby")
                    Text("Cloud").tag("cloud")
                }.pickerStyle(.segmented).padding(.horizontal, 20).padding(.vertical, 8)
                    .background(.bar)
            }
            .navigationTitle("Devices").navigationBarTitleDisplayMode(.inline)
            .contentMargins(.top, 8, for: .scrollContent)
            .listSectionSpacing(12)
            .searchable(text: $search, prompt: "Find a device")
            .refreshable { await refresh() }
            .toolbar {
                ToolbarItem(placement: .topBarLeading) { Button { accounts = true } label: { Image(systemName: "person.crop.circle") }.accessibilityLabel("Accounts") }
                ToolbarItem(placement: .topBarTrailing) { Button { adding = true } label: { Image(systemName: "plus") }.accessibilityLabel("Connect by address") }
            }
        } detail: {
            NavigationStack {
                switch destination {
                case .organization: CloudManagementView().navigationTitle("Organization").navigationBarTitleDisplayMode(.inline)
                case .sites: SitesView { select($0) }.navigationTitle("Sites").navigationBarTitleDisplayMode(.inline)
                case .device:
                    if let device = model.selection { MobileDeviceView(device: device) }
                    else { ContentUnavailableView("Choose a device", systemImage: "network") }
                case nil: ContentUnavailableView("Your devices", systemImage: "network", description: Text("Choose a nearby or cloud device."))
                }
            }
        }
        .task { await model.start() }
        .task(id: model.initialized) { await model.observeEvents() }
        .task(id: model.account) { await model.refreshMobileCloudSelection() }
        .onChange(of: model.account?.signedIn) { _, signedIn in
            if signedIn != true, destination == .organization || destination == .sites {
                destination = nil
                preferredColumn = .sidebar
            }
        }
        .task(id: source) { await refresh() }
        .task(id: destination) {
            if case .device(let id) = destination, let device = (model.nearby + model.cloud + [model.selection].compactMap { $0 }).first(where: { $0.id == id }) {
                model.selection = device
                preferredColumn = .detail
                await model.connect(device)
            }
        }
        .onChange(of: scenePhase) { _, phase in
            if phase == .background { model.hideCameraWall() }

        }
        .task(id: scenePhase) {
            if scenePhase == .active { await model.refreshAccount() }
        }
        .sheet(isPresented: $accounts) {
            MobileAccountsView(model: model) { development in
                accounts = false
                Task { await model.signIn(development: development) }
            }
        }
        .sheet(isPresented: Binding(get: { model.loginURL != nil }, set: { if !$0 { Task { await model.cancelLogin() } } })) {
            if let url = model.loginURL { BrowserSheet(url: url).ignoresSafeArea() }
        }
        .sheet(isPresented: Binding(get: { model.webpageURL != nil }, set: { if !$0 { model.webpageURL = nil } })) {
            if let url = model.webpageURL { BrowserSheet(url: url).ignoresSafeArea() }
        }
        .alert("Connect to device", isPresented: $adding) {
            TextField("hostname:50051", text: $address).textInputAutocapitalization(.never).autocorrectionDisabled()
            Button("Cancel", role: .cancel) {}
            Button("Connect") {
                let value = address.trimmingCharacters(in: .whitespacesAndNewlines)
                guard !value.isEmpty else { return }
                select(Device(id: value, name: value, selector: value, transport: .lan, address: value))
            }
        }
        .alert("Could not complete request", isPresented: Binding(get: { model.alert != nil }, set: { if !$0 { model.alert = nil } })) {
            Button("Details") { details = model.alert; model.alert = nil }
            Button("OK", role: .cancel) { model.alert = nil }
        } message: { Text(PresentedFailure(model.alert ?? "").message) }
        .sheet(isPresented: Binding(get: { details != nil }, set: { if !$0 { details = nil } })) {
            NavigationStack { ScrollView { Text(PresentedFailure(details ?? "").details).font(.caption.monospaced()).textSelection(.enabled).padding() }.navigationTitle("Details").toolbar { Button("Done") { details = nil } } }
        }
        .confirmationDialog("Delete app?", isPresented: Binding(get: { model.appDeletion != nil }, set: { if !$0 { model.appDeletion = nil } }), titleVisibility: .visible) {
            if let target = model.appDeletion { Button("Delete \(target.app)", role: .destructive) { Task { await model.deleteApp(target) }; model.appDeletion = nil } }
        } message: { Text("The app will be removed from the device.") }
    }
    private func select(_ device: Device) {
        model.selection = device
        destination = .device(device.id)
        preferredColumn = .detail
    }
    private func refresh() async {
        if source == "nearby" { await model.refreshNearby() } else { await model.refreshCloud() }
    }
}

private enum MobilePane: String, CaseIterable, Identifiable {
    case apps = "Apps", cameras = "Cameras", campaigns = "Campaigns", chat = "Chat"
    var id: String { rawValue }
}
private struct MobileDeviceView: View {
    @EnvironmentObject private var model: AppModel
    let device: Device
    @State private var pane = MobilePane.chat
    var body: some View {
        VStack(spacing: 0) {
            if !model.connectedLite { ScrollView(.horizontal) {
                HStack(spacing: 8) {
                    ForEach(MobilePane.allCases) { item in
                        Button { pane = item } label: {
                            Text(item.rawValue).font(.subheadline.weight(.medium)).padding(.horizontal, 14).padding(.vertical, 10)
                                .background(pane == item ? Color.accentColor : Color.secondary.opacity(0.1), in: Capsule())
                                .foregroundStyle(pane == item ? Color.white : Color.primary)
                        }.buttonStyle(.plain)
                    }
                }.padding(.horizontal).padding(.vertical, 8)
            }.scrollIndicators(.hidden) }
            Divider()
            if model.connectingDevice != nil { ProgressView("Connecting to \(device.title)…").frame(maxWidth: .infinity, maxHeight: .infinity) }
            else if let error = model.detailError { FailureView(title: "Could not connect", error: error) { Task { await model.connect(device) } } }
            else if model.selectedDeviceReady {
                if model.connectedLite { LiteDeviceView(model: model).id(model.deviceSession) }
                else { content.id(model.deviceSession) }
            }
        }
        .navigationTitle(device.title).navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .topBarTrailing) {
                NavigationLink {
                    DeviceSettingsView(session: model.deviceSession, deviceName: device.title)
                        .navigationTitle("Settings").navigationBarTitleDisplayMode(.inline)
                } label: { Image(systemName: "gearshape") }
                .accessibilityLabel("Device settings")
                .disabled(!model.selectedDeviceReady || model.connectedLite)
            }
        }

    }
    @ViewBuilder private var content: some View {
        switch pane {
        case .apps: ActivityView(model: model)
        case .cameras: CameraWallView(model: model)
        case .campaigns: CampaignsView(session: model.deviceSession, deviceName: device.title) { _ in }
        case .chat: DeviceChatView(deviceName: device.title, chat: model.deviceChat)
        }
    }
}

struct CameraWallView: View {
    @Environment(\.scenePhase) private var scenePhase
    @ObservedObject var model: AppModel
    var body: some View {
        ScrollView {
            LazyVStack(spacing: 16) {
                if let error = model.camerasError { FailureNote(message: error) }
                if model.cameraFeeds.isEmpty { ContentUnavailableView("No cameras found", systemImage: "video.slash") }
                ForEach(model.cameraFeeds) { feed in MobileCameraTile(feed: feed, model: model) }
            }.padding()
        }.refreshable { await model.refreshDetails() }
            .scrollBounceBehavior(.always, axes: .vertical)
            .task(id: scenePhase) {
                if scenePhase == .active { await model.showCameraWall() }
                else if scenePhase == .background { model.hideCameraWall() }
            }
            .onDisappear { model.hideCameraWall() }
    }
}
private struct MobileCameraTile: View {
    @ObservedObject var feed: CameraFeed
    @ObservedObject var model: AppModel
    @State private var showingError = false
    var body: some View {
        CameraFeedDisplay(feed: feed)
            .aspectRatio(4 / 3, contentMode: .fit).background(.black)
            .overlay(alignment: .bottom) {
                HStack(alignment: .bottom, spacing: 12) {
                    VStack(alignment: .leading, spacing: 5) {
                        Text(feed.camera.name).font(.subheadline.weight(.semibold)).lineLimit(2)
                        if let error = feed.error {
                            Button { showingError = true } label: {
                                Label(PresentedFailure(error).message, systemImage: "exclamationmark.triangle")
                                    .font(.caption).lineLimit(3).multilineTextAlignment(.leading)
                            }.buttonStyle(.plain)
                        } else if !feed.running || !feed.receivedVideo {
                            Text(feed.running ? "Connecting…" : "Paused").font(.caption).foregroundStyle(.white.opacity(0.8))
                        }
                    }.frame(maxWidth: .infinity, alignment: .leading)
                    Button {
                        if feed.running { model.pauseCamera(feed) } else { Task { await model.playCamera(feed) } }
                    } label: {
                        Image(systemName: feed.running ? "pause.fill" : "play.fill")
                            .font(.system(size: 15, weight: .semibold))
                            .frame(width: 44, height: 44)
                            .background(.black.opacity(0.5), in: Circle())
                    }.buttonStyle(.plain)
                        .accessibilityLabel("\(feed.running ? "Pause" : "Play") \(feed.camera.name)")
                }
                .foregroundStyle(.white).padding(12).padding(.top, 24)
                .background(LinearGradient(colors: [.clear, .black.opacity(0.8)], startPoint: .top, endPoint: .bottom))
            }
            .clipShape(RoundedRectangle(cornerRadius: 12))
            .sheet(isPresented: $showingError) {
                NavigationStack {
                    ScrollView { Text(feed.error ?? "No current error").font(.callout.monospaced()).textSelection(.enabled).padding() }
                        .navigationTitle("Camera error").navigationBarTitleDisplayMode(.inline)
                        .toolbar { ToolbarItem(placement: .confirmationAction) { Button("Close") { showingError = false } } }
                }.presentationBackground(Color(uiColor: .systemBackground))
            }
    }
}

private struct MobileAccountsView: View {
    @ObservedObject var model: AppModel
    let signIn: (Bool) -> Void
    private var accounts: [SavedAccount] { model.mobileCloudAccounts }

    var body: some View {
        NavigationStack {
            List {
                if accounts.isEmpty && model.account?.signedIn != true {
                    Text("Add an account to access your fleet and organization.")
                        .font(.subheadline).foregroundStyle(.secondary)
                } else {
                    Section("Accounts") {
                    ForEach(accounts) { entry in
                        Button { Task { await model.selectMobileCloudAccount(entry.id) } } label: {
                        HStack {
                            VStack(alignment: .leading, spacing: 4) {
                                Text(entry.account.email.isEmpty ? "Email unavailable" : entry.account.email)
                                Text(entry.account.cloud).font(.caption).foregroundStyle(.secondary)
                            }
                            Spacer()
                            if entry.id == model.mobileCloudAccount { Image(systemName: "checkmark.circle.fill").foregroundStyle(.green) }
                        }.padding(.vertical, 4).foregroundStyle(.primary)
                        }.buttonStyle(.plain).disabled(model.mobileCloudSelectionLoading)
                    }
                    }
                    if !model.mobileCloudOrganizations.isEmpty {
                        Section("Organization") {
                            Picker("Organization", selection: $model.mobileCloudOrganization) {
                                ForEach(model.mobileCloudOrganizations) { Text($0.name).tag($0.id) }
                            }.labelsHidden().pickerStyle(.menu).disabled(model.mobileCloudSelectionLoading)
                        }
                    }
                }
                if model.mobileCloudSelectionLoading { ProgressView() }
                if let error = model.mobileCloudSelectionError { FailureNote(message: error) }
            }
            .scrollContentBackground(.hidden)
            .background(Color(uiColor: .systemGroupedBackground))
            .navigationTitle("Accounts").navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .topBarTrailing) {
                    Button("Add account", systemImage: "plus") { signIn(false) }
                        .contextMenu {
                            Button("Wendy Cloud") { signIn(false) }
                            Button("Development Cloud") { signIn(true) }
                        }
                }
            }
            .refreshable { await refresh() }
            .task { await refresh() }
        }
        .presentationBackground(Color(uiColor: .systemGroupedBackground))
        .presentationDetents([.medium, .large])
        .presentationDragIndicator(.visible)
    }
    private func refresh() async {
        await model.refreshMobileCloudSelection()
    }
}

extension AppModel {
    func refreshMobileCloudSelection() async {
        mobileCloudSelectionGeneration += 1
        let request = mobileCloudSelectionGeneration
        mobileCloudSelectionLoading = true
        mobileCloudSelectionError = nil
        defer { if request == mobileCloudSelectionGeneration { mobileCloudSelectionLoading = false } }
        do {
            let accounts = try await client.accounts()
            guard request == mobileCloudSelectionGeneration else { return }
            let account = accounts.contains(where: { $0.id == mobileCloudAccount }) ? mobileCloudAccount
                : accounts.first(where: { $0.active })?.id ?? accounts.first?.id ?? ""
            let organizations = account.isEmpty ? [] : try await CloudManagementClient().organizations(account: account)
            guard request == mobileCloudSelectionGeneration else { return }
            mobileCloudAccounts = accounts
            mobileCloudAccount = account
            mobileCloudOrganizations = organizations
            if !organizations.contains(where: { $0.id == mobileCloudOrganization }) {
                mobileCloudOrganization = organizations.first?.id ?? ""
            }
        } catch { if request == mobileCloudSelectionGeneration { mobileCloudSelectionError = error.localizedDescription } }
    }
    func selectMobileCloudAccount(_ id: String) async {
        guard id != mobileCloudAccount else { return }
        mobileCloudAccount = id
        mobileCloudOrganization = ""
        mobileCloudOrganizations = []
        await refreshMobileCloudSelection()
    }
}
