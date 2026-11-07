import SwiftUI
import UniformTypeIdentifiers

enum MobileSitesStyle {
    static let background = Color(uiColor: UIColor { traits in
        traits.userInterfaceStyle == .dark ? .systemGroupedBackground : UIColor(red: 241/255, green: 238/255, blue: 231/255, alpha: 1)
    })
}

private enum SitesTab: String, CaseIterable { case home = "Home", missions = "Missions", activity = "Activity" }
private enum SitesSheet: Identifiable {
    case create, edit(UUID), rename(UUID, MobileSiteModelLink), model(UUID), deviceModels(UUID, MobileSiteModelLink?)
    var id: String { switch self { case .create: "create"; case .rename(_, let link): "rename-" + link.id.uuidString; case .edit(let id): "edit-" + id.uuidString; case .model(let id): id.uuidString; case .deviceModels(let id, let link): "device-" + id.uuidString + (link?.id.uuidString ?? "load") } }
}

struct MobileSitesLens: View {
    @ObservedObject var model: AppModel
    @StateObject private var store: MobileSitesStore
    @State private var tab = SitesTab.home
    @State private var sheet: SitesSheet?
    @State private var viewing: MobileSiteModelLink?
    @State private var cameraModel: MobileSiteModelLink?
    @State private var message: String?

