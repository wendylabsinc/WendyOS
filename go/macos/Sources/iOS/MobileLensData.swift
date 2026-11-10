import SwiftUI

struct MobileEpisodeRoute: Identifiable {
    let id = UUID()
    let device: Device
    var trigger: String?
}

// Enrolled devices can appear both on the LAN and through Cloud. A display
// name or IP address alone cannot establish that two recordings have one owner.
func recordingDeviceID(_ device: Device) -> String {
    guard let url = URL(string: device.id) else { return device.id }
    let path = url.pathComponents
    guard path.count == 5, path[1] == "tenant",
          (url.scheme == "spiffe" && url.host == "wendy.sh" && path[3] == "device") ||
          (url.scheme == "cloud" && path[3] == "asset") else { return device.id }
    return "\(path[2])/\(path[4])"
}

struct MobileRecordingTarget: Hashable {
    let id: String
    var routes: [Device]
}

struct RecordingSources: Hashable {
    var targets: [MobileRecordingTarget] = []
    var unavailable = 0
    var inventory: [MobileRecordingTarget] = []

    @MainActor init(model: AppModel) {
        guard model.mobileScopeIsCurrent else { return }
        var routes: [String: [Device]] = [:]
        let nearby = model.nearby.filter { !$0.lite && !$0.selector.hasPrefix("vm:") }
        for device in nearby { routes[recordingDeviceID(device), default: []].append(device) }
        let fleet = model.mobilePresence
        let fresh = fleet.map { Date().timeIntervalSince($0.1) < 60 && $0.0.presenceError.isEmpty } == true && model.cloudError == nil
        var unreachable = Set<String>()
        for row in fleet?.0.devices ?? [] where !row.lite {
            let device = row.device, id = recordingDeviceID(device)
            if fresh && row.online { routes[id, default: []].append(device) }
            else { unreachable.insert(id) }
        }
        // Include a device explicitly connected by address, even if it wasn't advertised.
        if model.selectedDeviceReady, !model.connectedLite, let device = model.connected,
           device.transport != .cloud, !device.selector.hasPrefix("vm:"),
           routes[recordingDeviceID(device)] == nil { routes[recordingDeviceID(device)] = [device] }
        unavailable = unreachable.subtracting(routes.keys).count
        var knownRoutes = routes
        for row in fleet?.0.devices ?? [] where !row.lite {
            let id = recordingDeviceID(row.device)
            if knownRoutes[id] == nil { knownRoutes[id] = [row.device] }
        }
        inventory = knownRoutes.map { MobileRecordingTarget(id: $0.key, routes: $0.value) }.sorted { $0.id < $1.id }
        targets = routes.map { id, devices in
            MobileRecordingTarget(id: id, routes: devices.sorted {
                if ($0.transport == .cloud) != ($1.transport == .cloud) { return $1.transport == .cloud }
                return $0.selector < $1.selector
            })
        }.sorted { $0.id < $1.id }
    }
}

struct RecordingRead: Sendable {
    let target: MobileRecordingTarget
    var library: MobileRecordingLibrary?
    var problem: String?

    static func fetch(_ target: MobileRecordingTarget) async -> Self {
        var result = Self(target: target)
        for device in target.routes {
            guard !Task.isCancelled else { break }
            do {
                result.library = try await GoClient.shared.recordingLibrary(device: device)
                result.problem = nil
                break
            } catch { result.problem = error.localizedDescription }
        }
        return result
    }
}

@MainActor final class RecordingCatalog: ObservableObject {
    @Published var reads: [String: RecordingRead] = [:]
    @Published var loading = false
    @Published var unreadAlerts = 0
    @Published var detailProblems: [String: String] = [:]
    private var generation = 0
    // In-memory only: MobileRecordingScope is recreated for each account/org.
    // Independent screen refresh counters must not invalidate one another.
    private var lastRefresh: [String: Int] = [:]
    private var refreshedAt: [String: Date] = [:]
    private let now: () -> Date
    private let fetch: @Sendable (MobileRecordingTarget) async -> RecordingRead
    private let fetchDetails: @Sendable (Device, [MobileRecordingEpisode]) async throws -> [MobileRecordingEpisode]

