import SwiftUI

enum MobileLens: String, CaseIterable, Identifiable {
    case devices = "Devices", robots = "Robots", sites = "Sites"
    var id: Self { self }
    var icon: String {
        switch self { case .devices: "cpu"; case .robots: "figure.walk"; case .sites: "building.2" }
    }
    var color: Color {
        switch self { case .devices: MobileLensStyle.green; case .robots: .orange; case .sites: Color(red: 0.29, green: 0.36, blue: 0.48) }
    }
    var choice: String {
        switch self { case .devices: "I build with devices"; case .robots: "I work on robots"; case .sites: "I run a site" }
    }
    var summary: String {
        switch self {
        case .devices: "Pis, Jetsons and sensors: apps, logs, updates"
        case .robots: "Go2, G1, arms, drones: health, missions, sim"
        case .sites: "Rooms and warehouses: what’s happening, alerts"
        }
    }
}

struct MobileLensPreference: View {
    @State private var choice: MobileLens
    let save: (MobileLens) -> Void
    init(initial: MobileLens, save: @escaping (MobileLens) -> Void) {
        _choice = State(initialValue: initial)
        self.save = save
    }
    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 18) {
                Image("WendyMark")
                    .resizable().scaledToFit().frame(width: 64, height: 64).clipShape(.rect(cornerRadius: 15))
                Text("Welcome to Wendy").font(.largeTitle.bold())
                Text("What do you work with most? We’ll set up Home around it.").foregroundStyle(.secondary)
                ForEach(MobileLens.allCases) { lens in
                    Button { choice = lens } label: {
                        HStack(spacing: 12) {
                            LensIcon(symbol: lens.icon, color: lens.color)
                            VStack(alignment: .leading) {
                                Text(lens.choice).font(.headline)
                                Text(lens.summary).font(.caption).foregroundStyle(.secondary)
                            }
                            Spacer()
                            Image(systemName: choice == lens ? "checkmark.circle.fill" : "circle").foregroundStyle(lens.color)
                        }.padding(14).background(MobileLensStyle.card, in: .rect(cornerRadius: 20))
                    }.buttonStyle(.plain)
                }
                Button("Continue") { save(choice) }
                    .mobileActionStyle(primary: true).accessibilityIdentifier("lens.continue")
                Text("You can change this anytime in Settings.").font(.caption).foregroundStyle(.secondary)
            }.padding(.top, 48).padding(22).frame(maxWidth: 590)
        }.frame(maxWidth: .infinity).background(MobileLensStyle.background)
    }
}

struct MobileGeneralHome: View {
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize
    @ObservedObject var model: AppModel
    let primary: MobileLens
    let open: (MobileLens) -> Void
    let accounts: () -> Void
    let notifications: () -> Void
    let setup: () -> Void
    let recordings: () -> Void
    let openDevice: (Device) -> Void
    @StateObject private var widgets = HomeWidgetStore()
    @State private var editingHome = false
    @State private var addingWidget = false
    @State private var configuringWidget: HomeWidget?

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 16) {
                let layout = dynamicTypeSize.isAccessibilitySize
                    ? AnyLayout(VStackLayout(spacing: 8))
                    : AnyLayout(HStackLayout(spacing: 8))
                layout {
                    ForEach(MobileLens.allCases) { lens in
                        NavigationLink(value: MobileDestination.lens(lens)) {
                            if dynamicTypeSize.isAccessibilitySize {
                                HStack(spacing: 12) {
                                    LensIcon(symbol: lens.icon, color: lens.color, size: 36)
                                    Text(lens.rawValue).font(.headline)
                                    Spacer(minLength: 8)
                                    Image(systemName: "chevron.right").font(.body.weight(.semibold))
                                }.frame(maxWidth: .infinity, minHeight: 52)
                            } else {
                                VStack(spacing: 10) {
                                    LensIcon(symbol: lens.icon, color: lens.color, size: 36)
                                    HStack(spacing: 5) {
                                        Text(lens.rawValue).font(.subheadline.weight(.semibold))
                                        Image(systemName: "chevron.right").font(.caption.weight(.semibold))
                                    }
                                }.frame(maxWidth: .infinity, minHeight: 76)
                            }
                        }
                        .buttonStyle(.bordered)
                        .buttonBorderShape(.roundedRectangle(radius: 16))
                        .tint(lens.color)
                        .accessibilityLabel(lens.rawValue)
                        .accessibilityHint("Opens the \(lens.rawValue.lowercased()) view")
                        .accessibilityIdentifier("lens.open." + lens.rawValue)
                    }
                }
                if model.account != nil && model.mobileScopeIsCurrent {
                HomeWidgetDeck(model: model, store: widgets, editing: $editingHome,
                               add: { addingWidget = true }, configure: { configuringWidget = $0 },
                               setup: setup, recordings: recordings, notifications: notifications,
                               devices: { open(.devices) }, openDevice: openDevice)
                    .accessibilityElement(children: .contain)
                    .accessibilityIdentifier("home.widgets.ready")
                } else {
                    ProgressView("Loading Home…").frame(maxWidth: .infinity).padding(24)
                }
            }.padding(.horizontal, 16).padding(.bottom, 22).frame(maxWidth: 700)
                .frame(maxWidth: .infinity)
        }.background(MobileLensStyle.background)
            .navigationTitle("")
            .navigationBarTitleDisplayMode(.inline)
            .sheet(isPresented: $addingWidget) { HomeWidgetManager(store: widgets, model: model) }
            .sheet(item: $configuringWidget) { widget in
                HomeWidgetEditor(widget: widget, model: model, save: { widgets.save($0) })
            }
            .task(id: model.mobileCloudSelectionKey) {
                editingHome = false; configuringWidget = nil; addingWidget = false
                widgets.load(account: model.mobileCloudAccount, organization: model.mobileCloudOrganization)
            }
            .toolbar {
                if editingHome {
                    ToolbarItem(placement: .topBarLeading) {
                        Button("Add widget", systemImage: "plus") { addingWidget = true }
                    }
                    ToolbarItem(placement: .topBarTrailing) {
                        Button("Done") { editingHome = false }
                    }
                } else {
                ToolbarItem(placement: .topBarLeading) {
                    Button(action: accounts) { HStack(spacing: 7) {
                        Image("WendyMark").resizable().frame(width: 26, height: 26).clipShape(.circle)
                        Text(model.mobileCloudOrganizations.first { $0.id == model.mobileCloudOrganization }?.name ?? "Account").lineLimit(1).truncationMode(.tail)
                        Image(systemName: "chevron.down").font(.caption2)
                    } }.accessibilityIdentifier("lens.accounts")
                }
                ToolbarItem(placement: .topBarTrailing) { Button("Notifications", systemImage: "bell", action: notifications) }
                }
            }
    }

    private var attentionCount: Int {
        [model.nearbyErrorFailure, model.cloudErrorFailure, model.detailError].compactMap { $0 }.count +
            (model.selectedDeviceReady ? model.apps.filter { $0.runningState == .crashLooping }.count : 0)
    }

}

