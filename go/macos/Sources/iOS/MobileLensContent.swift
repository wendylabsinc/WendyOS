import SwiftUI

struct MobileLensData: View {
    @ObservedObject var model: AppModel
    let chooseDevice: () -> Void
    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 18) {
                Text("Device recordings").foregroundStyle(.secondary)
                if model.mobileScopeIsCurrent, model.selectedDeviceReady, let device = model.connected {
                    Button(action: chooseDevice) {
                        Label(device.title, systemImage: "cpu").font(.subheadline.bold())
                            .padding(.horizontal, 12).padding(.vertical, 7)
                            .background(MobileLensStyle.green.opacity(0.12), in: .capsule)
                    }
                    MobileRecordingSection(model: model)
                } else {
                    ContentUnavailableView {
                        Label("Choose a device", systemImage: "cylinder.split.1x2")
                    } description: { Text("Open a device to browse its recordings.") } actions: {
                        Button("Choose device", action: chooseDevice)
                    }
                }
            }.padding(16).frame(maxWidth: 700).frame(maxWidth: .infinity)
        }.background(MobileLensStyle.background).navigationTitle("Data")
    }
}

struct MobileRecordingSection: View {
    @ObservedObject var model: AppModel
    var recent = false
    @State private var episodes: [DataVisualEpisode] = []
    @State private var loaded = false
    @State private var error: String?
    private var owner: UInt64? {
        guard model.mobileScopeIsCurrent, model.selectedDeviceReady, !model.connectedLite else { return nil }
        return model.deviceSession
    }

    var body: some View {
        LensSection(title: recent ? "Recent data" : "Recordings") {
            if let session = owner, let device = model.connected {
                if let error {
                    FailureNote(message: error).padding(16)
                } else if !loaded {
                    ProgressView("Loading recordings…").padding(16)
                } else if episodes.isEmpty {
                    Text("No recordings on \(device.title).").font(.subheadline).foregroundStyle(.secondary).padding(16)
                } else {
                    let visible = recent ? Array(episodes.prefix(3)) : episodes
                    ForEach(visible) { episode in
                        NavigationLink {
                            if owner == session {
                                DataVisualizerView(session: session, deviceName: device.title, initialEpisode: episode.id)
                                    .navigationTitle(episode.title).navigationBarTitleDisplayMode(.inline)
                            } else { ContentUnavailableView("Device changed", systemImage: "bolt.slash") }
                        } label: {
                            LensRow(title: episode.title, subtitle: device.title, symbol: "cylinder.split.1x2", color: .gray,
                                    trailing: episode.started.formatted(date: .abbreviated, time: .omitted))
                        }.buttonStyle(.plain)
                        if episode.id != visible.last?.id { Divider().padding(.leading, 64) }
                    }
                }
            } else {
                Text("Connect a device to see its recordings.").font(.subheadline).foregroundStyle(.secondary).padding(16)
            }
        }.task(id: owner) {
            episodes = []; loaded = false; error = nil
            guard let session = owner else { return }
            do {
                let rows = try await GoClient.shared.visualEpisodes(session: session)
                guard !Task.isCancelled, owner == session else { return }
                episodes = rows.sorted { $0.started > $1.started }; loaded = true
            } catch {
                if !Task.isCancelled, owner == session { self.error = error.localizedDescription }
            }
        }
    }
}

struct MobileHomeAttention: View {
    @ObservedObject var model: AppModel
    let openDevices: () -> Void
    var body: some View {
        LensSection(title: "Needs attention") {
            if let issue = model.nearbyError ?? model.cloudError ?? model.mobileCloudSelectionError {
                FailureNote(message: issue).padding(16)
            } else if model.mobileScopeIsCurrent, model.selectedDeviceReady,
                      let app = model.apps.first(where: { $0.runningState == .crashLooping }), let device = model.connected {
                NavigationLink(value: MobileDestination.device(device)) {
                    LensRow(title: app.appName, subtitle: "Crash looping · \(device.title)", symbol: "exclamationmark.triangle", color: .orange)
                }.buttonStyle(.plain)
            } else if let device = model.failedConnection, let error = model.detailError {
                NavigationLink(value: MobileDestination.device(device)) {
                    LensRow(title: device.title, subtitle: PresentedFailure(error).message, symbol: "wifi.exclamationmark", color: .orange)
                }.buttonStyle(.plain)
            } else {
                if let snapshot = model.mobilePresence, model.mobileScopeIsCurrent, model.cloudError == nil,
                   snapshot.0.presenceError.isEmpty, Date().timeIntervalSince(snapshot.1) < 60,
                   let offline = snapshot.0.devices.first(where: { !$0.online }) {
                    NavigationLink(value: MobileDestination.device(offline.device)) {
                        LensRow(title: offline.name, subtitle: "Offline in Wendy Cloud", symbol: "wifi.slash", color: .orange)
                    }.buttonStyle(.plain)
                } else {
                    Button(action: openDevices) {
                        LensRow(title: "No reported issues", subtitle: "View device activity", symbol: "checkmark.circle")
                    }.buttonStyle(.plain)
                }
            }
        }
    }
}

