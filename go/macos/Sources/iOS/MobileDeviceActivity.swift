import SwiftUI

struct MobileRecordingScope<Content: View>: View {
    @StateObject private var catalog = RecordingCatalog()
    @ViewBuilder let content: () -> Content
    var body: some View { content().environmentObject(catalog) }
}

enum MobileActivitySegment: String, CaseIterable { case alerts = "Alerts", events = "Events", archived = "Archived" }

private struct AlertSummary: Codable, Identifiable {
    let id, title, deviceName: String
    let date: Date?
}
private struct ArchivedAlert: Codable {
    let summary: AlertSummary
    let archivedAt: Date
}
private struct ActivityReceipts: Codable {
    var read = Set<String>()
    var archived: [String: ArchivedAlert] = [:]
    var expired = Set<String>()

    mutating func expire(now: Date) {
        let cutoff = now.addingTimeInterval(-30 * 24 * 3600)
        for (id, alert) in archived where alert.archivedAt <= cutoff {
            expired.insert(id)
            archived.removeValue(forKey: id)
        }
    }
}

private struct ActivityAlert: Identifiable {
    let summary: AlertSummary
    let device: Device
    var appName: String?
    var updateAvailable = false
    var episode: MobileRecordingEpisode?
    var id: String { summary.id }

    init(device: Device, id: String, title: String, date: Date?, episode: MobileRecordingEpisode? = nil, appName: String? = nil, updateAvailable: Bool = false) {
        let owner = recordingDeviceID(device)
        let key = "\(owner.utf8.count):\(owner):\(id)"
        summary = AlertSummary(id: key, title: title, deviceName: device.title, date: date)
        self.device = device
        self.episode = episode
        self.appName = appName
        self.updateAvailable = updateAvailable
    }
}

struct MobileDeviceActivity: View {
    @ObservedObject var model: AppModel
    let isVisible: Bool
    @Binding var segment: MobileActivitySegment
    let select: (Device) -> Void
    @EnvironmentObject private var catalog: RecordingCatalog
    @Environment(\.scenePhase) private var phase
    @State private var posters = RecordingPosters()
    @State private var playing: ActivityAlert?
    @State private var refresh = 0
    @State private var updateStatus: DeviceUpdateSnapshot?
    @State private var updateOwner: UInt64?
    @State private var updateRoute: UInt64?
    @State private var inspecting: ActivityAlert?
    @AppStorage private var saved: Data

    init(model: AppModel, isVisible: Bool, segment: Binding<MobileActivitySegment>, select: @escaping (Device) -> Void) {
        self.model = model
        self.isVisible = isVisible
        _segment = segment
        self.select = select
        _saved = AppStorage(wrappedValue: Data(), "wendy.activity.receipts.\(model.mobileCloudSelectionKey)")
    }

    private var receipts: ActivityReceipts {
        (try? JSONDecoder().decode(ActivityReceipts.self, from: saved)) ?? ActivityReceipts()
    }
    private func update(_ edit: (inout ActivityReceipts) -> Void) {
        var state = receipts
        edit(&state)
        if let data = try? JSONEncoder().encode(state), data != saved { saved = data }
    }

