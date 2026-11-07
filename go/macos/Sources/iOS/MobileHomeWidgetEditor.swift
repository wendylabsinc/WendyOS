import SwiftUI

struct HomeWidgetManager: View {
    @ObservedObject var store: HomeWidgetStore
    @ObservedObject var model: AppModel
    @Environment(\.dismiss) private var dismiss
    @State private var draft: HomeWidget?
    var body: some View {
        Group {
        if let draft {
            HomeWidgetEditor(widget: draft, model: model, save: { store.save($0) }, cancel: { self.draft = nil })
                .id(draft.id)
        } else {
        VStack(spacing: 16) {
            HStack {
                Text("Add a widget").font(.title2.bold())
                Spacer()
                Button { dismiss() } label: {
                    Image(systemName: "xmark").font(.subheadline.weight(.semibold))
                        .foregroundStyle(.primary).frame(width: 44, height: 44)
                        .background(Color.primary.opacity(0.06), in: .circle)
                }.accessibilityLabel("Close widget picker")
            }.padding(.horizontal, 20).padding(.top, 20)
            ScrollView {
                VStack(spacing: 0) {
                    ForEach(HomeWidgetKind.allCases) { kind in
                        Button { draft = HomeWidget(kind: kind) } label: {
                            HStack(spacing: 16) {
                                Image(systemName: kind.symbol).font(.system(size: 21, weight: .medium))
                                    .foregroundStyle(.white).frame(width: 48, height: 48)
                                    .background(kind.color, in: .rect(cornerRadius: 14))
                                VStack(alignment: .leading, spacing: 3) {
                                    Text(kind.title).font(.headline).foregroundStyle(.primary)
                                    Text(kind.summary).font(.subheadline).foregroundStyle(.secondary)
                                }.frame(maxWidth: .infinity, alignment: .leading)
                                Image(systemName: "plus").font(.headline).foregroundStyle(.white)
                                    .frame(width: 40, height: 40).background(MobileLensStyle.green, in: .circle)
                            }.padding(.horizontal, 16).padding(.vertical, 14).contentShape(.rect)
                        }.buttonStyle(.plain).accessibilityLabel("Add " + kind.title)
                        if kind != HomeWidgetKind.allCases.last { Divider().padding(.leading, 80) }
                    }
                }.background(RecordingAppearance.card, in: .rect(cornerRadius: 26))
                    .clipShape(.rect(cornerRadius: 26)).padding(.horizontal, 20).padding(.bottom, 20)
            }
        }.background(MobileLensStyle.background).tint(MobileLensStyle.green)
            .presentationDragIndicator(.visible)
        }
        }
    }
}

struct HomeWidgetEditor: View {
    @State var widget: HomeWidget
    @ObservedObject var model: AppModel
    let save: (HomeWidget) -> Void
    var cancel: (() -> Void)? = nil
    @Environment(\.dismiss) private var dismiss
    @State private var actions = AsyncActions()
    @State private var cameras: [Camera] = []
    @State private var campaigns: [DeviceCampaign] = []
    @State private var apps: [DeviceApp] = []
    @State private var deviceSearch = ""
    @State private var resourceSearch = ""
    @State private var path: [Destination] = []
    @State private var loading = false
    @State private var error: String?
    private enum Destination: Hashable { case devices, resources }
    private var devices: [Device] { HomeWidgetInventory.devices(model).filter { widget.kind == .favorites || !$0.lite } }
    private var device: Device? { HomeWidgetInventory.resolve(widget.devices.first, model: model) }
    private var resourceKind: String { widget.kind == .camera ? "Camera" : widget.kind == .campaign ? "Campaign" : "App" }
    private var deviceSummary: String {
        if widget.kind == .favorites { return widget.devices.isEmpty ? "Choose devices" : "\(widget.devices.count) selected" }
        if widget.kind == .gallery && widget.devices.isEmpty { return "All devices" }
        return widget.devices.first?.title ?? "Choose a device"
    }

