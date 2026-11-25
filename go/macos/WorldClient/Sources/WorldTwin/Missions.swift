import CryptoKit
import Darwin
import Foundation
import SwiftUI
import WorldClient

// Explicit private launch configuration. Loading this file does not sign or
// deliver anything; the existing WorldClient owns all authority and delivery.
struct DemoMissionConfig: Decodable {
    let version: Int
    let profile: Profile
    let credentials_file: String
    let context_file: String
    let clock_pins_file: String
    let journal_directory: String
    let handoff_file: String
    let evidence_credentials_file: String?
    let selection_request_file: String?
    let selection_response_file: String?
    struct Profile: Decodable { let kind: String; let floor_y: Double }

    func validate() throws {
        guard version == 1, profile.kind == "planar", profile.floor_y == 0.24,
              [credentials_file, context_file, clock_pins_file, journal_directory, handoff_file]
                .allSatisfy({ $0.hasPrefix("/") && !$0.contains("\0") }) else { throw MissionError.invalidProfile }
        if selection_request_file != nil || selection_response_file != nil {
            guard let request = selection_request_file, let response = selection_response_file,
                  [request, response].allSatisfy({ $0.hasPrefix("/") && !$0.contains("\0") }),
                  Set([request, response, handoff_file]).count == 3 else { throw MissionError.invalidProfile }
        }
        if let path = evidence_credentials_file {
            guard path.hasPrefix("/"), !path.contains("\0") else { throw MissionError.invalidProfile }
        }
    }
}

/// Requests preparation of one selected original. This does not authorize a task.
struct DemoMissionSelection: Sendable {
    let requestID = UUID().uuidString.lowercased()
    let runID: String
    let envelopeSHA256: String

    enum Failure: LocalizedError {
        case missingSelection, unavailable, inspectionUnavailable, unknownOutcome, timedOut
        var errorDescription: String? {
            switch self {
            case .missingSelection: return "Select a person or hazard observation, then reopen Missions."
            case .unavailable: return "That observation is no longer available for a proposal. Select a current observation."
            case .inspectionUnavailable: return "No reachable inspection point is available for this observation within the current mission limits."
            case .unknownOutcome: return "Proposal preparation has an unknown outcome. This request will not be sent again."
            case .timedOut: return "Proposal preparation did not finish in time. No inspection was signed or sent."
            }
        }
    }

    init(runID: String, selectedEnvelope: String?) throws {
        guard !runID.isEmpty, let selectedEnvelope, selectedEnvelope.utf8.count == 64,
              selectedEnvelope.utf8.allSatisfy({ (48...57).contains($0) || (97...102).contains($0) }) else { throw Failure.missingSelection }
        self.runID = runID; envelopeSHA256 = selectedEnvelope
    }

    func writeRequest(to path: String) throws {
        let value = JSONValue.object(["version": .integer(1), "request_id": .string(requestID), "run_id": .string(runID),
                                      "original_envelope_sha256": .string(envelopeSHA256)])
        let target = URL(fileURLWithPath: path)
        let temporary = target.deletingLastPathComponent().appendingPathComponent(".selection-" + UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: temporary) }
        guard FileManager.default.createFile(atPath: temporary.path, contents: try value.encoded(),
                                             attributes: [.posixPermissions: 0o600]),
              rename(temporary.path, target.path) == 0 else { throw MissionError.invalidDraft }
    }

    func responseReady(_ data: Data) throws -> Bool {
        let value = try JSONValue.decode(data, limit: 16 << 10)
        guard value["request_id"]?.string == requestID else { return false }
        guard value["version"]?.int == 1,
              value["original_envelope_sha256"]?.string == envelopeSHA256 else { throw MissionError.invalidDraft }
        switch value["status"]?.string {
        case "ready": return true
        case "failed":
            if value["error"]?.string == "selection_unavailable" { throw Failure.unavailable }
            if value["error"]?.string == "inspection_unavailable" { throw Failure.inspectionUnavailable }
            if value["error"]?.string == "unknown_outcome" { throw Failure.unknownOutcome }
            throw MissionError.invalidDraft
        default: throw MissionError.invalidDraft
        }
    }

    func waitForResponse(at path: String, timeout: Duration = .seconds(10)) async throws {
        let clock = ContinuousClock(), deadline = ContinuousClock.now + timeout
        let url = URL(fileURLWithPath: path)
        while clock.now < deadline {
            try Task.checkCancellation()
            if FileManager.default.fileExists(atPath: path) {
                let size = try url.resourceValues(forKeys: [.fileSizeKey]).fileSize ?? 0
                guard size > 0, size <= 16 << 10 else { throw MissionError.invalidDraft }
                if try responseReady(Data(contentsOf: url)) { return }
            }
            try await Task.sleep(for: .milliseconds(100))
        }
        throw Failure.timedOut
    }
}

