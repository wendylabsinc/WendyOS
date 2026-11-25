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
        Group {
            #if os(iOS)
            mobileRows
            #else
            desktopContent
            #endif
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
    private var desktopContent: some View {
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
                            })).toggleStyle(.automatic)
                        }
                        HStack { Text("Trigger when any selected category is recognized.").font(.caption).foregroundStyle(.secondary); Spacer(); Button("Done") { showingLabels = false } }
                    }.padding(16).desktopFrame(width: 420, height: 420)
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
    }
    #if os(iOS)
    private var mobileRows: some View {
        Group {
            NavigationLink {
                CampaignModelSelection(draft: $draft, choices: choices)
            } label: {
                LabeledContent("Model", value: shortName(draft.model))
            }
            if loading { HStack { ProgressView(); Text("Loading categories…").foregroundStyle(.secondary) } }
            if let info {
                NavigationLink {
                    List(info.labels.filter { labelSearch.isEmpty || $0.localizedCaseInsensitiveContains(labelSearch) }, id: \.self) { label in
                        Button {
                            if draft.labels.contains(label) { draft.labels.remove(label) }
                            else if draft.labels.count < 32 { draft.labels.insert(label) }
                        } label: {
                            HStack { Text(label.capitalized).foregroundStyle(.primary); Spacer(); if draft.labels.contains(label) { Image(systemName: "checkmark") } }
                        }
                    }
                    .searchable(text: $labelSearch, prompt: "Find a category")
                    .navigationTitle("Detect categories").navigationBarTitleDisplayMode(.inline)
                    .safeAreaInset(edge: .bottom) { Text("\(draft.labels.count) of 32 selected").font(.footnote).foregroundStyle(.secondary).padding(8).frame(maxWidth: .infinity).background(.bar) }
                } label: {
                    LabeledContent("Detect", value: draft.labels.isEmpty ? "Choose categories" : draft.labels.sorted().map { $0.capitalized }.joined(separator: ", "))
                        .lineLimit(2)
                }
                NavigationLink("Model details") {
                    Form {
                        Section { Text(shortName(draft.model)).font(.headline); Text(draft.model).font(.caption).textSelection(.enabled) }
                        Section {
                            LabeledContent("Download size", value: ByteCountFormatter.string(fromByteCount: info.weightsBytes, countStyle: .file))
                            LabeledContent("License", value: info.license)
                            LabeledContent("Categories", value: info.labels.count.formatted())
                            if let url = URL(string: "https://huggingface.co/" + draft.model) { Link("Model card", destination: url) }
                        }
                    }.navigationTitle("Model details").navigationBarTitleDisplayMode(.inline)
                }
            }
            if let error { FailureNote(message: error) }
        }
    }
    #endif
    private func shortName(_ repo: String) -> String {
        switch repo {
        case "facebook/detr-resnet-50": "DETR · Standard"
        case "facebook/detr-resnet-101": "DETR · Large"
        case "hustvl/yolos-tiny": "YOLOS · Compact"
        case "google/mobilenet_v2_1.0_224": "MobileNet V2"
        case "google/vit-base-patch16-224": "Vision Transformer"
        default: repo.split(separator: "/").last.map(String.init) ?? repo
        }
    }

}

#if os(iOS)
private struct CampaignModelSelection: View {
    @Binding var draft: CampaignDraft
    let choices: [(String, String)]
    @Environment(\.dismiss) private var dismiss
    @State private var repository = ""
    var body: some View {
        Form {
            Section("Models") {
                ForEach(choices, id: \.1) { choice in
                    Button { draft.model = choice.1; dismiss() } label: {
                        HStack {
                            VStack(alignment: .leading, spacing: 5) {
                                Text(choice.0).foregroundStyle(.primary).multilineTextAlignment(.leading)
                                Text(choice.1).font(.caption).foregroundStyle(.secondary)
                            }
                            Spacer()
                            if draft.model == choice.1 { Image(systemName: "checkmark") }
                        }.padding(.vertical, 4)
                    }
                }
            }
            Section("Other model") {
                TextField("Hugging Face owner/repository", text: $repository).textInputAutocapitalization(.never).autocorrectionDisabled()
                Button("Use model") { draft.model = repository.trimmingCharacters(in: .whitespacesAndNewlines); dismiss() }
                    .disabled(!repository.contains("/"))
            }
        }.navigationTitle("Choose a model").navigationBarTitleDisplayMode(.inline)
    }
}
#endif
