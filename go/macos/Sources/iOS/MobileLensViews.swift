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
                    .buttonStyle(LensPillStyle(primary: true)).accessibilityIdentifier("lens.continue")
                Text("You can change this anytime in Settings.").font(.caption).foregroundStyle(.secondary)
            }.padding(.top, 48).padding(22).frame(maxWidth: 590)
        }.frame(maxWidth: .infinity).background(MobileLensStyle.background)
    }
}

struct MobileGeneralHome: View {
    @ObservedObject var model: AppModel
    let primary: MobileLens
    let open: (MobileLens) -> Void
    let accounts: () -> Void
    let notifications: () -> Void
    let setup: () -> Void
    let recordings: () -> Void
    @AppStorage("wendy.primaryLens") private var preferred = MobileLens.devices.rawValue
    @State private var editingHome = false
    @AppStorage("wendy.home.sections") private var sections = "Needs you,Happening now,Quick actions"
    @AppStorage("wendy.home.hidden") private var hidden = ""
    private var order: [String] { sections.components(separatedBy: ",") }
    private var hiddenItems: Set<String> { Set(hidden.split(separator: ",").map(String.init)) }

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 16) {
                Text(Date.now.formatted(.dateTime.weekday(.wide).month(.wide).day())).foregroundStyle(.secondary)
                Button { open(primary) } label: {
                    VStack(alignment: .leading, spacing: 14) {
                        HStack(spacing: 12) {
                            LensIcon(symbol: primary.icon, color: primary.color, size: 52)
                            Text(primary.rawValue).font(.title.bold())
                            Spacer(minLength: 0)
                            if primary == .devices && attentionCount > 0 {
                                Text("\(attentionCount) need attention").font(.caption.bold()).foregroundStyle(.red)
                                    .padding(6).background(.red.opacity(0.12), in: .capsule)
                            }
                        }
                        if primary == .devices {
                            HStack(spacing: 8) {
                                stat(model.nearbyLoaded && model.nearbyError == nil ? model.nearby.count : nil, "Local")
                                stat(model.cloudLoaded && model.cloudError == nil ? model.cloud.count : nil, "Cloud")
                            }
                        }
                        Spacer()
                        HStack {
                            Text("Open \(primary.rawValue)").font(.headline)
                            Image(systemName: "chevron.right").font(.caption.bold())
                        }.padding(.horizontal, 16).padding(.vertical, 10)
                            .foregroundStyle(.white).background(primary.color, in: .capsule)
                    }.padding(20).frame(maxWidth: .infinity, minHeight: 380, alignment: .leading)
                        .background {
                            ZStack(alignment: .bottomTrailing) {
                                LinearGradient(colors: [MobileLensStyle.card, primary.color.opacity(0.16)], startPoint: .top, endPoint: .bottom)
                                if primary == .devices { LensMeshArtwork().frame(width: 260, height: 195).padding(.trailing, 4) }
                                else { Image(systemName: primary.icon).font(.system(size: 110)).foregroundStyle(primary.color.opacity(0.16)).padding(25) }
                            }
                        }.clipShape(.rect(cornerRadius: 30))
                }.buttonStyle(.plain).accessibilityIdentifier("lens.open." + primary.rawValue)
                HStack(spacing: 12) {
                    ForEach(MobileLens.allCases.filter { $0 != primary && !hiddenItems.contains($0.rawValue) }) { lens in
                        Button { open(lens) } label: {
                            VStack(alignment: .leading, spacing: 18) {
                                LensIcon(symbol: lens.icon, color: lens.color)
                                VStack(alignment: .leading, spacing: 2) {
                                    Text(lens.rawValue).font(.headline)
                                    Text(lens == .sites ? "Places and facilities" : lens == .robots ? "Paired robots" : "Local and cloud devices")
                                        .font(.caption).foregroundStyle(.secondary)
                                }
                            }.padding(14).frame(maxWidth: .infinity, alignment: .leading)
                                .background(alignment: .bottomTrailing) {
                                    Image(systemName: lens.icon).font(.system(size: 58)).foregroundStyle(lens.color.opacity(0.13)).offset(x: 6, y: 8)
                                }
                                .background(lens.color.opacity(0.10), in: .rect(cornerRadius: 22)).clipShape(.rect(cornerRadius: 22))
                        }.buttonStyle(.plain).accessibilityIdentifier("lens.open." + lens.rawValue)
                    }
                }
                ForEach(order.filter { !hiddenItems.contains($0) }, id: \.self) { section in
                    switch section {
                    case "Needs you":
                        if attentionCount > 0 {
                            LensSection(title: section) {
                                Button(action: { open(.devices) }) {
                                    LensRow(title: "Devices need attention", subtitle: "\(attentionCount) reported issues", symbol: "bell", color: .orange)
                                }.buttonStyle(.plain)
                            }
                        }
                    case "Happening now":
                        if let event = model.experienceEvents.first {
                            LensSection(title: section) {
                                Button(action: notifications) {
                                    LensRow(title: event.title, subtitle: event.deviceName, symbol: event.symbol)
                                }.buttonStyle(.plain)
                            }
                        }
                    case "Quick actions":
                        LensSection(title: section) {
                            Button(action: setup) { LensRow(title: "Set up a new device", subtitle: "Nearby over Bluetooth", symbol: "plus") }.buttonStyle(.plain)
                            Divider().padding(.leading, 64)
                            Button(action: recordings) { LensRow(title: "Device recordings", subtitle: "", symbol: "cylinder.split.1x2") }.buttonStyle(.plain)
                        }
                    default: EmptyView()
                    }
                }
                Button("Edit Home", systemImage: "slider.horizontal.3") { editingHome = true }
                    .font(.subheadline).foregroundStyle(MobileLensStyle.accent)
            }.padding(.horizontal, 16).padding(.bottom, 22).frame(maxWidth: 700)
                .frame(maxWidth: .infinity)
        }.background(MobileLensStyle.background)
            .navigationTitle("Good morning")
            .sheet(isPresented: $editingHome) {
                NavigationStack {
                    List {
                        Section("Secondary lenses") {
                            ForEach(MobileLens.allCases.filter { $0 != primary }) { lens in
                                Toggle(lens.rawValue, isOn: visibility(lens.rawValue))
                            }
                        }
                        Section("Home sections") {
                            ForEach(order, id: \.self) { title in Toggle(title, isOn: visibility(title)) }
                                .onMove { from, to in
                                    var newOrder = order
                                    newOrder.move(fromOffsets: from, toOffset: to)
                                    sections = newOrder.joined(separator: ",")
                                }
                        }
                        Picker("Primary lens", selection: $preferred) {
                            ForEach(MobileLens.allCases) { Text($0.rawValue).tag($0.rawValue) }
                        }
                    }.environment(\.editMode, .constant(.active))
                        .navigationTitle("Edit Home")
                        .toolbar { Button("Done") { editingHome = false } }
                }
            }
            .toolbar {
                ToolbarItem(placement: .topBarLeading) {
                    Button(action: accounts) { HStack(spacing: 7) {
                        Image("WendyMark").resizable().frame(width: 26, height: 26).clipShape(.circle)
                        Text(model.mobileCloudOrganizations.first { $0.id == model.mobileCloudOrganization }?.name ?? "Account")
                        Image(systemName: "chevron.down").font(.caption2)
                    } }.accessibilityIdentifier("lens.accounts")
                }
                ToolbarItem(placement: .topBarTrailing) { Button("Notifications", systemImage: "bell", action: notifications) }
            }
    }

    private var attentionCount: Int {
        [model.nearbyError, model.cloudError, model.detailError].compactMap { $0 }.count +
            (model.selectedDeviceReady ? model.apps.filter { $0.runningState == .crashLooping }.count : 0)
    }
    private func visibility(_ key: String) -> Binding<Bool> {
        Binding(get: { !hiddenItems.contains(key) }, set: { visible in
            var changed = hiddenItems
            if visible { changed.remove(key) } else { changed.insert(key) }
            hidden = changed.sorted().joined(separator: ",")
        })
    }

    private func stat(_ count: Int?, _ title: String) -> some View {
        VStack(alignment: .leading) {
            Text(count.map(String.init) ?? "—").font(.title2.bold()).foregroundStyle(primary.color)
            Text(title).font(.caption).foregroundStyle(.secondary)
        }.padding(.horizontal, 12).padding(.vertical, 8).frame(maxWidth: .infinity, alignment: .leading)
            .background(Color(uiColor: .systemBackground).opacity(0.44), in: .rect(cornerRadius: 14))
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

struct MobileLensBar: View {
    @Binding var tab: MobileHome
    @ObservedObject var chatModel: DeviceChatModel
    let context: String?
    let chat: () -> Void
    var body: some View {
        VStack(spacing: 8) {
            Button(action: chat) {
                HStack(spacing: 10) {
                    Image(systemName: "sparkles")
                    Text("Ask Wendy about \(context ?? "a device")…").lineLimit(1).truncationMode(.middle)
                    Spacer(minLength: 0)
                    if chatModel.unreadReplies > 0 || chatModel.approval != nil {
                        Text(chatModel.approval != nil ? "Review" : "\(chatModel.unreadReplies)")
                            .font(.caption.bold()).padding(6).foregroundStyle(.white).background(.red, in: .capsule)
                    }
                }.font(.subheadline).padding(.horizontal, 16).frame(minHeight: 44)
            }.buttonStyle(.plain).glassEffect(.regular, in: .capsule)
                .accessibilityIdentifier("lens.chat")
            HStack(spacing: 1) {
                ForEach(MobileHome.allCases, id: \.self) { item in
                    Button { tab = item } label: {
                        VStack(spacing: 2) {
                            Image(systemName: symbol(item)).font(.system(size: 20, weight: .medium))
                            Text(item.rawValue).font(.caption2.weight(.semibold))
                        }.frame(maxWidth: .infinity, minHeight: 48)
                            .foregroundStyle(tab == item ? MobileLensStyle.accent : .primary)
                            .background(tab == item ? MobileLensStyle.green.opacity(0.12) : .clear, in: .capsule)
                    }.buttonStyle(.plain).accessibilityAddTraits(tab == item ? .isSelected : [])
                        .accessibilityIdentifier("lens.tab." + item.rawValue)
                }
            }.padding(4).glassEffect(.regular, in: .capsule)

        }.frame(maxWidth: 520).padding(.horizontal, 14).padding(.bottom, 2).frame(maxWidth: .infinity)
    }
    private func symbol(_ item: MobileHome) -> String {
        switch item { case .home: "house"; case .devices: "cpu"; case .data: "cylinder.split.1x2"; case .activity: "clock" }
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

struct LensPillStyle: ButtonStyle {
    var primary = false
    func makeBody(configuration: Configuration) -> some View {
        configuration.label.font(.subheadline.weight(.semibold))
            .frame(maxWidth: .infinity, minHeight: 44)
            .foregroundStyle(primary ? Color.white : MobileLensStyle.accent)
            .background(primary ? MobileLensStyle.green : MobileLensStyle.green.opacity(0.12), in: .capsule)
            .opacity(configuration.isPressed ? 0.75 : 1)
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
                if !subtitle.isEmpty { Text(subtitle).font(.caption).foregroundStyle(.secondary) }
            }
            Spacer(minLength: 4)
            if !trailing.isEmpty { Text(trailing).font(.caption).foregroundStyle(.secondary) }
            if navigates { Image(systemName: "chevron.right").font(.caption.weight(.semibold)).foregroundStyle(.tertiary) }
        }.padding(.horizontal, 16).padding(.vertical, 10).contentShape(.rect)
    }
}

struct MobileLensChat: View {
    @ObservedObject var model: AppModel
    let chooseDevice: () -> Void
    @Environment(\.dismiss) private var dismiss
    var body: some View {
        NavigationStack {
            Group {
                if model.mobileScopeIsCurrent, model.selectedDeviceReady, let device = model.connected, !model.connectedLite {
                    VStack(spacing: 0) {
                        HStack(spacing: 10) {
                            MobileDeviceAvatar(device: device, size: 30)
                            Text(device.title).font(.subheadline.weight(.semibold)).lineLimit(1)
                            Spacer()
                        }.padding(.horizontal, 16).padding(.vertical, 6)
                        DeviceChatView(deviceName: device.title, chat: model.deviceChat, ownsResponse: false).id(model.deviceSession)
                    }
                } else {
                    ContentUnavailableView {
                        Label("Choose a device", systemImage: "sparkles")
                    } description: { Text("Open a device to ask Wendy about it.") } actions: { Button("Choose device", action: chooseDevice) }
                }
            }.navigationTitle("Ask Wendy").navigationBarTitleDisplayMode(.inline)
                .toolbar {
                    ToolbarItem(placement: .topBarLeading) { Button("Change device", action: chooseDevice) }
                    ToolbarItem(placement: .topBarTrailing) {
                        Button("Close", systemImage: "xmark") { dismiss() }.labelStyle(.iconOnly)
                    }
                }
                .background(MobileLensStyle.background)
        }
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