/// An exact prepared original from the verified reader, paired with an existing
/// signed proposal. This decoder checks correspondence, not origin authority.
struct DemoMissionHandoff {
    let proposalID: String
    let trigger: MissionNativeTrigger
    let sampleValidUntil: String
    let physicalValidUntil: String
    let navigationTarget: [Double]
    let inspectionDestination: MissionInspectionDestination?
    let routeLengthM: Double?
    let nominalTravelMS: Int64?
    let minimumDurationMS: Int64?
    let navigationRoute: [[Double]]?

    init(data: Data, selectedEnvelopeSHA256: String?, requestID: String? = nil) throws {
        let value = try JSONValue.decode(data, limit: 512 << 10)
        guard value["version"]?.int == 1, let id = value["proposal_id"]?.string, !id.isEmpty,
              requestID == nil || value["request_id"]?.string == requestID,
              id.utf8.count <= 128, let input = value["mission_input"], input["version"]?.int == 1,
              let bytes = input["trigger_json"]?.string.flatMap({ Data(base64Encoded: $0) }),
              let targetValues = input["target"]?.array, targetValues.count == 3,
              let target = input["target"]?.array?.compactMap(\.double), target.count == 3,
              let envelope = input["envelope_sha256"]?.string,
              value["original_envelope_sha256"]?.string == envelope,
              selectedEnvelopeSHA256 == nil || selectedEnvelopeSHA256 == envelope,
              let sample = input["sample_valid_until"]?.string, !sample.isEmpty, sample.count <= 64,
              let physical = input["physical_valid_until"]?.string, !physical.isEmpty, physical.count <= 64 else { throw MissionError.invalidDraft }
        let trigger = try MissionNativeTrigger(canonical: bytes, fixedTarget: target, originalEnvelopeSHA256: envelope)
        guard input["trigger_sha256"]?.string == trigger.sha256,
              input["result_schema"]?.string == trigger.resultSchema else { throw MissionError.invalidDraft }
        if let encoded = value["envelope_json"] {
            guard let original = encoded.string.flatMap({ Data(base64Encoded: $0) }),
                  SHA256.hash(data: original).map({ String(format: "%02x", $0) }).joined() == envelope else { throw MissionError.invalidDraft }
        }
        if let raw = value["inspection_destination"] {
            inspectionDestination = try MissionInspectionDestination(json: raw)
            guard let values = value["navigation_target"]?.array, values.count == 3 else { throw MissionError.invalidDraft }
            let point = values.compactMap(\.double)
            guard point.count == 3, point.allSatisfy({ $0.isFinite && abs($0) <= 1_000_000 }),
                  point[1] == inspectionDestination!.floorY,
                  hypot(point[0]-trigger.fixedTarget[0], point[2]-trigger.fixedTarget[2]) <= inspectionDestination!.maxOffsetM else { throw MissionError.invalidDraft }
            navigationTarget = point
        } else {
            guard value["navigation_target"] == nil else { throw MissionError.invalidDraft }
            inspectionDestination = nil; navigationTarget = trigger.fixedTarget
        }
        routeLengthM = value["route_length_m"]?.double
        nominalTravelMS = value["nominal_travel_ms"]?.int
        minimumDurationMS = value["minimum_duration_ms"]?.int
        let estimateKeys = ["route_length_m", "nominal_travel_ms", "minimum_duration_ms"]
        if estimateKeys.contains(where: { value[$0] != nil }) {
            guard inspectionDestination != nil, let length = routeLengthM, length.isFinite, (0...1_000_000).contains(length),
                  let nominal = nominalTravelMS, (0...86_400_000).contains(nominal), let minimum = minimumDurationMS,
                  minimum >= nominal, minimum > 0, minimum <= 86_400_000 else { throw MissionError.invalidDraft }
        }
        if let destination = inspectionDestination {
            guard estimateKeys.allSatisfy({ value[$0] != nil }), let navigation = value["navigation"], navigation["version"]?.int == 1,
                  let startRaw = navigation["start_position_m"]?.array, startRaw.count == 3,
                  let routeRaw = navigation["route_world"]?.array, !routeRaw.isEmpty, routeRaw.count <= 4096,
                  ["robot_id", "state_boot_id", "state_session"].allSatisfy({ navigation[$0]?.string.map { !$0.isEmpty && $0.utf8.count <= 512 } == true }),
                  navigation["state_sequence"]?.uint != nil,
                  let captured = navigation["state_sample_unix_ms"]?.int, captured > 0,
                  let time = navigation["simulation_time_s"]?.double, time.isFinite, (0...86_400).contains(time),
                  let offset = navigation["offset_m"]?.double, offset.isFinite, offset >= 0, offset <= destination.maxOffsetM,
                  let basis = navigation["estimate_basis"]?.string, !basis.isEmpty, basis.utf8.count <= 1024 else { throw MissionError.invalidDraft }
            let start = startRaw.compactMap(\.double)
            let route = routeRaw.map { $0.array?.compactMap(\.double) ?? [] }
            guard start.count == 3, route.allSatisfy({ $0.count == 3 && $0.allSatisfy { $0.isFinite && abs($0) <= 1_000_000 } && $0[1] == destination.floorY }),
                  route.first == start, route.last == navigationTarget,
                  abs(offset-hypot(navigationTarget[0]-trigger.fixedTarget[0],navigationTarget[2]-trigger.fixedTarget[2])) <= 0.000001 else { throw MissionError.invalidDraft }
            navigationRoute = route
        } else {
            guard value["navigation"] == nil else { throw MissionError.invalidDraft }
            navigationRoute = nil
        }
        proposalID = id; self.trigger = trigger; sampleValidUntil = sample; physicalValidUntil = physical
    }
}

