import SwiftUI

struct CampaignsView: View {
    let session: UInt64
    let deviceName: String
    let openClip: (String) -> Void
    @State private var campaigns: [DeviceCampaign] = []
    @State private var loading = false
    @State private var error: String?
    @State private var creating = false
    @State private var clips: [CampaignClip] = []
    @State private var clipWarning = ""
    @State private var clipError: String?
    @State private var loadingClips = true
    @State private var selectedCampaign: DeviceCampaign?
    @State private var editingCampaign: DeviceCampaign?
    var body: some View {
        Group {
            #if os(iOS)
            mobileContent
            #else
            desktopContent
            #endif
        }
        .sheet(item: $editingCampaign) { campaign in CampaignSettingsView(session: session, name: campaign.name) { Task { await refresh() } } }
        .task { await refresh() }
        .sheet(isPresented: $creating) { CampaignEditor(session: session, deviceName: deviceName) { Task { await refresh() } } }
    }
    #if os(iOS)
    private var mobileContent: some View {
        List {
            Section {
                ForEach(campaigns) { campaign in
                    NavigationLink {
                        CampaignRecordingsView(session: session, deviceName: deviceName, campaign: campaign)
                    } label: {
                        VStack(alignment: .leading, spacing: 6) {
                            Text(campaign.name).font(.headline)
                            Text(campaign.summary).font(.subheadline).foregroundStyle(.secondary).lineLimit(3)
                            HStack(spacing: 6) {
                                Circle().fill(campaign.state == "armed" ? Color.green : .secondary).frame(width: 6, height: 6)
                                Text(campaign.state.capitalized)
                                if !loadingClips { Text("· " + clipCountLabel(campaign.name)) }
                            }.font(.caption).foregroundStyle(.secondary)
                            if !campaign.problem.isEmpty { Text(campaign.problem).font(.caption).foregroundStyle(.orange).lineLimit(2) }
                        }.padding(.vertical, 4)
                    }
                }
                if campaigns.isEmpty && !loading {
                    ContentUnavailableView("No campaigns", systemImage: "bolt.horizontal.circle", description: Text("Create a campaign to record when an event happens."))
                    Button("Create campaign", systemImage: "plus") { creating = true }
                }
                if loading && campaigns.isEmpty { ProgressView("Loading campaigns…") }
                if let error { FailureNote(message: error) }
            }
            Section {
                NavigationLink {
                    DataVisualizerView(session: session, deviceName: deviceName).navigationTitle("All recordings").navigationBarTitleDisplayMode(.inline)
                } label: { Label("All recordings", systemImage: "film.stack") }
            }
        }.listStyle(.insetGrouped)
            .refreshable { await refresh() }
            .toolbar { ToolbarItem(placement: .topBarTrailing) { Button("Create campaign", systemImage: "plus") { creating = true } } }
    }
    #endif
    private var desktopContent: some View {
        VStack(alignment: .leading, spacing: 12) {
            HStack {
                Label("Campaigns", systemImage: "bolt.horizontal.circle").font(.headline)
                if loading { ProgressView().controlSize(.small) }
                Spacer()
                Button { creating = true } label: { Label("Create campaign", systemImage: "plus") }
            }
            if let error { FailureNote(message: error) }
            if campaigns.isEmpty && !loading {
                ContentUnavailableView("When this happens, do that", systemImage: "bolt.horizontal.circle",
                                       description: Text("Detect a human on any camera, send a notification, and keep a clip on \(deviceName)."))
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
                    .mobileRefreshablePlaceholder { await refresh() }
            } else {
                ScrollView {
                    LazyVStack(spacing: 12) {
                        ForEach(campaigns) { campaign in
                            VStack(alignment: .leading, spacing: 10) {
                                HStack {
                                    Text(campaign.name).font(.headline)
                                    Spacer()
                                    if loadingClips { ProgressView().controlSize(.small) }
                                    else {
                                        Button { selectedCampaign = campaign } label: {
                                            Label(clipCountLabel(campaign.name), systemImage: "film.stack")
                                        }.buttonStyle(.bordered)
                                            .help("View recordings stored on this device")
                                    }
                                    Button("Campaign settings", systemImage: "gearshape") { editingCampaign = campaign }.labelStyle(.iconOnly)
                                    Label(campaign.state.capitalized, systemImage: "circle.fill").font(.caption)
                                        .foregroundStyle(campaign.state == "armed" ? Color.green : .secondary)
                                }
                                Text(campaign.summary).foregroundStyle(.secondary)
                                if !campaign.inferenceState.isEmpty { Text("Detection: \(campaign.inferenceState)").font(.caption) }
                                if !campaign.problem.isEmpty { FailureNote(message: campaign.problem) }
                                if !campaign.warnings.isEmpty { Text(campaign.warnings).font(.caption).foregroundStyle(.orange) }
                            }.padding(18).frame(maxWidth: .infinity, alignment: .leading)
                                .background(.quaternary.opacity(0.5), in: RoundedRectangle(cornerRadius: 12))
                        }
                    }
                }.mobileRefreshable { await refresh() }
            }
            Text("Rules run on this device. Notification delivery also requires an enabled grant for the campaign in Wendy Cloud Apps.")
                .font(.caption).foregroundStyle(.secondary)
        }.padding(.horizontal, 20).padding(.bottom, 16)
            .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
            .focusedSceneValue(\.devicePaneRefresh, PaneRefreshAction(enabled: !loading, perform: { Task { await refresh() } }))
            .sheet(item: $selectedCampaign) { campaign in
                CampaignClipsView(campaign: campaign.name, deviceName: deviceName,
                                  clips: clips.filter { $0.campaign == campaign.name }, warning: clipWarning,
                                  error: clipError, loading: loadingClips, refresh: { await refreshClips() }) { id in
                    selectedCampaign = nil
                    openClip(id)
                }
            }
    }
    private func clipCountLabel(_ campaign: String) -> String {
        if clipError != nil { return "Clips unavailable" }
        let count = clips.filter { $0.campaign == campaign }.count
        return "\(count)\(clipWarning.isEmpty ? "" : "+") \(count == 1 && clipWarning.isEmpty ? "clip" : "clips")"
    }
    private func refresh() async {
        loading = true; error = nil; defer { loading = false }
        async let recordingRefresh: () = refreshClips()
        do { let value = try await GoClient.shared.campaigns(session: session); if !Task.isCancelled { campaigns = value } }
        catch { if !Task.isCancelled { self.error = error.localizedDescription } }
        await recordingRefresh
    }
    private func refreshClips() async {
        loadingClips = true; clipError = nil; defer { loadingClips = false }
        do {
            let value = try await GoClient.shared.campaignClips(session: session)
            guard !Task.isCancelled else { return }
            clips = value.clips; clipWarning = value.warning
        } catch { if !Task.isCancelled { clipError = error.localizedDescription } }
    }
}