    init(now: @escaping () -> Date = Date.init,
         fetch: @escaping @Sendable (MobileRecordingTarget) async -> RecordingRead = RecordingRead.fetch,
         details: @escaping @Sendable (Device, [MobileRecordingEpisode]) async throws -> [MobileRecordingEpisode] = { device, episodes in
             try await GoClient.shared.recordingDetails(device: device, episodes: episodes)
         }) {
        self.now = now
        self.fetch = fetch
        self.fetchDetails = details
    }
    private var attempted: [String: Set<String>] = [:]

    func load(_ targets: [MobileRecordingTarget], refresh: Int, priority: String?, consumer: String = "data", retaining inventory: [MobileRecordingTarget]? = nil) async {
        generation += 1
        let run = generation
        let knownIDs = Set((inventory ?? targets).map(\.id))
        reads = reads.filter { knownIDs.contains($0.key) }
        refreshedAt = refreshedAt.filter { reads[$0.key] != nil }
        let force = lastRefresh[consumer].map { $0 != refresh } ?? (refresh != 0)
        if force { attempted = [:]; detailProblems = [:] }
        lastRefresh[consumer] = refresh
        let remaining = targets.filter { $0.id != priority }
        // Check nearby routes first so a cloud fleet cannot delay a local device.
        let ordered = targets.filter { $0.id == priority }
            + remaining.filter { $0.routes.first?.transport != .cloud }
            + remaining.filter { $0.routes.first?.transport == .cloud }
        let pending = ordered.filter { target in
            force || reads[target.id]?.library == nil || reads[target.id]?.problem != nil ||
                refreshedAt[target.id].map { now().timeIntervalSince($0) >= 30 } != false
        }
        let fetch = self.fetch
        loading = !pending.isEmpty
        defer { if generation == run { loading = false } }
        if !force, let priority, reads[priority]?.library != nil {
            await loadDetails(for: [priority], run: run)
        }
        await withTaskGroup(of: RecordingRead.self) { group in
            var remaining = pending.makeIterator()
            for _ in 0..<3 {
                if !Task.isCancelled, let target = remaining.next() { group.addTask { await fetch(target) } }
            }
            for await read in group {
                guard !Task.isCancelled, run == generation else { group.cancelAll(); return }
                var current = read
                if read.library == nil, let saved = reads[read.target.id]?.library {
                    current.library = saved
                    current.problem = "Couldn’t refresh. Showing saved recordings. " + (read.problem ?? "Try again.")
                }
                if let library = read.library, let previous = reads[read.target.id]?.library {
                    // Finalized IDs retain their recording metadata. Keep it across
                    // refreshes while applying any freshly reported manifest first.
                    let known = Dictionary(previous.episodes.filter(\.detailsLoaded).map { ($0.id, $0) }, uniquingKeysWith: { a, _ in a })
                    let episodes = library.episodes.map { row in
                        guard !row.detailsLoaded, let cached = known[row.id], row.bytes == cached.bytes,
                              row.started == cached.started, row.title == cached.title else { return row }
                        return cached
                    }
                    current.library = MobileRecordingLibrary(device: library.device, model: library.model ?? previous.model, episodes: episodes,
                                                            warning: episodes.contains { !$0.detailsLoaded } ? library.warning : nil)
                }
                if read.library != nil {
                    refreshedAt[read.target.id] = now()
                    attempted[read.target.id] = nil
                    detailProblems[read.target.id] = nil
                }
                reads[read.target.id] = current
                if let target = remaining.next() { group.addTask { await fetch(target) } }
            }
        }
        guard !Task.isCancelled, generation == run else { return }
        loading = false
        await loadDetails(for: ordered.map(\.id), run: run)
    }