    init(model: AppModel) {
        self.model = model
        var base: URL?
        #if DEBUG
        if let namespace = ProcessInfo.processInfo.environment["WENDY_SITES_UI_TEST"], UUID(uuidString: namespace) != nil {
            base = FileManager.default.temporaryDirectory.appendingPathComponent("SitesUITests/" + namespace, isDirectory: true)
        }
        #endif
        _store = StateObject(wrappedValue: MobileSitesStore(account: model.mobileCloudAccount, organization: model.mobileCloudOrganization, base: base))
    }
    private var current: MobileSiteRecord? { store.current }
    var body: some View {
        TabView(selection: $tab) {
            home.tabItem { Label("Home", systemImage: "house") }.tag(SitesTab.home)
            missions.tabItem { Label("Missions", systemImage: "flag") }.tag(SitesTab.missions)
            activity.tabItem { Label("Activity", systemImage: "clock") }.tag(SitesTab.activity)
        }.tint(MobileLensStyle.accent)
            .task(id: "\(model.mobileCloudSelectionKey)-\(model.mobileScopeIsCurrent)") {
                guard model.mobileScopeIsCurrent else { return }
                await model.refreshCloud()
                await model.refreshNearby(automatic: true)
            }
            .navigationTitle(tab == .home ? "Home" : tab.rawValue).navigationBarTitleDisplayMode(.inline)
            .toolbar {
                if !store.sites.isEmpty {
                    ToolbarItem(placement: .principal) {
                        Menu {
                            ForEach(store.sites) { site in
                                Button { perform { try store.select(site.id) } } label: {
                                    if current?.id == site.id { Label(site.name, systemImage: "checkmark") }
                                    else { Text(site.name) }
                                }
                            }
                        } label: {
                            HStack(spacing: 6) {
                                Text(current?.name ?? "Sites").lineLimit(1)
                                Image(systemName: "chevron.down").font(.caption)
                            }.font(.headline)
                        }
                    }
                    ToolbarItem(placement: .topBarTrailing) {
                        Button("Create Site", systemImage: "plus") { sheet = .create }
                            .disabled(store.loadError != nil || !model.mobileScopeIsCurrent)
                    }
                }
            }
            .sheet(item: $sheet) { route in
                Group { switch route {
                case .create:
                    MobileCreateSite(model: model, store: store) { tab = .home }
                case .edit(let siteID):
                    if let site = store.sites.first(where: { $0.id == siteID }) {
                        MobileCreateSite(model: model, store: store, editing: site) {}
                    }
                case .rename(let siteID, let link):
                    MobileRenameSiteModel(store: store, siteID: siteID, link: link)
                case .model(let siteID):
                    MobileAddSiteModel(store: store, siteID: siteID)
                case .deviceModels(let siteID, let link):
                    MobileSiteDeviceModels(model: model, store: store, siteID: siteID, saving: link)
                } }.tint(MobileLensStyle.accent)
            }
            .fullScreenCover(item: $viewing) { link in
                if let site = current {
                    MobileSiteModelViewer(model: model, store: store, siteID: site.id, link: link)
                }
            }
            .fullScreenCover(item: $cameraModel) { link in
                if let site = current { MobileSiteCameras(model: model, store: store, siteID: site.id, initialModel: link.id) }
            }
            .alert("Sites", isPresented: Binding(get: { message != nil }, set: { if !$0 { message = nil } })) {
                Button("OK") { message = nil }
            } message: { Text(message ?? "") }
    }
    private var empty: some View {
        ContentUnavailableView {
            Label("Set up your first site", systemImage: "building.2")
        } description: {
            Text(store.loadError ?? "Bring your devices, robots and 3D models together in one place.")
        } actions: {
            Button("Create Site") { sheet = .create }.buttonStyle(.borderedProminent)
                .disabled(store.loadError != nil || !model.mobileScopeIsCurrent)
            Text("Saved on this iPhone. Cloud sync is not available yet.").font(.footnote).foregroundStyle(.secondary)
        }.background(MobileSitesStyle.background).accessibilityIdentifier("sites.empty")
    }
    @ViewBuilder private var home: some View {
        if let site = current {
            List {
                if !site.hideSetup {
                    Section {
                        Label {
                            VStack(alignment: .leading) {
                                Text("Site, devices and robots")
                                Text(site.assetSummary).font(.caption).foregroundStyle(.secondary)
                            }
                        } icon: { Image(systemName: "checkmark.circle.fill").foregroundStyle(MobileLensStyle.green) }
                        if site.models.isEmpty {
                            Button { sheet = .model(site.id) } label: { Label("Add a 3D model", systemImage: "cube") }
                        } else {
                            Label("3D model added", systemImage: "checkmark.circle.fill").foregroundStyle(.secondary)
                        }
                        setupAction("Place and calibrate cameras", symbol: "camera", site: site)
                        setupAction("Create a mission", symbol: "flag", site: site)
                    } header: {
                        HStack {
                            Text("Finish setting up · \(site.models.isEmpty ? 1 : 2) of 4")
                            Spacer()
                            Button("Not Now") { perform { try store.setSetupHidden(true, siteID: site.id) } }
                        }
                    }
                } else {
                    Button("Continue setup") { perform { try store.setSetupHidden(false, siteID: site.id) } }
                }
                Section("3D models") {
                    ForEach(site.models) { link in
                        HStack {
                            Button { viewing = link } label: {
                                HStack(spacing: 12) {
                                    MobileSiteModelThumbnail(store: store, modelID: link.id)
                                    VStack(alignment: .leading) {
                                        Text(link.name).foregroundStyle(.primary).lineLimit(2)
                                        if site.defaultModel == link.id { Text("Default").font(.caption).foregroundStyle(.secondary) }
                                    }
                                }
                            }.buttonStyle(.plain)
                            Spacer()
                            Menu {
                                Button("Rename", systemImage: "pencil") { sheet = .rename(site.id, link) }
                                Button("Save to device", systemImage: "square.and.arrow.up") { sheet = .deviceModels(site.id, link) }
                                Button("Make default", systemImage: "checkmark") { perform { try store.setDefault(modelID: link.id, siteID: site.id) } }
                            } label: { Image(systemName: "ellipsis.circle").padding(8) }
                            .accessibilityLabel("Options for \(link.name)")
                        }
                    }
                    Button("Add a 3D model", systemImage: "plus") { sheet = .model(site.id) }
                    Button("Load from device", systemImage: "square.and.arrow.down") { sheet = .deviceModels(site.id, nil) }
                }
                if let link = site.models.first(where: { $0.id == site.defaultModel }) ?? site.models.first {
                    MobileSiteLiveCameras(model: model, store: store, site: site, link: link)
                }
                Section("Devices and Robots") {
                    Button("Edit devices and robots", systemImage: "pencil") { sheet = .edit(site.id) }
                    if site.assets.isEmpty { Text("No devices or robots selected").foregroundStyle(.secondary) }
                    ForEach(site.assets) { asset in Label(asset.name, systemImage: asset.robot ? "figure.walk" : "cpu") }
                }
                Section {
                    Text("On this iPhone").font(.footnote).foregroundStyle(.secondary)
                } footer: { Text("Sites, model links and activity are saved locally for this account and organization. Cloud sync is not available yet.") }
            }.listStyle(.insetGrouped).scrollContentBackground(.hidden).background(MobileSitesStyle.background)
        } else { empty }
    }
    private func setupAction(_ title: String, symbol: String, site: MobileSiteRecord) -> some View {
        Button {
            if site.models.isEmpty { message = "Add a 3D model first." }
            else if symbol == "camera" { cameraModel = site.models.first { $0.id == site.defaultModel } ?? site.models.first }
            else { message = "\(title) is not available in this build yet." }
        } label: { Label(title, systemImage: symbol).foregroundStyle(site.models.isEmpty ? Color.secondary : MobileLensStyle.green) }
    }
    @ViewBuilder private var sites: some View {
        if store.sites.isEmpty { empty }
        else {
            List(store.sites) { site in
                Button {
                    perform { try store.select(site.id); tab = .home }
                } label: {
                    Label {
                        VStack(alignment: .leading, spacing: 4) {
                            Text(site.name).foregroundStyle(.primary)
                            Text("\(site.assets.count) devices and robots · \(site.models.isEmpty ? "no 3D model" : "\(site.models.count) 3D models")")
                                .font(.caption).foregroundStyle(.secondary)
                        }
                    } icon: { Image(systemName: "building.2") }
                }
            }.listStyle(.insetGrouped).scrollContentBackground(.hidden).background(MobileSitesStyle.background)
        }
    }
    private var missions: some View {
        ContentUnavailableView {
            Label("Missions", systemImage: "flag")
        } description: {
            Text(current?.models.isEmpty != false ? "Missions use places from a 3D model. Add a model to prepare your site." : "Mission planning and execution are not available in this build yet.")
        } actions: {
            if let site = current, site.models.isEmpty { Button("Add a 3D Model") { sheet = .model(site.id) }.buttonStyle(.borderedProminent) }
            else if current == nil { Button("Create Site") { sheet = .create }.buttonStyle(.borderedProminent).disabled(store.loadError != nil) }
        }
    }
    private var activity: some View {
        let events = store.events.filter { $0.siteID == current?.id }.reversed()
        return Group {
            if events.isEmpty { ContentUnavailableView("No activity yet", systemImage: "clock", description: Text("Site creation and model additions will appear here.")) }
            else {
                List(Array(events)) { event in
                    VStack(alignment: .leading, spacing: 4) {
                        Text(event.text)
                        Text(event.date.formatted(date: .abbreviated, time: .shortened)).font(.caption).foregroundStyle(.secondary)
                    }
                }.listStyle(.insetGrouped).scrollContentBackground(.hidden).background(MobileSitesStyle.background)
            }
        }
    }
    private func perform(_ action: () throws -> Void) {
        do { try action() } catch { message = error.localizedDescription }
    }
}

