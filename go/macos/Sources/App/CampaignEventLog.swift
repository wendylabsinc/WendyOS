import SwiftUI

/// Read receipts belong to a device and clip ID, not to a connection or timestamp.
@MainActor
final class CampaignEventStore: ObservableObject {
    @Published private(set) var clips: [CampaignClip] = []
    @Published private(set) var readIDs: Set<String>
    @Published private(set) var loading = false
    @Published private(set) var error: String?
    @Published private(set) var warning = ""
    private let defaults: UserDefaults
    private let key: String
    private var lastEpisodes: [DataVisualEpisode]?

    init(deviceID: String, defaults: UserDefaults = .standard) {
        self.defaults = defaults
        key = "campaign-events.read.\(deviceID)"
        readIDs = Set(defaults.stringArray(forKey: key) ?? [])
    }

    var hasUnread: Bool { clips.contains { !readIDs.contains($0.id) } }
    var latest: [CampaignClip] { Array(clips.prefix(3)) }

    func receive(_ snapshot: CampaignClipsSnapshot) {
        var unique: [String: CampaignClip] = [:]
        for clip in snapshot.clips { unique[clip.id] = clip }
        clips = unique.values.sorted {
            $0.started == $1.started ? $0.id < $1.id : $0.started > $1.started
        }
        warning = snapshot.warning
        error = nil
    }

    func markRead(_ ids: [String]) {
        readIDs.formUnion(ids)
        defaults.set(readIDs.sorted(), forKey: key)
    }

    func refresh(session: UInt64, force: Bool = true) async {
        guard !loading else { return }
        loading = true
        defer { loading = false }
        do {
            // Poll the inexpensive episode index first. Fetch manifests only when
            // recordings change, or when the user explicitly asks for a refresh.
            var episodes: [DataVisualEpisode]?
            if !force {
                episodes = try await GoClient.shared.visualEpisodes(session: session).sorted { $0.id < $1.id }
                guard !Task.isCancelled else { return }
                if episodes == lastEpisodes && error == nil && warning.isEmpty { return }
            }
            let snapshot = try await GoClient.shared.campaignClips(session: session)
            guard !Task.isCancelled else { return }
            receive(snapshot)
            lastEpisodes = episodes
        } catch {
            if !Task.isCancelled { self.error = error.localizedDescription }
        }
    }
}

struct CampaignEventButton: View {
    let session: UInt64
    let deviceName: String
    @Binding var showingLog: Bool
    @StateObject private var events: CampaignEventStore
    @State private var showingRecent = false
    @State private var selectedClip: String?
    @State private var search = ""
    @State private var actions = AsyncActions()
    @Environment(\.scenePhase) private var scenePhase

    init(session: UInt64, deviceID: String, deviceName: String, showingLog: Binding<Bool>) {
        self.session = session
        self.deviceName = deviceName
        _showingLog = showingLog
        _events = StateObject(wrappedValue: CampaignEventStore(deviceID: deviceID))
    }

