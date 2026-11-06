import SwiftUI

struct RecordingEpisodePicker: View {
    let library: MobileRecordingLibrary
    let issue: String?
    let retry: () -> Void
    @Environment(\.dismiss) private var dismiss
    @Environment(\.dynamicTypeSize) private var textSize
    @State private var trigger: String?
    @State private var selection: String?
    @State private var player: String?
    @State private var exportItems: [MobileRecordingEpisode]?
    @State private var proposedExport: [MobileRecordingEpisode]?
    @State private var files: MobileRecordingExport?
    @State private var exportError: String?

    init(library: MobileRecordingLibrary, trigger: String?, issue: String? = nil, retry: @escaping () -> Void = {}) {
        self.library = library
        self.issue = issue
        self.retry = retry
        _trigger = State(initialValue: trigger)
    }

    private var filtered: [MobileRecordingEpisode] {
        library.episodes(matching: trigger)
    }
    private var selected: MobileRecordingEpisode? { filtered.first { $0.id == selection } }
    private var incompleteFilter: Bool { selected == nil && trigger != nil && library.labelCoverage != nil }
    private var groups: [(date: Date?, episodes: [MobileRecordingEpisode])] {
        let calendar = Calendar.current
        let days = Dictionary(grouping: filtered) { $0.started.map { calendar.startOfDay(for: $0) } }
        return days.map { (date: $0.key, episodes: $0.value) }.sorted { ($0.date ?? .distantPast) > ($1.date ?? .distantPast) }
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack(alignment: .top, spacing: 12) {
                VStack(alignment: .leading, spacing: 2) {
                    Text(library.device.title).font(.title3.bold())
                        .lineLimit(textSize.isAccessibilitySize ? 2 : 1).truncationMode(.tail)
                    Text("\(library.episodes.count) \(library.episodes.count == 1 ? "episode" : "episodes") · \(library.sizeText)")
                        .font(.footnote).foregroundStyle(.secondary)
                }.frame(maxWidth: .infinity, alignment: .leading)
                Button { dismiss() } label: {
                    Image(systemName: "xmark").font(.footnote.bold()).frame(width: 36, height: 36)
                        .background(Color.secondary.opacity(0.12), in: .circle).frame(width: 44, height: 44)
                }.buttonStyle(.plain).accessibilityLabel("Close recordings")
            }.padding(.horizontal, 18).padding(.top, 20)
            ScrollView(.horizontal) {
                HStack(spacing: 6) {
                    filterChip(nil, count: library.episodes.count)
                    ForEach(library.labels, id: \.name) { item in filterChip(item.name, count: item.count) }
                }.padding(.horizontal, 16)
            }.scrollIndicators(.hidden)
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 14) {
                    ForEach(groups, id: \.date) { group in
                        VStack(alignment: .leading, spacing: 6) {
                            Text(dayTitle(group.date)).font(.caption.weight(.semibold)).foregroundStyle(.secondary)
                                .padding(.horizontal, 6)
                            LazyVStack(spacing: 0) {
                                ForEach(group.episodes) { episode in
                                    episodeRow(episode)
                                    if episode.id != group.episodes.last?.id { Divider().padding(.leading, 108) }
                                }
                            }.background(RecordingAppearance.card, in: .rect(cornerRadius: 20)).clipShape(.rect(cornerRadius: 20))
                        }
                    }
                    if filtered.isEmpty {
                        ContentUnavailableView("No saved episodes", systemImage: "cylinder.split.1x2")
                    }
                    if let warning = issue ?? library.warning {
                        Text(PresentedFailure(warning).details).font(.footnote).foregroundStyle(.secondary)
                        Button("Try again", action: retry)
                    }
                }.padding(.horizontal, 16).padding(.vertical, 8)
            }
            Divider()
            if exportItems != nil {
                Button("Cancel export") { exportItems = nil }
                    .font(.footnote).padding(.horizontal, 16).padding(.top, 8)
            } else if let coverage = library.labelCoverage {
                Text(incompleteFilter ? "\(coverage). Filtered export needs all labels." : coverage)
                    .font(.caption).foregroundStyle(.secondary).padding(.horizontal, 16).padding(.top, 8)
            }
            let buttons = textSize.isAccessibilitySize ? AnyLayout(VStackLayout(spacing: 8)) : AnyLayout(HStackLayout(spacing: 8))
            buttons {
                Button { beginExport() } label: {
                    if exportItems != nil { ProgressView().accessibilityLabel("Exporting recordings") }
                    else { Text("Export") }
                }.buttonStyle(LensPillStyle()).disabled(filtered.isEmpty || incompleteFilter || exportItems != nil)
                    .accessibilityHint(selected == nil ? "Export the episodes in this filter" : "Export the selected episode")
                Button { player = selected?.id } label: {
                    Text(selected == nil ? "Pick an episode" : "Play episode")
                        .font(.subheadline.weight(.semibold)).frame(maxWidth: .infinity, minHeight: 48)
                        .foregroundStyle(selected == nil ? Color.secondary : Color.white)
                        .background(selected == nil ? Color.secondary.opacity(0.10) : MobileLensStyle.green, in: .capsule)
                }.buttonStyle(.plain).disabled(selected == nil || exportItems != nil)
                    .accessibilityIdentifier("data.play")
            }.padding(.horizontal, 16).padding(.vertical, 10)
        }
        .background(MobileLensStyle.background)
        .navigationDestination(item: $player) { id in
            DataVisualizerView(session: 0, deviceName: library.device.title, initialEpisode: id,
                               recordingDevice: library.device, startPlaying: true)
                .id(id).navigationTitle(library.episodes.first { $0.id == id }?.title ?? "Episode")
                .navigationBarTitleDisplayMode(.inline)
        }
        .sheet(isPresented: Binding(get: { files != nil }, set: { if !$0 { files = nil } })) {
            if let files { RecordingShareSheet(files: files) }
        }
        .confirmationDialog("Export recordings?", isPresented: Binding(get: { proposedExport != nil },
                            set: { if !$0 { proposedExport = nil } }), titleVisibility: .visible, presenting: proposedExport) { items in
            Button("Export \(items.count) episodes") { exportItems = items }
        } message: { items in
            Text("The selected filter includes \(MobileRecordingLibrary.sizeText(for: items)) of saved recordings.")
        }
        .alert("Could not export recordings", isPresented: Binding(get: { exportError != nil }, set: { if !$0 { exportError = nil } })) {
            Button("OK") { exportError = nil }
        } message: { Text(exportError ?? "") }
        .task(id: exportItems?.map(\.id)) {
            guard let items = exportItems else { return }
            defer { if !Task.isCancelled { exportItems = nil } }
            do {
                let prepared = try await GoClient.shared.exportRecordingEpisodes(device: library.device, episodes: items)
                if !Task.isCancelled { files = prepared }
            } catch { if !Task.isCancelled { exportError = PresentedFailure(error.localizedDescription).details } }
        }
    }

    private func filterChip(_ name: String?, count: Int) -> some View {
        let active = name == trigger
        return Button {
            trigger = name
            if selected == nil { selection = nil }
        } label: {
            HStack(spacing: 6) {
                Circle().fill(name == nil && active ? .white : RecordingAppearance.dot(name)).frame(width: 7, height: 7)
                Text(name ?? "All")
                Text("\(count)").opacity(0.7)
            }.font(.footnote.weight(.semibold)).padding(.horizontal, 12).padding(.vertical, 7)
                .foregroundStyle(active ? Color.white : .primary)
                .background(active ? RecordingAppearance.ink : RecordingAppearance.card, in: .capsule)
                .frame(minHeight: 44).contentShape(.rect)
        }.buttonStyle(.plain).accessibilityAddTraits(active ? .isSelected : [])
    }

    private func episodeRow(_ episode: MobileRecordingEpisode) -> some View {
        let picked = selection == episode.id
        return Button { selection = episode.id } label: {
            HStack(spacing: 12) {
                RecordingThumbnail(device: library.device, episode: episode).frame(width: 84, height: 54).clipShape(.rect(cornerRadius: 10))
                VStack(alignment: .leading, spacing: 3) {
                    Text(episode.title).font(.subheadline.weight(.semibold))
                        .lineLimit(textSize.isAccessibilitySize ? 2 : 1).truncationMode(.tail)
                    Text([episode.started?.formatted(date: .omitted, time: .shortened),
                          episode.duration.map { Duration.seconds($0).formatted(.time(pattern: .minuteSecond)) },
                          episode.kind.rawValue].compactMap { $0 }.joined(separator: " · "))
                        .font(.caption).foregroundStyle(.secondary)
                    if let trigger = episode.trigger {
                        HStack(spacing: 4) {
                            Circle().fill(RecordingAppearance.dot(trigger)).frame(width: 6, height: 6)
                            Text(trigger).font(.caption2.weight(.semibold))
                                .lineLimit(textSize.isAccessibilitySize ? 2 : 1).truncationMode(.tail)
                        }
                    }
                }.frame(maxWidth: .infinity, alignment: .leading)
                if picked { Image(systemName: "checkmark").font(.body.bold()).foregroundStyle(MobileLensStyle.accent) }
            }.padding(.horizontal, 12).padding(.vertical, 10).contentShape(.rect)
                .background(picked ? MobileLensStyle.green.opacity(0.09) : .clear)
        }.buttonStyle(.plain).accessibilityAddTraits(picked ? .isSelected : []).accessibilityIdentifier("data.episode." + episode.id)
    }

    private func dayTitle(_ date: Date?) -> String {
        guard let date else { return "DATE UNKNOWN" }
        if Calendar.current.isDateInToday(date) { return "TODAY" }
        if Calendar.current.isDateInYesterday(date) { return "YESTERDAY" }
        return date.formatted(date: .long, time: .omitted).uppercased()
    }

    private func beginExport() {
        guard exportItems == nil, !incompleteFilter else { return }
        let items = library.exportTargets(trigger: trigger, selectedID: selection)
        guard !items.isEmpty else { return }
        if items.count > 1 { proposedExport = items } else { exportItems = items }
    }
}

private struct RecordingShareSheet: UIViewControllerRepresentable {
    let files: MobileRecordingExport
    func makeUIViewController(context: Context) -> UIActivityViewController {
        UIActivityViewController(activityItems: files.urls, applicationActivities: nil)
    }
    func updateUIViewController(_ controller: UIActivityViewController, context: Context) {}
}