@MainActor final class DemoMissions: ObservableObject {
    struct ReviewSummary {
        let assignee: String
        let site: String
        let speedMPS: Double
        let frameID: String
        let frameRevision: String
        var durationMS: Int64
    }
    @Published private(set) var handoff: DemoMissionHandoff?
    @Published private(set) var reviewSummary: ReviewSummary?
    @Published private(set) var draft: MissionDraft?
    @Published private(set) var reviewedPayloadBytes: Int?
    @Published private(set) var entries: [MissionEntry] = []
    @Published private(set) var busy = false
    @Published private(set) var enabled = false
    @Published private(set) var message = "Load an observed location to review an inspection."
    @Published var openedResult: DemoInspectionPresentation?
    @Published var durationSeconds = 30.0
    private var client: MissionClient?
    private var journal: MissionJournal?
    private var mediaClient: WorldClient?
    private var mediaCache: InspectionMediaCache?
    private var mediaNotice: String?
    private var loadGeneration = 0

    private func read(_ path: String, limit: Int) throws -> Data {
        let url = URL(fileURLWithPath: path), size = try url.resourceValues(forKeys: [.fileSizeKey]).fileSize ?? 0
        guard path.hasPrefix("/"), size > 0, size <= limit else { throw MissionError.invalidProfile }
        let data = try Data(contentsOf: url)
        guard data.count <= limit else { throw MissionError.invalidProfile }
        return data
    }
    func load(configURL: URL, runID: String?, selectedEnvelope: String?, replay: Bool) async {
        guard !busy else { return }
        busy = true; defer { busy = false }
        suspend(); handoff = nil; reviewSummary = nil; draft = nil; entries = []; openedResult = nil
        mediaClient = nil; mediaCache = nil; mediaNotice = nil
        let generation = loadGeneration
        do {
            let config = try JSONDecoder().decode(DemoMissionConfig.self, from: read(configURL.path, limit: 16 << 10))
            try config.validate()
            let credentials = try MissionCredentials(data: read(config.credentials_file, limit: 128 << 10))
            let context = try JSONValue.decode(read(config.context_file, limit: 32 << 10))
            let pins = try MissionClockPins(json: JSONValue.decode(read(config.clock_pins_file, limit: 16 << 10)))
            let client = try MissionClient(credentials: credentials, context: context, pins: pins,
                                           navigationProfile: .planar(floorY: config.profile.floor_y))
            guard !replay, client.context.runID == runID else { throw MissionError.invalidContext }
            let journal = try MissionJournal(directory: URL(fileURLWithPath: config.journal_directory), credentials: credentials, context: client.context)
            self.client = client; self.journal = journal
            reviewSummary = ReviewSummary(assignee: client.context.assignee, site: credentials.site,
                speedMPS: client.context.speedMPS, frameID: client.context.frameID,
                frameRevision: client.context.frameRevision, durationMS: min(30_000, client.context.maxDurationMS))
            entries = await journal.entries()
            try Task.checkCancellation()
            guard generation == loadGeneration else { throw CancellationError() }
            enabled = true
            do {
                mediaCache = try InspectionMediaCache(directory: URL(fileURLWithPath: config.journal_directory).appendingPathComponent("inspection-media"), scopeID: journal.scopeID)
                if let path = config.evidence_credentials_file {
                    let media = try WorldCredentials(data: read(path, limit: 128 << 10))
                    guard media.tenant == credentials.tenant, media.site == credentials.site,
                          media.audience == credentials.audience, media.endpoint == credentials.endpoint,
                          media.scopes == ["read", "evidence-read"] else { throw MissionError.invalidProfile }
                    mediaClient = WorldClient(credentials: media)
                }
            } catch { mediaNotice = "Media access is unavailable. Saved result details remain available." }
            message = entries.isEmpty ? "Select an observation to prepare an inspection." : "Saved inspections are available below. Select an observation to prepare another."
            var requestID: String?
            if let request = config.selection_request_file, let response = config.selection_response_file {
                guard let selectedEnvelope, !selectedEnvelope.isEmpty else { return }
                let selection = try DemoMissionSelection(runID: client.context.runID, selectedEnvelope: selectedEnvelope)
                message = "Preparing a proposal for the selected observation…"
                try selection.writeRequest(to: request)
                try await selection.waitForResponse(at: response)
                requestID = selection.requestID
            } else if selectedEnvelope == nil && !FileManager.default.fileExists(atPath: config.handoff_file) {
                return
            }
            try Task.checkCancellation()
            guard generation == loadGeneration else { throw CancellationError() }
            let prepared = try DemoMissionHandoff(data: read(config.handoff_file, limit: 512 << 10), selectedEnvelopeSHA256: selectedEnvelope, requestID: requestID)
            let minimum = prepared.minimumDurationMS ?? 1
            guard minimum <= client.context.maxDurationMS else { throw DemoMissionSelection.Failure.inspectionUnavailable }
            try prepared.trigger.validate(context: client.context)
            try client.context.validate(target:prepared.navigationTarget,durationMS:minimum)
            handoff = prepared
            durationSeconds = Double(min(client.context.maxDurationMS, max(30_000, minimum))) / 1000
            reviewSummary?.durationMS = Int64((durationSeconds * 1000).rounded())
            message = "Review the observation and fixed inspection destination before signing."
        } catch { message = error.localizedDescription }
    }
    func suspend() { loadGeneration += 1; enabled = false; client?.setMissionActive(false) }
    func checkRun(_ runID: String?, replay: Bool) {
        if replay || client?.context.runID != runID { suspend(); message = "The displayed simulation changed. Reopen Missions to review its configuration." }
    }
    func openResult(_ id: String) {
        guard let client, let entry = entries.first(where: { $0.id == id }) else { return }
        do {
            let result = try MissionInspectionResult(entry: entry, context: client.context)
            openedResult = DemoInspectionPresentation(result: result,
                media: DemoInspectionMedia(cache: mediaCache, client: mediaClient, notice: mediaNotice))
        } catch { message = "This saved inspection has no complete result to display." }
    }
    func review() async {
        guard enabled, !busy, let handoff, let client, let journal else { return }
        busy = true; defer { busy = false }
        do {
            let proposal = try await client.proposal(id: handoff.proposalID)
            guard enabled else { throw CancellationError() }
            guard durationSeconds.isFinite, durationSeconds > 0,
                  durationSeconds <= Double(client.context.maxDurationMS) / 1000 else { throw MissionError.invalidDraft }
            let duration = Int64((durationSeconds * 1000).rounded())
            guard duration >= (handoff.minimumDurationMS ?? 1) else { throw MissionError.invalidDraft }
            let value = try MissionDraft(target: handoff.navigationTarget,
                                         durationMS: duration, context: client.context,
                                         proposal: proposal, trigger: handoff.trigger, inspectionDestination: handoff.inspectionDestination)
            if value.trigger?.resultSchema == "wendy.rising-incident.inspection.v1" {
                let count=try client.context.reviewedInstructionPayloadSize(draft:value)
                guard count<=32<<10 else{throw MissionError.invalidDraft};reviewedPayloadBytes=count
            }
            try await journal.saveDraft(value)
            draft = value; reviewSummary?.durationMS = value.durationMS; entries = await journal.entries()
            message = "Reviewed. Sign and send authorizes this simulated inspection."
        } catch { message = error.localizedDescription }
    }
    func signAndSend() async {
        guard enabled, !busy, let draft, let client, let journal else { return }
        busy = true; defer { busy = false }
        do { try await journal.signAndSend(id: draft.id, using: client); message = "Inspection submitted. Check its status below." }
        catch { message = error.localizedDescription }
        entries = await journal.entries()
    }
    func reconcile(_ id: String) async {
        guard enabled, !busy, let client, let journal else { return }
        busy = true; defer { busy = false }
        do { try await journal.reconcile(id: id, using: client); message = "Status refreshed." }
        catch { message = error.localizedDescription }
        entries = await journal.entries()
    }
    func retry(_ id: String) async {
        guard enabled, !busy, let client, let journal else { return }
        busy = true; defer { busy = false }
        do { try await journal.retry(id: id, using: client); message = "The saved instruction was sent again without changing it." }
        catch { message = error.localizedDescription }
        entries = await journal.entries()
    }
    func stop(_ id: String) async {
        // Stop remains a separate operation, including while delivery is pending.
        guard let client, let journal else { return }
        do { try await journal.stop(id: id, using: client); message = "Stop acknowledged." }
        catch { message = error.localizedDescription }
        entries = await journal.entries()
    }
    var canSign: Bool { enabled && !busy && draft.map { value in entries.contains { $0.id == value.id && $0.state == .draft } } == true }
}