private struct DeviceCampaignNotifications: View {
    @StateObject private var events: CampaignEventStore
    let session: UInt64
    let deviceName: String
    @State private var selected: CampaignClip?

    init(device: Device, session: UInt64) {
        _events = StateObject(wrappedValue: CampaignEventStore(deviceID: device.id))
        deviceName = device.title
        self.session = session
    }

    var body: some View {
        Group {
            if let error = events.error { FailureNote(message: error) }
            if !events.warning.isEmpty { Text(events.warning).foregroundStyle(.secondary) }
            if events.loading && events.clips.isEmpty { ProgressView("Loading notifications…") }
            ForEach([true, false], id: \.self) { today in
                let clips = events.clips.filter { Calendar.current.isDateInToday($0.started) == today }
                if !clips.isEmpty {
                    Section(today ? "Today" : "Earlier") {
                        ForEach(clips) { clip in
                            Button {
                                events.markRead([clip.id])
                                selected = clip
                            } label: {
                                LensRow(title: clip.campaign, subtitle: clip.reason.isEmpty ? deviceName : clip.reason,
                                        symbol: "bell", color: events.readIDs.contains(clip.id) ? .gray : .orange,
                                        trailing: clip.started.formatted(date: .omitted, time: .shortened))
                            }.buttonStyle(.plain)
                        }
                    }
                }
            }
            if events.clips.isEmpty && !events.loading && events.error == nil {
                ContentUnavailableView("No campaign notifications", systemImage: "bell")
            }
        }.task(id: session) { await events.refresh(session: session) }
            .navigationDestination(isPresented: Binding(get: { selected != nil }, set: { if !$0 { selected = nil } })) {
                if let selected { DataVisualizerView(session: session, deviceName: deviceName, initialEpisode: selected.id) }
            }
    }
}

struct MobileNotifications: View {
    @ObservedObject var model: AppModel
    let select: (Device) -> Void
    var body: some View {
        List {
            if model.mobileScopeIsCurrent, model.selectedDeviceReady, !model.connectedLite, let device = model.connected {
                DeviceCampaignNotifications(device: device, session: model.deviceSession).id(model.deviceSession)
            } else {
                ContentUnavailableView("No connected device", systemImage: "bell",
                                       description: Text("Open a device to view its campaign notifications."))
            }
            if !model.experienceEvents.isEmpty {
                Section("Recent device activity") {
                    ForEach(model.experienceEvents.prefix(5)) { event in
                        let device = (model.nearby + model.cloud).first { $0.id == event.deviceID }
                        Button { if let device { select(device) } } label: {
                            LensRow(title: event.title, subtitle: event.deviceName, symbol: event.symbol,
                                    trailing: event.date.formatted(date: .omitted, time: .shortened), navigates: device != nil)
                        }.disabled(device == nil).buttonStyle(.plain)
                    }
                }
            }
        }.scrollContentBackground(.hidden).background(MobileLensStyle.background).navigationTitle("Notifications")
    }
}



