import SwiftUI

struct CampaignRecordingsView: View {
    @State private var actions = AsyncActions()
    let session: UInt64
    let deviceName: String
    @State var campaign: DeviceCampaign
    @State private var clips: [CampaignClip] = []
    @State private var warning = ""
    @State private var error: String?
    @State private var loading = true
    @State private var settings = false
    var body: some View {
        actionContent.task { await actions.run() }
    }

    @ViewBuilder private var actionContent: some View {
        List {
            Section {
                LabeledContent("Status", value: campaign.state.capitalized)
                Text(campaign.summary).font(.subheadline).foregroundStyle(.secondary)
                if !campaign.problem.isEmpty { FailureNote(message: campaign.problem) }
                if !campaign.warnings.isEmpty { Text(campaign.warnings).font(.caption).foregroundStyle(.orange) }
            }
            Section("Recordings") {
                if loading && clips.isEmpty { ProgressView("Loading recordings…") }
                if let error { FailureNote(message: error) }
                if !warning.isEmpty { Text(warning).font(.footnote).foregroundStyle(.secondary) }
                ForEach(clips) { clip in
                    NavigationLink {
                        DataVisualizerView(session: session, deviceName: deviceName, initialEpisode: clip.id)
                            .navigationTitle("Recording").navigationBarTitleDisplayMode(.inline)
                    } label: {
                        VStack(alignment: .leading, spacing: 6) {
                            Text(clip.started.formatted(date: .abbreviated, time: .shortened)).font(.headline)
                            if !clip.reason.isEmpty { Text(clip.reason).font(.subheadline).foregroundStyle(.secondary) }
                            Text(clip.durationLabel + " · " + ByteCountFormatter.string(fromByteCount: clip.bytes, countStyle: .file) + " · " + clip.state.capitalized)
                                .font(.caption).foregroundStyle(.secondary)
                        }.padding(.vertical, 4)
                    }
                }
                if !loading && clips.isEmpty && error == nil {
                    ContentUnavailableView("No recordings yet", systemImage: "film.stack", description: Text("Recordings appear here when this campaign triggers."))
                }
            }
        }.listStyle(.insetGrouped)
            .navigationTitle(campaign.name).navigationBarTitleDisplayMode(.inline)
            .toolbar { ToolbarItem(placement: .topBarTrailing) { Button("Campaign settings", systemImage: "gearshape") { settings = true } } }
            .sheet(isPresented: $settings) { CampaignSettingsView(session: session, name: campaign.name) { actions.submit { await refresh() } } }
            .task { await refresh() }
            .refreshable { await refresh() }
    }
    private func refresh() async {
        loading = true; error = nil; defer { loading = false }
        do {
            let result = try await GoClient.shared.campaignClips(session: session)
            guard !Task.isCancelled else { return }
            clips = result.clips.filter { $0.campaign == campaign.name }.sorted { $0.started > $1.started }
            warning = result.warning
            let values = try await GoClient.shared.campaigns(session: session)
            if let updated = values.first(where: { $0.name == campaign.name }), !Task.isCancelled { campaign = updated }
        } catch { if !Task.isCancelled { self.error = error.localizedDescription } }
    }
}