private struct LensMeshArtwork: View {
    var body: some View {
        Canvas { context, size in
            let points = [CGPoint(x: 0.03, y: 0.59), CGPoint(x: 0.28, y: 0.20), CGPoint(x: 0.41, y: 0.78),
                          CGPoint(x: 0.65, y: 0.43), CGPoint(x: 0.90, y: 0.13), CGPoint(x: 0.95, y: 0.82)]
                .map { CGPoint(x: $0.x * size.width, y: $0.y * size.height) }
            for (a,b) in [(0,1),(0,2),(1,2),(1,3),(2,3),(3,4),(3,5),(4,5)] {
                var line = Path(); line.move(to: points[a]); line.addLine(to: points[b])
                context.stroke(line, with: .color(MobileLensStyle.green.opacity(0.23)), lineWidth: 2)
            }
            for point in points {
                context.fill(Path(ellipseIn: CGRect(x: point.x - 6, y: point.y - 6, width: 12, height: 12)), with: .color(MobileLensStyle.green.opacity(0.48)))
            }
        }.accessibilityHidden(true)
    }
}

struct LensIcon: View {
    let symbol: String
    let color: Color
    var size: CGFloat = 36
    var body: some View {
        Image(systemName: symbol).font(.system(size: size * 0.48, weight: .semibold)).foregroundStyle(.white)
            .frame(width: size, height: size).background(color, in: .rect(cornerRadius: size > 40 ? 16 : 10))
    }
}

extension View {
    @ViewBuilder func mobileActionStyle(primary: Bool = false) -> some View {
        if primary { self.buttonStyle(.borderedProminent).controlSize(.large).tint(MobileLensStyle.green) }
        else { self.buttonStyle(.bordered).controlSize(.large).tint(MobileLensStyle.green) }
    }
}

struct LensSection<Content: View>: View {
    let title: String
    @ViewBuilder let content: Content
    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text(title.uppercased()).font(.caption.weight(.semibold)).foregroundStyle(.secondary).padding(.leading, 16)
            VStack(spacing: 0) { content }.frame(maxWidth: .infinity, alignment: .leading)
                .background(MobileLensStyle.card, in: .rect(cornerRadius: 22))
        }
    }
}