private struct MobileSiteAssetChoice: Identifiable {
    let asset: MobileSiteAsset
    let status: String
    let online: Bool
    var id: String { asset.id }
}

private struct MobileCreateSite: View {
    @ObservedObject var model: AppModel
    @ObservedObject var store: MobileSitesStore
    let created: () -> Void
    let editing: MobileSiteRecord?
    @Environment(\.dismiss) private var dismiss
    @State private var name = ""
    @State private var selected = Set<String>()
    @State private var choices: [MobileSiteAssetChoice] = []
    @State private var error: String?
    @State private var initialized = false
    @State private var loadingRobots = true
    @State private var robotError: String?
    init(model: AppModel, store: MobileSitesStore, editing: MobileSiteRecord? = nil, created: @escaping () -> Void) {
        self.model = model; self.store = store; self.editing = editing; self.created = created
        _name = State(initialValue: editing?.name ?? "")
        _selected = State(initialValue: Set(editing?.assets.map(\.id) ?? []))
        _choices = State(initialValue: (editing?.assets ?? []).map { MobileSiteAssetChoice(asset: $0, status: "Offline", online: false) })
    }
    var body: some View {
        NavigationStack {
            Form {
                if editing == nil {
                    Section("Name") { TextField("e.g. Oakland warehouse", text: $name).accessibilityIdentifier("sites.name") }
                }
                Section("Devices and robots") {
                    NavigationLink {
                        MobileSiteAssetPicker(title: "Devices", choices: choices.filter { !$0.asset.robot }, selected: $selected)
                    } label: { LabeledContent("Devices", value: "\(choices.filter { !$0.asset.robot && selected.contains($0.id) }.count) selected") }
                    NavigationLink {
                        MobileSiteAssetPicker(title: "Robots", choices: choices.filter { $0.asset.robot }, selected: $selected, loading: loadingRobots, warning: robotError)
                    } label: { LabeledContent("Robots", value: "\(choices.filter { $0.asset.robot && selected.contains($0.id) }.count) selected") }
                }
                Section {
                    Text("On this iPhone").foregroundStyle(.secondary)
                } footer: { Text(editing == nil ? "This creates a local site. It does not change Cloud device assignments or pair robots." : "Changes apply to this site on your iPhone. Saved models and camera placements are kept.") }
                if let error { Text(error).foregroundStyle(.red) }
            }.scrollContentBackground(.hidden).background(MobileSitesStyle.background)
                .navigationTitle(editing == nil ? "New Site" : "Edit devices and robots").navigationBarTitleDisplayMode(.inline)
                .toolbar {
                    ToolbarItem(placement: .cancellationAction) { Button("Cancel") { dismiss() } }
                    ToolbarItem(placement: .confirmationAction) {
                        Button(editing == nil ? "Create" : "Save") {
                            do {
                                let assets = choices.filter { selected.contains($0.id) }.map(\.asset)
                                if let editing { try store.setAssets(assets, siteID: editing.id) }
                                else { try store.create(name: name, assets: assets) }
                                created(); dismiss()
                            } catch { self.error = error.localizedDescription }
                        }.disabled(name.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty || name.count > 200 || !model.mobileScopeIsCurrent)
                    }
                }
        }.tint(MobileLensStyle.green)
            .onChange(of: HomeWidgetInventory.devices(model), initial: true) { _, inventory in
                let known = Set(choices.map(\.id))
                let refreshed = inventory.map { device in
                    let status = HomeWidgetInventory.status(device, model: model)
                    return MobileSiteAssetChoice(asset: .init(id: recordingDeviceID(device), name: device.title, robot: false), status: status, online: ["Online", "Connected", "Nearby"].contains(status))
                }
                // Discovery may finish after the sheet appears. Add new rows
                // without overwriting choices the user already made.
                let refreshedIDs = Set(refreshed.map(\.id))
                choices = refreshed + choices.filter { $0.asset.robot || !refreshedIDs.contains($0.id) }.map {
                    $0.asset.robot ? $0 : MobileSiteAssetChoice(asset: $0.asset, status: "Offline", online: false)
                }
                if editing == nil { selected.formUnion(refreshed.filter { $0.online && !known.contains($0.id) }.map(\.id)) }
            }
            .task {
                guard !initialized else { return }; initialized = true
                defer { loadingRobots = false }
                do {
                    let snapshot = try await UnitreeClient.shared.robots()
                    guard !Task.isCancelled else { return }
                    var seen = Set<String>()
                    let robots = snapshot.robots.filter { !$0.serial.isEmpty && seen.insert($0.serial).inserted }.map {
                        MobileSiteAssetChoice(asset: .init(id: "unitree:" + $0.serial, name: $0.name.isEmpty ? $0.serial : $0.name, robot: true), status: $0.connected ? "Connected" : "Not connected", online: $0.connected)
                    }
                    let robotIDs = Set(robots.map(\.id))
                    choices = choices.filter { !robotIDs.contains($0.id) } + robots
                    if editing == nil { selected.formUnion(robots.filter(\.online).map(\.id)) }
                    robotError = snapshot.warning.isEmpty ? nil : snapshot.warning
                } catch { if !Task.isCancelled { robotError = error.localizedDescription } }
            }
    }
}

