import SwiftUI

enum MobileHome: String, CaseIterable { case home = "Home", devices = "Devices", data = "Data", activity = "Activity" }
enum MobileDestination: Hashable { case inventory(MobileInventorySource), device(Device) }

enum MobileInventorySource: CaseIterable, Hashable {
    case local, cloud

    var title: String { self == .local ? "Local devices" : "Cloud devices" }
    var tileLabel: String { self == .local ? "Local · nearby" : "Cloud" }
    var symbol: String { self == .local ? "antenna.radiowaves.left.and.right" : "globe" }
}

struct MobileRootView: View {
    @StateObject private var appearances = DeviceAppearanceStore()
    @State private var actions = AsyncActions()
    @EnvironmentObject private var model: AppModel
    @Environment(\.scenePhase) private var scenePhase
    @State private var destination: MobileDestination?
    @AppStorage("wendy.primaryLens") private var primaryLens = ""
    @State private var lens: MobileLens?
    @State private var homeTab = MobileHome.home
    @State private var recordingPicker: MobileEpisodeRoute?
    @State private var showingChat = false
    @State private var activitySegment = MobileActivitySegment.alerts
    @State private var showMesh = false
    @State private var homePath: [MobileDestination] = []
    @State private var preferredColumn = NavigationSplitViewColumn.sidebar
    @State private var search = ""
    @State private var address = ""
    @State private var adding = false
    @State private var showingNearbySetup = false
    @State private var setupSignInAfterDismiss = false
    @State private var recognizing = false
    @State private var accounts = false
    @State private var pendingAccountSignIn: Bool?
    @State private var details: String?
    private var selectedRouteDevice: Device? {
        guard lens == .devices, model.mobileScopeIsCurrent else { return nil }
        let route: MobileDestination? = switch homeTab {
        case .home: homePath.last
        case .devices: destination
        case .data, .activity: nil
        }
        if case .device(let device) = route { return device }
        return nil
    }
    var body: some View {
        presentedTabs
        .background { MobileChatLifetime(chat: model.deviceChat, isPresented: showingChat, active: model.mobileScopeIsCurrent && model.selectedDeviceReady && scenePhase != .background).id(model.deviceSession) }
        .task { await model.start() }
        .task(id: model.account) { await model.refreshMobileCloudSelection() }
        .task(
            id: "\(model.initialized)/\(scenePhase == .background)/\(model.mobileCloudSelectionKey)"
        ) {
            await model.runMobileMesh(active: scenePhase != .background)
        }
        .task { await actions.run() }
        .task(id: "\(model.initialized)/\(scenePhase == .active)") {
            if model.initialized && scenePhase == .active { await model.maintainExperience() }
        }
        .onChange(of: model.requestedDevice) { _, device in
            if let device { select(device); model.requestedDevice = nil }
        }
        .task(id: selectedRouteDevice) {
            if let device = selectedRouteDevice { await model.connect(device) }
        }
        .onChange(of: model.mobileCloudSelectionKey) { _, _ in
            // A source list can reload in the new organization; a device or log route cannot.
            if let first = homePath.first, case .inventory = first { homePath = [first] }
            else { homePath = [] }
            destination = nil
            recordingPicker = nil
            preferredColumn = .sidebar
            search = ""
        }
        .modifier(NearbySetupPrompt())
        .environmentObject(appearances)
    }

    private var tabs: some View {
        VStack(spacing: 0) {
            tabContent
            MobileLensBar(tab: $homeTab, chatModel: model.deviceChat, context: model.selectedDeviceReady ? model.connected?.title : nil, chat: { showingChat = true })
        }
        .background(MobileLensStyle.background)
        .tint(MobileLensStyle.accent)
    }

