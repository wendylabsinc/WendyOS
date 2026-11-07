import SwiftUI
import UniformTypeIdentifiers

struct HomeWidgetDeck: View {
    @ObservedObject var model: AppModel
    @ObservedObject var store: HomeWidgetStore
    @Binding var editing: Bool
    let add: () -> Void
    let configure: (HomeWidget) -> Void
    let setup: () -> Void
    let recordings: () -> Void
    let notifications: () -> Void
    let devices: () -> Void
    let openDevice: (Device) -> Void
    @EnvironmentObject private var catalog: RecordingCatalog
    @Environment(\.scenePhase) private var phase
    @State private var active: HomeWidget?
    @State private var activeCamera: UUID?
    @State private var connectingCamera: UUID?
    @State private var cameraProblem: (UUID, String)?
    @State private var actions = AsyncActions()
    @Environment(\.recordingPosters) private var posters
    @State private var expandedCamera: HomeWidget?
    @State private var refresh = 0
    @State private var dragged: UUID?
    @State private var selectedEpisode: String?
    @State private var galleryDevice: String?
    @Environment(\.dynamicTypeSize) private var textSize
    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    private var targets: [MobileRecordingTarget] {
        let wanted = Set(store.items.filter { $0.kind == .gallery }.flatMap { $0.devices.map(\.id) })
        let all = store.items.contains { $0.kind == .gallery && $0.devices.isEmpty }
        return RecordingSources(model: model).targets.filter { all || wanted.contains($0.id) }
    }
    private struct LoadKey: Hashable { let targets: [MobileRecordingTarget]; let inventory: [MobileRecordingTarget]; let active: Bool; let refresh: Int }
    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            if editing {
              HStack {
                Spacer()
                    Button("Add widget", systemImage: "plus", action: add).labelStyle(.iconOnly).frame(minWidth: 44, minHeight: 44)
                    Button("Done") { editing = false; dragged = nil }.font(.subheadline.bold())
              }.tint(MobileLensStyle.green)
            }
            if let error = store.error { Text(error).font(.footnote).foregroundStyle(.secondary) }
            if store.items.isEmpty {
                Button { beginEditing(); add() } label: {
                    VStack(spacing: 10) {
                        Image(systemName: "square.grid.2x2").font(.title2)
                            .foregroundStyle(MobileLensStyle.green)
                            .frame(width: 52, height: 52)
                            .background(MobileLensStyle.green.opacity(0.12), in: .rect(cornerRadius: 16))
                        Text("Add widgets").font(.headline).foregroundStyle(MobileLensStyle.green)
                        Text("Add widgets for the devices, cameras and data you check most.").font(.subheadline).foregroundStyle(.secondary)
                    }.multilineTextAlignment(.center).frame(maxWidth: .infinity).padding(24)
                        .contentShape(.rect(cornerRadius: 22))
                        .overlay { RoundedRectangle(cornerRadius: 22).strokeBorder(.secondary.opacity(0.3), style: StrokeStyle(lineWidth: 1.5, dash: [6, 5])) }
                }.buttonStyle(.plain)
            }
            HomeWidgetGrid(sizes: store.items.map(\.size), accessible: textSize.isAccessibilitySize) {
                ForEach(store.items) { widget in
                    widgetCard(widget)
                }
            }.animation(reduceMotion ? nil : .snappy(duration: 0.22), value: store.items)
            if editing && !store.items.isEmpty {
                Text("Drag to reorder · tap to configure · tap S/M/L to resize")
                    .font(.caption).foregroundStyle(.secondary)
            }
        }.task { await actions.run() }
        .onChange(of: editing) { _, value in if value { activeCamera = nil } }
        .onDisappear { activeCamera = nil }
        .task(id: LoadKey(targets: targets, inventory: RecordingSources(model: model).inventory, active: phase == .active, refresh: refresh)) {
            if phase == .active { await catalog.load(targets, refresh: refresh, priority: nil, consumer: "home.widgets", retaining: RecordingSources(model: model).inventory) }
        }
        .fullScreenCover(item: $expandedCamera) { widget in
            HomeWidgetSession(widget: widget, model: model)
        }
        .sheet(item: $active) { widget in
            if widget.kind == .gallery, let id = galleryDevice ?? widget.devices.first?.id,
               let library = catalog.reads[id]?.library {
                NavigationStack {
                    RecordingEpisodePicker(library: library, trigger: widget.label.isEmpty ? nil : widget.label,
                                           kind: widget.dataKind.flatMap(MobileRecordingKind.init(rawValue:)), selectedID: selectedEpisode,
                                           issue: catalog.detailProblems[id], retry: { refresh += 1 })
                }.presentationDetents([.large])
            } else { HomeWidgetSession(widget: widget, model: model) }
        }
        .environment(\.recordingPosters, phase == .active ? posters : nil)
    }
    private func widgetCard(_ widget: HomeWidget) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            widgetHeader(widget)
            Group {
                if widget.kind == .favorites { favorites(widget) }
                else if widget.kind == .gallery { gallery(widget) }
                else if widget.kind.builtIn { builtIn(widget) }
                else { shortcut(widget) }
            }.frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
                .allowsHitTesting(!editing)
        }.padding(12).frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
            .background(RecordingAppearance.card, in: .rect(cornerRadius: 22))
            .clipShape(.rect(cornerRadius: 22))
            .overlay { if editing {
                RoundedRectangle(cornerRadius: 22).strokeBorder(Color.secondary.opacity(0.3), style: StrokeStyle(lineWidth: 1.5, dash: [5, 4]))
                    .contentShape(.rect).onTapGesture { configure(widget) }
            } }
            .overlay(alignment: .topLeading) { if editing {
                Button { store.remove(widget.id) } label: {
                    Image(systemName: "minus").font(.caption.bold()).foregroundStyle(.white).frame(width: 28, height: 28).background(Color(white: 0.35), in: .circle)
                }.accessibilityLabel("Remove " + widget.displayTitle).offset(x: 7, y: 7)
            } }
            .overlay(alignment: .topTrailing) { if editing {
                Button { store.resize(widget.id) } label: {
                    HStack(spacing: 4) {
                        Text(widget.size.short)
                        Image(systemName: "chevron.up.chevron.down").font(.system(size: 8, weight: .bold))
                    }.font(.caption.bold()).foregroundStyle(.white).frame(width: 44, height: 28).background(RecordingAppearance.ink, in: .capsule)
                }.accessibilityLabel("Resize " + widget.displayTitle).accessibilityValue(widget.size.rawValue).offset(x: -7, y: 7)
            } }
            .scaleEffect(editing ? (dragged == widget.id ? 1.03 : 0.97) : 1)
            .opacity(dragged == widget.id ? 0.55 : 1)
            .onLongPressGesture(minimumDuration: 0.55) { beginEditing() }
            .onDrag { beginEditing(); dragged = widget.id; return NSItemProvider(object: widget.id.uuidString as NSString) }
            .onDrop(of: [UTType.text], delegate: HomeWidgetDrop(target: widget.id, store: store, dragged: $dragged))
            .accessibilityAction(named: "Configure") { configure(widget) }
            .accessibilityAction(named: "Resize") { store.resize(widget.id) }
            .accessibilityAction(named: "Move earlier") { move(widget, offset: -1) }
            .accessibilityAction(named: "Move later") { move(widget, offset: 1) }
            .accessibilityElement(children: .contain)
            .accessibilityIdentifier("home.widget." + widget.id.uuidString)
    }
    @ViewBuilder private func widgetHeader(_ widget: HomeWidget) -> some View {
        if editing {
            headerLabel(widget)
                .padding(.leading, 26).padding(.trailing, 44)
        } else {
            Menu {
                Button("Edit widget", systemImage: "slider.horizontal.3") {
                    activeCamera = nil
                    configure(widget)
                }
                Button("Remove widget", systemImage: "trash", role: .destructive) {
                    if activeCamera == widget.id { activeCamera = nil }
                    store.remove(widget.id)
                }
            } label: {
                headerLabel(widget)
            }.menuStyle(.borderlessButton).buttonStyle(.plain).tint(HomeWidgetKind.headerColor)
                .highPriorityGesture(LongPressGesture(minimumDuration: 0.55).onEnded { _ in beginEditing() })
                .accessibilityLabel(widget.displayTitle + " widget options")
                .accessibilityHint("Edit or remove this widget")
                .accessibilityIdentifier("home.widget.options." + widget.id.uuidString)
        }
    }
    private func headerLabel(_ widget: HomeWidget) -> some View {
        HStack(spacing: 8) {
            Image(systemName: widget.kind.symbol)
                .font(.system(size: 14, weight: .regular)).foregroundStyle(widget.kind.color)
            Text(widget.displayTitle).font(.system(size: 12, weight: .semibold))
                .foregroundStyle(HomeWidgetKind.headerColor)
                .lineLimit(textSize.isAccessibilitySize ? 2 : 1)
        }
            .frame(maxWidth: .infinity, minHeight: 24, alignment: .leading)
            .contentShape(.rect)
    }
    private func move(_ widget: HomeWidget, offset: Int) {
        guard let index = store.items.firstIndex(where: { $0.id == widget.id }), store.items.indices.contains(index + offset) else { return }
        store.move(widget.id, before: store.items[index + offset].id)
    }
    @ViewBuilder private func favorites(_ widget: HomeWidget) -> some View {
        if widget.size == .small && !textSize.isAccessibilitySize {
            LazyVGrid(columns: [GridItem(.flexible()), GridItem(.flexible())], spacing: 5) {
                ForEach(widget.devices.prefix(4)) { reference in
                    Button { if let device = HomeWidgetInventory.resolve(reference, model: model) { openDevice(device) } } label: {
                        VStack(spacing: 2) {
                            if let device = HomeWidgetInventory.resolve(reference, model: model) {
                                let status = HomeWidgetInventory.status(device, model: model)
                                MobileDeviceAvatar(device: device, size: 32, initialsFallback: true).saturation(status == "Offline" ? 0 : 1)
                                    .overlay(alignment: .bottomTrailing) { Circle().fill(status == "Online" || status == "Connected" || status == "Nearby" ? .green : .gray).frame(width: 7, height: 7) }
                            } else { Image(systemName: "wifi.slash").frame(width: 32, height: 32) }
                            Text(reference.title).font(.system(size: 10)).lineLimit(1)
                        }.frame(maxWidth: .infinity).frame(minHeight: 44)
                    }.buttonStyle(.plain).accessibilityLabel(reference.title)
                }
            }
        } else {
            VStack(spacing: 4) {
                ForEach(widget.devices.prefix(widget.size == .large ? 6 : 3)) { reference in
                    if let device = HomeWidgetInventory.resolve(reference, model: model) {
                        Button { openDevice(device) } label: {
                            HStack(spacing: 10) {
                                let status = HomeWidgetInventory.status(device, model: model)
                                MobileDeviceAvatar(device: device, size: 32, initialsFallback: true).saturation(status == "Offline" ? 0 : 1)
                                    .overlay(alignment: .bottomTrailing) {
                                        Circle().fill(status == "Online" || status == "Connected" || status == "Nearby" ? Color.green : Color.gray)
                                            .frame(width: 9, height: 9).overlay { Circle().strokeBorder(RecordingAppearance.card, lineWidth: 1.5) }
                                    }
                                VStack(alignment: .leading, spacing: 1) {
                                    Text(device.title).font(.subheadline.weight(.semibold)).lineLimit(1)
                                    Text(HomeWidgetInventory.status(device, model: model) + " · " + (device.transport == .cloud ? "Cloud" : "Local")).font(.caption2).foregroundStyle(.secondary)
                                }
                                Spacer(minLength: 0)
                                Image(systemName: "chevron.right").font(.caption2).foregroundStyle(.secondary)
                            }
                        }.buttonStyle(.plain)
                    } else { Text(reference.title + " · Unavailable").font(.caption).foregroundStyle(.secondary) }
                }
            }
        }
    }
    @ViewBuilder private func shortcut(_ widget: HomeWidget) -> some View {
        let device = HomeWidgetInventory.resolve(widget.devices.first, model: model)
        let offline = device.map { HomeWidgetInventory.status($0, model: model) == "Offline" } ?? true
        VStack(alignment: .leading, spacing: 6) {
            if widget.kind == .camera {
                Group {
                if activeCamera == widget.id, model.selectedDeviceReady, model.connected?.id == device?.id,
                   let feed = model.cameraFeeds.first(where: { $0.camera.stableID == widget.resource }) {
                    HomeWidgetCamera(feed: feed, model: model, compact: true)
                        .onScrollVisibilityChange(threshold: 0.05) { if !$0 { activeCamera = nil } }
                } else {
                    Button { actions.submit { await openCamera(widget) } } label: {
                        ZStack {
                            RecordingAppearance.ink
                            Image(systemName: offline ? "video.slash" : "play.circle.fill").font(.title).foregroundStyle(.white.opacity(0.85))
                            VStack { Spacer(); Text(widget.resourceTitle).font(.caption2).lineLimit(1).foregroundStyle(.white).padding(6) }
                        }.frame(maxWidth: .infinity, maxHeight: .infinity).clipShape(.rect(cornerRadius: 12))
                    }.buttonStyle(.plain).disabled(offline || connectingCamera != nil).accessibilityLabel("Watch " + widget.resourceTitle)
                }
                }.overlay(alignment: .bottomTrailing) {
                    Button {
                        activeCamera = nil
                        expandedCamera = widget
                    } label: {
                        Image(systemName: "arrow.up.left.and.arrow.down.right")
                            .font(.system(size: 12, weight: .semibold))
                            .frame(width: 30, height: 30)
                            .background(.black.opacity(0.6), in: .circle)
                            .frame(width: 44, height: 44)
                    }.buttonStyle(.plain).foregroundStyle(.white)
                        .disabled(offline || connectingCamera != nil)
                        .accessibilityLabel("Open camera full screen")
                }
            } else {
                Text(widget.resourceTitle).font(.subheadline.weight(.semibold)).lineLimit(2)
                Text(device?.title ?? widget.devices.first?.title ?? "Device unavailable").font(.caption).foregroundStyle(.secondary).lineLimit(1)
                if widget.kind == .portal, model.selectedDeviceReady, model.connected?.id == device?.id,
                   let app = model.apps.first(where: { $0.appName == widget.resource }), app.hasWebpage {
                    Text(":\(app.httpPort) · \(app.state)").font(.caption2).foregroundStyle(.secondary).lineLimit(1)
                }
                if widget.size == .large {
                    Image(systemName: widget.kind.symbol).font(.system(size: 36)).foregroundStyle(widget.kind.color.opacity(0.6)).frame(maxWidth: .infinity, maxHeight: .infinity)
                } else { Spacer(minLength: 0) }
            }
            if widget.kind != .camera || (widget.size != .small && activeCamera != widget.id) {
                Button(offline ? "Offline" : widget.kind == .camera ? "Watch camera" : widget.kind == .campaign ? (widget.size == .small ? "Record" : "Record a clip") : (widget.size == .small ? "Open" : "Open web portal")) {
                    if widget.kind == .camera { actions.submit { await openCamera(widget) } }
                    else { activeCamera = nil; active = widget }
                }.font(.caption.weight(.semibold)).frame(maxWidth: .infinity, minHeight: 32)
                    .background(widget.kind == .campaign ? Color.red.opacity(0.12) : MobileLensStyle.green, in: .capsule)
                    .foregroundStyle(widget.kind == .campaign ? Color.red : .white)
                    .disabled(offline || connectingCamera != nil)
            }
            if connectingCamera == widget.id { ProgressView().controlSize(.mini) }
            if cameraProblem?.0 == widget.id { Text(cameraProblem?.1 ?? "").font(.caption2).foregroundStyle(.secondary).lineLimit(2) }
        }
    }
    @ViewBuilder private func builtIn(_ widget: HomeWidget) -> some View {
        switch widget.kind {
        case .attention:
            let issues = [model.nearbyError, model.cloudError, model.detailError].compactMap { $0 }
            VStack(alignment: .leading, spacing: 8) {
                if issues.isEmpty {
                    Image(systemName: "checkmark.circle").font(.title).foregroundStyle(MobileLensStyle.green)
                    Text("No issues reported").font(.subheadline)
                } else {
                    if widget.size == .small { Text("\(issues.count)").font(.largeTitle.bold()).foregroundStyle(.orange) }
                    ForEach(Array(issues.prefix(widget.size == .small ? 1 : widget.size == .large ? 3 : 2).enumerated()), id: \.offset) { _, issue in
                        Button(action: devices) {
                            HStack { Text(PresentedFailure(issue).message).font(.caption).lineLimit(2); Spacer(minLength: 0); Image(systemName: "chevron.right").font(.caption2) }
                        }.buttonStyle(.plain)
                    }
                }
            }
        case .activity:
            VStack(alignment: .leading, spacing: 8) {
                ForEach(Array(model.experienceEvents.prefix(widget.size == .small ? 1 : 2).enumerated()), id: \.offset) { _, event in
                    Button(action: notifications) { VStack(alignment: .leading, spacing: 3) {
                        Text(event.title).font(.subheadline.weight(.semibold)).lineLimit(2)
                        Text(event.deviceName).font(.caption).foregroundStyle(.secondary).lineLimit(1)
                    } }.buttonStyle(.plain)
                }
                if model.experienceEvents.isEmpty { Text("No recent activity").font(.subheadline).foregroundStyle(.secondary) }
            }
        case .actions:
            let columns = Array(repeating: GridItem(.flexible(), spacing: 6), count: widget.size == .small ? 2 : 4)
            LazyVGrid(columns: columns, spacing: 6) {
                quick("Set up", "plus", setup)
                quick("Devices", "cpu", devices)
                quick("Activity", "clock", notifications)
                quick("Data", "cylinder.split.1x2", recordings)
            }
        default: EmptyView()
        }
    }
    private func quick(_ title: String, _ icon: String, _ action: @escaping () -> Void) -> some View {
        Button(action: action) { VStack(spacing: 5) {
            Image(systemName: icon).font(.body)
            Text(title).font(.system(size: 10, weight: .medium)).lineLimit(1)
        }.frame(maxWidth: .infinity, minHeight: 46).padding(.vertical, 3)
            .background(MobileLensStyle.green.opacity(0.08), in: .rect(cornerRadius: 12))
        }.buttonStyle(.plain).foregroundStyle(MobileLensStyle.green)
    }
    private func beginEditing() { activeCamera = nil; editing = true }
    private func openCamera(_ widget: HomeWidget) async {
        guard let device = HomeWidgetInventory.resolve(widget.devices.first, model: model), connectingCamera == nil else { return }
        activeCamera = nil; cameraProblem = nil; connectingCamera = widget.id
        defer { connectingCamera = nil }
        let scope = model.mobileCloudSelectionKey
        await model.connect(device)
        guard !Task.isCancelled, scope == model.mobileCloudSelectionKey, model.mobileScopeIsCurrent else { return }
        guard model.selectedDeviceReady, model.connected?.id == device.id else {
            cameraProblem = (widget.id, model.detailError ?? "Could not connect to the device."); return
        }
        guard model.cameraFeeds.contains(where: { $0.camera.stableID == widget.resource }) else {
            cameraProblem = (widget.id, model.camerasError ?? "This camera is unavailable. Edit the widget to choose another."); return
        }
        activeCamera = widget.id
    }
    @ViewBuilder private func gallery(_ widget: HomeWidget) -> some View {
        let libraries = catalog.reads.values.compactMap(\.library).filter { library in
            widget.devices.isEmpty || widget.devices.contains { $0.id == recordingDeviceID(library.device) }
        }
        let entries = libraries.flatMap { library in
            library.episodes(matching: widget.label.isEmpty ? nil : widget.label,
                             kind: widget.dataKind.flatMap(MobileRecordingKind.init(rawValue:))).map { (library.device, $0) }
        }.sorted { ($0.1.started ?? .distantPast) > ($1.1.started ?? .distantPast) }
        VStack(alignment: .leading, spacing: 6) {
            if entries.isEmpty {
                if catalog.loading && libraries.isEmpty {
                    MobileLoadingPlaceholder(label: "Loading recordings…", style: .recordings, rows: 1)
                } else { Text("No matching recordings").font(.caption).foregroundStyle(.secondary) }
                Spacer(minLength: 0)
            } else {
                LazyVGrid(columns: Array(repeating: GridItem(.flexible(), spacing: 6), count: widget.size == .small ? 1 : 3), spacing: 6) {
                    ForEach(Array(entries.prefix(widget.size == .small ? 1 : widget.size == .large ? 6 : 3).enumerated()), id: \.offset) { _, entry in
                        Button {
                            galleryDevice = recordingDeviceID(entry.0); selectedEpisode = entry.1.id; activeCamera = nil; active = widget
                        } label: {
                            ZStack(alignment: .bottomLeading) {
                                RecordingThumbnail(device: entry.0, episode: entry.1)
                                LinearGradient(colors: [.clear, .black.opacity(0.75)], startPoint: .center, endPoint: .bottom)
                                VStack(alignment: .leading, spacing: 1) {
                                    Text(entry.1.title).font(.system(size: 10, weight: .semibold)).lineLimit(1)
                                    Text(entry.1.kind.rawValue).font(.system(size: 9)).lineLimit(1)
                                }.foregroundStyle(.white).padding(6)
                            }.frame(height: widget.size == .large ? 112 : 82).clipShape(.rect(cornerRadius: 12))
                        }.buttonStyle(.plain).accessibilityLabel("Open " + entry.1.title)
                    }
                }
            }
            HStack {
                if widget.size != .small { Text("\(entries.count) recordings").font(.caption2).foregroundStyle(.secondary).lineLimit(1) }
                Spacer(minLength: 0)
                Button("Open gallery") {
                    selectedEpisode = nil; galleryDevice = widget.devices.first?.id; activeCamera = nil
                    if widget.devices.isEmpty { recordings() } else { active = widget }
                }.font(.caption2.weight(.semibold)).foregroundStyle(MobileLensStyle.green)
            }
            if libraries.contains(where: { $0.labelCoverage != nil }) { Text("Some details unchecked").font(.system(size: 9)).foregroundStyle(.secondary) }
            if let problem = targets.compactMap({ target -> String? in
                guard widget.devices.isEmpty || widget.devices.contains(where: { $0.id == target.id }) else { return nil }
                return catalog.reads[target.id]?.problem ?? catalog.detailProblems[target.id]
            }).first {
                Button { refresh += 1 } label: {
                    Label(PresentedFailure(problem).message, systemImage: "arrow.clockwise").font(.system(size: 9)).lineLimit(1)
                }.accessibilityLabel("Retry loading recordings. " + PresentedFailure(problem).message)
            }
        }
    }

}