struct DemoMissionLauncher: View {
    let configURL: URL
    let runID: String?
    let selectedEnvelope: String?
    let replay: Bool
    var onNavigationPreview: (DemoInspectionRoute?) -> Void = { _ in }
    @StateObject private var missions = DemoMissions()
    @State private var presented = false
    @State private var openedRunID: String?
    @State private var openedEnvelope: String?
    @State private var openedReplay = false
    var body: some View {
        Button {
            onNavigationPreview(nil)
            openedRunID = runID; openedEnvelope = selectedEnvelope; openedReplay = replay
            presented = true
        } label: { Label("Missions", systemImage: "location.circle") }
            .buttonStyle(.bordered).disabled(replay || runID == nil)
            .sheet(isPresented: $presented) {
                DemoMissionPanel(missions: missions)
                    .task {
                        await missions.load(configURL: configURL, runID: openedRunID, selectedEnvelope: openedEnvelope, replay: openedReplay)
                        guard !Task.isCancelled, missions.enabled, let handoff = missions.handoff, let run = openedRunID,
                              let route = handoff.navigationRoute else { return }
                        onNavigationPreview(try? DemoInspectionRoute(runID:run,originalEnvelopeSHA256:handoff.trigger.originalEnvelopeSHA256,
                            observed:handoff.trigger.fixedTarget,target:handoff.navigationTarget,route:route))
                    }
                    .onDisappear { missions.suspend() }
            }
            .onChange(of: runID) { _, value in missions.checkRun(value, replay: replay); onNavigationPreview(nil) }
            .onChange(of: replay) { _, value in missions.checkRun(runID, replay: value); onNavigationPreview(nil) }
    }
}