    // Reserve the bars’ actual height outside the tab/navigation containers.
    // An inset on TabView can be lost by its nested NavigationSplitView/Form,
    // leaving their final rows behind the floating controls.
    private var tabContent: some View {
        TabView(selection: $homeTab) {
            NavigationStack(path: $homePath) {
                MobileDevicesHome(model: model, launcher: { lens = nil }, notifications: { openActivity() },
                                  setup: { showingNearbySetup = true })
                    .navigationDestination(for: MobileDestination.self) { route in
                        switch route {
                        case .inventory(let inventory):
                            MobileInventoryList(model: model, source: inventory, showAccounts: { accounts = true })
                        case .device(let device):
                            MobileDeviceView(device: device, openRecordings: { openRecordings(for: device) }).id(device.id)
                        }
                    }
            }.toolbarVisibility(.hidden, for: .tabBar).tag(MobileHome.home)
            deviceBrowser.toolbarVisibility(.hidden, for: .tabBar).tag(MobileHome.devices)
            NavigationStack {
                MobileLensData(model: model, isVisible: homeTab == .data, picker: $recordingPicker)
                    .id(model.mobileCloudSelectionKey)
                    .toolbar { launcherButton }
            }.toolbarVisibility(.hidden, for: .tabBar).tag(MobileHome.data)
            NavigationStack {
                MobileDeviceActivity(model: model, isVisible: homeTab == .activity, segment: $activitySegment, select: select)
                    .id(model.mobileCloudSelectionKey)
                    .background(MobileLensStyle.background)
                    .toolbar { launcherButton }
            }.toolbarVisibility(.hidden, for: .tabBar).tag(MobileHome.activity)
        }
        .toolbarVisibility(.hidden, for: .tabBar)
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }

    private var launcherButton: some ToolbarContent {
        ToolbarItem(placement: .topBarLeading) {
            Button("General Home", systemImage: "chevron.left") { lens = nil }
                .accessibilityIdentifier("lens.back")
        }
    }

    @ViewBuilder private var surface: some View {
        if MobileLens(rawValue: primaryLens) == nil {
            MobileLensPreference(initial: .devices) { primaryLens = $0.rawValue }
        } else if let lens {
            switch lens {
            case .devices: tabs
            case .sites:
                NavigationStack { MobileLensUnderConstruction(title: "Sites").toolbar { launcherButton } }
            case .robots:
                NavigationStack { MobileLensUnderConstruction(title: "Robots").toolbar { launcherButton } }
            }
        } else {
            NavigationStack {
                MobileGeneralHome(model: model, primary: MobileLens(rawValue: primaryLens) ?? .devices,
                                  open: enter, accounts: { accounts = true }, notifications: { openActivity() },
                                  setup: { showingNearbySetup = true }, recordings: { enter(.devices); homeTab = .data }, openDevice: select)
                    .safeAreaInset(edge: .bottom, alignment: .trailing) {
                        Button("Ask Wendy", systemImage: "sparkles") { showingChat = true }
                            .labelStyle(.iconOnly).font(.title3).foregroundStyle(.white).frame(width: 52, height: 52)
                            .background(MobileLensStyle.green, in: .circle).padding(.trailing, 16)
                    }
            }
        }
    }

    private func openActivity() {
        enter(.devices)
        activitySegment = .alerts
        homeTab = .activity
    }

    private func openRecordings(for device: Device) {
        recordingPicker = MobileEpisodeRoute(device: device)
        homeTab = .data
    }

    private func enter(_ next: MobileLens) {
        lens = next
    }