private struct MobileSiteAssetPicker: View {
    let title: String
    let choices: [MobileSiteAssetChoice]
    @Binding var selected: Set<String>
    var loading = false
    var warning: String?
    @State private var search = ""

    private var visible: [MobileSiteAssetChoice] {
        let query = search.trimmingCharacters(in: .whitespacesAndNewlines)
        return choices.filter { query.isEmpty || $0.asset.name.localizedCaseInsensitiveContains(query) }
            .sorted {
                if $0.online != $1.online { return $0.online }
                let order = $0.asset.name.localizedStandardCompare($1.asset.name)
                return order == .orderedSame ? $0.id < $1.id : order == .orderedAscending
            }
    }
    private var visibleSelection: Binding<Set<String>> {
        let ids = Set(visible.map(\.id))
        return Binding(get: { selected.intersection(ids) }, set: { updated in
            selected.subtract(ids)
            selected.formUnion(updated.intersection(ids))
        })
    }
    var body: some View {
        List(selection: visibleSelection) {
            Section {
                ForEach(visible) { choice in
                    HStack(spacing: 12) {
                        Image(systemName: choice.asset.robot ? "figure.walk" : "cpu")
                            .foregroundStyle(choice.online ? MobileLensStyle.green : Color.secondary)
                        VStack(alignment: .leading, spacing: 3) {
                            Text(choice.asset.name).foregroundStyle(choice.online ? Color.primary : Color.secondary)
                            HStack(spacing: 5) {
                                Circle().fill(choice.online ? MobileLensStyle.green : Color.gray).frame(width: 7, height: 7)
                                Text(choice.status).font(.caption)
                            }.foregroundStyle(choice.online ? MobileLensStyle.green : Color.secondary)
                        }
                    }.tag(choice.id)
                }
                if visible.isEmpty && !loading {
                    Text(search.isEmpty ? "No \(title.lowercased()) available" : "No matching \(title.lowercased())")
                        .foregroundStyle(.secondary)
                }
            } header: {
                HStack {
                    Text(title)
                    Spacer()
                    let ids = Set(visible.map(\.id))
                    Button("Select All") { selected.formUnion(ids) }
                        .disabled(ids.isEmpty || ids.isSubset(of: selected))
                    let allIDs = Set(choices.map(\.id))
                    Button("Unselect All") { selected.subtract(allIDs) }
                        .disabled(selected.isDisjoint(with: allIDs))
                }
            }
            if loading { ProgressView("Looking for \(title.lowercased())…") }
            if let warning { Text(warning).foregroundStyle(.secondary) }
        }.environment(\.editMode, .constant(.active))
            .searchable(text: $search, placement: .navigationBarDrawer(displayMode: .always), prompt: "Search by name")
            .scrollContentBackground(.hidden).background(MobileSitesStyle.background)
            .navigationTitle(title).navigationBarTitleDisplayMode(.inline)
    }
}