    var body: some View {
        NavigationStack(path: $path) {
            Form {
                Section("Title") {
                    TextField(widget.kind.title, text: $widget.title).accessibilityLabel("Widget title")
                }
                if !widget.kind.builtIn {
                    Section {
                        NavigationLink(value: Destination.devices) {
                            LabeledContent(widget.kind == .favorites ? "Devices" : "Device", value: deviceSummary)
                                .lineLimit(2).truncationMode(.middle)
                        }.accessibilityIdentifier("home.widget.selectDevices")
                        if widget.kind != .favorites && widget.kind != .gallery {
                            NavigationLink(value: Destination.resources) {
                                LabeledContent(resourceKind, value: widget.resourceTitle.isEmpty ? "Choose a " + resourceKind.lowercased() : widget.resourceTitle)
                                    .lineLimit(2).truncationMode(.middle)
                            }.disabled(device == nil)
                                .accessibilityIdentifier("home.widget.selectResource")
                        }
                    } footer: {
                        if widget.kind == .favorites { Text("Choose up to six favorite devices.") }
                        else if widget.kind != .gallery && device == nil { Text("Choose a device to see its available choices.") }
                    }
                }
                if widget.kind == .gallery {
                    Section("Recordings") {
                        Picker("Kind", selection: $widget.dataKind) {
                            Text("All kinds").tag(nil as String?)
                            ForEach(MobileRecordingKind.allCases, id: \.self) { Text($0 == .unknown ? "Unknown" : $0.rawValue).tag(Optional($0.rawValue)) }
                        }
                        TextField("Label (optional)", text: $widget.label).autocorrectionDisabled()
                    }
                }
                if widget.kind == .campaign {
                    Section { Text("Clips finish automatically using the campaign’s settings.").foregroundStyle(.secondary) }
                }
            }
            .navigationTitle(widget.kind.title).navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    if let cancel { Button("Back", systemImage: "chevron.left", action: cancel) }
                    else { Button("Cancel") { dismiss() } }
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Done") {
                        widget.label = widget.label.trimmingCharacters(in: .whitespacesAndNewlines)
                        save(widget)
                        dismiss()
                    }.disabled(!widget.valid || loading)
                }
            }
            .navigationDestination(for: Destination.self) { destination in
                switch destination {
                case .devices: devicePicker
                case .resources: resourcePicker
                }
            }
        }
        .tint(MobileLensStyle.green)
        .presentationDragIndicator(.visible)
        .task { await actions.run() }
    }

    private var devicePicker: some View {
        List {
            if widget.kind == .gallery {
                Button {
                    widget.devices = []
                    path.removeLast()
                } label: {
                    selectionLabel("All devices", selected: widget.devices.isEmpty)
                }
            }
            Section {
                ForEach(matchingDevices) { candidate in
                    let id = recordingDeviceID(candidate)
                    let selected = widget.devices.contains { $0.id == id }
                    Button {
                        if widget.kind == .favorites {
                            if selected { widget.devices.removeAll { $0.id == id } }
                            else if widget.devices.count < 6 { widget.devices.append(HomeWidgetDevice(candidate)) }
                        } else {
                            if !selected {
                                widget.devices = [HomeWidgetDevice(candidate)]
                                widget.resource = ""; widget.resourceTitle = ""
                                cameras = []; campaigns = []; apps = []; error = nil
                            }
                            path.removeLast()
                        }
                    } label: {
                        selectionLabel(candidate.title,
                                       subtitle: HomeWidgetInventory.status(candidate, model: model) + " · " + (candidate.transport == .cloud ? "Cloud" : "Local"),
                                       selected: selected)
                    }.disabled(widget.kind == .favorites && !selected && widget.devices.count >= 6)
                        .accessibilityAddTraits(selected ? .isSelected : [])
                }
                if matchingDevices.isEmpty {
                    Text(devices.isEmpty ? "No devices available" : "No matching devices").foregroundStyle(.secondary)
                }
            } footer: {
                if widget.kind == .favorites { Text("\(widget.devices.count) of 6 devices selected") }
            }
            let unavailable = widget.devices.filter { reference in !devices.contains { recordingDeviceID($0) == reference.id } }
            if !unavailable.isEmpty {
                Section("Unavailable devices") {
                    ForEach(unavailable) { reference in
                        HStack {
                            Text(reference.title).foregroundStyle(.secondary).lineLimit(3).truncationMode(.middle)
                            Spacer()
                            Button("Remove", systemImage: "minus.circle") {
                                widget.devices.removeAll { $0.id == reference.id }
                                if widget.kind != .favorites { widget.resource = ""; widget.resourceTitle = "" }
                            }.labelStyle(.iconOnly)
                        }
                    }
                }
            }
        }
        .navigationTitle(widget.kind == .favorites ? "Favorite devices" : "Choose a device")
        .navigationBarTitleDisplayMode(.inline)
        .searchable(text: $deviceSearch, placement: .navigationBarDrawer(displayMode: .always), prompt: "Search devices")
        .scrollDismissesKeyboard(.interactively)
    }

    private var matchingDevices: [Device] {
        let query = deviceSearch.trimmingCharacters(in: .whitespacesAndNewlines)
        return devices.filter { query.isEmpty || $0.title.localizedCaseInsensitiveContains(query) }.sorted {
            let leftOffline = HomeWidgetInventory.status($0, model: model) == "Offline"
            let rightOffline = HomeWidgetInventory.status($1, model: model) == "Offline"
            if leftOffline != rightOffline { return !leftOffline }
            return $0.title.localizedStandardCompare($1.title) == .orderedAscending
        }
    }

    private func selectionLabel(_ title: String, subtitle: String? = nil, selected: Bool) -> some View {
        HStack {
            VStack(alignment: .leading, spacing: 3) {
                Text(title).foregroundStyle(.primary).lineLimit(3).truncationMode(.middle)
                    .accessibilityLabel(title)
                if let subtitle { Text(subtitle).font(.caption).foregroundStyle(.secondary).lineLimit(2).truncationMode(.tail) }
            }
            Spacer()
            if selected { Image(systemName: "checkmark").foregroundStyle(MobileLensStyle.green) }
        }.contentShape(Rectangle())
    }

    private var resourcePicker: some View {
        List {
            if loading {
                HStack { Text("Loading " + resourceKind.lowercased() + " choices…"); Spacer(); ProgressView() }
            }
            if !widget.resource.isEmpty {
                Section("Selected") { resourceRow(widget.resource, widget.resourceTitle.isEmpty ? widget.resource : widget.resourceTitle) }
            }
            Section {
                ForEach(cameras.filter { $0.stableID != widget.resource && matchesResource($0.name) }, id: \.orderKey) { camera in
                    resourceRow(camera.stableID, camera.name)
                }
                ForEach(campaigns.filter { $0.name != widget.resource && matchesResource($0.name) }) { campaign in
                    resourceRow(campaign.name, campaign.name)
                }
                ForEach(apps.filter { $0.appName != widget.resource && matchesResource($0.appName) }) { app in
                    resourceRow(app.appName, app.appName)
                }
                if !loading && error == nil && !resourceSearch.isEmpty &&
                    !cameras.contains(where: { matchesResource($0.name) }) &&
                    !campaigns.contains(where: { matchesResource($0.name) }) &&
                    !apps.contains(where: { matchesResource($0.appName) }) {
                    Text("No matching choices").foregroundStyle(.secondary)
                }
            }
            if let error {
                Section {
                    Text(error).foregroundStyle(.secondary)
                    Button("Try again") { actions.submit { await loadChoices() } }.disabled(loading)
                }
            }
        }
        .navigationTitle("Choose a " + resourceKind.lowercased()).navigationBarTitleDisplayMode(.inline)
        .searchable(text: $resourceSearch, placement: .navigationBarDrawer(displayMode: .always), prompt: "Search " + resourceKind.lowercased() + "s")
        .task(id: widget.devices.first?.id) { await loadChoices() }
        .refreshable { await loadChoices() }
    }

    private func matchesResource(_ title: String) -> Bool {
        let query = resourceSearch.trimmingCharacters(in: .whitespacesAndNewlines)
        return query.isEmpty || title.localizedCaseInsensitiveContains(query)
    }

    private func resourceRow(_ id: String, _ title: String) -> some View {
        Button {
            widget.resource = id; widget.resourceTitle = title
            path.removeLast()
        } label: { selectionLabel(title, selected: widget.resource == id) }
    }

    private func loadChoices() async {
        guard let device, !loading else { return }
        loading = true; error = nil; defer { loading = false }
        let scope = model.mobileCloudSelectionKey
        await model.connect(device)
        guard !Task.isCancelled, scope == model.mobileCloudSelectionKey, model.mobileScopeIsCurrent,
              model.selectedDeviceReady, model.connected?.id == device.id else {
            error = model.detailError ?? "Could not connect to this device."; return
        }
        let session = model.deviceSession
        do {
            switch widget.kind {
            case .camera:
                if let problem = model.camerasError { throw WendyError(message: problem) }
                cameras = model.cameras.filter { !$0.stableID.isEmpty }
                if cameras.isEmpty { error = "No cameras with a stable identity were reported." }
            case .campaign:
                let rows = try await GoClient.shared.campaigns(session: session)
                guard !Task.isCancelled, session == model.deviceSession, scope == model.mobileCloudSelectionKey else { return }
                campaigns = rows
                if rows.isEmpty { error = "No campaigns are installed on this device." }
            case .portal:
                if let problem = model.appsError { throw WendyError(message: problem) }
                apps = model.apps.filter(\.hasWebpage)
                if apps.isEmpty { error = "No apps publish a web endpoint." }
            default: break
            }
        } catch { self.error = PresentedFailure(error.localizedDescription).details }
    }
}