    @ViewBuilder private var deviceBrowser: some View {
        NavigationSplitView(preferredCompactColumn: $preferredColumn) {
            List(selection: $destination) {
                if showMesh {
                    MobileMeshSection(model: model)
                } else {
                    Section {
                        Button("Set up a new device", systemImage: "plus.circle.fill") { showingNearbySetup = true }
                    }
                    ForEach(MobileInventorySource.allCases, id: \.self) { inventory in
                        MobileInventorySection(model: model, source: inventory, search: search, showAccounts: { accounts = true })
                    }
                }
            }
            .scrollContentBackground(.hidden).background(MobileLensStyle.background)
            .safeAreaInset(edge: .top, spacing: 0) {
                Picker("Device view", selection: $showMesh) {
                    Text("List").tag(false)
                    Text("Mesh").tag(true)
                }.pickerStyle(.segmented).padding(.horizontal, 16).padding(.vertical, 8)
                    .background(MobileLensStyle.background)
            }
            .navigationTitle("Devices").navigationBarTitleDisplayMode(.inline)
            .contentMargins(.top, 8, for: .scrollContent)
            .listSectionSpacing(12)
            .searchable(text: $search, placement: .navigationBarDrawer(displayMode: .always), prompt: "Find a device")
            .refreshable {
                async let local: Void = MobileInventorySource.local.refresh(model)
                async let cloud: Void = MobileInventorySource.cloud.refresh(model)
                _ = await (local, cloud)
            }
            .toolbar {
                launcherButton
                ToolbarItem(placement: .topBarTrailing) { Button { recognizing = true } label: { Image(systemName: "viewfinder") }.accessibilityLabel("Recognize a device") }
                ToolbarItem(placement: .topBarTrailing) {
                    Menu {
                        Button("Set up a device", systemImage: "cpu") { showingNearbySetup = true }
                        Button("Connect by address", systemImage: "link") { adding = true }
                    } label: { Image(systemName: "plus") }.accessibilityLabel("Add device")
                }
            }
        } detail: {
            NavigationStack {
                switch destination {
                case .device(let device): MobileDeviceView(device: device, openRecordings: { openRecordings(for: device) }).id(device.id)
                default: ContentUnavailableView("Your devices", systemImage: "network", description: Text("Choose a nearby or cloud device."))
                }
            }.id(destination)
        }
        .task(id: model.mobileCloudSelectionKey) {
            async let local: Void = MobileInventorySource.local.refresh(model)
            async let cloud: Void = MobileInventorySource.cloud.refresh(model)
            _ = await (local, cloud)
        }
        .onChange(of: showMesh) { _, _ in destination = nil; preferredColumn = .sidebar }

        .onChange(of: destination) { _, route in
            if case .device = route { preferredColumn = .detail }
        }
    }

    private var presentedTabs: some View {
        MobileRecordingScope { surface }.id(model.mobileCloudSelectionKey)
        .sheet(isPresented: $showingChat) {
            MobileLensChat(model: model, chooseDevice: { showingChat = false; enter(.devices); homeTab = .devices })
                .presentationDetents([.medium, .large])
                .presentationDragIndicator(.visible)
                .presentationBackgroundInteraction(.enabled(upThrough: .medium))
        }
        .onChange(of: scenePhase) { _, phase in
            if phase == .background { model.hideCameraWall() }
        }
        .task(id: scenePhase) {
            if scenePhase == .active { await model.refreshAccount() }
        }
        .fullScreenCover(isPresented: $recognizing) { DeviceViewfinder() }
        .sheet(isPresented: $showingNearbySetup, onDismiss: {
            actions.submit {
                if setupSignInAfterDismiss { setupSignInAfterDismiss = false; await model.signIn() }
                await model.refreshNearby()
            }
        }) { NearbySetupView { setupSignInAfterDismiss = true } }
        .sheet(isPresented: $accounts, onDismiss: {
            if let development = pendingAccountSignIn {
                pendingAccountSignIn = nil
                actions.submit { await model.signIn(development: development) }
            }
        }) {
            MobileAccountsView(model: model) { development in
                pendingAccountSignIn = development
                accounts = false
            }
        }
        .sheet(isPresented: Binding(get: { model.loginURL != nil }, set: { if !$0 { actions.submit { await model.cancelLogin() } } })) {
            if let url = model.loginURL {
                BrowserSheet(url: url).ignoresSafeArea()
                    #if targetEnvironment(simulator) && DEBUG
                    .safeAreaInset(edge: .bottom) { SimulatorSignInLink(url: url) }
                    #endif
            }
        }
        .sheet(item: $model.webpage) { target in MobileAppBrowser(target: target) }
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
            if let target = model.appDeletion { Button("Delete \(target.app)", role: .destructive) { actions.submit { await model.deleteApp(target) }; model.appDeletion = nil } }
        } message: { Text("The app will be removed from the device.") }
    }
    private func select(_ device: Device) {
        lens = .devices
        homeTab = .devices
        showMesh = false
        destination = .device(device)
        preferredColumn = .detail
    }
}

