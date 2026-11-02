import SwiftUI

struct ContentView: View {
    @State private var actions = AsyncActions()
    @EnvironmentObject private var model: AppModel
    @State private var tab = "overview"
    @State private var showingAppLogs = false
    @State private var showingEventLog = false
    @State private var requestedClip: (session: UInt64, id: String)?
    @State private var showingOrganization = false
    @State private var showingSites = false
    @State private var showingActivity = false
    @State private var showingSimulators = false
    @StateObject private var simulators = SimulatorModel()
    @FocusedValue(\.simulatorRefresh) private var simulatorRefresh
    @State private var refreshingPane = false
    @FocusedValue(\.organizationRefresh) private var organizationRefresh
    @FocusedValue(\.devicePaneRefresh) private var devicePaneRefresh
    @FocusedValue(\.fleetRefresh) private var fleetRefresh
    @State private var columnVisibility: NavigationSplitViewVisibility = .all
    @FocusState private var searchFocused: Bool
    @FocusState private var deviceTabsFocused: Bool
    @State private var deviceListFocusRequest = 0
    @State private var search = ""
    @State private var sidebarSelection: String?
    @State private var deviceSource = "nearby"
    @State private var manualAddress = ""
    @State private var showingAddress = false
    @State private var showingNearbySetup = false
    @State private var setupSignInAfterDismiss = false
    @State private var showingUnitree = false
    @State private var selectedUnitree: UnitreeRobot?
    @State private var showingInstaller = false
    @ObservedObject private var installer = InstallSession.shared
    @State private var errorDetails: String?
    @Environment(\.scenePhase) private var scenePhase

    var body: some View {
        actionContent.task { await actions.run() }
            .task(id: "\(model.initialized)/\(scenePhase == .active)") {
                if model.initialized && scenePhase == .active { await model.maintainExperience() }
            }
            .onChange(of: model.requestedDevice) { _, device in
                if let device {
                    showingSites = false; showingActivity = false; showingOrganization = false; showingSimulators = false
                    tab = "overview"; model.requestedDevice = nil
                    actions.submit { await model.connect(device) }
                }
            }
            .modifier(NearbySetupPrompt())
    }