// One explicitly opened widget owns the selected connection. Home never cycles
// connections or starts camera streams merely because cards are visible.
private struct HomeWidgetSession: View {
    let widget: HomeWidget
    @ObservedObject var model: AppModel
    @Environment(\.dismiss) private var dismiss
    @Environment(\.scenePhase) private var phase
    @State private var session: UInt64 = 0
    @State private var busy = true
    @State private var error: String?
    @State private var webpage: URL?
    @State private var campaigns: [DeviceCampaign] = []
    @State private var episodes: [DataVisualEpisode] = []
    @State private var checked: Date?
    @State private var confirmRecording = false
    @State private var actions = AsyncActions()
    private var device: Device? { HomeWidgetInventory.resolve(widget.devices.first, model: model) }
    private var ready: Bool { session != 0 && session == model.deviceSession && model.mobileScopeIsCurrent && model.selectedDeviceReady && model.connected?.id == device?.id }
    private var campaign: DeviceCampaign? { campaigns.first { $0.name == widget.resource } }
    var body: some View {
        NavigationStack {
            Group {
                if let webpage, ready { BrowserSheet(url: webpage) }
                else if busy && (session == 0 || widget.kind == .portal) { ProgressView(session == 0 ? "Connecting…" : "Opening portal…") }
                else if ready {
                    if widget.kind == .camera {
                        if let feed = model.cameraFeeds.first(where: { $0.camera.stableID == widget.resource }) {
                            HomeWidgetCamera(feed: feed, model: model)
                        } else { ContentUnavailableView("Camera unavailable", systemImage: "video.slash", description: Text("This camera was not reported by the device. Edit the widget to choose another camera.")) }
                    } else if widget.kind == .campaign {
                        Form {
                            Section {
                                Text(widget.resourceTitle).font(.headline)
                                Text(device?.title ?? "")
                                if let campaign {
                                    LabeledContent("Campaign", value: campaign.state)
                                    if !campaign.problem.isEmpty { Text(campaign.problem).foregroundStyle(.secondary) }
                                }
                                ForEach(episodes) { episode in LabeledContent(episode.title, value: episode.state) }
                                if episodes.isEmpty { Text("No active clips reported.").foregroundStyle(.secondary) }
                                if let checked { Text("Checked \(checked.formatted(date: .omitted, time: .standard))").font(.caption).foregroundStyle(.secondary) }
                            }
                            Section {
                                Button("Record clip", systemImage: "record.circle") { confirmRecording = true }
                                    .disabled(busy || campaign == nil || !episodes.isEmpty || phase != .active)
                                Button("Refresh status", systemImage: "arrow.clockwise") { actions.submit { await refreshCampaign() } }.disabled(busy)
                            } footer: { Text("Clips stop automatically. Notifications and upload follow the campaign’s settings.") }
                            if let error { Section { Text(error).foregroundStyle(.secondary) } }
                        }
                    } else if let error { FailureView(title: "Could not open widget", error: error) { actions.submit { await connect() } } }
                } else {
                    FailureView(title: "Widget unavailable", error: error ?? "The selected device is no longer connected.") { actions.submit { await connect() } }
                }
            }.frame(maxWidth: .infinity, maxHeight: .infinity)
                .navigationTitle(widget.displayTitle).navigationBarTitleDisplayMode(.inline)
                .toolbar { ToolbarItem(placement: .confirmationAction) { Button("Done") { dismiss() } } }
        }.task { await actions.run() }
            .task { await connect() }
            .task(id: "\(ready)/\(phase == .active)") {
                guard ready, widget.kind == .campaign, phase == .active else { return }
                while !Task.isCancelled {
                    do { try await Task.sleep(for: .seconds(5)) } catch { return }
                    if !busy { await refreshCampaign() }
                }
            }
            .confirmationDialog("Record a clip on \(device?.title ?? "this device")?", isPresented: $confirmRecording, titleVisibility: .visible) {
                Button("Record clip") { actions.submit { await triggerCampaign() } }
            } message: { Text("Run \(widget.resourceTitle) once with its configured sources, duration, notifications, and upload policy.") }
    }
    private func connect() async {
        guard let device else { busy = false; error = "Device unavailable in this account and organization."; return }
        busy = true; error = nil
        let scope = model.mobileCloudSelectionKey
        await model.connect(device)
        guard !Task.isCancelled, model.mobileScopeIsCurrent, scope == model.mobileCloudSelectionKey,
              model.selectedDeviceReady, model.connected?.id == device.id else {
            busy = false; error = model.detailError ?? "Could not connect to the device."; return
        }
        session = model.deviceSession
        busy = false
        if widget.kind == .campaign { await refreshCampaign() }
        if widget.kind == .portal {
            busy = true; defer { busy = false }
            guard let app = model.apps.first(where: { $0.appName == widget.resource }), app.hasWebpage else {
                error = model.appsError ?? "This app no longer publishes a web endpoint."; return
            }
            do {
                let url = try await GoClient.shared.openApp(app.appName, session: session)
                guard ready, !Task.isCancelled else { return }
                webpage = url
            } catch { self.error = PresentedFailure(error.localizedDescription).details }
        }
    }
    private func refreshCampaign() async {
        guard ready, phase == .active, !busy else { return }
        busy = true; defer { busy = false }
        do {
            let rows = try await GoClient.shared.campaigns(session: session)
            let active = try await GoClient.shared.homeCampaign(session: session, name: widget.resource)
            guard ready, !Task.isCancelled else { return }
            campaigns = rows; episodes = active; checked = Date(); error = nil
            if campaign == nil { error = "This campaign is no longer installed. Edit the widget to choose another." }
        } catch { if ready, !Task.isCancelled { self.error = PresentedFailure(error.localizedDescription).details } }
    }
    private func triggerCampaign() async {
        guard ready, phase == .active, !busy, campaign != nil else { return }
        busy = true; error = nil
        do {
            let rows = try await GoClient.shared.homeCampaign(session: session, name: widget.resource, trigger: true)
            guard ready, !Task.isCancelled else { busy = false; return }
            episodes = rows; checked = Date()
        } catch { if ready, !Task.isCancelled { self.error = PresentedFailure(error.localizedDescription).details } }
        busy = false
    }
}

