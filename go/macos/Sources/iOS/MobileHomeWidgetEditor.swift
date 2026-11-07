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
    var remove: (() -> Void)? = nil
    var cancel: (() -> Void)? = nil
    @Environment(\.dismiss) private var dismiss
    @State private var actions = AsyncActions()
    @State private var cameras: [Camera] = []
    @State private var campaigns: [DeviceCampaign] = []
    @State private var apps: [DeviceApp] = []
    @State private var search = ""
    @State private var loading = false
    @State private var error: String?
    private var devices: [Device] { HomeWidgetInventory.devices(model).filter { widget.kind == .favorites || !$0.lite } }
    private var device: Device? { HomeWidgetInventory.resolve(widget.devices.first, model: model) }
    var body: some View {
        NavigationStack {
            Group {
                if widget.kind.builtIn { editorForm }
                else {
                    editorForm.searchable(text: $search, placement: .navigationBarDrawer(displayMode: .always),
                                          prompt: "Search devices by name")
                }
            }
            .navigationTitle(widget.kind.title).navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    if let cancel { Button("Back", action: cancel) }
                    else { Button("Cancel") { dismiss() } }
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Done") {
                        widget.label = widget.label.trimmingCharacters(in: .whitespacesAndNewlines)
                        save(widget); dismiss()
                    }.disabled(!widget.valid || loading)
                }
            }
        }.tint(MobileLensStyle.green).presentationDragIndicator(.visible)
            .task { await actions.run() }
    }

    private var editorForm: some View {
        let available = devices
        let matches = matchingDevices(available)
        return Form {
            Section("Title") {
                TextField(widget.displayTitle, text: $widget.title).accessibilityLabel("Widget title")
            }
            Section("Size") {
                Picker("Size", selection: $widget.size) {
                    ForEach(widget.kind.sizes, id: \.self) { Text($0.rawValue).tag($0) }
                }.pickerStyle(.segmented)
            }
            if !widget.kind.builtIn {
                Section {
                    if widget.kind == .favorites {
                        ForEach(matches) { candidate in
                            let id = recordingDeviceID(candidate)
                            let selected = widget.devices.contains { $0.id == id }
                            Toggle(isOn: Binding(get: { selected }, set: { chosen in
                                if !chosen { widget.devices.removeAll { $0.id == id } }
                                else if widget.devices.count < 6 { widget.devices.append(HomeWidgetDevice(candidate)) }
                            })) { deviceLabel(candidate) }
                                .disabled(!selected && widget.devices.count >= 6)
                        }
                    } else {
                        Picker("Device", selection: selectedDevice) {
                            Text(widget.kind == .gallery ? "All devices" : "Choose a device").tag("")
                            ForEach(matches) { candidate in
                                deviceLabel(candidate).tag(recordingDeviceID(candidate))
                            }
                            // Keep a selected value valid even when search hides its row.
                            ForEach(widget.devices.filter { ref in !matches.contains { recordingDeviceID($0) == ref.id } }) { ref in
                                Text(ref.title).tag(ref.id)
                            }
                        }.pickerStyle(.inline).disabled(loading)
                    }
                    if matches.isEmpty {
                        Text(available.isEmpty ? "No devices available" : "No matching devices").foregroundStyle(.secondary)
                    }
                    ForEach(widget.devices.filter { ref in !available.contains { recordingDeviceID($0) == ref.id } }) { ref in
                        Button("Remove unavailable device: " + ref.title, role: .destructive) {
                            widget.devices.removeAll { $0.id == ref.id }
                            widget.resource = ""; widget.resourceTitle = ""
                        }
                    }
                } header: {
                    Text(widget.kind == .favorites ? "Devices · pick up to 6" : "Device")
                } footer: {
                    if widget.kind == .favorites && widget.devices.count >= 6 {
                        Text("6 devices selected. Deselect one to choose another.")
                    }
                }
            }
            if widget.kind == .gallery {
                Section("Recordings") {
                    Picker("Kind", selection: $widget.dataKind) {
                        Text("All kinds").tag(nil as String?)
                        ForEach(MobileRecordingKind.allCases, id: \.self) {
                            Text($0 == .unknown ? "Unknown" : $0.rawValue).tag(Optional($0.rawValue))
                        }
                    }
                    TextField("Label, e.g. Person (optional)", text: $widget.label).autocorrectionDisabled()
                }
            } else if !widget.kind.builtIn && widget.kind != .favorites {
                Section {
                    Picker(resourceLabel, selection: selectedResource) {
                        Text("Choose " + resourceLabel.lowercased()).tag("")
                        if !widget.resource.isEmpty {
                            Text(widget.resourceTitle.isEmpty ? widget.resource : widget.resourceTitle).tag(widget.resource)
                        }
                        ForEach(cameras.filter { $0.stableID != widget.resource }, id: \.orderKey) { Text($0.name).tag($0.stableID) }
                        ForEach(campaigns.filter { $0.name != widget.resource }) { Text($0.name).tag($0.name) }
                        ForEach(apps.filter { $0.appName != widget.resource }) { Text($0.appName).tag($0.appName) }
                    }.pickerStyle(.inline).disabled(loading)
                    Button(error == nil ? "Load choices" : "Try again") { actions.submit { await loadChoices() } }
                        .disabled(device == nil || loading)
                    if loading { ProgressView("Loading choices…") }
                    if let error { Text(error).foregroundStyle(.secondary) }
                } header: { Text(resourceLabel) } footer: {
                    if device == nil { Text("Choose a device first.") }
                    else if widget.kind == .campaign { Text("Clips finish automatically using the campaign’s settings.") }
                }
            }
            if let remove {
                Section { Button("Remove widget", role: .destructive) { remove(); dismiss() } }
            }
        }.scrollContentBackground(.hidden).background(MobileLensStyle.background)
            .scrollDismissesKeyboard(.interactively)
    }
    private var resourceLabel: String { widget.kind == .camera ? "Camera" : widget.kind == .campaign ? "Campaign" : "App" }
    private var selectedDevice: Binding<String> {
        Binding(get: { widget.devices.first?.id ?? "" }, set: { id in
            guard id != widget.devices.first?.id else { return }
            widget.devices = devices.first { recordingDeviceID($0) == id }.map { [HomeWidgetDevice($0)] } ?? []
            widget.resource = ""; widget.resourceTitle = ""
            cameras = []; campaigns = []; apps = []; error = nil
        })
    }
    private var selectedResource: Binding<String> {
        Binding(get: { widget.resource }, set: { id in
            widget.resource = id
            widget.resourceTitle = cameras.first { $0.stableID == id }?.name
                ?? campaigns.first { $0.name == id }?.name
                ?? apps.first { $0.appName == id }?.appName ?? id
        })
    }
    private func deviceLabel(_ candidate: Device) -> some View {
        VStack(alignment: .leading, spacing: 3) {
            Text(candidate.title)
            Text(HomeWidgetInventory.status(candidate, model: model) + " · " + (candidate.transport == .cloud ? "Cloud" : "Local"))
                .font(.caption).foregroundStyle(.secondary)
        }
    }
    private func matchingDevices(_ available: [Device]) -> [Device] {
        let query = search.trimmingCharacters(in: .whitespacesAndNewlines)
        return available.filter { query.isEmpty || $0.title.localizedCaseInsensitiveContains(query) }.sorted {
            let leftOffline = HomeWidgetInventory.status($0, model: model) == "Offline"
            let rightOffline = HomeWidgetInventory.status($1, model: model) == "Offline"
            if leftOffline != rightOffline { return !leftOffline }
            return $0.title.localizedStandardCompare($1.title) == .orderedAscending
        }
    }
    private func loadChoices() async {
        guard let device else { return }
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