    var body: some View {
        Button {
            showingRecent.toggle()
        } label: {
            Image(systemName: "bell")
                .overlay(alignment: .topTrailing) {
                    if events.hasUnread {
                        Circle().fill(.red).frame(width: 7, height: 7).offset(x: 4, y: -3)
                    }
                }
        }
        .accessibilityLabel("Event log")
        .accessibilityValue(events.hasUnread ? "New activity" : "No unread activity")
        .help("Recent campaign events")
        .popover(isPresented: $showingRecent) {
            VStack(alignment: .leading, spacing: 0) {
                HStack {
                    Text("Recent activity").font(.headline)
                    Spacer()
                    if events.loading { ProgressView().controlSize(.small) }
                }.padding()
                status
                if events.clips.isEmpty && !events.loading {
                    Text("No events yet. Campaign recordings will appear here.")
                        .foregroundStyle(.secondary).padding()
                }
                ForEach(events.latest) { clip in
                    Divider()
                    Button {
                        selectedClip = clip.id
                        showingRecent = false
                        showingLog = true
                    } label: { CampaignEventRow(clip: clip, unread: false) }
                        .buttonStyle(.plain)
                }
                Divider()
                Button("More…") {
                    selectedClip = nil
                    showingRecent = false
                    showingLog = true
                }.buttonStyle(.bordered).padding().frame(maxWidth: .infinity, alignment: .trailing)
            }
            .frame(idealWidth: 370, maxWidth: 420)
            .presentationCompactAdaptation(.popover)
        }
        .sheet(isPresented: $showingLog) {
            NavigationStack {
                VStack(spacing: 0) {
                    status
                    if events.clips.isEmpty && !events.loading {
                        ContentUnavailableView("No events yet", systemImage: "bell", description: Text("Campaign recordings on \(deviceName) will appear here."))
                    } else {
                        List {
                            ForEach(filteredClips) { clip in
                                Button {
                                    events.markRead([clip.id])
                                    selectedClip = clip.id
                                } label: { CampaignEventRow(clip: clip, unread: !events.readIDs.contains(clip.id)) }
                                    .buttonStyle(.plain)
                            }
                            if filteredClips.isEmpty { Text("No matching events").foregroundStyle(.secondary) }
                        }
                        .searchable(text: $search, prompt: "Search campaigns and events")
                        .refreshable { await events.refresh(session: session) }
                    }
                }
                .navigationTitle("Event log")
                .navigationDestination(item: $selectedClip) { id in
                    DataVisualizerView(session: session, deviceName: deviceName, initialEpisode: id)
                        .navigationTitle("Recording")
                }
                .toolbar {
                    ToolbarItem(placement: .confirmationAction) {
                        Button("Done") { showingLog = false }.keyboardShortcut(.cancelAction)
                    }
                    ToolbarItem {
                        Button("Refresh", systemImage: "arrow.clockwise") { actions.submit { await events.refresh(session: session) } }
                            .disabled(events.loading)
                    }
                }
            }.desktopFrame(width: 780, height: 600)
        }
        .onChange(of: showingRecent) { _, showing in
            if showing { events.markRead(events.clips.map(\.id)) }
        }
        .onChange(of: showingLog) { _, showing in
            if showing { events.markRead(events.clips.map(\.id)) }
            else { selectedClip = nil }
        }
        .onChange(of: events.clips.map(\.id)) { _, _ in
            if showingRecent || showingLog { events.markRead(events.clips.map(\.id)) }
        }
        .task { await actions.run() }
        .task(id: scenePhase) {
            guard scenePhase == .active else { return }
            while !Task.isCancelled {
                await events.refresh(session: session, force: false)
                do { try await Task.sleep(for: .seconds(5)) } catch { return }
            }
        }
    }

    private var filteredClips: [CampaignClip] {
        events.clips.filter { search.isEmpty || ($0.campaign + " " + $0.reason).localizedCaseInsensitiveContains(search) }
    }

    @ViewBuilder private var status: some View {
        if let error = events.error {
            Text("Could not refresh events: \(error)").font(.caption).foregroundStyle(.orange).padding()
        }
        if !events.warning.isEmpty { Text(events.warning).font(.caption).foregroundStyle(.orange).padding() }
    }
}

private struct CampaignEventRow: View {
    let clip: CampaignClip
    let unread: Bool
    var body: some View {
        HStack(alignment: .top, spacing: 12) {
            Image(systemName: "video.badge.waveform").foregroundStyle(.tint).font(.title3)
            VStack(alignment: .leading, spacing: 5) {
                HStack {
                    Text(clip.campaign).font(.headline).lineLimit(1)
                    if unread { Circle().fill(.red).frame(width: 6, height: 6) }
                }
                if !clip.reason.isEmpty { Text(clip.reason).lineLimit(2) }
                Text(clip.started.formatted(date: .abbreviated, time: .standard))
                    .font(.caption).foregroundStyle(.secondary)
                Text("\(clip.durationLabel) · \(clip.state.capitalized)")
                    .font(.caption).foregroundStyle(.secondary)
            }
            Spacer(minLength: 8)
            Image(systemName: "play.circle").foregroundStyle(.tint)
        }
        .padding(12).frame(maxWidth: .infinity, alignment: .leading).contentShape(Rectangle())
    }
}