enum MobileLensStyle {
    static let background = Color(uiColor: UIColor { $0.userInterfaceStyle == .dark ? .systemBackground : UIColor(red: 241/255, green: 238/255, blue: 231/255, alpha: 1) })
    static let card = Color(uiColor: UIColor { $0.userInterfaceStyle == .dark ? .secondarySystemBackground : UIColor(red: 230/255, green: 226/255, blue: 216/255, alpha: 1) })
    static let green = Color(red: 42/255, green: 112/255, blue: 80/255)
    static let accent = Color(uiColor: UIColor { $0.userInterfaceStyle == .dark ? UIColor(red: 159/255, green: 226/255, blue: 191/255, alpha: 1) : UIColor(red: 42/255, green: 112/255, blue: 80/255, alpha: 1) })
}

@MainActor private extension MobileInventorySource {
    func devices(_ model: AppModel) -> [Device] {
        guard model.mobileScopeIsCurrent else { return [] }
        return self == .local ? model.nearby : model.cloud
    }
    func error(_ model: AppModel) -> String? {
        guard model.mobileScopeIsCurrent else { return nil }
        return self == .local ? model.nearbyError : model.cloudError
    }
    func count(_ model: AppModel) -> Int? {
        guard model.mobileScopeIsCurrent else { return nil }
        if self == .cloud && (model.mobileCloudOrganization.isEmpty || model.mobileCloudSelectionError != nil) { return nil }
        guard (self == .local ? model.nearbyLoaded : model.cloudLoaded), error(model) == nil else { return nil }
        return devices(model).count
    }
    func refresh(_ model: AppModel) async {
        if self == .local { await model.refreshNearby() } else { await model.refreshCloud() }
    }
}

