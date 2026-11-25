import SwiftUI
import UniformTypeIdentifiers
import WorldClient

struct ProposalInboxView: View {
    @ObservedObject var model: OperatorModel
    let onReview: (MissionEntry) -> Void
    @State private var duration = "60"
    @State private var importingHandoff = false
    @Environment(\.horizontalSizeClass) private var sizeClass
    private var columns: AnyLayout { sizeClass == .compact ? AnyLayout(VStackLayout(alignment: .leading, spacing: 20)) : AnyLayout(HStackLayout(alignment: .top, spacing: 20)) }
    private var selected: RemoteProposal? { model.inboxRows.first { $0.id == model.inboxSelectedID } }
    var body: some View {
        if !model.inboxOrigins.isEmpty {
            VStack(alignment: .leading, spacing: 16) {
                HStack {
                    Label("Remote proposal inbox", systemImage: "tray.full").font(.title2.weight(.semibold))
                    Spacer()
                    Text("HISTORICAL · NEVER EXECUTABLE").font(.caption.weight(.semibold)).foregroundStyle(.orange)
                }
                Text("Read suggestions relayed by your selected replica. Only a separate, explicit review at the configured origin can prepare an unsigned mission draft.").foregroundStyle(.secondary)
                HStack {
                    Picker("Pinned origin", selection: Binding(get: { model.inboxSelectedOrigin }, set: { model.selectInboxOrigin($0) })) {
                        ForEach(model.inboxOrigins) { origin in Text(origin.origin + " · " + origin.assignee).tag(origin.origin) }
                    }.frame(maxWidth: 500)
                    Button("Refresh complete inbox") { model.actions.submit { await model.refreshInbox() } }.disabled(model.inboxBusy)
                }
                if let notice = model.inboxNotice { Text(notice).font(.callout).foregroundStyle(model.inboxOnline ? Color.secondary : Color.orange) }
                if let doc = model.inboxDocuments.first(where: { $0.origin.origin == model.inboxSelectedOrigin }) {
                    Text("Saved \(Date(timeIntervalSince1970: Double(doc.fetchedUnixMS) / 1000).formatted()) · \(model.inboxRows.count) proposals · \(doc.origin.runID)").font(.caption).foregroundStyle(.secondary)
                }
                columns {
                    VStack(alignment: .leading, spacing: 8) {
                        if model.inboxRows.isEmpty { Text("No saved proposals for this origin.").foregroundStyle(.secondary) }
                        ForEach(model.inboxRows) { row in
                            Button {
                                model.inboxSelectedID = row.id; model.inboxReviewedID = nil; model.inboxPrepared = nil; model.reviewedProposal = nil
                            } label: {
                                VStack(alignment: .leading, spacing: 5) {
                                    Text(row.summary).font(.headline).lineLimit(2)
                                    Text(row.proposalID + " · " + row.principal).font(.caption).lineLimit(1)
                                }.frame(maxWidth: .infinity, alignment: .leading).padding(12)
                                    .background(model.inboxSelectedID == row.id ? WorldStyle.accent.opacity(0.12) : Color.white.opacity(0.04), in: RoundedRectangle(cornerRadius: 9))
                            }.buttonStyle(.plain)
                        }
                    }.frame(minWidth: 220, maxWidth: 380)
                    if let row = selected {
                        VStack(alignment: .leading, spacing: 10) {
                            Text(row.summary).font(.title3.weight(.semibold))
                            DetailRow(title: "Origin / proposal", value: row.id)
                            DetailRow(title: "Original author", value: row.principal)
                            DetailRow(title: "Original proposal deadline", value: Date(timeIntervalSince1970: Double(row.expiresAtUnixMS) / 1000).formatted())
                            DisclosureGroup("Signed record and provenance") {
                                VStack(alignment: .leading, spacing: 8) {
                                    DetailRow(title: "Signed origin type / class", value: row.type + " / " + (row.record["class"]?.string ?? "unknown"))
                                    DetailRow(title: "Immutable body SHA-256", value: row.bodySHA256)
                                    Text("Original proposal and purpose-bound origin signatures verified against the imported root. This does not establish current permission or historical revocation state.").font(.caption).foregroundStyle(.secondary)
                                    Text("Gateway reported at retrieval: origin \(row.gatewayOriginVerification); proposer \(row.gatewayProposerVerification). These are saved assertions.").font(.caption).foregroundStyle(.secondary)
                                }.padding(.top, 8)
                            }
                            Button("Review at configured origin") { model.actions.submit { await model.reviewRemoteProposal(id: row.id) } }.disabled(model.inboxBusy || !model.canMission)
                            if let entry = model.missionEntries.last(where: { $0.draft.proposal?.audience == row.origin.origin && $0.draft.proposal?.id == row.proposalID && $0.draft.proposal?.bodySHA256 == row.bodySHA256 }) {
                                Divider()
                                Text("Local approval outbox · " + entry.state.rawValue.uppercased()).font(.headline).foregroundStyle(WorldStyle.accent)
                                DetailRow(title: "Task ID", value: entry.id)
                                DetailRow(title: "Saved target (metres)", value: entry.draft.target.map { String(format: "%.2f", $0) }.joined(separator: ", "))
                                if let digest = entry.instructionSHA256 { DetailRow(title: "Saved instruction SHA-256", value: digest) }
                                if let issue = entry.lastError { Text(issue).font(.caption).foregroundStyle(.orange) }
                                if [.draft, .signed, .uncertain].contains(entry.state) {
                                    Button(entry.state == .draft ? "Review and sign…" : "Review exact retry…") { onReview(entry) }.buttonStyle(.borderedProminent).disabled(model.missionBusy)
                                }
                                Text("Acknowledgement and status are last-known. This inbox does not establish execution or physical effects.").font(.caption).foregroundStyle(.secondary)
                            }
                            if model.inboxReviewedID == row.id, let live = model.reviewedProposal {
                                Divider()
                                Text("Last explicit origin review matched").font(.headline).foregroundStyle(WorldStyle.accent)
                                DetailRow(title: "Proposed target", value: live.target.map { String(format: "%.2f", $0) }.joined(separator: ", ") + " m")
                                if live.triggerSHA256 != nil {
                                    Button("Import prepared-reader handoff…") { importingHandoff = true }
                                    if let handoff = model.inboxPrepared {
                                        Text("Prepared context matches · original physical deadline \(Date(timeIntervalSince1970: Double(handoff.notAfterUnixMS) / 1000).formatted())").font(.caption).foregroundStyle(WorldStyle.accent)
                                    } else {
                                        Text("Read-only until the matching prepared-reader file is imported. The file does not independently establish sensor truth or current permission.").font(.caption).foregroundStyle(.orange)
                                    }
                                } else if !live.canDraftWithoutPreparedHandoff {
                                    Text("Read-only: this target is outside the supported one-axis contract and has no commissioned prepared trigger/destination. It cannot be signed from this inbox.").font(.callout).foregroundStyle(.orange)
                                }
                                HStack {
                                    TextField("Seconds", text: $duration).textFieldStyle(.roundedBorder).frame(width: 90)
                                    Button("Save unsigned draft") {
                                        guard let seconds = Int64(duration), seconds > 0, seconds <= (model.missionContext?.maxDurationMS ?? 0) / 1000 else { return }
                                        model.actions.submit { _ = await model.saveRemoteProposalDraft(durationMS: seconds * 1000) }
                                    }.buttonStyle(.borderedProminent).disabled(model.inboxBusy || (!live.canDraftWithoutPreparedHandoff && model.inboxPrepared == nil))
                                }
                                Text("No task exists until you separately review and sign the draft in the mission outbox. Sending rechecks the origin again.").font(.caption).foregroundStyle(.secondary)
                            }
                        }.frame(maxWidth: .infinity, alignment: .leading)
                    } else { Text("Select a proposal to inspect its original scope and proof.").foregroundStyle(.secondary).frame(maxWidth: .infinity, alignment: .leading) }
                }
            }.padding(20).card()
                .fileImporter(isPresented: $importingHandoff, allowedContentTypes: [.json]) { result in
                    if case .success(let url) = result { model.importInboxHandoff(url) }
                }
        }
    }
}