struct MobileMeshSection: View {
    @ObservedObject var model: AppModel
    @State private var topology: DeviceMeshTopology?
    @State private var error: String?
    private var session: UInt64? { model.mobileScopeIsCurrent && model.selectedDeviceReady && !model.connectedLite ? model.deviceSession : nil }
    var body: some View {
        Section("Mesh network") {
            if let topology, let device = model.connected { MeshGraphView(topology: topology, deviceName: device.title) }
            else if let error { FailureNote(message: error) }
            else if session != nil { ProgressView("Loading mesh…") }
            else { Text("Open a nearby device to see its mesh peers.").foregroundStyle(.secondary) }
        }.task(id: session) {
            topology = nil; error = nil
            guard let session else { return }
            do {
                let settings = try await GoClient.shared.networkSettings(session: session)
                if !Task.isCancelled, self.session == session { topology = settings.topology }
            } catch { if !Task.isCancelled, self.session == session { self.error = error.localizedDescription } }
        }
    }
}

struct MobileRobotInventory: View {
    @ObservedObject var model: AppModel
    let chooseDevice: () -> Void
    @State private var robots: [UnitreeRobot] = []
    @State private var status: String?
    var body: some View {
        List {
            if model.selectedDeviceReady && !model.connectedLite {
                if let status { Text(status).foregroundStyle(.secondary) }
                ForEach(robots) { robot in
                    NavigationLink {
                        if model.selectedDeviceReady { NativeSiteView(model: model) }
                    } label: { LensRow(title: robot.name.isEmpty ? robot.serial : robot.name,
                                       subtitle: robot.connected ? "Connected" : "Paired", symbol: "figure.walk", color: .orange) }
                }
                if robots.isEmpty && status == nil { Text("No robots found on this device.").foregroundStyle(.secondary) }
            } else {
                ContentUnavailableView {
                    Label("Choose an onboard device", systemImage: "cpu")
                } description: { Text("Connect to the device that manages your robot.") } actions: { Button("Open Devices", action: chooseDevice) }
            }
        }.scrollContentBackground(.hidden).background(MobileLensStyle.background).navigationTitle("Robots")
            .task(id: model.deviceSession) {
                robots = []; status = nil
                guard model.mobileScopeIsCurrent, model.selectedDeviceReady, !model.connectedLite else { return }
                do {
                    let result = try await UnitreeClient.shared.robots(session: model.deviceSession)
                    guard !Task.isCancelled else { return }
                    robots = result.robots; status = result.warning.isEmpty ? nil : result.warning
                } catch { if !Task.isCancelled { status = error.localizedDescription } }
            }
    }
}

private struct PresenceCount: Identifiable {
    let title: String
    let count: Int
    let color: Color
    var id: String { title }
}
@MainActor private func presenceCounts(_ model: AppModel) -> [PresenceCount] {
    guard model.mobileScopeIsCurrent else { return [] }
    if !model.mobileCloudOrganization.isEmpty {
        guard model.cloudError == nil, model.cloudLoaded, let snapshot = model.mobilePresence,
              Date().timeIntervalSince(snapshot.1) < 60 else { return [] }
        guard snapshot.0.presenceError.isEmpty else { return [PresenceCount(title: "unknown", count: model.cloud.count, color: .gray)] }
        let online = snapshot.0.devices.filter(\.online).count
        return [PresenceCount(title: "online", count: online, color: .green),
                PresenceCount(title: "offline", count: snapshot.0.devices.count - online, color: .gray)].filter { $0.count > 0 }
    }
    guard model.nearbyLoaded && model.nearbyError == nil else { return [] }
    return [PresenceCount(title: "nearby", count: model.nearby.count, color: MobileLensStyle.green)]
}
struct MobilePresenceLegend: View {
    @ObservedObject var model: AppModel
    var body: some View {
        VStack(alignment: .trailing, spacing: 4) {
            ForEach(presenceCounts(model)) { value in HStack(spacing: 5) {
                Circle().fill(value.color).frame(width: 7, height: 7)
                Text("\(value.count) \(value.title)").font(.caption.weight(.semibold))
            } }
        }
    }
}
struct MobilePresenceBar: View {
    @ObservedObject var model: AppModel
    var body: some View {
        let values = presenceCounts(model)
        let total = max(1, values.reduce(0) { $0 + $1.count })
        GeometryReader { g in
            HStack(spacing: 0) {
                ForEach(values) { value in
                    Capsule().fill(value.color).frame(width: g.size.width * CGFloat(value.count) / CGFloat(total))
                }
            }
        }.frame(height: 7).background(.quaternary, in: .capsule).accessibilityHidden(true)
    }
}