private struct MobileDevicesHome: View {
    @EnvironmentObject private var catalog: RecordingCatalog
    @ObservedObject var model: AppModel
    let launcher: () -> Void
    let notifications: () -> Void
    let setup: () -> Void
    @Environment(\.dynamicTypeSize) private var dynamicType
    @ScaledMetric(relativeTo: .largeTitle) private var countSize = 44.0
    private var organization: String? { model.mobileCloudOrganizations.first { $0.id == model.mobileCloudOrganization }?.name }

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 18) {
                VStack(alignment: .leading, spacing: 14) {
                    let total = (organization == nil ? MobileInventorySource.local : .cloud).count(model)
                    HStack(alignment: .bottom) {
                        VStack(alignment: .leading, spacing: 2) {
                            Text(total?.formatted() ?? "—").font(.system(size: countSize, weight: .bold))
                        .monospacedDigit().accessibilityLabel(total.map { String($0) } ?? "Count unavailable")
                    Text(organization.map { "devices in \($0)" } ?? "devices nearby")
                                .font(.subheadline).foregroundStyle(.secondary)
                        }
                        Spacer()
                        MobilePresenceLegend(model: model)
                    }
                    MobilePresenceBar(model: model)
                    let layout = dynamicType.isAccessibilitySize ? AnyLayout(VStackLayout(spacing: 8)) : AnyLayout(HStackLayout(spacing: 8))
                    layout {
                        ForEach(MobileInventorySource.allCases, id: \.self) { source in
                            NavigationLink(value: MobileDestination.inventory(source)) {
                                HStack(spacing: 9) {
                                    Image(systemName: source.symbol).foregroundStyle(.white).frame(width: 34, height: 34)
                                        .background(MobileLensStyle.green, in: RoundedRectangle(cornerRadius: 10))
                                    VStack(alignment: .leading, spacing: 1) {
                                        Text(source.count(model)?.formatted() ?? "—").font(.title3.bold()).monospacedDigit()
                                        Text(source.tileLabel).font(.caption).foregroundStyle(.secondary)
                                    }
                                    Spacer(minLength: 0)
                                    Image(systemName: "chevron.right").font(.caption2.weight(.semibold)).foregroundStyle(.tertiary)
                                }.padding(12).frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .leading)
                                    .background(MobileLensStyle.background, in: RoundedRectangle(cornerRadius: 16))
                            }.buttonStyle(.plain)
                                .accessibilityLabel(source.title)
                                .accessibilityValue(source.count(model).map { "\($0) devices" } ?? "Count unavailable")
                                .accessibilityIdentifier(source == .local ? "devices.home.local" : "devices.home.cloud")
                        }
                    }
                }.padding(18).frame(maxWidth: .infinity, alignment: .leading)
                    .background(MobileLensStyle.card, in: RoundedRectangle(cornerRadius: 26))
                Button(action: setup) {
                    HStack(spacing: 12) {
                        Image(systemName: "plus").font(.headline).foregroundStyle(.white)
                            .frame(width: 36, height: 36).background(MobileLensStyle.green, in: Circle())
                        VStack(alignment: .leading, spacing: 2) {
                            Text("Set up a new device").font(.headline).foregroundStyle(MobileLensStyle.accent)
                            Text("Nearby over Bluetooth").font(.caption).foregroundStyle(.secondary)
                        }
                    }.padding(14).frame(maxWidth: .infinity, alignment: .leading)
                        .background(MobileLensStyle.accent.opacity(0.05), in: RoundedRectangle(cornerRadius: 22))
                        .overlay { RoundedRectangle(cornerRadius: 22).strokeBorder(MobileLensStyle.accent.opacity(0.45), style: StrokeStyle(lineWidth: 1.5, dash: [5, 4])) }
                }.buttonStyle(.plain).accessibilityIdentifier("devices.home.setup")
                MobileHomeAttention(model: model, openDevices: notifications)
                MobileRecordingSection(model: model, recent: true)
            }.padding(.horizontal, 16).padding(.vertical, 8).frame(maxWidth: 700)
                .frame(maxWidth: .infinity)
        }
        .background(MobileLensStyle.background)
        .navigationTitle("Devices")
        .toolbar {
            ToolbarItem(placement: .topBarLeading) {
                Button("General Home", systemImage: "chevron.left", action: launcher)
                    .accessibilityIdentifier("lens.back")
            }
            ToolbarItem(placement: .topBarTrailing) { Button("Notifications", systemImage: "bell", action: notifications).badge(catalog.unreadAlerts) }
        }
        .refreshable {
            await model.refreshNearby()
            if !model.mobileCloudOrganization.isEmpty { await model.refreshCloud() }
        }
    }
}

private struct MobileInventoryList: View {
    @ObservedObject var model: AppModel
    let source: MobileInventorySource
    let showAccounts: () -> Void
    @State private var search = ""
    var body: some View {
        List {
            MobileInventorySection(model: model, source: source, search: search, showAccounts: showAccounts)
        }
        .accessibilityIdentifier("devices.inventory")
        .scrollContentBackground(.hidden).background(MobileLensStyle.background)
        .navigationTitle(source.title)
        .searchable(text: $search, placement: .navigationBarDrawer(displayMode: .always), prompt: "Find a device")
        .refreshable { await source.refresh(model) }
        .task { await source.refresh(model) }
    }
}