    private var alerts: [ActivityAlert] {
        var result = catalog.reads.values.compactMap(\.library).flatMap { library in
            library.episodes.compactMap { episode -> ActivityAlert? in
                guard let campaign = episode.campaign else { return nil }
                return ActivityAlert(device: library.device, id: "clip." + episode.id,
                                     title: episode.trigger ?? campaign, date: episode.started, episode: episode)
            }
        }
        if model.mobileScopeIsCurrent, model.selectedDeviceReady, let device = model.connected {
            if updateOwner == model.deviceSession, let status = updateStatus, status.available {
                result.append(ActivityAlert(device: device, id: "update." + status.latest,
                                            title: "Agent update available", date: nil, updateAvailable: true))
            }
            for app in model.apps where app.runningState == .crashLooping {
                result.append(ActivityAlert(device: device, id: "crash." + app.id, title: "\(app.appName) keeps restarting", date: nil, appName: app.appName))
            }
        }
        return result.sorted {
            let left = $0.summary.date ?? .distantFuture, right = $1.summary.date ?? .distantFuture
            return left == right ? $0.id < $1.id : left > right
        }
    }
    private struct LoadKey: Equatable { let active: Bool; let targets: [MobileRecordingTarget]; let refresh: Int }
    var body: some View {
        let sources = RecordingSources(model: model)
        let key = LoadKey(active: isVisible && phase == .active && model.mobileScopeIsCurrent && segment == .alerts,
                          targets: sources.targets, refresh: refresh)
        let softwareSession = key.active && model.selectedDeviceReady && !model.connectedLite ? model.deviceSession : nil
        let state = receipts
        let active = alerts.filter { state.archived[$0.id] == nil && !state.expired.contains($0.id) }
        let unread = active.filter { !state.read.contains($0.id) }
        List {
            Text("Alerts and events from your devices").foregroundStyle(.secondary).plainActivityRow()
            HStack(spacing: 0) {
                ForEach(MobileActivitySegment.allCases, id: \.self) { tab in
                    Button {
                        segment = tab
                    } label: {
                        Text(tab == .archived && !state.archived.isEmpty ? "Archived \(state.archived.count)" : tab.rawValue)
                            .font(.subheadline.weight(segment == tab ? .semibold : .medium)).frame(maxWidth: .infinity, minHeight: 30)
                            .foregroundStyle(segment == tab ? Color.white : .primary)
                            .background(segment == tab ? MobileLensStyle.green : .clear, in: .capsule)
                    }.buttonStyle(.plain).accessibilityAddTraits(segment == tab ? .isSelected : [])
                }
            }.padding(2).background(Color.secondary.opacity(0.12), in: .capsule).plainActivityRow()
            switch segment {
            case .alerts:
                HStack {
                    Text("ALERTS · \(unread.count) UNREAD").font(.caption.weight(.semibold)).foregroundStyle(.secondary)
                    Spacer()
                    Button("Mark all read") { update { $0.read.formUnion(active.map(\.id)) } }
                        .disabled(unread.isEmpty).font(.subheadline.weight(.semibold))
                }.plainActivityRow()
                ForEach(active) { alert in
                    card(alert, unread: !state.read.contains(alert.id)).plainActivityRow()
                        .swipeActions { Button("Archive", systemImage: "archivebox") { archive(alert) }.tint(.gray) }
                        .contextMenu { Button("Archive", systemImage: "archivebox") { archive(alert) } }
                }
                let partial = catalog.reads.values.contains { $0.library?.labelCoverage != nil }
                let incomplete = sources.unavailable > 0 || !catalog.detailProblems.isEmpty || catalog.reads.values.contains { $0.problem != nil || $0.library?.warning != nil } || model.cloudError != nil || model.nearbyError != nil
                if catalog.loading || (!model.mobileCloudOrganization.isEmpty && !model.cloudLoaded && model.cloudError == nil) || (!model.nearbyLoaded && model.nearbyError == nil) || (partial && catalog.detailProblems.isEmpty) {
                    ProgressView("Checking for alerts…").plainActivityRow()
                } else if active.isEmpty {
                    ContentUnavailableView(incomplete ? "No alerts from available devices" : "No alerts", systemImage: "bell").plainActivityRow()
                }
                if incomplete {
                    HStack {
                        Text("Some devices or recordings could not be checked.").font(.footnote).foregroundStyle(.secondary)
                        Spacer()
                        Button("Retry") { refresh += 1 }.font(.footnote.bold())
                    }.plainActivityRow()
                }
            case .events: eventRows
            case .archived:
                if state.archived.isEmpty {
                    ContentUnavailableView("Nothing archived", systemImage: "archivebox", description: Text("Swipe left on an alert to archive it.")).plainActivityRow()
                } else {
                    Text("Archived alerts stay on this iPhone for 30 days. Their recordings stay in Data.")
                        .font(.footnote).foregroundStyle(.secondary).plainActivityRow()
                    ForEach(state.archived.values.sorted { $0.archivedAt > $1.archivedAt }, id: \.summary.id) { entry in
                        HStack {
                            VStack(alignment: .leading) {
                                Text(entry.summary.title).font(.headline)
                                Text(entry.summary.deviceName).font(.caption).foregroundStyle(.secondary)
                            }
                            Spacer()
                            Button("Restore") { update { $0.archived.removeValue(forKey: entry.summary.id) } }
                                .accessibilityIdentifier("activity.restore." + entry.summary.id)
                        }.padding(14).background(RecordingAppearance.card, in: .rect(cornerRadius: 18)).plainActivityRow()
                    }
                }
            }
        }.listStyle(.plain).scrollContentBackground(.hidden).background(MobileLensStyle.background)
        .navigationTitle("Activity").navigationBarTitleDisplayMode(.large)
        .task(id: key) {
            if isVisible && phase == .active { update { $0.expire(now: Date()) } }
            if key.active { await catalog.load(key.targets, refresh: refresh, priority: nil, consumer: "activity") }
        }
        .task(id: softwareSession) {
            updateStatus = nil
            updateOwner = nil
            guard let session = softwareSession else { return }
            let status = try? await GoClient.shared.onlineUpdate(session: session, action: 0)
            guard !Task.isCancelled, model.deviceSession == session else { return }
            updateStatus = status
            updateOwner = session
        }
        .onChange(of: unread.count, initial: true) { _, count in catalog.unreadAlerts = count }
        .refreshable { refresh += 1 }
        .environment(\.recordingPosters, posters)
        .navigationDestination(item: $updateRoute) { session in
            if model.selectedDeviceReady, model.deviceSession == session, let device = model.connected {
                DeviceInfoView(session: session, deviceName: device.title).navigationTitle("Device update")
            } else { ContentUnavailableView("Device changed", systemImage: "bolt.slash") }
        }
        .navigationDestination(isPresented: Binding(get: { inspecting != nil }, set: { if !$0 { inspecting = nil } })) {
            if let alert = inspecting, let app = alert.appName { MobileAppDetail(model: model, device: alert.device, name: app) }
        }
        .sheet(item: $playing) { alert in
            NavigationStack {
                if let library = catalog.reads[recordingDeviceID(alert.device)]?.library, let episode = alert.episode {
                    RecordingEpisodePicker(library: library, trigger: nil, selectedID: episode.id)
                } else {
                    ContentUnavailableView("Recording unavailable", systemImage: "wifi.slash")
                }
            }.environment(\.recordingPosters, posters)
                .presentationDetents([.height(640), .large]).presentationCornerRadius(38)
                .presentationDragIndicator(.visible).presentationBackground(MobileLensStyle.background)
        }
    }

