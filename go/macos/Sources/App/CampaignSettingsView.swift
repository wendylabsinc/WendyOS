import SwiftUI

struct CampaignSettingsDraft: Equatable {
    var name, revision, before, after, upload, cooldown: String
    var inference: Bool
    var threshold: Double
}
extension GoClient {
    func campaignSettings(session: UInt64, name: String) async throws -> CampaignSettingsDraft {
        try await perform {
            try Self.withCString(name) {
                guard let p = WDCampaignSettings(session, $0) else { throw WendyError(message: "Missing campaign settings.") }
                defer { WDCampaignSettingsFree(p) }; try Self.check(p.pointee.status)
                let v = p.pointee
                return CampaignSettingsDraft(name: Self.string(v.name), revision: Self.string(v.revision), before: Self.string(v.before), after: Self.string(v.after), upload: Self.string(v.upload), cooldown: Self.string(v.cooldown), inference: v.inference != 0, threshold: v.threshold)
            }
        }
    }
    func updateCampaignSettings(session: UInt64, draft: CampaignSettingsDraft) async throws {
        try await perform {
            let strings = CStringStorage()
            defer { withExtendedLifetime(strings) {} }
            try Self.finish(WDUpdateCampaignSettings(session, strings.add(draft.name), strings.add(draft.revision), strings.add(draft.before), strings.add(draft.after), strings.add(draft.upload), strings.add(draft.cooldown), draft.threshold))
        }
    }
}
struct CampaignSettingsView: View {
    let session: UInt64
    let name: String
    let saved: () -> Void
    @Environment(\.dismiss) private var dismiss
    @State private var draft: CampaignSettingsDraft?
    @State private var original: CampaignSettingsDraft?
    @State private var error: String?
    @State private var saving = false
    var body: some View {
        NavigationStack {
            Form {
                if draft != nil {
                    Section("Campaign") { Text(name).textSelection(.enabled) }
                    Section {
                        durationField(draft?.inference == true ? "Before detection" : "Before trigger", value: field(\.before))
                        durationField(draft?.inference == true ? "After detection" : "After trigger", value: field(\.after))
                    } header: { Text("Recording") } footer: {
                        Text("How much video to keep before and after the event. Enter a duration such as 10s or 1m.")
                    }
                    Section("Upload recordings") {
                        Picker("Upload", selection: field(\.upload)) {
                            Text("Manually").tag("manual")
                            Text("On Wi-Fi").tag("wifi")
                            Text("Always").tag("always")
                        }
                    }
                    if draft?.inference == true {
                        Section("Detection") {
                            LabeledContent("Confidence", value: (draft?.threshold ?? 0).formatted(.percent.precision(.fractionLength(0))))
                            Slider(value: Binding(get: { draft?.threshold ?? 0.9 }, set: { draft?.threshold = $0 }), in: 0.01...1, step: 0.01)
                            durationField("Time between alerts", value: field(\.cooldown))
                        }
                    }
                } else if error == nil { ProgressView("Loading settings…") }
                if let error { Section { FailureNote(message: error) } }
            }
            .formStyle(.grouped)
            .navigationTitle("Campaign settings")
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button("Cancel") { dismiss() }.keyboardShortcut(.cancelAction).disabled(saving) }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Save") { Task { await save() } }.keyboardShortcut(.defaultAction).disabled(draft == nil || draft == original || saving)
                }
            }
            .disabled(saving)
            .overlay { if saving { ProgressView("Saving…").padding().background(.regularMaterial, in: RoundedRectangle(cornerRadius: 12)) } }
        }.desktopFrame(width: 480, height: 540).interactiveDismissDisabled(saving)
            .task {
                do { let value = try await GoClient.shared.campaignSettings(session: session, name: name); draft = value; original = value }
                catch { self.error = error.localizedDescription }
            }
    }
    private func durationField(_ title: String, value: Binding<String>) -> some View {
        LabeledContent(title) {
            TextField(title, text: value, prompt: Text("10s"))
                .labelsHidden()
                .lineLimit(1)
                .multilineTextAlignment(.trailing)
                .textFieldStyle(.plain)
                .frame(width: 110)
                .accessibilityLabel(title)
                #if os(iOS)
                .textInputAutocapitalization(.never)
                .autocorrectionDisabled()
                .keyboardType(.asciiCapable)
                #endif
        }
    }
    private func field(_ key: WritableKeyPath<CampaignSettingsDraft, String>) -> Binding<String> {
        Binding(get: { draft?[keyPath: key] ?? "" }, set: { draft?[keyPath: key] = $0 })
    }
    private func save() async {
        guard let draft else { return }
        saving = true; error = nil; defer { saving = false }
        do { try await GoClient.shared.updateCampaignSettings(session: session, draft: draft); saved(); dismiss() }
        catch { self.error = error.localizedDescription }
    }
}