private struct MobileInventorySection: View {
    @ObservedObject var model: AppModel
    let source: MobileInventorySource
    let search: String
    let showAccounts: () -> Void
    @State private var actions = AsyncActions()
    @Environment(\.openURL) private var openURL
    private var matching: [Device] {
        source.devices(model).filter { search.isEmpty || $0.title.localizedCaseInsensitiveContains(search) }
            .sorted { left, right in
                let leftOffline = isOffline(left), rightOffline = isOffline(right)
                if leftOffline != rightOffline { return !leftOffline }
                let order = left.title.localizedStandardCompare(right.title)
                return order == .orderedSame ? left.id < right.id : order == .orderedAscending
            }
    }
    private func isOffline(_ device: Device) -> Bool {
        guard device.transport == .cloud, model.connected?.id != device.id,
              model.mobileScopeIsCurrent, model.cloudError == nil, model.cloudLoaded,
              let snapshot = model.mobilePresence, snapshot.0.presenceError.isEmpty,
              Date().timeIntervalSince(snapshot.1) < 60 else { return false }
        return snapshot.0.devices.first { $0.selector == device.selector }?.online == false
    }
    private var error: String? { source.error(model) ?? (source == .cloud ? model.mobileCloudSelectionError : nil) }
    private var loading: Bool {
        !model.initialized || (source == .local ? model.nearbyLoading : model.cloudLoading || model.mobileCloudSelectionLoading)
    }
    var body: some View {
        Section {
            if source == .cloud && model.mobileCloudOrganization.isEmpty && !loading && error == nil {
                if model.mobileCloudAccounts.isEmpty && model.account?.signedIn != true {
                    Button("Sign in to Wendy Cloud") { actions.submit { await model.signIn() } }
                        .disabled(model.signingIn)
                } else { Button("Choose an organization", action: showAccounts) }
            } else {
                ForEach(matching) { device in
                    NavigationLink(value: MobileDestination.device(device)) {
                        DeviceIdentityRow(device: device, connected: model.connected?.id == device.id,
                                          connecting: model.connectingDevice?.id == device.id,
                                          offline: isOffline(device))
                    }.listRowBackground(isOffline(device) ? Color(uiColor: .tertiarySystemFill) : Color(uiColor: .secondarySystemGroupedBackground))
                        .accessibilityIdentifier("devices.inventory.row." + device.id)
                }
                if let error {
                    FailureNote(message: error)
                    retry
                    if source == .cloud { Button("Accounts", action: showAccounts) }
                    else if PresentedFailure(error).title == "Allow local network access" { openSettings }
                } else if matching.isEmpty {
                    if loading || source.count(model) == nil { MobileLoadingPlaceholder(label: "Loading devices…") }
                    else if !search.isEmpty { ContentUnavailableView.search(text: search) }
                    else {
                        ContentUnavailableView(source == .local ? "No nearby devices" : "No cloud devices",
                            systemImage: source.symbol, description: Text(source == .local
                                ? "Connect to the device’s network. Check that Local Network is enabled for Wendy in Settings."
                                : "Devices enrolled in this organization will appear here."))
                        if source == .local { retry; openSettings }
                    }
                }
            }
        } header: { Text(source == .local ? "Local · nearby now" : "Wendy Cloud") }
        .task { await actions.run() }
    }

    private var retry: some View {
        Button("Try again") { actions.submit {
            if source == .cloud && model.mobileCloudSelectionError != nil { await model.refreshMobileCloudSelection() }
            await source.refresh(model)
        } }.accessibilityIdentifier("devices.inventory.retry")
    }

    private var openSettings: some View {
        Button("Open Settings") {
            if let url = URL(string: UIApplication.openSettingsURLString) { openURL(url) }
        }.accessibilityIdentifier("devices.inventory.settings")
    }
}

private struct MobileAccountsView: View {
    @AppStorage("wendy.primaryLens") private var primaryLens = MobileLens.devices.rawValue
    @State private var actions = AsyncActions()
    @ObservedObject var model: AppModel
    let signIn: (Bool) -> Void
    private var accounts: [SavedAccount] { model.mobileCloudAccounts }

    var body: some View {
        actionContent.task { await actions.run() }
    }