private struct MobileAddSiteModel: View {
    @ObservedObject var store: MobileSitesStore
    let siteID: UUID
    @Environment(\.dismiss) private var dismiss
    @State private var importing = false
    @State private var capture: Capture?
    @State private var pending: URL?
    @State private var busy = false
    @State private var error: String?
    private enum Capture: String, Identifiable { case room, photos; var id: String { rawValue } }
    var body: some View {
        NavigationStack {
            List {
                Section {
                    Button("Scan with iPhone", systemImage: "viewfinder") { capture = .room }
                        .disabled(!SiteRoomCapture.isSupported || busy)
                    Button("Import a file", systemImage: "square.and.arrow.down") { importing = true }.disabled(busy)
                    if SitePhotoReconstruction.isSupported {
                        Button("Build from photos", systemImage: "photo.on.rectangle") { capture = .photos }.disabled(busy)
                    }
                } header: { Text("A site can have several models for floors, areas or rescans.") } footer: {
                    Text(SiteRoomCapture.isSupported ? "Import USDZ or .wendysite files. Your first model becomes the default." : "Room scanning needs a supported LiDAR device. You can import USDZ or .wendysite files here.")
                }
                if busy { ProgressView("Saving model…") }
                if let error { Text(error).foregroundStyle(.red) }
            }.scrollContentBackground(.hidden).background(MobileSitesStyle.background)
                .navigationTitle("Add a 3D model").navigationBarTitleDisplayMode(.inline)
                .toolbar { ToolbarItem(placement: .cancellationAction) { Button("Cancel") { dismiss() }.disabled(busy) } }
        }.presentationDetents([.medium, .large]).interactiveDismissDisabled(busy)
            .fileImporter(isPresented: $importing, allowedContentTypes: [.usdz, .data]) { result in
                switch result {
                case .success(let url): pending = url
                case .failure(let failure): if (failure as NSError).code != NSUserCancelledError { error = failure.localizedDescription }
                }
            }
            .fullScreenCover(item: $capture) { method in
                switch method {
                case .room: SiteRoomCapture(completion: captured)
                case .photos: SitePhotoReconstruction(completion: captured)
                }
            }
            .task(id: pending) {
                guard let pending else { return }
                busy = true; error = nil
                defer { busy = false; self.pending = nil }
                do { try await store.addModel(from: pending, to: siteID); dismiss() }
                catch is CancellationError {} catch { self.error = error.localizedDescription }
            }
    }
    private func captured(_ result: Result<URL, Error>) {
        capture = nil
        switch result { case .success(let url): pending = url; case .failure(let failure): error = failure.localizedDescription }
    }
}