private struct CampaignClipsView: View {
    let campaign, deviceName: String
    let clips: [CampaignClip]
    let warning: String
    let error: String?
    let loading: Bool
    let refresh: () async -> Void
    let open: (String) -> Void
    @Environment(\.dismiss) private var dismiss
    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            VStack(alignment: .leading, spacing: 14) {
                HStack {
                    VStack(alignment: .leading, spacing: 4) {
                        Text(campaign).font(.title2.bold())
                        Text(error == nil ? "\(clips.count)\(warning.isEmpty ? "" : "+") \(clips.count == 1 && warning.isEmpty ? "clip" : "clips") stored on \(deviceName)" : "Recordings on \(deviceName) are unavailable")
                            .foregroundStyle(.secondary)
                    }
                    Spacer()
                    if loading { ProgressView().controlSize(.small) }
                    Button { Task { await refresh() } } label: { Image(systemName: "arrow.clockwise") }.help("Refresh clips").disabled(loading)
                    Button("Done") { dismiss() }.keyboardShortcut(.cancelAction)
                }
                if let error { Text(error).font(.callout).foregroundStyle(.orange).textSelection(.enabled) }
                if !warning.isEmpty { Text(warning).font(.callout).foregroundStyle(.orange) }
            }.padding(20)
            if clips.isEmpty {
                ContentUnavailableView(error == nil && warning.isEmpty ? "No clips yet" : "Clips unavailable", systemImage: "film.stack",
                                       description: Text("Recordings appear here when this campaign triggers. Clips removed from the device are not included."))
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
                    .mobileRefreshablePlaceholder { await refresh() }
            } else {
                #if os(iOS)
                List(clips) { clip in
                    Button { open(clip.id) } label: {
                        HStack {
                            VStack(alignment: .leading, spacing: 5) {
                                Text(clip.started.formatted(date: .abbreviated, time: .shortened)).font(.headline)
                                Text(clip.reason).font(.caption).foregroundStyle(.secondary)
                                Text("\(clip.durationLabel) · \(ByteCountFormatter.string(fromByteCount: clip.bytes, countStyle: .file)) · \(clip.state.capitalized)")
                                    .font(.caption).foregroundStyle(.secondary)
                            }
                            Spacer()
                            Image(systemName: "play.circle")
                        }.padding(.vertical, 5)
                    }.buttonStyle(.plain)
                }.listStyle(.plain).refreshable { await refresh() }
                #else
                Table(clips) {
                    TableColumn("Recorded") { clip in
                        VStack(alignment: .leading, spacing: 3) {
                            Text(clip.started.formatted(date: .abbreviated, time: .standard))
                            Text(clip.reason).font(.caption).foregroundStyle(.secondary).lineLimit(1)
                        }.padding(.vertical, 5).help(clip.id)
                    }.width(min: 230)
                    TableColumn("Duration") { Text($0.durationLabel).monospacedDigit() }.width(70)
                    TableColumn("Size") { Text(ByteCountFormatter.string(fromByteCount: $0.bytes, countStyle: .file)) }.width(80)
                    TableColumn("Status") { Text($0.state.capitalized) }.width(90)
                    TableColumn("") { clip in
                        Button { open(clip.id) } label: { Label("Open", systemImage: "play.rectangle") }.buttonStyle(.bordered)
                    }.width(80)
                }.tableStyle(.bordered)
                #endif
            }
        }.desktopFrame(width: 760, height: 500)
    }
}