    @ViewBuilder private var actionContent: some View {
        NavigationStack {
            List {
                if !model.mobileCloudOrganizations.isEmpty {
                    Section {
                        VStack(spacing: 0) {
                            ForEach(model.mobileCloudOrganizations) { organization in
                                Button { model.mobileCloudOrganization = organization.id } label: {
                                    HStack(spacing: 16) {
                                        Text(String(organization.name.prefix(1)).uppercased())
                                            .font(.headline).foregroundStyle(.white)
                                            .frame(width: 48, height: 48)
                                            .background(RecordingAppearance.ink, in: .circle)
                                        Text(organization.name).font(.headline)
                                            .foregroundStyle(.primary)
                                            .frame(maxWidth: .infinity, alignment: .leading)
                                        if organization.id == model.mobileCloudOrganization {
                                            Image(systemName: "checkmark")
                                                .font(.body.weight(.semibold))
                                                .foregroundStyle(MobileLensStyle.green)
                                        }
                                    }.padding(.horizontal, 20).padding(.vertical, 16)
                                        .contentShape(.rect)
                                }.buttonStyle(.plain)
                                    .accessibilityAddTraits(organization.id == model.mobileCloudOrganization ? .isSelected : [])
                                    .disabled(model.mobileCloudSelectionLoading)
                            }
                        }.background(MobileLensStyle.card, in: .rect(cornerRadius: 28))
                            .listRowInsets(EdgeInsets())
                            .listRowBackground(Color.clear)
                    } header: {
                        Text("Organization").font(.subheadline.weight(.semibold))
                    }
                }
                Section {
                    Picker("Primary lens", selection: $primaryLens) {
                        ForEach(MobileLens.allCases) { Text($0.rawValue).tag($0.rawValue) }
                    }
                    NavigationLink { CloudManagementView(requestSignIn: { signIn(false) }).navigationTitle("Organization") } label: { Label("Manage organization", systemImage: "person.2") }
                    NavigationLink { MobileVPNView(model: model, vpn: model.vpn) } label: { Label("Connection settings", systemImage: "network") }
                }
                if accounts.isEmpty && model.account?.signedIn != true {
                    Text("Add an account to access your fleet and organization.")
                        .font(.subheadline).foregroundStyle(.secondary)
                } else {
                    Section("Accounts") {
                    ForEach(accounts) { entry in
                        Button { actions.submit { await model.selectMobileCloudAccount(entry.id) } } label: {
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
                }
                if model.mobileCloudSelectionLoading { ProgressView() }
                if let error = model.mobileCloudSelectionError { FailureNote(message: error) }
            }
            .scrollContentBackground(.hidden)
            .background(MobileLensStyle.background)
            .navigationTitle("Settings").navigationBarTitleDisplayMode(.large)
            .tint(MobileLensStyle.green)
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
        .presentationBackground(MobileLensStyle.background)
        .presentationDetents([.medium, .large])
        .presentationDragIndicator(.visible)
    }
    private func refresh() async {
        await model.refreshMobileCloudSelection()
    }
}

extension AppModel {
    func refreshMobileCloudSelection(
        loadAccounts: () async throws -> [SavedAccount] = { try await GoClient.shared.accounts() },
        loadOrganizations: (String) async throws -> [CloudOrganization] = { try await CloudManagementClient().organizations(account: $0) }
    ) async {
        mobileCloudSelectionGeneration += 1
        let request = mobileCloudSelectionGeneration
        mobileCloudSelectionLoading = true
        mobileCloudSelectionError = nil
        defer { if request == mobileCloudSelectionGeneration { mobileCloudSelectionLoading = false } }
        do {
            let accounts = try await loadAccounts()
            guard request == mobileCloudSelectionGeneration else { return }
            let account = accounts.contains(where: { $0.id == mobileCloudAccount }) ? mobileCloudAccount
                : accounts.first(where: { $0.active })?.id ?? accounts.first?.id ?? ""
            // Account availability is independent of the organization request.
            // A failed Cloud read must not make a saved account look signed out.
            mobileCloudAccounts = accounts
            if account != mobileCloudAccount || account.isEmpty {
                mobileCloudOrganizations = []
                mobileCloudOrganization = ""
            }
            mobileCloudAccount = account
            let organizations = account.isEmpty ? [] : try await loadOrganizations(account)
            guard request == mobileCloudSelectionGeneration else { return }
            mobileCloudOrganizations = organizations
            if !organizations.contains(where: { $0.id == mobileCloudOrganization }) {
                mobileCloudOrganization = organizations.first?.id ?? ""
            }
        } catch {
            if request == mobileCloudSelectionGeneration && !Task.isCancelled {
                mobileCloudSelectionError = error.localizedDescription
            }
        }
    }
    func selectMobileCloudAccount(_ id: String) async {
        guard id != mobileCloudAccount else { return }
        mobileCloudAccount = id
        mobileCloudOrganization = ""
        mobileCloudOrganizations = []
        await refreshMobileCloudSelection()
    }
}