    private func archive(_ alert: ActivityAlert) {
        update { state in
            state.read.insert(alert.id)
            state.archived[alert.id] = ArchivedAlert(summary: alert.summary, archivedAt: Date())
        }
    }
    private func card(_ alert: ActivityAlert, unread: Bool) -> some View {
        VStack(spacing: 0) {
            Button {
                update { $0.read.insert(alert.id) }
                select(alert.device)
            } label: {
                HStack(spacing: 12) {
                    Image(systemName: alert.episode == nil ? "exclamationmark.triangle" : alert.episode?.kind == .audio ? "waveform" : "camera")
                        .frame(width: 36, height: 36).foregroundStyle(.white)
                        .background(alert.episode == nil ? Color.orange : MobileLensStyle.green, in: .rect(cornerRadius: 10))
                    VStack(alignment: .leading, spacing: 3) {
                        Text(alert.summary.title).font(.headline).lineLimit(2)
                        Text([alert.device.title, alert.episode?.campaign].compactMap { $0 }.joined(separator: " · "))
                            .font(.caption).foregroundStyle(.secondary).lineLimit(2)
                    }
                    Spacer()
                    VStack(alignment: .trailing) {
                        if let date = alert.summary.date { Text(RecordingAppearance.time(date)).font(.caption).foregroundStyle(.secondary) }
                        if unread { Circle().fill(MobileLensStyle.green).frame(width: 9, height: 9).accessibilityLabel("Unread") }
                    }
                }.contentShape(.rect)
            }.buttonStyle(.plain)
            if let episode = alert.episode {
                Button {
                    update { $0.read.insert(alert.id) }
                    playing = alert
                } label: {
                    RecordingThumbnail(device: alert.device, episode: episode)
                        .frame(height: episode.kind == .audio ? 96 : 180)
                        .overlay { Image(systemName: "play.fill").foregroundStyle(RecordingAppearance.ink).frame(width: 50, height: 50).background(.white.opacity(0.9), in: .circle) }
                        .overlay(alignment: .topLeading) { if let label = episode.trigger { badge(label) } }
                        .overlay(alignment: .bottomTrailing) { if let duration = episode.duration { badge(Duration.seconds(duration).formatted(.time(pattern: .minuteSecond))) } }
                        .overlay(alignment: .topTrailing) { if let synced = episode.synced { Image(systemName: synced ? "icloud.fill" : "internaldrive").font(.caption).foregroundStyle(.white).padding(5).background(.black.opacity(0.65), in: .circle).padding(8) } }
                        .clipShape(.rect(cornerRadius: 16))
                }.buttonStyle(.plain).accessibilityLabel("Play \(episode.title)").padding(.top, 10)
            } else {
                HStack {
                    Button("Open device") { update { $0.read.insert(alert.id) }; select(alert.device) }
                    if alert.appName != nil {
                        Button("Inspect app") { update { $0.read.insert(alert.id) }; inspecting = alert }
                    }
                    if alert.updateAvailable, updateOwner == model.deviceSession {
                        Button("Review update") { update { $0.read.insert(alert.id) }; updateRoute = model.deviceSession }
                    }
                }.buttonStyle(.bordered).padding(.top, 8)
            }
        }.padding(14).background(RecordingAppearance.card, in: .rect(cornerRadius: 22))
    }
    private func badge(_ value: String) -> some View {
        Text(value).font(.caption2.bold()).lineLimit(1).foregroundStyle(.white)
            .padding(.horizontal, 7).padding(.vertical, 4).background(.black.opacity(0.7), in: .capsule).padding(8)
    }