struct DemoMissionPanel: View {
    @State private var actions = TwinActions()

    @ObservedObject var missions: DemoMissions
    @Environment(\.dismiss) private var dismiss
    private func coordinates(_ target: [Double]) -> String { target.map { String(format: "%.2f", $0) }.joined(separator: ", ") }
    var body: some View {
        Group {

        VStack(alignment: .leading, spacing: 18) {
            HStack {
                VStack(alignment: .leading, spacing: 4) {
                    Text(missions.handoff == nil ? "Inspections" : "Inspect an observed location").font(.title2.weight(.semibold))
                    Text("SIMULATED INSPECTION").font(.caption.weight(.semibold)).foregroundStyle(.secondary)
                }
                Spacer(); Button("Close") { dismiss() }
            }
            Divider()
            if let handoff = missions.handoff {
                let trigger = missions.draft?.trigger ?? handoff.trigger
                Grid(alignment: .leading, horizontalSpacing: 20, verticalSpacing: 8) {
                    GridRow { Text("Observation"); Text(trigger.resultSchema == "wendy.person.track.v1" ? "Person location" : trigger.resultSchema == "wendy.incident.inspection.v1" ? "Gas / visual association" : trigger.resultSchema == "wendy.rising-incident.inspection.v1" ? "Spatially joined rising gas episode" : "Hazard location") }
                    if trigger.resultSchema == "wendy.person.track.v1" {
                        GridRow { Text("Observed location · X, Z"); Text(coordinates([trigger.fixedTarget[0],trigger.fixedTarget[2]]) + " m").monospacedDigit() }
                    } else {
                        GridRow { Text("Observed location · X, Y, Z"); Text(coordinates(trigger.fixedTarget) + " m").monospacedDigit() }
                    }
                    GridRow { Text("Inspection destination · X, Y, Z"); Text(coordinates(missions.draft?.target ?? handoff.navigationTarget) + " m").monospacedDigit() }
                    GridRow { Text("Simulation observation time"); Text(trigger.sampleTime).textSelection(.enabled) }
                    if let summary = missions.reviewSummary {
                        GridRow { Text("Assignee"); Text(summary.assignee).fixedSize(horizontal: false, vertical: true).textSelection(.enabled) }
                        GridRow { Text("Site"); Text(summary.site).textSelection(.enabled) }
                        GridRow { Text("Signed speed limit"); Text(String(format: "%.2f m/s", summary.speedMPS)).monospacedDigit() }
                        GridRow {
                            Text("Inspection duration")
                            if let draft = missions.draft { Text(String(format: "%.3g seconds", Double(draft.durationMS) / 1000)).monospacedDigit() }
                            else { HStack { TextField("Seconds",value:$missions.durationSeconds,format:.number.precision(.fractionLength(0...3))).textFieldStyle(.roundedBorder).frame(width:90); Text("seconds") }.disabled(missions.busy) }
                        }
                    }
                    if let length = handoff.routeLengthM { GridRow { Text("Planned route"); Text(String(format:"%.2f m",length)) } }
                    if let travel = handoff.nominalTravelMS { GridRow { Text("Estimated travel time"); Text(String(format:"%.1f seconds",Double(travel)/1000)) } }
                    if let minimum = handoff.minimumDurationMS { GridRow { Text("Minimum proposed duration"); Text(String(format:"%.3g seconds",Double(minimum)/1000)) } }
                }.font(.callout)
                if trigger.resultSchema == "wendy.incident.inspection.v1" { IncidentTriggerReview(trigger:trigger) }
                if trigger.resultSchema == "wendy.rising-incident.inspection.v1" { RisingTriggerReview(trigger:trigger) }
                DisclosureGroup("Observation details") {
                    Grid(alignment: .leading, horizontalSpacing: 16, verticalSpacing: 8) {
                        GridRow { Text("Schema"); Text(trigger.resultSchema).textSelection(.enabled) }
                        if let summary = missions.reviewSummary { GridRow { Text("Frame"); Text(summary.frameID + " / " + summary.frameRevision).fixedSize(horizontal: false, vertical: true).textSelection(.enabled) } }
                        ForEach(["device", "app", "service", "stream"], id: \.self) { field in
                            GridRow { Text(field.capitalized); Text(trigger.source[field]?.string ?? "").fixedSize(horizontal: false, vertical: true).textSelection(.enabled) }
                        }
                        GridRow { Text("Original digest"); Text(trigger.originalEnvelopeSHA256).font(.system(.caption, design: .monospaced)).fixedSize(horizontal: false, vertical: true).textSelection(.enabled) }
                        if let destination = handoff.inspectionDestination {
                            GridRow { Text("Planar map digest"); Text(destination.planarConfigSHA256).font(.system(.caption,design:.monospaced)).fixedSize(horizontal:false,vertical:true).textSelection(.enabled) }
                        }
                    }.font(.caption).padding(.top, 8)
                }.font(.callout)
                Text(handoff.inspectionDestination == nil ? "This is a retained observed location. The destination stays fixed if the person moves or the observation expires." : "The inspection uses a reachable nearby floor point. The original observed location is unchanged. Travel time is an estimate; the reviewed destination and duration stay fixed.")
                    .font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                HStack {
                    Button("Review proposal") { actions.submit { [self] in  await missions.review() } }
                        .disabled(!missions.enabled || missions.busy || missions.draft != nil)
                    Button("Sign and send inspection") { actions.submit { [self] in  await missions.signAndSend() } }
                        .buttonStyle(.borderedProminent).tint(.teal).disabled(!missions.canSign).opacity(missions.canSign ? 1 : 0.45)
                    if missions.busy { ProgressView().controlSize(.small) }
                }
            }
            Text(missions.message).font(.callout).fixedSize(horizontal: false, vertical: true)
            if !missions.entries.isEmpty {
                Divider(); Text("Saved inspections").font(.headline)
                ScrollView {
                    VStack(alignment: .leading, spacing: 12) {
                        ForEach(missions.entries.reversed()) { entry in
                            VStack(alignment: .leading, spacing: 6) {
                                Text(coordinates(entry.draft.target) + " m · " + entry.phase).font(.callout.weight(.medium))
                                Text(entry.id).font(.caption).foregroundStyle(.secondary).textSelection(.enabled)
                                HStack {
                                    if entry.record?["result"] != nil {
                                        Button("Open result") { missions.openResult(entry.id) }
                                    }
                                    Button("Check status") { actions.submit { [self] in  await missions.reconcile(entry.id) } }.disabled(missions.busy || !missions.enabled || entry.instruction == nil)
                                    if entry.state == .uncertain || entry.state == .signed {
                                        Button("Retry saved instruction") { actions.submit { [self] in  await missions.retry(entry.id) } }.disabled(missions.busy || !missions.enabled)
                                    }
                                    if entry.instruction != nil && !entry.terminal { Button("Stop inspection", role: .destructive) { actions.submit { [self] in  await missions.stop(entry.id) } } }
                                }.controlSize(.small)
                            }
                        }
                    }
                }.frame(maxHeight: 180)
            }
        }.padding(28).frame(width: 640).foregroundStyle(Color(red: 0.14, green: 0.22, blue: 0.24))
            .background(Color(red: 0.97, green: 0.97, blue: 0.95)).preferredColorScheme(.light)
            .sheet(item: $missions.openedResult) { presentation in DemoInspectionResultView(presentation: presentation) }

        }.task { await actions.run() }
}
}