private struct CampaignEditor: View {
    let session: UInt64
    let deviceName: String
    let deployed: () -> Void
    @Environment(\.dismiss) private var dismiss
    @State private var draft = CampaignDraft()
    @State private var sources: [CampaignSource] = []
    @State private var sourceError: String?
    @State private var error: String?
    @State private var loading = true
    @State private var saving = false
    @State private var reviewing = false

    var body: some View {
        Group {
            #if os(iOS)
            mobileEditor
            #else
            desktopEditor
            #endif
        }.interactiveDismissDisabled(saving)
            .onChange(of: draft.trigger) { _, trigger in
                draft.modelRevision = ""
                if trigger == .classification { draft.model = "google/mobilenet_v2_1.0_224"; draft.labels = [] }
                else if trigger == .detection { draft.model = "facebook/detr-resnet-50"; draft.labels = ["person"] }
            }
            .task {
                defer { loading = false }
                do { sources = try await GoClient.shared.campaignSources(session: session) }
                catch { sourceError = error.localizedDescription }
            }
    }
    #if os(iOS)
    private var mobileEditor: some View {
        NavigationStack {
            Form {
                Section { TextField("Campaign name", text: $draft.name).textInputAutocapitalization(.never).autocorrectionDisabled() }
                Section("Trigger") {
                    Picker("When", selection: $draft.trigger) { ForEach(CampaignTrigger.allCases) { Text($0 == .event ? "App event" : $0 == .classification ? "Image classification" : "Object detection").tag($0) } }
                    if draft.trigger == .event { TextField("Event name", text: $draft.event).textInputAutocapitalization(.never).autocorrectionDisabled() }
                    else { CampaignModelPicker(draft: $draft) }
                }
                Section("Camera") {
                    Picker("Camera", selection: $draft.camera) {
                        Text("Any camera").tag("*")
                        ForEach(sources) { Text($0.title).tag($0.id).disabled(!$0.healthy) }
                    }
                    if loading { ProgressView() }
                    if let sourceError { FailureNote(message: sourceError) }
                }
                Section("Action") {
                    Picker("Action", selection: $draft.action) { ForEach(CampaignAction.allCases) { Text($0 == .notify ? "Record & notify" : "Record").tag($0) } }
                    Toggle("Upload to Wendy Cloud", isOn: $draft.upload)
                }
                Section("Recording") {
                    Stepper("Before trigger: \(draft.before)s", value: $draft.before, in: 0...300, step: 5)
                    Stepper("After trigger: \(draft.after)s", value: $draft.after, in: 5...300, step: 5)
                }
                if draft.trigger != .event {
                    Section("Detection") {
                        LabeledContent("Confidence", value: draft.threshold.formatted(.percent.precision(.fractionLength(0))))
                        Slider(value: $draft.threshold, in: 0.5...0.99, step: 0.01)
                        Stepper("Cooldown: \(draft.cooldown)s", value: $draft.cooldown, in: 5...3600, step: 5)
                    }
                }
                if let error { Section { FailureNote(message: error) } }
                Section { Text("The campaign starts on \(deviceName) when you create it.").font(.footnote).foregroundStyle(.secondary) }
            }.disabled(saving)
                .navigationTitle("New campaign").navigationBarTitleDisplayMode(.inline)
                .toolbar {
                    ToolbarItem(placement: .cancellationAction) { Button("Cancel") { dismiss() }.disabled(saving) }
                    ToolbarItem(placement: .confirmationAction) { Button(saving ? "Creating…" : "Create") { Task { await deploy() } }.disabled(!draft.valid || saving) }
                }
        }
    }
    #endif
    private var desktopEditor: some View {
        VStack(alignment: .leading, spacing: 20) {
            HStack {
                VStack(alignment: .leading, spacing: 5) {
                    Text(reviewing ? "Review campaign" : "Create a campaign").font(.title2.bold())
                    Text(deviceName).foregroundStyle(.secondary)
                }
                Spacer()
                Image(systemName: "bolt.horizontal.circle.fill").font(.largeTitle).foregroundStyle(.orange)
            }
            ScrollView {
                VStack(alignment: .leading, spacing: 14) {
                    TextField("Campaign name", text: $draft.name).textFieldStyle(.roundedBorder).disabled(reviewing)
                    ruleCard("When", icon: "person.crop.rectangle", color: .purple) {
                        Picker("Trigger", selection: $draft.trigger) { ForEach(CampaignTrigger.allCases) { Text($0.rawValue).tag($0) } }.labelsHidden()
                        if draft.trigger == .event {
                            TextField("Event name, for example door_opened", text: $draft.event).textFieldStyle(.roundedBorder)
                            Text("Your app must already emit this event through Wendy Data.").font(.caption).foregroundStyle(.secondary)
                        } else {
                            CampaignModelPicker(draft: $draft)
                        }
                    }
                    ruleCard("On", icon: "web.camera", color: .blue) {
                        Picker("Camera", selection: $draft.camera) {
                            Text("Any camera").tag("*")
                            ForEach(sources) { Text($0.title + ($0.healthy ? "" : " · Unavailable")).tag($0.id).disabled(!$0.healthy) }
                        }.labelsHidden()
                        Text(draft.camera == "*" ? "All healthy cameras on \(deviceName), including cameras paired later." : "Only this camera on \(deviceName).")
                            .font(.caption).foregroundStyle(.secondary)
                        if loading { ProgressView().controlSize(.small) }
                        if let sourceError { FailureNote(message: sourceError) }
                    }
                    ruleCard("Then", icon: "bell.badge", color: .orange) {
                        Picker("Action", selection: $draft.action) { ForEach(CampaignAction.allCases) { Text($0.rawValue).tag($0) } }.labelsHidden()
                        if draft.action == .notify {
                            Text("Send through Wendy Cloud and record a clip. The device must be enrolled, and an owner or admin must enable this campaign’s notification grant in Cloud Apps.")
                                .font(.caption).foregroundStyle(.secondary)
                        }
                    }
                    if reviewing {
                        Label("Deploying arms this campaign immediately on \(deviceName).", systemImage: "bolt.fill").font(.callout)
                        Text("Keep \(draft.before) seconds before and \(draft.after) seconds after each trigger. \(draft.upload ? "Upload clips to Wendy Cloud." : "Keep clips on the device; upload manually.")")
                            .font(.callout).foregroundStyle(.secondary)
                        if draft.trigger != .event {
                            Text("Match \(draft.labels.sorted().joined(separator: ", ")) at \(Int(draft.threshold * 100))% confidence, at most once every \(draft.cooldown) seconds per detection cycle. The device downloads the detection model and runtime on first use.")
                                .font(.callout).foregroundStyle(.secondary)
                        }
                    } else {
                        DisclosureGroup("Detection and recording") {
                            Form {
                                if draft.trigger != .event {
                                    Slider(value: $draft.threshold, in: 0.5...0.99, step: 0.01) { Text("Confidence \(Int(draft.threshold * 100))%") }
                                    Stepper("Cooldown: \(draft.cooldown) seconds", value: $draft.cooldown, in: 5...3600, step: 5)
                                }
                                Stepper("Before trigger: \(draft.before) seconds", value: $draft.before, in: 0...300, step: 5)
                                Stepper("After trigger: \(draft.after) seconds", value: $draft.after, in: 5...300, step: 5)
                                Toggle("Upload clips to Wendy Cloud", isOn: $draft.upload)
                            }.padding(.top, 12)
                        }
                    }
                }
            }
            if let error { FailureNote(message: error) }
            HStack {
                if saving { ProgressView().controlSize(.small); Text("Deploying").foregroundStyle(.secondary) }
                Spacer()
                Button("Cancel") { dismiss() }.keyboardShortcut(.cancelAction).disabled(saving)
                if reviewing { Button("Back") { reviewing = false }.disabled(saving) }
                Button(reviewing ? "Deploy campaign" : "Review") {
                    if reviewing { Task { await deploy() } } else { reviewing = true }
                }.keyboardShortcut(.defaultAction).disabled(!draft.valid || saving)
            }
        }.padding(24).desktopFrame(width: 680, height: 760).interactiveDismissDisabled(saving)
    }

    private func ruleCard<Content: View>(_ title: String, icon: String, color: Color, @ViewBuilder content: () -> Content) -> some View {
        HStack(alignment: .top, spacing: 14) {
            Image(systemName: icon).font(.title2).foregroundStyle(color).frame(width: 30).padding(.top, 2)
            VStack(alignment: .leading, spacing: 10) { Text(title).font(.caption.bold()).foregroundStyle(.secondary); content().disabled(reviewing || saving) }
                .frame(maxWidth: .infinity, alignment: .leading)
        }.padding(16).background(color.opacity(0.06), in: RoundedRectangle(cornerRadius: 12))
    }
    private func deploy() async {
        saving = true; error = nil; defer { saving = false }
        do { try await GoClient.shared.deployCampaign(session: session, draft: draft); deployed(); dismiss() }
        catch { self.error = error.localizedDescription }
    }
}
