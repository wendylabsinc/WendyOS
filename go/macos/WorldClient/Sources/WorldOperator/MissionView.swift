import SwiftUI
import WorldClient

struct MissionView: View {
    @ObservedObject var model: OperatorModel
    @State private var targetX = "1.8"
    @State private var duration = "60"
    @State private var review: MissionEntry?
    @State private var reviewContext = ""
    @State private var stopTaskID = ""

    var body: some View {
        VStack(alignment: .leading, spacing: 18) {
            ProposalInboxView(model: model) { entry in
                reviewContext = model.missionContext?.digest ?? ""; review = entry
            }
            if let context = model.missionContext, model.canMission {
                VStack(alignment: .leading, spacing: 12) {
                    Label("Simulated inspection", systemImage: "signature").font(.title3.weight(.semibold))
                    Text("Autonomous missions continue without this app or a network connection while the robot's local operating conditions hold. The commissioned fallback is hold.").font(.callout)
                    DetailRow(title: "Assignee", value: context.assignee)
                    DetailRow(title: "Simulation run", value: context.runID)
                    DetailRow(title: "Local frame", value: context.frameID + " @ " + context.frameRevision)
                    DetailRow(title: "Motion", value: "\(context.speedMPS.formatted()) m/s · " + navigationLabel(context))
                    DetailRow(title: "Required sensors", value: context.requiredSensors.joined(separator: ", "))
                    DetailRow(title: "Allowed area (metres)", value: missionPosition(context.areaMin) + " to " + missionPosition(context.areaMax))
                    DetailRow(title: "Minimum energy", value: context.minimumEnergy.formatted(.percent))
                    Text("Supervised motion is unavailable in this client.").font(.caption).foregroundStyle(.secondary)
                    if context.navigationProfile == .oneAxis {
                    HStack(alignment: .top) {
                        VStack(alignment: .leading, spacing: 5) {
                            Text("Target X (metres)").font(.caption).foregroundStyle(.secondary)
                            TextField("Target X (metres)", text: $targetX).textFieldStyle(.roundedBorder)
                        }
                        VStack(alignment: .leading, spacing: 5) {
                            Text("Duration (seconds)").font(.caption).foregroundStyle(.secondary)
                            TextField("Duration (seconds)", text: $duration).textFieldStyle(.roundedBorder)
                        }
                    }
                    Text("X must be inside the commissioned area and between −2 and 2 metres. Maximum duration \(context.maxDurationMS / 1000) seconds.").font(.caption).foregroundStyle(.secondary)
                    Button("Save unsigned draft") {
                        guard let seconds = Int64(duration), seconds > 0, seconds <= context.maxDurationMS / 1000 else { model.missionNotice = "Enter a whole duration in seconds within the commissioned limit."; return }
                        model.actions.submit { await model.saveMissionDraft(x: targetX, durationMS: seconds * 1000) }
                    }.buttonStyle(.borderedProminent)
                    } else { Text("Planar inspections preserve the complete reviewed target. Select a proposal to create a draft; there is no implicit X-axis conversion.").font(.caption).foregroundStyle(.secondary) }
                    Divider()
                    Button("Load proposals") { model.actions.submit { await model.loadMissionProposals() } }.disabled(model.missionBusy)
                    ForEach(model.missionProposals, id: \.selfID) { proposal in
                        Button { model.actions.submit { await model.reviewMissionProposal(id: text(proposal["id"], "")) } } label: {
                            VStack(alignment: .leading) {
                                Text(text(proposal["summary"], "Inspection proposal"))
                                Text(text(proposal["principal"], "Unknown author")).font(.caption).foregroundStyle(.secondary)
                            }
                        }.buttonStyle(.bordered)
                    }
                    if let proposal = model.reviewedProposal {
                        VStack(alignment: .leading, spacing: 8) {
                            Text("Proposal review").font(.headline)
                            Text(proposal.summary)
                            DetailRow(title: "Proposed by", value: proposal.principal)
                            DetailRow(title: "Proposed target", value: missionPosition(proposal.target) + " m")
                            Text("Only this typed target is copied. Saving keeps the proposal's original ID and body digest.").font(.caption).foregroundStyle(.secondary)
                            Button("Save proposal as unsigned draft") {
                                guard let seconds = Int64(duration), seconds > 0, seconds <= context.maxDurationMS / 1000 else { return }
                                model.actions.submit { await model.saveMissionDraft(x: String(proposal.target[0]), durationMS: seconds * 1000, proposal: proposal) }
                            }.buttonStyle(.bordered)
                        }.padding(12).background(.white.opacity(0.04), in: RoundedRectangle(cornerRadius: 8))
                    }
                }.padding(20).card()
                VStack(alignment: .leading, spacing: 10) {
                    Text("Stop a task").font(.headline)
                    Text("An enrolled operator can stop a task issued by another operator. A stop uses fresh gateway time and does not require an unexpired mission context.").font(.caption).foregroundStyle(.secondary)
                    TextField("Task ID", text: $stopTaskID).textFieldStyle(.roundedBorder)
                    Button("Send signed stop", role: .destructive) { model.actions.submit { await model.stopMission(id: stopTaskID) } }.buttonStyle(.bordered).disabled(stopTaskID.isEmpty || model.missionStopping)
                    ForEach(model.missionStopEntries) { stop in
                        Text("\(stop.taskID): stop \(stop.state)").font(.caption)
                        if let error = stop.error { Text(error).font(.caption).foregroundStyle(.orange) }
                    }
                }.padding(20).card()
                if let notice = model.missionNotice { Text(notice).font(.callout).foregroundStyle(.orange) }
                ForEach(model.missionEntries) { entry in
                    VStack(alignment: .leading, spacing: 12) {
                        HStack { Text(entry.id).font(.headline).textSelection(.enabled); Spacer(); Badge(label: entry.phase.uppercased(), color: entry.terminal ? WorldStyle.accent : .orange) }
                        Text("Target \(missionPosition(entry.draft.target)) m · \(entry.draft.durationMS / 1000) seconds").font(.callout)
                        DetailRow(title: "Delivery", value: entry.state.rawValue)
                        if let stop = entry.stopState { DetailRow(title: "Stop delivery", value: stop) }
                        if let ref = entry.draft.proposal { DetailRow(title: "Reviewed proposal", value: ref.id) }
                        if let error = entry.lastError { Text(error).font(.caption).foregroundStyle(.orange) }
                        if let reason = entry.record?["status"]?["Reason"]?.string, !reason.isEmpty { Text(reason).font(.callout) }
                        HStack {
                            if [.draft, .signed, .uncertain].contains(entry.state) {
                                Button(entry.state == .draft ? "Review and sign…" : "Review exact retry…") { reviewContext = context.digest; review = entry }.buttonStyle(.borderedProminent).disabled(model.missionBusy)
                            }
                            if entry.instruction != nil {
                                Button("Reconcile status") { model.actions.submit { await model.reconcileMission(id: entry.id) } }.disabled(model.missionBusy)
                                Button("Stop", role: .destructive) { model.actions.submit { await model.stopMission(id: entry.id) } }.buttonStyle(.bordered).disabled(model.missionStopping)
                            }
                        }
                        if let result = entry.record?["result"] {
                            Text("Inspection result retained").font(.headline)
                            DetailRow(title: "Run", value: text(result["run_id"]))
                            ForEach((result["samples"]?.array ?? []).enumerated().compactMap { index, sample in EvidenceItem(sample["evidence"], name: text(sample["sensor_id"], "Sample \(index + 1)")) }) { item in
                                Button { model.actions.submit { await model.openEvidence(item) } } label: { Label(item.name, systemImage: "arrow.down.circle") }.disabled(model.evidenceLoading != nil)
                            }
                            Text("Evidence is fetched on demand and verified by SHA-256 before display.").font(.caption).foregroundStyle(.secondary)
                        }
                        if entry.record != nil { Text("Last-known assignee status. This does not establish current motion or reachability.").font(.caption).foregroundStyle(.secondary) }
                    }.padding(20).card()
                }
            } else {
                Text("Import a separately commissioned simulation mission profile to review and sign inspections. Reader and annotation profiles cannot send motion instructions.").foregroundStyle(.secondary)
            }
        }
        .sheet(item: $review) { entry in
            VStack(alignment: .leading, spacing: 18) {
                Text(entry.state == .draft ? "Authorize simulated inspection" : "Retry the saved instruction").font(.title2.bold())
                if let context = model.missionContext {
                    DetailRow(title: "Assignee", value: context.assignee)
                    DetailRow(title: "Run", value: context.runID)
                    DetailRow(title: "Frame", value: context.frameID + " @ " + context.frameRevision)
                    DetailRow(title: "Target (metres)", value: missionPosition(entry.draft.target))
                    DetailRow(title: "Maximum duration", value: "\(entry.draft.durationMS / 1000) seconds")
                    DetailRow(title: "Speed", value: "\(context.speedMPS.formatted()) m/s")
                    DetailRow(title: "Sensors", value: context.requiredSensors.joined(separator: ", "))
                    DetailRow(title: "Allowed area (metres)", value: missionPosition(context.areaMin) + " to " + missionPosition(context.areaMax))
                    DetailRow(title: "Minimum energy", value: context.minimumEnergy.formatted(.percent))
                }
                if let proposal = entry.draft.proposal {
                    DetailRow(title: "Origin / proposal", value: proposal.audience + " / " + proposal.id)
                    DetailRow(title: "Proposal body SHA-256", value: proposal.bodySHA256)
                }
                if let trigger = entry.draft.trigger {
                    DisclosureGroup("Prepared observation and destination") {
                        VStack(alignment: .leading, spacing: 10) {
                            DetailRow(title: "Original observed position", value: missionPosition(trigger.fixedTarget))
                            DetailRow(title: "Original trigger SHA-256", value: trigger.sha256)
                            if let destination = entry.draft.inspectionDestination {
                                DetailRow(title: "Destination map SHA-256", value: destination.planarConfigSHA256)
                                DetailRow(title: "Maximum horizontal offset", value: "\(destination.maxOffsetM) m")
                            }
                            if let deadline = entry.draft.preparedUntilUnixMS {
                                DetailRow(title: "Original physical deadline", value: Date(timeIntervalSince1970: Double(deadline) / 1000).formatted())
                            }
                            Text("The prepared reader owns producer verification and map preparation. File import does not prove sensor truth or present route clearance or current simulation-time freshness.").font(.caption).foregroundStyle(.secondary)
                        }.padding(.top, 8)
                    }
                }
                Text("This autonomous MOVE continues if the operator or network disconnects. The robot must hold when its required local conditions fail.")
                Text(entry.state == .draft ? "Confirming obtains fresh verified gateway time, signs this exact target and saves the instruction before sending it." : "Retry preserves the original signature, task ID, target and deadline. It will fail if the original authority has expired.").font(.callout).foregroundStyle(.secondary)
                HStack {
                    Button("Cancel") { review = nil }
                    Button(entry.state == .draft ? "Sign and send inspection" : "Retry exact instruction") {
                        let id = entry.id; review = nil
                        model.actions.submit { await model.sendMission(id: id) }
                    }.buttonStyle(.borderedProminent)
                        .disabled(!model.sceneActive || model.missionBusy || model.missionContext?.digest != reviewContext || model.missionEntries.first(where: { $0.id == entry.id })?.draft != entry.draft)
                }
            }.padding(28).frame(minWidth: 340, idealWidth: 560)
        }
    }
}

private extension JSONValue {
    var selfID: String { self["id"]?.string ?? "invalid" }
}

private func missionPosition(_ coordinates: [Double]) -> String {
    zip(["X", "Y", "Z"], coordinates).map { "\($0.0) \($0.1)" }.joined(separator: " · ")
}

private func navigationLabel(_ context: MissionContext) -> String {
    switch context.navigationProfile {
    case .oneAxis: "one axis · Y 0.2 m · Z 0 m"
    case .planar(let floor): "planar · commissioned floor Y \(floor) m"
    }
}
