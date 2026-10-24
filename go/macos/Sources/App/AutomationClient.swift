import Foundation

enum PairingKind: String, CaseIterable, Identifiable {
    case lite = "Wendy Lite", camera = "IP cameras"
    var id: Self { self }
    var bridge: WDPairingKind { self == .lite ? WD_PAIR_SENSOR : WD_PAIR_IP_CAMERA }
}
struct PairingDevice: Identifiable {
    let id, name, address, detail: String
    let cameraID: UInt32
    let paired, connected, canPair: Bool
}
struct PairingSnapshot { let devices: [PairingDevice]; let warning: String }
struct CampaignSource: Identifiable {
    let id, detail: String
    let healthy: Bool
    var title: String { detail.isEmpty ? id : "\(detail) · \(id)" }
}
struct DeviceCampaign: Identifiable {
    let name, state, revision, summary, inferenceState, problem, warnings: String
    var id: String { name }
}
enum CampaignTrigger: String, CaseIterable, Identifiable {
    case detection = "An event is detected", classification = "An image is classified", event = "An app emits an event"
    var bridge: WDCampaignTrigger { switch self { case .detection: WD_TRIGGER_DETECTION; case .classification: WD_TRIGGER_CLASSIFICATION; case .event: WD_TRIGGER_APP_EVENT } }
    var id: Self { self }
}
enum CampaignAction: String, CaseIterable, Identifiable {
    case notify = "Record a clip and send a notification", record = "Record a clip"
    var id: Self { self }
}
struct CampaignDraft {
    var name = "human-detected"
    var camera = "*"
    var trigger: CampaignTrigger = .detection
    var model = "facebook/detr-resnet-50"
    var modelRevision = ""
    var labels: Set<String> = ["person"]
    var action: CampaignAction = .notify
    var event = "model_matched"
    var threshold = 0.9
    var cooldown = 30
    var before = 10
    var after = 10
    var upload = false
    var valid: Bool {
        !name.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty && !camera.isEmpty &&
        !event.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty &&
        (trigger == .event || (!model.isEmpty && modelRevision.count == 40 && !labels.isEmpty && labels.count <= 32))
    }
}

extension GoClient {
    func pairings(session: UInt64, kind: PairingKind) async throws -> PairingSnapshot {
        try await perform {
            guard let p = WDPairings(session, kind.bridge) else { throw WendyError(message: "Missing pairing result.") }
            defer { WDPairingsFree(p) }
            try Self.check(p.pointee.status)
            let items: [PairingDevice] = UnsafeBufferPointer(start: p.pointee.items, count: p.pointee.count).map {
                PairingDevice(id: Self.string($0.id), name: Self.string($0.name), address: Self.string($0.address), detail: Self.string($0.detail),
                              cameraID: $0.camera_id, paired: $0.paired != 0, connected: $0.connected != 0, canPair: $0.can_pair != 0)
            }
            let sorted = items.sorted { a, b in a.name == b.name ? a.id < b.id : a.name.localizedStandardCompare(b.name) == .orderedAscending }
            return PairingSnapshot(devices: sorted, warning: Self.string(p.pointee.warning))
        }
    }
    func pairSensor(session: UInt64, id: String, forget: Bool = false) async throws {
        try await perform { try Self.withCString(id) { try Self.finish(forget ? WDForgetSensor(session, $0) : WDPairSensor(session, $0)) } }
    }
    func pairCamera(session: UInt64, id: UInt32, username: String, password: String) async throws {
        try await perform {
            try Self.withCString(username) { user in
                try Self.withCString(password) { password in try Self.finish(WDPairIPCamera(session, id, user, password)) }
            }
        }
    }
    func forgetCamera(session: UInt64, id: UInt32) async throws { try await perform { try Self.finish(WDForgetIPCamera(session, id)) } }
    func campaignSources(session: UInt64) async throws -> [CampaignSource] {
        try await perform {
            guard let p = WDCampaignSources(session) else { throw WendyError(message: "Missing camera sources.") }
            defer { WDCampaignSourcesFree(p) }; try Self.check(p.pointee.status)
            return UnsafeBufferPointer(start: p.pointee.items, count: p.pointee.count).map {
                CampaignSource(id: Self.string($0.id), detail: Self.string($0.detail), healthy: $0.healthy != 0)
            }.sorted { $0.id.localizedStandardCompare($1.id) == .orderedAscending }
        }
    }
    func campaigns(session: UInt64) async throws -> [DeviceCampaign] {
        try await perform {
            guard let p = WDCampaigns(session) else { throw WendyError(message: "Missing campaigns.") }
            defer { WDCampaignsFree(p) }; try Self.check(p.pointee.status)
            return UnsafeBufferPointer(start: p.pointee.items, count: p.pointee.count).map {
                DeviceCampaign(name: Self.string($0.name), state: Self.string($0.state), revision: Self.string($0.revision), summary: Self.string($0.summary),
                               inferenceState: Self.string($0.inference_state), problem: Self.string($0.problem), warnings: Self.string($0.warnings))
            }.sorted { $0.name.localizedStandardCompare($1.name) == .orderedAscending }
        }
    }
    func deployCampaign(session: UInt64, draft: CampaignDraft) async throws {
        try await perform {
            let strings = CStringStorage()
            defer { withExtendedLifetime(strings) {} }
            var input = WDCampaignInput(name: strings.add(draft.name), camera: strings.add(draft.camera), event: strings.add(draft.event),
                                        trigger: draft.trigger.bridge,
                                        action: draft.action == .notify ? WD_ACTION_NOTIFY_AND_RECORD : WD_ACTION_RECORD,
                                        threshold: draft.threshold, cooldown_seconds: UInt32(clamping: draft.cooldown),
                                        before_seconds: UInt32(clamping: draft.before), after_seconds: UInt32(clamping: draft.after), upload: draft.upload ? 1 : 0, model: strings.add(draft.model), model_revision: strings.add(draft.modelRevision), labels: nil, label_count: 0)
            var labels: [UnsafeMutablePointer<CChar>?] = draft.labels.sorted().map { strings.add($0) }
            try labels.withUnsafeMutableBufferPointer {
                input.labels = $0.baseAddress; input.label_count = $0.count
                try Self.finish(WDDeployCampaign(session, &input))
            }
        }
    }
}

struct VisionModelInfo { let revision, license: String; let labels: [String]; let weightsBytes: Int64 }
extension GoClient {
    func visionModel(_ repo: String, trigger: CampaignTrigger) async throws -> VisionModelInfo {
        try await performModelRequest {
            try Self.withCString(repo) {
                guard let p = WDVisionModel($0, trigger.bridge) else { throw WendyError(message: "Missing model details.") }
                defer { WDVisionModelFree(p) }; try Self.check(p.pointee.status)
                return VisionModelInfo(revision: Self.string(p.pointee.revision), license: Self.string(p.pointee.license), labels: UnsafeBufferPointer(start: p.pointee.labels, count: p.pointee.label_count).map { Self.string($0) }, weightsBytes: p.pointee.weights_bytes)
            }
        }
    }
}