    @ViewBuilder private var actionContent: some View {
        NavigationSplitView(columnVisibility: $columnVisibility) {
            VStack(spacing: 0) {
                VStack(spacing: 2) {
                    sidebarDestination("Devices", icon: "square.grid.2x2", selected: !showingSites && !showingActivity && !showingSimulators && model.selection == nil) {
                        showingOrganization = false; showingSites = false; showingActivity = false; showingSimulators = false
                        sidebarSelection = nil; model.selection = nil
                    }
                    sidebarDestination("Sites", icon: "building.2", selected: showingSites) {
                        showingSites = true; showingActivity = false; showingSimulators = false; showingOrganization = false; sidebarSelection = nil
                    }
                    sidebarDestination("Activity", icon: "clock", selected: showingActivity) {
                        showingActivity = true; showingSites = false; showingSimulators = false; showingOrganization = false; sidebarSelection = nil
                    }
                    sidebarDestination("Simulators", icon: "cube.transparent", selected: showingSimulators) {
                        showingSimulators = true; showingActivity = false; showingOrganization = false; showingSites = false; sidebarSelection = nil
                    }
                }
                .padding(.horizontal, 10).padding(.top, 4).padding(.bottom, 8)

                HStack(spacing: 6) {
                    HStack(spacing: 5) {
                        Image(systemName: "magnifyingglass").foregroundStyle(.secondary)
                        TextField("Find a device", text: $search).textFieldStyle(.plain)
                            .focused($searchFocused)
                            .onSubmit { focusDevices() }
                            .onKeyPress(.downArrow) { focusDevices(); return .handled }
                            .onKeyPress(.escape) {
                                if search.isEmpty { focusDevices() } else { search = "" }
                                return .handled
                            }
                        if !search.isEmpty {
                            Button { search = "" } label: { Image(systemName: "xmark.circle.fill").foregroundStyle(.secondary) }
                                .buttonStyle(.plain).accessibilityLabel("Clear search")
                        }
                    }
                    .padding(.horizontal, 7).frame(height: 26)
                    .background(Color.primary.opacity(0.045), in: RoundedRectangle(cornerRadius: 5))
                    HStack(spacing: 1) {
                        sourceTab("Nearby", source: "nearby", icon: "network")
                        sourceTab("Cloud", source: "cloud", icon: "cloud")
                    }
                }
                .font(.system(size: 12))
                .padding(.horizontal, 10).padding(.bottom, 8)
                Divider()
                List(selection: $sidebarSelection) {
                    if model.discovering { ProgressView().controlSize(.small) }
                    deviceRows
                    if !model.nearbyUnitree.isEmpty {
                        ForEach(model.nearbyUnitree.filter { search.isEmpty || "\($0.name) \($0.serial) \($0.address)".localizedCaseInsensitiveContains(search) }) { robot in
                            Button {
                                selectedUnitree = robot; showingUnitree = true
                            } label: {
                                VStack(alignment: .leading, spacing: 4) {
                                    Label { Text(robot.name) } icon: { RobotIcon() }
                                    Text("\(robot.address) · Remote control").font(.caption).foregroundStyle(.secondary)
                                }
                            }.buttonStyle(.plain)
                            .contextMenu {
                                Button("Forget saved robot", role: .destructive) { model.forgetSavedUnitree(robot) }
                            }
                        }
                    }
                    if deviceSource == "nearby", let error = model.unitreeDiscoveryError {
                        Text(error).font(.caption).foregroundStyle(.secondary)
                    }
                    if deviceSource == "nearby" {
                        if let message = model.nearbyError { FailureNote(message: message) }
                        if model.nearby.isEmpty && model.nearbyUnitree.isEmpty && !model.discovering {
                            Text("No nearby devices found").foregroundStyle(.secondary)
                        }
                    } else {
                        if let message = model.cloudError { FailureNote(message: message) }
                        if model.cloud.isEmpty && !model.discovering {
                            if model.account?.signedIn == true {
                                Text("No cloud devices found").foregroundStyle(.secondary)
                            } else {
                                Button("Sign in to Wendy Cloud") { actions.submit { await model.signIn() } }
                            }
                        }
                    }
                }
                .listStyle(.sidebar)
                .keyboardRowSelection(visibleDevices.map(\.id), selection: $sidebarSelection, focusRequest: deviceListFocusRequest)
                .onChange(of: sidebarSelection) { _, id in
                    guard let device = visibleDevices.first(where: { $0.id == id }) else { return }
                    showingOrganization = false; showingSites = false; showingSimulators = false; showingActivity = false
                    actions.submit { await model.connect(device) }
                }
                Divider()
                HStack {
                    AccountPicker(app: model)
                    Button { showingOrganization = true; showingSites = false; showingActivity = false; showingSimulators = false } label: { Image(systemName: "gearshape") }
                        .buttonStyle(.plain).help("Manage organization")
                }

            }
            .navigationSplitViewColumnWidth(min: 230, ideal: 270, max: 380)
        } detail: {
            if showingActivity {
                ExperienceActivityView(model: model) { device in
                    showingActivity = false; tab = "overview"
                    actions.submit { await model.connect(device) }
                }
            } else if showingSimulators {
                SimulatorsView(model: simulators) { item in
                    showingSimulators = false
                    actions.submit { await model.connect(Device(id: "vm:\(item.name)", name: item.name, selector: "vm:\(item.name)", transport: .lan, address: item.address)) }
                }
            } else if showingSites {
                SitesView { device in
                    showingSites = false; showingOrganization = false
                    actions.submit { await model.connect(device) }
                }.id(model.account?.context)
            } else if showingOrganization {
                CloudManagementView().id(model.account?.context)
            } else if let device = model.selection {
                deviceDetail(device)
            } else {
                ScrollView {
                    VStack(alignment: .leading, spacing: 24) {
                        Text("Your devices").font(.largeTitle.weight(.semibold))
                        Text("Pick a device and see what’s happening.").foregroundStyle(.secondary)
                        LazyVGrid(columns: [GridItem(.adaptive(minimum: 280), spacing: 20)], spacing: 20) {
                            ForEach(visibleDevices) { device in
                                Button {
                                    tab = "overview"
                                    actions.submit { await model.connect(device) }
                                } label: {
                                    DeviceIdentityRow(device: device, connected: model.connected?.id == device.id)
                                        .padding(20).background(.quaternary.opacity(0.4), in: RoundedRectangle(cornerRadius: 24))
                                }.buttonStyle(.plain)
                            }
                        }
                        if visibleDevices.isEmpty {
                            ContentUnavailableView("Your next device starts here", systemImage: "cpu", description: Text("Power on a nearby device, or sign in to see your organization’s devices."))
                        }
                        Button("Install Wendy on new hardware") { showingInstaller = true }
                    }.padding(32)
                }

            }
        }
        .toolbar {
            ToolbarItem { VPNToolbarControls(model: model, vpn: model.vpn) }
            ToolbarItemGroup {
                Button {
                    if showingSimulators { simulatorRefresh?.perform() }
                    else if showingSites { fleetRefresh?.perform() }
                    else if showingOrganization { organizationRefresh?.perform() }
                    else if ["campaigns", "pairing", "chat", "data", "settings"].contains(tab), let devicePaneRefresh { devicePaneRefresh.perform() }
                    else {
                        refreshingPane = true
                        actions.submit {
                            defer { refreshingPane = false }
                            if model.connected != nil { await model.refreshDetails() }
                            else if deviceSource == "cloud" { await model.refreshCloud() }
                            else { await model.refreshNearby() }
                        }
                    }
                } label: { Label("Refresh", systemImage: "arrow.clockwise") }
                    .help(showingSimulators ? "Refresh simulators" : showingSites ? "Refresh fleet" : showingOrganization ? "Refresh organization" : model.connected != nil ? "Refresh device" : "Refresh devices")
                    .keyboardShortcut("r", modifiers: .command)
                    .disabled(!showingSimulators && !showingSites && !showingOrganization && model.selection != nil && !model.selectedDeviceReady)
                    .disabled(showingSimulators ? simulatorRefresh?.enabled != true : showingSites ? fleetRefresh?.enabled != true : showingOrganization ? organizationRefresh?.enabled != true : ["campaigns", "pairing", "chat", "data", "settings"].contains(tab) ? devicePaneRefresh?.enabled != true : refreshingPane || model.discovering || !model.initialized)
                Menu {
                    Button { showingNearbySetup = true } label: {
                        Label("Set up nearby device…", systemImage: "antenna.radiowaves.left.and.right")
                    }
                    Divider()
                    Button(installer.running ? "Show running installer" : "Install Wendy on a device") { showingInstaller = true }
                    Button("Connect by address") { showingAddress = true }
                    Button("Connect to Unitree Go2") { selectedUnitree = nil; showingUnitree = true }
                    if model.selectedDeviceReady {
                        Button("Pair a device") { showingOrganization = false; showingSites = false; showingSimulators = false; showingActivity = false; tab = "pairing" }
                        Button("Campaigns") { showingOrganization = false; showingSites = false; showingSimulators = false; showingActivity = false; tab = "campaigns" }
                    }
                } label: { Label("Add", systemImage: "plus") }
            }
        }
        .sheet(isPresented: $showingNearbySetup, onDismiss: {
            actions.submit {
                if setupSignInAfterDismiss { setupSignInAfterDismiss = false; await model.signIn() }
                await model.refreshNearby()
            }
        }) {
            NearbySetupView { setupSignInAfterDismiss = true }
        }
        .sheet(isPresented: $showingUnitree) {
            VStack(spacing: 0) {
                HStack { Spacer(); Button("Done") { showingUnitree = false } }.padding([.top, .horizontal])
                UnitreePairingView(session: 0, deviceName: "this Mac", initialRobot: selectedUnitree)
            }.frame(width: 800, height: 760)
        }
        .sheet(isPresented: $showingInstaller) {
            InstallView(session: installer) { actions.submit { await model.refreshNearby() } }
        }
        .sheet(isPresented: $showingAddress) {
            VStack(alignment: .leading, spacing: 16) {
                Text("Connect to a device").font(.title2)
                TextField("Hostname or host:port", text: $manualAddress).textFieldStyle(.roundedBorder)
                HStack {
                    Spacer()
                    Button("Cancel") { showingAddress = false }.keyboardShortcut(.cancelAction)
                    Button("Connect") {
                        let address = manualAddress.trimmingCharacters(in: .whitespacesAndNewlines)
                        showingAddress = false
                        showingOrganization = false; showingSites = false; showingSimulators = false; showingActivity = false
                        actions.submit { await model.connect(Device(id: address, name: address, selector: address, transport: .lan, address: address)) }
                    }.keyboardShortcut(.defaultAction).disabled(manualAddress.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
                }
            }.padding(24).frame(width: 420)
        }
        .alert("Delete App?", isPresented: Binding(
            get: { model.appDeletion != nil },
            set: { if !$0 { model.appDeletion = nil } }
        ), presenting: model.appDeletion) { target in
            Button("Cancel", role: .cancel) { model.appDeletion = nil }
            Button("Delete App", role: .destructive) {
                model.appDeletion = nil
                actions.submit { await model.deleteApp(target) }
            }
        } message: { target in
            Text("Delete \"\(target.app)\" from \"\(target.device)\"? This stops and removes the app and its services. Persistent data and container images are kept.")
        }
        .alert("Could not complete the action", isPresented: Binding(get: { model.alert != nil }, set: { if !$0 { model.alert = nil } })) {
            Button("OK") { model.alert = nil }
            Button("Technical details") { errorDetails = model.alert; model.alert = nil }
        } message: { Text(PresentedFailure(model.alert ?? "").message) }
        .alert(PresentedFailure(model.connectionError ?? "").title ?? "Could not connect", isPresented: Binding(get: { model.connectionError != nil }, set: { if !$0 { model.connectionError = nil } })) {
            Button("OK", role: .cancel) { model.connectionError = nil }
            if let device = model.failedConnection {
                Button("Try again") { actions.submit { await model.connect(device) } }
                if model.canForgetIdentity {
                    Button("Forget saved identity and retry") { actions.submit { await model.connect(device, forgetIdentity: true) } }
                }
            }
            Button("Technical details") { errorDetails = model.connectionError; model.connectionError = nil }
        } message: { Text(PresentedFailure(model.connectionError ?? "").message) }
        .sheet(isPresented: Binding(get: { errorDetails != nil }, set: { if !$0 { errorDetails = nil } })) {
            VStack(alignment: .leading, spacing: 16) {
                Text("Technical details").font(.title2)
                ScrollView { Text(PresentedFailure(errorDetails ?? "").details).font(.system(.callout, design: .monospaced)).textSelection(.enabled).frame(maxWidth: .infinity, alignment: .leading) }
                HStack { Spacer(); Button("Done") { errorDetails = nil }.keyboardShortcut(.defaultAction) }
            }.padding(24).frame(width: 640, height: 360)
        }
        .focusedSceneValue(\.desktopNavigation, DesktopNavigationActions(
            deviceAvailable: model.selectedDeviceReady && !model.connectedLite,
            findDevice: { columnVisibility = .all; searchFocused = true },
            focusDevices: focusDevices,
            showOrganization: { showingActivity = false; showingOrganization = true; showingSimulators = false; showingSites = false; sidebarSelection = nil },
            showSites: { showingActivity = false; showingSites = true; showingSimulators = false; showingOrganization = false; sidebarSelection = nil },
            showSource: { source in deviceSource = source; focusDevices() },
            showPage: showPage,
            cyclePage: { forward in showPage(DesktopDevicePage.adjacent(to: tab, forward: forward)) },
            connectByAddress: { showingAddress = true }
        ))
        .onChange(of: model.deviceSession) { _, _ in showingEventLog = false }
        .onChange(of: model.selection?.id) { _, _ in tab = "overview" }
        .task { await model.start() }
        .task(id: deviceSource) {
            if deviceSource == "cloud", model.cloud.isEmpty, model.account?.signedIn == true {
                await model.refreshCloud()
            }
        }
        .task(id: scenePhase) {
            if scenePhase == .active { await model.refreshAccount() }
        }
    }

    private func focusDevices() {
        columnVisibility = .all
        searchFocused = false
        deviceListFocusRequest += 1
    }

    private func showPage(_ page: DesktopDevicePage) {
        guard model.selectedDeviceReady, !model.connectedLite else { return }
        showingOrganization = false
        showingSites = false
        showingSimulators = false
        showingActivity = false
        tab = page.rawValue
        deviceTabsFocused = true
    }

    private func sidebarDestination(_ title: String, icon: String, selected: Bool, action: @escaping () -> Void) -> some View {
        Button(action: action) {
            HStack(spacing: 8) {
                Image(systemName: icon).foregroundStyle(.secondary).frame(width: 20)
                Text(title)
                Spacer(minLength: 0)
            }
            .font(.system(size: 13))
            .padding(.horizontal, 8).frame(height: 28)
            .background(selected ? Color.primary.opacity(0.08) : .clear, in: RoundedRectangle(cornerRadius: 5))
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .accessibilityAddTraits(selected ? [.isSelected] : [])
    }

    private func sourceTab(_ title: String, source: String, icon: String) -> some View {
        Button { deviceSource = source } label: {
            Image(systemName: icon).font(.system(size: 13, weight: .medium))
                .frame(width: 26, height: 26)
                .foregroundStyle(deviceSource == source ? Color.primary : .secondary)
                .background(deviceSource == source ? Color.primary.opacity(0.09) : .clear, in: RoundedRectangle(cornerRadius: 5))
                .contentShape(Rectangle())
        }.buttonStyle(.plain).help(title).accessibilityLabel(title)
            .accessibilityValue(deviceSource == source ? "Selected" : "")
            .contextMenu {
                Button("Find devices again") {
                    actions.submit { if source == "cloud" { await model.refreshCloud() } else { await model.refreshNearby() } }
                }.disabled(model.discovering || !model.initialized)
            }
    }

    private var visibleDevices: [Device] {
        (deviceSource == "nearby" ? model.nearby : model.cloud)
            .filter { search.isEmpty || $0.title.localizedCaseInsensitiveContains(search) }
            .sorted { $0.title.localizedStandardCompare($1.title) == .orderedAscending }
    }

    @ViewBuilder private var deviceRows: some View {
        ForEach(visibleDevices) { device in
            HStack(spacing: 10) {
                DeviceEmblem(lite: device.lite, size: 30)
                Text(device.title).lineLimit(1)
                Spacer(minLength: 0)
                if model.connectingDevice?.id == device.id { ProgressView().controlSize(.small) }
                else if model.connected?.id == device.id { Image(systemName: "checkmark.circle.fill").foregroundStyle(.green) }
            }.frame(maxWidth: .infinity, minHeight: 30, alignment: .leading)
                .contentShape(Rectangle()).help(device.title)
                .tag(device.id)
        }
    }

    @ViewBuilder private func deviceDetail(_ device: Device) -> some View {
        VStack(alignment: .leading, spacing: 0) {
            ViewThatFits(in: .horizontal) {
                HStack(spacing: 24) {
                    deviceIdentity(device)
                    if !model.connectedLite { deviceNavigation(device).fixedSize() }
                    Spacer(minLength: 0)
                }
                VStack(alignment: .leading, spacing: 10) {
                    deviceIdentity(device)
                    if !model.connectedLite { deviceNavigation(device) }
                }
            }.padding(.horizontal, 20).padding(.vertical, 12)
            Divider().padding(.bottom, ["apps", "activity", "data"].contains(tab) ? 0 : 12)
            if model.connectingDevice?.id == device.id || model.detailError != nil {
                DeviceConnectionView(model: model, device: device)
            } else if model.selectedDeviceReady {
                if model.connectedLite { LiteDeviceView(model: model).id(model.deviceSession) } else {
                switch tab {
                case "overview": DeviceOverview(model: model, device: device) { tab = $0 }.id(model.deviceSession)
                case "cameras": camerasPane
                case "siteModel": NativeSiteView(model: model).id(model.deviceSession)
                case "apps", "activity": ActivityView(model: model).id(device.id)
                case "campaigns": CampaignsView(session: model.deviceSession, deviceName: device.title, openClip: { id in
                    requestedClip = (model.deviceSession, id)
                    tab = "data"
                }, showEventLog: { showingEventLog = true }).id(model.deviceSession)
                case "settings": DeviceSettingsView(session: model.deviceSession, deviceName: device.title).id(model.deviceSession)
                case "chat": DeviceChatView(deviceName: device.title, chat: model.deviceChat).id(model.deviceSession)
                case "data": DataVisualizerView(session: model.deviceSession, deviceName: device.title,
                                                initialEpisode: requestedClip?.session == model.deviceSession ? requestedClip?.id : nil).id(model.deviceSession)
                default: appsPane
                }
                }
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
        .sheet(isPresented: $showingAppLogs) {
            NavigationStack {
                DeviceLogsPage(model: model)
                    .toolbar { Button { showingAppLogs = false } label: { Image(systemName: "xmark") }.accessibilityLabel("Close logs").keyboardShortcut(.cancelAction) }
            }.frame(minWidth: 640, minHeight: 500)
        }

    }

    @ViewBuilder private func eventButton(_ device: Device) -> some View {
        if model.selectedDeviceReady {
            CampaignEventButton(session: model.deviceSession, deviceID: device.id, deviceName: device.title, showingLog: $showingEventLog)
                .id(model.deviceSession)
        }
    }

    private func deviceNavigation(_ device: Device) -> some View {
        HStack(alignment: .center, spacing: 12) {
            deviceTabs
            eventButton(device)
        }
    }

    private func deviceIdentity(_ device: Device) -> some View {
        HStack(spacing: 8) {
            Image(systemName: "cpu").foregroundStyle(.secondary)
            Text(device.title).font(.headline).lineLimit(1).help(device.title)
        }.frame(minWidth: 100, maxWidth: 220, alignment: .leading)
    }

    private var deviceTabs: some View {
        Picker("Device view", selection: $tab) {
            ForEach(DesktopDevicePage.allCases, id: \.self) { page in
                Text(page.title).tag(page.rawValue)
            }
        }.pickerStyle(.segmented).labelsHidden()
            .focused($deviceTabsFocused)
    }

    private var appsPane: some View {
        Group {
            if let error = model.appsError {
                unavailable("Could not load apps", error: error)
            } else if model.apps.isEmpty {
                ContentUnavailableView("No apps installed", systemImage: "square.stack.3d.up")
            } else {
                AppsView(model: model, apps: model.apps) { app in model.logApp = app.appName; showingAppLogs = true }
            }
        }.frame(maxWidth: .infinity, maxHeight: .infinity)
    }

    private var camerasPane: some View {
        CameraWallView(model: model)
    }

    private var logsPane: some View { LogsView(model: model) }

    private func unavailable(_ title: String, error: String) -> some View {
        FailureView(title: title, error: error) { actions.submit { await model.refreshDetails() } }
    }
}