    private func loadDetails(for ids: [String], run: Int) async {
        // One batch per device per round keeps smaller libraries responsive
        // while a board with thousands of recordings is being indexed.
        while !Task.isCancelled, generation == run {
            var hadWork = false
            for id in ids {
                guard !Task.isCancelled, generation == run else { return }
                guard let read = reads[id], read.problem == nil, let library = read.library else { continue }
                let visited = attempted[id] ?? []
                let batch = Array(library.episodes.lazy.filter { !$0.detailsLoaded && !visited.contains($0.id) }
                    .prefix(MobileRecordingEpisode.detailBatchLimit))
                guard !batch.isEmpty else { continue }
                hadWork = true
                do {
                    let updated = try await fetchDetails(library.device, batch)
                    guard !Task.isCancelled, generation == run else { return }
                    let known = Dictionary(updated.filter(\.detailsLoaded).map { ($0.id, $0) }, uniquingKeysWith: { a, _ in a })
                    let episodes = library.episodes.map { known[$0.id] ?? $0 }
                    var next = read
                    next.library = MobileRecordingLibrary(device: library.device, model: library.model, episodes: episodes, warning: nil)
                    reads[id] = next
                    attempted[id, default: []].formUnion(batch.map(\.id))
                    if known.count < batch.count { detailProblems[id] = "Some episode details could not be read on \(library.device.title)." }
                } catch {
                    guard !Task.isCancelled, generation == run else { return }
                    detailProblems[id] = "\(library.device.title): \(PresentedFailure(error.localizedDescription).details)"
                    attempted[id, default: []].formUnion(library.episodes.lazy.filter { !$0.detailsLoaded }.map(\.id))
                }
            }
            if !hadWork { return }
        }
    }
}

struct MobileLensData: View {
    @ObservedObject var model: AppModel
    let isVisible: Bool
    @Binding var picker: MobileEpisodeRoute?
    @Environment(\.scenePhase) private var phase
    @EnvironmentObject private var catalog: RecordingCatalog
    @Environment(\.recordingPosters) private var posters
    @State private var refresh = 0
    @State private var showingProblems = false
    @State private var kind: MobileRecordingKind?

    private var sources: RecordingSources { RecordingSources(model: model) }
    private var libraries: [MobileRecordingLibrary] {
        catalog.reads.values.compactMap(\.library).map { library in
            MobileRecordingLibrary(device: library.device, model: library.model,
                episodes: library.episodes(matching: nil, kind: kind), warning: library.warning)
        }.filter { !$0.episodes.isEmpty }.sorted {
            let left = $0.episodes.first?.started ?? .distantPast, right = $1.episodes.first?.started ?? .distantPast
            return left == right ? recordingDeviceID($0.device) < recordingDeviceID($1.device) : left > right
        }
    }
    private var emptyCount: Int { catalog.reads.values.filter { $0.library?.episodes.isEmpty == true }.count }
    private var problems: [String] {
        var lines = [model.nearbyError, model.cloudError, model.mobileCloudSelectionError,
                     model.mobilePresence?.0.presenceError].compactMap { $0 }.filter { !$0.isEmpty }.map { PresentedFailure($0).details }
        for result in catalog.reads.values {
            guard let problem = result.problem ?? result.library?.warning, let device = result.target.routes.first else { continue }
            lines.append("\(device.title): \(PresentedFailure(problem).details)")
        }
        lines.append(contentsOf: catalog.detailProblems.filter { catalog.reads[$0.key] != nil }.values)
        return lines.sorted()
    }

