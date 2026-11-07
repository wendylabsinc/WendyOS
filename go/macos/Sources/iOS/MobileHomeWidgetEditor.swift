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
        Group {
            if widget.kind == .favorites { favoritesEditor }
            else { standardEditor }
        }.task { await actions.run() }
    }
    private var standardEditor: some View {
        let available = devices
        let matches = matchingDevices(available)
        return VStack(spacing: 0) {
            HStack {
                if let cancel { Button("Back", systemImage: "chevron.left", action: cancel).labelStyle(.iconOnly).frame(minWidth: 44, minHeight: 44) }
                Text(widget.kind.title).font(.title2.bold())
                Spacer()
                Button("Done") {
                    widget.label = widget.label.trimmingCharacters(in: .whitespacesAndNewlines)
                    save(widget); dismiss()
                }.font(.headline).foregroundStyle(.white).padding(.horizontal, 22).frame(minHeight: 44)
                    .background(MobileLensStyle.green, in: .capsule)
                    .disabled(!widget.valid || loading).opacity(widget.valid && !loading ? 1 : 0.45)
            }.padding(20)
            ScrollView {
                VStack(alignment: .leading, spacing: 20) {
                    editorSection("Title") {
                        TextField(widget.displayTitle, text: $widget.title).padding(18).accessibilityLabel("Widget title")
                    }
                    VStack(alignment: .leading, spacing: 8) {
                        favoriteSectionTitle("Size")
                        Picker("Size", selection: $widget.size) {
                            ForEach(widget.kind.sizes, id: \.self) { Text($0.rawValue).tag($0) }
                        }.pickerStyle(.segmented)
                    }
                    if !widget.kind.builtIn {
                        VStack(alignment: .leading, spacing: 10) {
                            favoriteSectionTitle("Device")
                            HStack(spacing: 8) {
                                Image(systemName: "magnifyingglass").foregroundStyle(.secondary)
                                TextField("Search devices by name", text: $search).autocorrectionDisabled().textInputAutocapitalization(.never)
                                if !search.isEmpty {
                                    Button { search = "" } label: { Image(systemName: "xmark.circle.fill").foregroundStyle(.secondary) }.accessibilityLabel("Clear search")
                                }
                            }.padding(14).background(RecordingAppearance.card, in: .rect(cornerRadius: 16))
                            LazyVStack(spacing: 0) {
                                if widget.kind == .gallery {
                                    choiceRow("All devices", selected: widget.devices.isEmpty) { widget.devices = [] }
                                    Divider().padding(.horizontal, 18)
                                }
                                ForEach(matches) { candidate in
                                    choiceRow(candidate.title,
                                        subtitle: HomeWidgetInventory.status(candidate, model: model) + " · " + (candidate.transport == .cloud ? "Cloud" : "Local"),
                                        selected: widget.devices.first?.id == recordingDeviceID(candidate)) {
                                        guard widget.devices.first?.id != recordingDeviceID(candidate) else { return }
                                        widget.devices = [HomeWidgetDevice(candidate)]; widget.resource = ""; widget.resourceTitle = ""
                                        cameras = []; campaigns = []; apps = []; error = nil
                                    }.disabled(loading)
                                    if candidate.id != matches.last?.id { Divider().padding(.horizontal, 18) }
                                }
                                if matches.isEmpty {
                                    Text(available.isEmpty ? "No devices available" : "No matching devices").foregroundStyle(.secondary).padding(18)
                                }
                                ForEach(widget.devices.filter { ref in !available.contains { recordingDeviceID($0) == ref.id } }) { ref in
                                    HStack {
                                        Text(ref.title + " · Unavailable").foregroundStyle(.secondary)
                                        Spacer()
                                        Button("Remove") { widget.devices.removeAll { $0.id == ref.id }; widget.resource = ""; widget.resourceTitle = "" }
                                    }.padding(18)
                                }
                            }.background(RecordingAppearance.card, in: .rect(cornerRadius: 22))
                        }
                    }
                    if widget.kind == .gallery {
                        editorSection("Recordings") {
                            Picker("Kind", selection: $widget.dataKind) {
                                Text("All kinds").tag(nil as String?)
                                ForEach(MobileRecordingKind.allCases, id: \.self) { Text($0 == .unknown ? "Unknown" : $0.rawValue).tag(Optional($0.rawValue)) }
                            }.padding(14)
                            Divider().padding(.horizontal, 18)
                            TextField("Label, e.g. Person (optional)", text: $widget.label).autocorrectionDisabled().padding(18)
                        }
                    } else if !widget.kind.builtIn {
                        editorSection(widget.kind == .camera ? "Camera" : widget.kind == .campaign ? "Campaign" : "App") {
                            if !widget.resource.isEmpty {
                                choiceRow(widget.resourceTitle.isEmpty ? widget.resource : widget.resourceTitle, selected: true) {}
                            }
                            Button(loading ? "Loading…" : (error == nil ? "Load choices" : "Try again")) { actions.submit { await loadChoices() } }
                                .font(.subheadline.weight(.semibold)).frame(maxWidth: .infinity, minHeight: 48)
                                .disabled(device == nil || loading)
                            if loading { ProgressView().padding(12) }
                            ForEach(cameras.filter { $0.stableID != widget.resource }, id: \.orderKey) { camera in resourceRow(camera.stableID, camera.name) }
                            ForEach(campaigns.filter { $0.name != widget.resource }) { campaign in resourceRow(campaign.name, campaign.name) }
                            ForEach(apps.filter { $0.appName != widget.resource }) { app in resourceRow(app.appName, app.appName) }
                            if let error { Text(error).font(.caption).foregroundStyle(.secondary).padding(18) }
                            if device == nil { Text("Choose a device first.").font(.caption).foregroundStyle(.secondary).padding(.bottom, 14) }
                        }
                        if widget.kind == .campaign {
                            Text("Clips finish automatically using the campaign’s settings.").font(.caption).foregroundStyle(.secondary)
                        }
                    }
                    if let remove {
                        Button("Remove widget", role: .destructive) { remove(); dismiss() }
                            .font(.headline).frame(maxWidth: .infinity, minHeight: 56)
                            .background(RecordingAppearance.card, in: .rect(cornerRadius: 20))
                    }
                }.padding(.horizontal, 20).padding(.bottom, 24)
            }.scrollDismissesKeyboard(.interactively)
        }.background(MobileLensStyle.background).tint(MobileLensStyle.green).presentationDragIndicator(.visible)
    }
    private func editorSection<Content: View>(_ title: String, @ViewBuilder content: () -> Content) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            favoriteSectionTitle(title)
            VStack(spacing: 0, content: content).frame(maxWidth: .infinity)
                .background(RecordingAppearance.card, in: .rect(cornerRadius: 22))
        }
    }
    private func choiceRow(_ title: String, subtitle: String? = nil, selected: Bool, action: @escaping () -> Void) -> some View {
        Button(action: action) {
            HStack(spacing: 12) {
                VStack(alignment: .leading, spacing: 2) {
                    Text(title).font(.headline).foregroundStyle(.primary)
                    if let subtitle { Text(subtitle).font(.subheadline).foregroundStyle(.secondary) }
                }.frame(maxWidth: .infinity, alignment: .leading)
                ZStack {
                    RoundedRectangle(cornerRadius: 9).fill(selected ? MobileLensStyle.green : .clear)
                    if selected { Image(systemName: "checkmark").font(.subheadline.bold()).foregroundStyle(.white) }
                    else { RoundedRectangle(cornerRadius: 9).strokeBorder(.secondary.opacity(0.35), lineWidth: 1.5) }
                }.frame(width: 28, height: 28)
            }.padding(.horizontal, 18).padding(.vertical, 12).contentShape(.rect)
        }.buttonStyle(.plain).accessibilityAddTraits(selected ? .isSelected : [])
    }
    private func resourceRow(_ id: String, _ title: String) -> some View {
        choiceRow(title, selected: widget.resource == id) { widget.resource = id; widget.resourceTitle = title }
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
    private var favoritesEditor: some View {
        let available = devices
        let matches = matchingDevices(available)
        return VStack(spacing: 0) {
            HStack {
                if let cancel { Button("Back", systemImage: "chevron.left", action: cancel).labelStyle(.iconOnly).frame(minWidth: 44, minHeight: 44) }
                Text("Favorite devices").font(.title2.bold())
                Spacer()
                Button("Done") { save(widget); dismiss() }
                    .font(.headline).foregroundStyle(.white).padding(.horizontal, 22).frame(minHeight: 44)
                    .background(MobileLensStyle.green, in: .capsule).disabled(!widget.valid)
                    .opacity(widget.valid ? 1 : 0.45)
            }.padding(20)
            ScrollView {
                VStack(alignment: .leading, spacing: 20) {
                    VStack(alignment: .leading, spacing: 8) {
                        favoriteSectionTitle("Title")
                        TextField("Favorite devices", text: $widget.title).padding(18)
                            .background(RecordingAppearance.card, in: .rect(cornerRadius: 20))
                            .accessibilityLabel("Widget title")
                    }
                    VStack(alignment: .leading, spacing: 8) {
                        favoriteSectionTitle("Size")
                        Picker("Size", selection: $widget.size) {
                            ForEach(widget.kind.sizes, id: \.self) { Text($0.rawValue).tag($0) }
                        }.pickerStyle(.segmented)
                    }
                    VStack(alignment: .leading, spacing: 10) {
                        favoriteSectionTitle("Devices · pick up to 6")
                        HStack(spacing: 8) {
                            Image(systemName: "magnifyingglass").foregroundStyle(.secondary)
                            TextField("Search devices by name", text: $search).autocorrectionDisabled()
                                .textInputAutocapitalization(.never).accessibilityIdentifier("home.favorites.search")
                            if !search.isEmpty {
                                Button { search = "" } label: { Image(systemName: "xmark.circle.fill").foregroundStyle(.secondary) }
                                    .accessibilityLabel("Clear search")
                            }
                        }.padding(14).background(RecordingAppearance.card, in: .rect(cornerRadius: 16))
                        LazyVStack(spacing: 0) {
                            ForEach(matches) { device in
                                let id = recordingDeviceID(device)
                                let selected = widget.devices.contains { $0.id == id }
                                Button {
                                    if selected { widget.devices.removeAll { $0.id == id } }
                                    else if widget.devices.count < 6 { widget.devices.append(HomeWidgetDevice(device)) }
                                } label: {
                                    HStack(spacing: 12) {
                                        VStack(alignment: .leading, spacing: 2) {
                                            Text(device.title).font(.headline).foregroundStyle(.primary)
                                            Text(HomeWidgetInventory.status(device, model: model) + " · " + (device.transport == .cloud ? "Cloud" : "Local"))
                                                .font(.subheadline).foregroundStyle(.secondary)
                                        }.frame(maxWidth: .infinity, alignment: .leading)
                                        Image(systemName: "checkmark").opacity(selected ? 1 : 0)
                                            .font(.subheadline.bold()).foregroundStyle(.white).frame(width: 28, height: 28)
                                            .background(selected ? MobileLensStyle.green : .clear, in: .rect(cornerRadius: 9))
                                            .overlay { if !selected { RoundedRectangle(cornerRadius: 9).strokeBorder(.secondary.opacity(0.35), lineWidth: 1.5) } }
                                    }.padding(.horizontal, 18).padding(.vertical, 12).contentShape(.rect)
                                }.buttonStyle(.plain).disabled(!selected && widget.devices.count >= 6)
                                    .accessibilityAddTraits(selected ? .isSelected : [])
                                if device.id != matches.last?.id { Divider().padding(.horizontal, 18) }
                            }
                            if matches.isEmpty {
                                Text(available.isEmpty ? "No devices available" : "No matching devices")
                                    .foregroundStyle(.secondary).frame(maxWidth: .infinity).padding(20)
                            }
                            ForEach(widget.devices.filter { ref in !available.contains { recordingDeviceID($0) == ref.id } }) { ref in
                                HStack {
                                    Text(ref.title + " · Unavailable").font(.subheadline).foregroundStyle(.secondary)
                                    Spacer()
                                    Button("Remove") { widget.devices.removeAll { $0.id == ref.id } }
                                }.padding(18)
                            }
                        }.background(RecordingAppearance.card, in: .rect(cornerRadius: 22))
                        if widget.devices.count >= 6 { Text("6 devices selected. Deselect one to choose another.").font(.caption).foregroundStyle(.secondary) }
                    }
                    if let remove {
                        Button("Remove widget", role: .destructive) { remove(); dismiss() }
                            .font(.headline).frame(maxWidth: .infinity, minHeight: 56)
                            .background(RecordingAppearance.card, in: .rect(cornerRadius: 20))
                    }
                }.padding(.horizontal, 20).padding(.bottom, 24)
            }.scrollDismissesKeyboard(.interactively)
        }.background(MobileLensStyle.background).tint(MobileLensStyle.green)
            .presentationDragIndicator(.visible)
    }
    private func favoriteSectionTitle(_ title: String) -> some View {
        Text(title.uppercased()).font(.subheadline.weight(.semibold)).foregroundStyle(.secondary).padding(.leading, 8)
    }
    private func option(_ id: String, _ name: String) -> some View {
        Button { widget.resource = id; widget.resourceTitle = name } label: {
            HStack { Text(name); Spacer(); if widget.resource == id { Image(systemName: "checkmark") } }
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
