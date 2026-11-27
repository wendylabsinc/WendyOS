import SwiftUI
import WorldClient

struct AnnotationView: View {
    @ObservedObject var model: OperatorModel
    @State private var note = ""
    @State private var x = "0"
    @State private var y = "0"
    @State private var z = "0"
    @State private var saving = false
    @State private var editingID: String?
    @State private var amendmentText = ""

    var body: some View {
        VStack(alignment: .leading, spacing: 20) {
            Text("Human annotations").font(.title2.weight(.semibold))
            Text("Share a human-authored note in this simulation. A note does not establish that an area is safe.").foregroundStyle(.secondary)
            Picker("Notes section", selection: $model.annotationSection) {
                Text("Shared state").tag("Shared state")
                Text("My notes").tag("My notes")
                Text("New note").tag("New note")
            }.pickerStyle(.segmented)
            if model.annotationSection == "New note", model.canAnnotate, let context = model.annotationContext {
                VStack(alignment: .leading, spacing: 14) {
                    Text(model.annotationClockStatus).font(.caption).foregroundStyle(.secondary)
                    Label("New note", systemImage: "square.and.pencil").font(.headline)
                    DetailRow(title: "Author", value: model.annotationAuthor)
                    DetailRow(title: "Simulation / frame", value: context.runID + " · " + context.frameID + " @ " + context.frameRevision)
                    if model.selectedFrame != model.annotationFrameKey {
                        Button("Use annotation frame") { model.selectedFrame = model.annotationFrameKey }.buttonStyle(.bordered)
                    }
                    TextField("Note text", text: $note, axis: .vertical).lineLimit(3...8).textFieldStyle(.roundedBorder)
                        .accessibilityIdentifier("annotation-text")
                    HStack {
                        coordinate("X metres", text: $x)
                        coordinate("Y metres", text: $y)
                        coordinate("Z metres", text: $z)
                    }
                    Text("Manual site coordinates · Y up · up to 3 decimal places · \(note.utf8.count)/2,048 UTF-8 bytes").font(.caption).foregroundStyle(.secondary)
                    Text("Timed notes expire within 24 hours. Without verified time, text stays untimed until you explicitly create a separate note.").font(.caption).foregroundStyle(.secondary)
                    DisclosureGroup("Technical details") {
                        Text("Time is relative to the pinned simulation gateway, not verified UTC. The bound includes measured round-trip delay, 1 ms quantization and assumed 100 ppm drift for at most one hour. Signed retries retain the original interval and proof.").font(.caption).foregroundStyle(.secondary)
                    }.font(.caption)
                    Button(saving ? "Saving…" : "Save note on device") {
                        saving = true
                        model.actions.submit {
                            if await model.saveAnnotation(text: note, coordinates: [x, y, z]) { note = "" }
                            saving = false
                        }
                    }.buttonStyle(.borderedProminent).accessibilityIdentifier("annotation-save")
                        .disabled(saving || model.annotationBusy || model.selectedFrame != model.annotationFrameKey || note.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty || note.utf8.count > 2048 || (try? AnnotationInput.millimetres([x, y, z])) == nil)
                }.padding(20).card()
            } else if model.annotationSection == "New note" {
                Text("This profile can read notes. Import a simulation contributor profile to add your own.").foregroundStyle(.secondary).padding(20).card()
            }
            if let notice = model.annotationNotice { Label(notice, systemImage: "info.circle").font(.callout).foregroundStyle(.orange) }
            if model.canAnnotate && model.annotationSection == "My notes" {
                HStack {
                    Text("On-device outbox").font(.headline)
                    Spacer()
                    Button(model.annotationBusy ? "Syncing…" : "Sync pending notes") { model.actions.submit { await model.syncAnnotations() } }
                        .buttonStyle(.bordered).disabled(model.annotationBusy)
                }
                Text("Saved here before sending. Pending notes retry when connected; rejected or expired notes stay here for review.").font(.caption).foregroundStyle(.secondary)
                ForEach(model.annotationEntries.reversed()) { entry in
                    VStack(alignment: .leading, spacing: 10) {
                        HStack { Badge(label: entry.state.rawValue.uppercased(), color: entry.state == .published ? WorldStyle.accent : .orange); Spacer(); Text(entry.id.prefix(8)).font(.caption.monospaced()) }
                        Text(entry.draft.text).textSelection(.enabled)
                        DetailRow(title: "Author / frame", value: entry.draft.authorPrincipal + " · " + entry.draft.context.frameID + " @ " + entry.draft.context.frameRevision)
                        Text(entry.state == .untimed ? "Untimed text · not eligible for publication" : "Authored \(date(entry.draft.authoredAtUnixMS)) · expires \(date(entry.draft.expiresAtUnixMS))").font(.caption).foregroundStyle(.secondary)
                        if entry.state == .published {
                            Button("Review correction or withdrawal") { editingID = entry.id; amendmentText = entry.draft.text; x = String(Double(entry.draft.positionMM[0])/1000); y = String(Double(entry.draft.positionMM[1])/1000); z = String(Double(entry.draft.positionMM[2])/1000) }.buttonStyle(.bordered).disabled(model.amendmentBusy)
                            if editingID == entry.id {
                                Text("Original signed note stays unchanged. A withdrawal is terminal for this original.").font(.caption).foregroundStyle(.orange)
                                TextField("Corrected text", text: $amendmentText, axis: .vertical).lineLimit(2...5).textFieldStyle(.roundedBorder)
                                HStack { coordinate("X metres", text: $x); coordinate("Y metres", text: $y); coordinate("Z metres", text: $z) }
                                HStack {
                                    Button("Save correction for review") { model.actions.submit { await model.prepareAmendment(originalID: entry.id, action: "correct", text: amendmentText, coordinates: [x,y,z]) } }
                                    Button("Save withdrawal for review", role: .destructive) { model.actions.submit { await model.prepareAmendment(originalID: entry.id, action: "withdraw", text: "", coordinates: [x,y,z]) } }
                                }.buttonStyle(.bordered).disabled(model.amendmentBusy)
                            }
                        }
                        if entry.draft.schemaVersion == nil { Text("Legacy v1 · assumed 1-second bound was not calibrated. Original bytes retained.").font(.caption).foregroundStyle(.orange) }
                        if let issue = entry.issue { Text(issue).font(.caption).foregroundStyle(.orange) }
                        if let digest = entry.bodySHA256 {
                            DisclosureGroup("Technical details") {
                                DetailRow(title: "Signed body SHA-256", value: digest)
                                DetailRow(title: "Publishing version", value: "Epoch \(entry.epoch.map(String.init) ?? "?") · sequence \(entry.sequence.map(String.init) ?? "?")")
                            }.font(.caption)
                        }
                    }.padding(18).card()
                }
                if model.annotationEntries.isEmpty { Text("No notes in this profile's outbox.").foregroundStyle(.secondary) }
            }
            if model.canAnnotate && model.annotationSection == "My notes" {
                HStack { Text("Saved amendments").font(.headline); Spacer(); Button("Send saved amendments") { model.actions.submit { await model.syncAmendments() } }.buttonStyle(.borderedProminent).disabled(model.amendmentBusy || !model.amendmentEntries.contains(where: {$0.state == .pending})) }
                if let notice = model.amendmentNotice { Text(notice).font(.callout).foregroundStyle(.orange) }
                ForEach(model.amendmentEntries.reversed()) { entry in
                    VStack(alignment: .leading, spacing: 8) {
                        Badge(label: entry.state.rawValue.uppercased() + " · " + entry.draft.action.uppercased(), color: .orange)
                        if entry.draft.action == "correct" { Text(entry.draft.text).textSelection(.enabled) }
                        DetailRow(title: "Original signed digest", value: entry.draft.originalSHA256)
                        Text("Original expiry " + date(entry.draft.expiresAtUnixMS)).font(.caption)
                        Text("Earlier signed bytes and ancestry remain in the private outbox.").font(.caption).foregroundStyle(.secondary)
                        if let issue = entry.issue { Text(issue).font(.caption).foregroundStyle(.orange) }
                    }.padding(18).card()
                }
            }
            if model.annotationSection == "Shared state" {
            Text("Amendment-aware replica notes").font(.headline)
            Text("Local replica knowledge. Conflicting edits stay visible; withdrawals are terminal. History does not prove current authorization.").font(.caption).foregroundStyle(.secondary)
            if model.annotationViewsUnavailable { Text("Amendment view unavailable or unsupported. Saved rows are historical; the legacy plan cannot show corrected text.").foregroundStyle(.orange) }
            ForEach(Array(model.annotationViews.enumerated()), id: \.offset) { _, row in
                TimelineView(.periodic(from: .now, by: 1)) { _ in
                    let state = AnnotationViewPresentation.state(row, offline: model.offline || model.annotationViewsUnavailable)
                    VStack(alignment: .leading, spacing: 10) {
                        Badge(label: state.uppercased(), color: state == "corrected" ? WorldStyle.accent : .orange)
                        if ["original","corrected"].contains(state) { Text(text(row["text"])).textSelection(.enabled) }
                        if state == "withdrawn" || state == "historical_withdrawn" {
                            Text("This note was withdrawn.").font(.headline)
                            if row["conflict"]?.bool == true { Text("Earlier conflicting corrections remain as signed history.").font(.caption).foregroundStyle(.secondary) }
                        } else if row["conflict"]?.bool == true { Text("Conflicting signed branches retained. No correction is chosen automatically.").foregroundStyle(.orange) }
                        if ["original","corrected"].contains(state) { DetailRow(title: "Position in frame", value: (row["position_mm"]?.array ?? []).map {String($0.int ?? 0)}.joined(separator: ", ") + " mm") }
                        DetailRow(title: "Author", value: text(row["author_principal"]))
                        DetailRow(title: "Frame", value: text(row["frame"]?["id"]) + " @ " + text(row["frame"]?["revision"]))
                        DetailRow(title: "Original signed digest", value: text(row["original_sha256"]))
                        Text("Verified at last response; a partition can hide later amendments or revocations.").font(.caption).foregroundStyle(.secondary)
                    }.padding(18).card()
                }
            }

            }
        }
    }
    private func date(_ millis: Int64) -> String { Date(timeIntervalSince1970: Double(millis) / 1000).formatted(date: .abbreviated, time: .shortened) }
    private func coordinate(_ title: String, text: Binding<String>) -> some View {
        VStack(alignment: .leading) { Text(title).font(.caption); TextField(title, text: text).textFieldStyle(.roundedBorder).accessibilityIdentifier("annotation-" + title.prefix(1).lowercased()) }
    }
}