    private struct LoadKey: Hashable { let active: Bool; let targets: [MobileRecordingTarget]; let refresh: Int; let priority: String? }
    var body: some View {
        let targets = sources.targets
        let key = LoadKey(active: isVisible && phase != .background && model.mobileScopeIsCurrent, targets: targets, refresh: refresh,
                          priority: picker.map { recordingDeviceID($0.device) })
        ScrollView {
            LazyVStack(alignment: .leading, spacing: 12) {
                RecordingKindFilter(selection: $kind)
                if kind != nil && catalog.reads.values.contains(where: { $0.library?.labelCoverage != nil }) {
                    Text("Results are partial while episode details load.").font(.caption).foregroundStyle(.secondary)
                }
                ForEach(libraries, id: \.device.id) { library in
                    RecordingDeviceCard(library: library) { label in
                        picker = MobileEpisodeRoute(device: library.device, trigger: label)
                    }
                }
                if catalog.loading || !model.mobileScopeIsCurrent || (!model.nearbyLoaded && model.nearbyError == nil) ||
                    (!model.mobileCloudOrganization.isEmpty && !model.cloudLoaded && model.cloudError == nil) {
                    if libraries.isEmpty {
                        MobileLoadingPlaceholder(label: "Loading recordings…", style: .recordings)
                            .padding(16).background(RecordingAppearance.card, in: .rect(cornerRadius: 22))
                    } else { ProgressView("Updating recordings…").padding(.vertical, 10) }
                } else if libraries.isEmpty {
                    let incomplete = !problems.isEmpty || sources.unavailable > 0 || kind != nil
                    ContentUnavailableView(incomplete ? "No recordings to show" : "No saved recordings",
                                           systemImage: "cylinder.split.1x2",
                                           description: Text(incomplete ? (kind != nil ? "Try another kind, or wait for episode details." : "Some devices could not be checked.") : "Devices appear here after they save their first episode."))
                }
                if emptyCount > 0 {
                    Label(emptyCount == 1 ? "1 device has no saved data" : "\(emptyCount) devices have no saved data",
                          systemImage: "minus.circle").font(.footnote).foregroundStyle(.secondary).padding(.horizontal, 6)
                }
                if sources.unavailable > 0 {
                    Text("\(sources.unavailable) \(sources.unavailable == 1 ? "device is" : "devices are") unavailable")
                        .font(.footnote).foregroundStyle(.secondary).padding(.horizontal, 6)
                }
                if !problems.isEmpty {
                    DisclosureGroup("Some recording details are unavailable", isExpanded: $showingProblems) {
                        ForEach(Array(problems.enumerated()), id: \.offset) { _, message in
                            Text(message).font(.footnote).foregroundStyle(.secondary).frame(maxWidth: .infinity, alignment: .leading)
                        }
                        Button("Try again") { refresh += 1 }
                    }.font(.subheadline).padding(.horizontal, 6)
                }
            }.padding(.horizontal, 16).padding(.bottom, 20).frame(maxWidth: 700).frame(maxWidth: .infinity)
        }
        .background(MobileLensStyle.background).navigationTitle("Data").navigationBarTitleDisplayMode(.large)
        .task(id: key) { if key.active { await catalog.load(targets, refresh: refresh, priority: key.priority, retaining: sources.inventory) } }
        .onChange(of: phase) { old, new in if old == .background && new != .background { refresh += 1 } }
        .refreshable {
            async let nearby: Void = model.refreshNearby()
            async let cloud: Void = model.refreshCloud()
            _ = await (nearby, cloud)
            refresh += 1
        }
        .sheet(item: $picker) { route in
            let id = recordingDeviceID(route.device)
            NavigationStack {
                if let library = catalog.reads[id]?.library {
                    RecordingEpisodePicker(library: library, trigger: route.trigger, kind: kind, issue: catalog.detailProblems[id], retry: { refresh += 1 })
                } else {
                    Group {
                        if let issue = catalog.reads[id]?.problem {
                            FailureView(title: "Could not load recordings", error: issue) { refresh += 1 }
                        } else if targets.contains(where: { $0.id == id }) {
                            MobileLoadingPlaceholder(label: "Loading recordings…", style: .recordings).padding(20)
                        } else {
                            ContentUnavailableView("Device unavailable", systemImage: "wifi.slash", description: Text(route.device.title))
                        }
                    }.toolbar { ToolbarItem(placement: .topBarTrailing) { Button("Close recordings", systemImage: "xmark") { picker = nil } } }
                }
            }
            .presentationDetents([.height(640), .large]).presentationCornerRadius(38).presentationDragIndicator(.visible)
            .presentationBackground(MobileLensStyle.background)
        }
        .environment(\.recordingPosters, key.active ? posters : nil)
    }
}