private struct MobileSiteModelViewer: View {
    @ObservedObject var model: AppModel
    @ObservedObject var store: MobileSitesStore
    let siteID: UUID
    let link: MobileSiteModelLink
    @State private var savingToDevice = false
    @State private var preview: SiteCamera?
    @State private var placingCameras = false
    @Environment(\.dismiss) private var dismiss
    @State private var saved: StoredSite?
    @State private var error: String?
    var body: some View {
        NavigationStack {
            Group {
                if let saved {
                    MobileSiteCoverageScene(model: model, saved: saved, assets: store.sites.first { $0.id == siteID }?.assets ?? [], active: !placingCameras && !savingToDevice && preview == nil, placing: nil, select: { id in preview = saved.site.cameras.first { $0.id == id } }, place: { _, _ in })
                } else if let error { ContentUnavailableView("Model unavailable", systemImage: "cube", description: Text(error)) }
                else { ProgressView("Loading model…") }
            }.navigationTitle(link.name).navigationBarTitleDisplayMode(.inline)
                .toolbar {
                    ToolbarItem(placement: .confirmationAction) { Button("Done") { dismiss() } }
                    ToolbarItem(placement: .topBarLeading) { Button("Cameras", systemImage: "camera") { placingCameras = true }.disabled(saved == nil) }
                }
            .safeAreaInset(edge: .bottom) {
                Button {
                    savingToDevice = true
                } label: {
                    Label("Save to device", systemImage: "square.and.arrow.up")
                        .frame(maxWidth: .infinity)
                }
                .buttonStyle(.borderedProminent)
                .tint(MobileLensStyle.green)
                .disabled(saved == nil)
                .accessibilityIdentifier("sites.model.saveToDevice")
                .padding()
                .background(.regularMaterial)
            }
        }.fullScreenCover(isPresented: $placingCameras) {
            MobileSiteCameras(model: model, store: store, siteID: siteID, initialModel: link.id)
        }.sheet(item: $preview) { camera in
            MobileSiteCameraLive(model: model, site: store.sites.first { $0.id == siteID }, camera: camera, calibrationModel: saved, store: store)
        }.sheet(isPresented: $savingToDevice) {
            MobileSiteDeviceModels(model: model, store: store, siteID: siteID, saving: link)
        }.task(id: "\(placingCameras)-\(preview?.id.uuidString ?? "none")") {
            guard !placingCameras, preview == nil else { return }
            do { saved = try await store.models.load(id: link.id) } catch { self.error = error.localizedDescription }
        }
    }
}