private struct HomeWidgetCamera: View {
    @ObservedObject var feed: CameraFeed
    @ObservedObject var model: AppModel
    @Environment(\.scenePhase) private var phase
    var compact = false
    @State private var playing = true
    @State private var stream: UInt64 = 0
    @State private var retry = 0
    var body: some View {
        Group {
            if compact {
                ZStack {
                    Color.black
                    CameraFeedDisplay(feed: feed)
                    if !feed.receivedVideo { ProgressView().tint(.white) }
                    VStack {
                        HStack {
                            TimelineView(.periodic(from: .now, by: 1)) { context in
                                let fresh = feed.lastVideoReceivedAt.map { context.date.timeIntervalSince($0) < 5 } ?? false
                                Text(feed.error != nil ? "Unavailable" : feed.running ? (fresh ? "LIVE" : "Waiting") : "Paused")
                                    .font(.system(size: 9, weight: .bold)).padding(4).background(fresh && feed.running ? Color.red : .gray, in: .capsule)
                            }
                            Spacer()
                            Button(playing ? "Stop" : "Play") { playing.toggle() }
                                .font(.caption2.bold()).padding(6).background(.black.opacity(0.5), in: .capsule)
                        }
                        Spacer()
                        if let date = feed.lastVideoReceivedAt { Text(date.formatted(date: .omitted, time: .standard)).font(.system(size: 9)).padding(3).background(.black.opacity(0.5), in: .capsule) }
                        if let error = feed.error { Text(PresentedFailure(error).message).font(.caption2).lineLimit(2) }
                    }.foregroundStyle(.white).padding(6)
                }.clipShape(.rect(cornerRadius: 12))
            } else {
                VStack(spacing: 12) {
                    ZStack { Color.black; CameraFeedDisplay(feed: feed) }.aspectRatio(4 / 3, contentMode: .fit).clipShape(.rect(cornerRadius: 22))
                    Text(feed.camera.name).font(.headline)
                    Text(feed.error ?? (feed.running ? (feed.receivedVideo ? "Live" : "Waiting for video…") : "Paused")).font(.subheadline).foregroundStyle(.secondary)
                    TimelineView(.periodic(from: .now, by: 1)) { context in
                        if let date = feed.lastVideoReceivedAt {
                            Text("Received \(date.formatted(date: .omitted, time: .standard))\(context.date.timeIntervalSince(date) > 5 ? " · Stale" : "")").font(.caption).foregroundStyle(.secondary)
                        }
                    }
                    Button(feed.error != nil ? "Retry" : playing ? "Pause" : "Play", systemImage: feed.error != nil ? "arrow.clockwise" : playing ? "pause.fill" : "play.fill") {
                        if feed.error != nil { playing = true; retry += 1 } else { playing.toggle() }
                    }.mobileActionStyle(primary: true)
                }.padding(16)
            }
        }
            .task(id: "\(playing)/\(phase == .active)/\(retry)") {
                if playing && phase == .active {
                    let token = await model.playOwnedCamera(feed, inline: true)
                    if Task.isCancelled {
                        if token != 0 && feed.stream == token { model.pauseCamera(feed) }
                    } else { stream = token }
                }
                else { pause() }
            }.onDisappear { pause() }
    }
    private func pause() { if stream != 0 && feed.stream == stream { model.pauseCamera(feed) }; stream = 0 }
}


extension GoClient {
    func homeCampaign(session: UInt64, name: String, trigger: Bool = false) async throws -> [DataVisualEpisode] {
        try await perform {
            try Self.withCString(name) { name in
                guard let p = WDHomeCampaign(session, name, trigger ? 1 : 0) else { throw WendyError(message: "Missing campaign status.") }
                defer { WDVisualEpisodesFree(p) }
                try Self.check(p.pointee.status)
                return UnsafeBufferPointer(start: p.pointee.items, count: p.pointee.count).map { row in
                    DataVisualEpisode(id: Self.string(row.id), name: Self.string(row.name), state: Self.string(row.state),
                                      started: Date(timeIntervalSince1970: Double(row.started_ns) / 1e9), bytes: row.size_bytes)
                }
            }
        }
    }
}

private struct HomeWidgetDrop: DropDelegate {
    let target: UUID
    let store: HomeWidgetStore
    @Binding var dragged: UUID?
    func dropEntered(info: DropInfo) {
        guard let dragged, dragged != target else { return }
        store.move(dragged, before: target)
    }
    func dropUpdated(info: DropInfo) -> DropProposal? { DropProposal(operation: .move) }
    func performDrop(info: DropInfo) -> Bool { dragged = nil; return true }
}