    @ViewBuilder private var eventRows: some View {
        let devices = model.nearby + model.cloud + [model.connected].compactMap { $0 }
        let events = model.mobileScopeIsCurrent ? model.experienceEvents : []
        let days = Dictionary(grouping: events) { Calendar.current.startOfDay(for: $0.date) }
        if days.isEmpty { ContentUnavailableView("No device events yet", systemImage: "clock").plainActivityRow() }
        ForEach(days.keys.sorted(by: >), id: \.self) { day in
            Section(Calendar.current.isDateInToday(day) ? "Today" : Calendar.current.isDateInYesterday(day) ? "Yesterday" : day.formatted(date: .abbreviated, time: .omitted)) {
                ForEach((days[day] ?? []).sorted { $0.date > $1.date }) { event in
                    Button { if let device = devices.first(where: { $0.id == event.deviceID }) { select(device) } } label: {
                        LensRow(title: event.title, subtitle: event.deviceName, symbol: event.symbol,
                                trailing: event.date.formatted(date: .omitted, time: .shortened))
                    }.buttonStyle(.plain).disabled(!devices.contains { $0.id == event.deviceID }).listRowBackground(RecordingAppearance.card)
                }
            }
        }
    }
}

private extension View {
    func plainActivityRow() -> some View {
        listRowInsets(EdgeInsets(top: 5, leading: 16, bottom: 5, trailing: 16))
            .listRowSeparator(.hidden).listRowBackground(Color.clear)
    }
}