struct LensRow: View {
    @Environment(\.dynamicTypeSize) private var textSize
    let title: String
    let subtitle: String
    let symbol: String
    var color = MobileLensStyle.green
    var trailing = ""
    var navigates = true
    var body: some View {
        HStack(spacing: 12) {
            LensIcon(symbol: symbol, color: color)
            VStack(alignment: .leading, spacing: 2) {
                Text(title).font(.body.weight(.semibold))
                    .lineLimit(textSize.isAccessibilitySize ? 3 : 2).truncationMode(.middle)
                    .accessibilityLabel(title)
                if !subtitle.isEmpty { Text(subtitle).font(.caption).foregroundStyle(.secondary)
                    .lineLimit(textSize.isAccessibilitySize ? 3 : 2).truncationMode(.tail)
                    .accessibilityLabel(subtitle) }
            }.frame(maxWidth: .infinity, alignment: .leading)
            Spacer(minLength: 4)
            if !trailing.isEmpty { Text(trailing).font(.caption).foregroundStyle(.secondary)
                .lineLimit(textSize.isAccessibilitySize ? 2 : 1).truncationMode(.tail).frame(maxWidth: 92, alignment: .trailing) }
            if navigates { Image(systemName: "chevron.right").font(.caption.weight(.semibold)).foregroundStyle(.tertiary) }
        }.padding(.horizontal, 16).padding(.vertical, 10).contentShape(.rect)
    }
}

struct MobileLensChat: View {
    @ObservedObject var model: AppModel
    let chooseDevice: () -> Void
    @Environment(\.dismiss) private var dismiss
    @Environment(\.scenePhase) private var phase
    @State private var showingSuggestions = false
    @State private var selection: AISelection?
    @State private var selectionError: String?
    private struct AISelection: Identifiable {
        let id = UUID()
        let record: RememberedAIDevice
        let device: Device
        let scope: String
    }
    var body: some View {
        NavigationStack {
            Group {
                if selection != nil {
                    VStack(spacing: 16) {
                        ProgressView("Checking device and models…")
                        Button("Cancel") { selection = nil }
                    }.frame(maxWidth: .infinity, maxHeight: .infinity)
                } else if !showingSuggestions, model.mobileScopeIsCurrent, model.selectedDeviceReady, let device = model.connected, !model.connectedLite {
                    DeviceChatView(deviceName: device.title, chat: model.deviceChat, ownsResponse: false,
                                   chooseAIDevice: { showingSuggestions = true }).id(model.deviceSession)
                } else {
                    VStack(spacing: 0) {
                        if let selectionError { Text(selectionError).font(.footnote).foregroundStyle(.secondary).padding(.horizontal, 20) }
                        MobileAIDeviceSuggestions(model: model, remembered: model.rememberedAIDevices,
                            chooseDevice: chooseDevice, select: { record, device in
                                selectionError = nil
                                selection = AISelection(record: record, device: device, scope: model.mobileCloudSelectionKey)
                            }, excludedID: showingSuggestions ? model.connected.map(recordingDeviceID) : nil)
                        if showingSuggestions, model.selectedDeviceReady {
                            Button("Back to chat") { showingSuggestions = false }.padding(.bottom, 12)
                        }
                    }
                }
            }.navigationTitle("Ask Wendy").navigationBarTitleDisplayMode(.inline)
                .toolbar {
                    ToolbarItem(placement: .topBarTrailing) {
                        Button("Close", systemImage: "xmark") { dismiss() }.labelStyle(.iconOnly)
                    }
                }
                .background(Color(uiColor: .systemBackground))
                .task(id: selection?.id) {
                    guard let request = selection else { return }
                    await connectSuggestion(request)
                }
                .onChange(of: phase) { _, value in if value != .active { selection = nil } }
                .onChange(of: model.mobileCloudSelectionKey) { _, _ in
                    selection = nil; selectionError = nil; showingSuggestions = false
                }
        }
    }
    private func connectSuggestion(_ request: AISelection) async {
        await model.connect(request.device)
        guard !Task.isCancelled, selection?.id == request.id, request.scope == model.mobileCloudSelectionKey,
              model.mobileScopeIsCurrent, phase == .active else { return }
        guard model.selectedDeviceReady, model.connected.map(recordingDeviceID) == request.record.id else {
            selectionError = model.detailError.map { PresentedFailure($0).message } ?? "Could not connect. Try another device."
            showingSuggestions = true; selection = nil; return
        }
        let chat = model.deviceChat
        // A remembered model is a suggestion, never a ready-to-send connection.
        await chat.verifySuggestedModel(request.record.preferred?.id, port: request.record.preferred?.port)
        guard !Task.isCancelled, selection?.id == request.id, request.scope == model.mobileCloudSelectionKey,
              model.deviceSession == chat.session, model.mobileScopeIsCurrent, phase == .active else { return }
        // Show the freshly checked result (including empty/error), never silently try another device.
        showingSuggestions = false; selection = nil
    }

}

// The lens owns generation so dismissing the sheet does not cancel a reply.
// Replacing the session cancels this task before another device can own it.
struct MobileChatLifetime: View {
    @ObservedObject var chat: DeviceChatModel
    let isPresented: Bool
    let active: Bool
    var body: some View {
        Color.clear.frame(width: 0, height: 0)
            .task(id: active ? chat.turn : 0) { if active { await chat.respond() } }
            .onChange(of: active) { _, available in if !available { chat.stop() } }
            .onChange(of: isPresented, initial: true) { _, visible in chat.setPresented(visible) }
            .onDisappear { chat.setPresented(false); chat.stop() }
    }
}
