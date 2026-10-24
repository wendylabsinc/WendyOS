import SwiftUI

struct CampaignModelPicker: View {
    @Binding var draft: CampaignDraft
    @State private var info: VisionModelInfo?
    @State private var loading = false
    @State private var error: String?
    @State private var custom = false
    @State private var showingLabels = false
    @State private var labelSearch = ""
    private var choices: [(String, String)] {
        draft.trigger == .classification ? [
            ("Animals, plants & everyday objects · Compact", "google/mobilenet_v2_1.0_224"),
            ("Animals, plants & everyday objects · Standard", "google/vit-base-patch16-224")
        ] : [
            ("People, animals & everyday objects · Standard", "facebook/detr-resnet-50"),
            ("People, animals & everyday objects · Compact", "hustvl/yolos-tiny"),
            ("People, animals & everyday objects · Large", "facebook/detr-resnet-101")
        ]
    }
    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            if custom {
                HStack {
                    TextField("Hugging Face owner/repository", text: $draft.model).textFieldStyle(.roundedBorder)
                    Button("Defaults") { custom = false; draft.model = choices[0].1 }
                }
            } else {
                HStack {
                    Picker("Model", selection: $draft.model) {
                        ForEach(choices, id: \.1) { Text($0.0).tag($0.1) }
                    }
                    Button("Other model") { custom = true }
                }
            }
            if loading { ProgressView("Loading model labels").controlSize(.small) }
            if let info {
                HStack {
                    Button { showingLabels = true } label: {
                        Label(draft.labels.isEmpty ? "Choose what to detect" : "Change what to detect", systemImage: "tag")
                    }
                    Text(draft.labels.sorted().joined(separator: ", ")).font(.caption).foregroundStyle(.secondary).lineLimit(2)
                }
                .popover(isPresented: $showingLabels) {
                    VStack(alignment: .leading, spacing: 10) {
                        HStack { Text("What to detect").font(.headline); Spacer(); Text("\(draft.labels.count) / 32").foregroundStyle(.secondary) }
                        TextField("Find a person, animal or object", text: $labelSearch).textFieldStyle(.roundedBorder)
                        List(info.labels.filter { labelSearch.isEmpty || $0.localizedCaseInsensitiveContains(labelSearch) }, id: \.self) { label in
                            Toggle(label, isOn: Binding(get: { draft.labels.contains(label) }, set: { enabled in
                                if enabled { if draft.labels.count < 32 { draft.labels.insert(label) } }
                                else { draft.labels.remove(label) }
                            })).toggleStyle(.checkbox)
                        }
                        HStack { Text("Trigger when any selected category is recognized.").font(.caption).foregroundStyle(.secondary); Spacer(); Button("Done") { showingLabels = false } }
                    }.padding(16).frame(width: 420, height: 420)
                }
                if draft.trigger == .classification {
                    Text("Recognizes \(info.labels.count) categories across the whole image. Only your selected categories trigger an event.").font(.caption).foregroundStyle(.secondary)
                } else {
                    Text("Finds \(info.labels.count) kinds of people, animals and objects in camera frames. Only your selected categories trigger an event.").font(.caption).foregroundStyle(.secondary)
                }
                DisclosureGroup("Model details") {
                    VStack(alignment: .leading, spacing: 6) {
                        Text(draft.model).textSelection(.enabled)
                        Text("\(ByteCountFormatter.string(fromByteCount: info.weightsBytes, countStyle: .file)) weights · \(info.license)")
                        if let url = URL(string: "https://huggingface.co/" + draft.model) { Link("Model card", destination: url) }
                    }.font(.caption).foregroundStyle(.secondary).padding(.top, 4)
                }
            }
            if let error { Label(error, systemImage: "exclamationmark.triangle").font(.caption).foregroundStyle(.orange).textSelection(.enabled) }
        }
        .task(id: draft.model + draft.trigger.rawValue) {
            let repo = draft.model, trigger = draft.trigger
            loading = true; error = nil; info = nil; draft.modelRevision = ""
            defer { if !Task.isCancelled { loading = false } }
            do {
                if custom { try await Task.sleep(for: .milliseconds(450)) }
                let value = try await GoClient.shared.visionModel(repo, trigger: trigger)
                guard !Task.isCancelled, repo == draft.model, trigger == draft.trigger else { return }
                info = value; draft.modelRevision = value.revision
                draft.labels = draft.labels.intersection(Set(value.labels))
            } catch { if !Task.isCancelled { self.error = error.localizedDescription } }
        }
    }
}
