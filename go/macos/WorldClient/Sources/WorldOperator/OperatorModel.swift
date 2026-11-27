import Foundation
import CryptoKit
import SwiftUI
import WorldClient
#if DEBUG && os(macOS)
import AppKit
#endif

func value(_ json: JSONValue?, _ keys: String...) -> JSONValue? {
    keys.reduce(json) { $0?[$1] }
}

func text(_ json: JSONValue?, _ fallback: String = "Unavailable") -> String {
    json?.string ?? fallback
}

// Length-prefixing keeps source/frame strings containing separators distinct.
func tupleID(_ parts: [String]) -> String { parts.map { "\($0.utf8.count):\($0)" }.joined() }

struct FrameChoice: Identifiable {
    let id: String
    let title: String
}

enum AnnotationInput {
    static func millimetres(_ coordinates: [String]) throws -> [Int64] {
        guard coordinates.count == 3 else { throw AnnotationError.invalidNote }
        return try coordinates.map { value in
            let text = value.trimmingCharacters(in: .whitespacesAndNewlines)
            guard text.range(of: "^[+-]?[0-9]{1,7}(\\.[0-9]{1,3})?$", options: .regularExpression) != nil,
                  let metres = Double(text), metres.isFinite, abs(metres) <= 1_000_000 else { throw AnnotationError.invalidNote }
            return Int64((metres * 1000).rounded())
        }
    }
}

func dateValue(_ string: String?) -> Date? {
    guard let string else { return nil }
    let parser = ISO8601DateFormatter()
    parser.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    return parser.date(from: string) ?? ISO8601DateFormatter().date(from: string)
}

struct EvidenceItem: Identifiable {
    let digest: String
    let contentType: String
    let size: Int
    let name: String
    let nativeReference: NativeEvidenceReference?
    var id: String { tupleID([nativeReference?.cacheKey ?? digest, name]) }

    init?(_ raw: JSONValue?, name: String) {
        guard let digest = raw?["sha256"]?.string, digest.count == 64,
              let size = raw?["size"]?.int, size > 0, size <= 8 << 20,
              let type = raw?["content_type"]?.string else { return nil }
        self.digest = digest
        self.contentType = type
        self.size = Int(size)
        self.name = name
        nativeReference = nil
    }

    init(native: NativeInferenceEvidence) {
        digest = native.sha256; contentType = native.contentType; size = native.size
        nativeReference = native.reference
        let kind = native.isOriginalImage ? "Original PNG" : contentType == "audio/wav" ? "Audio" : "Image"
        name = text(native.reference.source["stream"]) + " · " + kind
    }
}

struct Observation: Identifiable {
    let raw: JSONValue
    let native: NativeInference?
    init(raw: JSONValue) {
        self.raw = raw
        native = (try? WorldComponent(raw: raw)).flatMap { try? NativeInference(component: $0) }
    }
    var id: String {
        tupleID([value(raw, "key", "tenant"), value(raw, "key", "site"), value(raw, "key", "entity"), value(raw, "key", "type"), value(raw, "key", "source", "device"), value(raw, "key", "source", "app"), value(raw, "key", "source", "service"), value(raw, "key", "source", "stream")].map { text($0, "") })
    }
    var payload: JSONValue? { native?.result ?? raw["payload"] }
    var kind: String {
        if let native { return native.kind.rawValue }
        if type == NativeInference.componentType { return "observation" }
        return ["wendy.annotation.note.v1", "wendy.annotation.note.v2"].contains(type) ? "annotation" : type.contains("hazard") ? "hazard" : text(payload?["kind"], "observation")
    }
    var type: String { text(value(raw, "key", "type"), "") }
    var name: String {
        if let person = native?.person { return "Person " + String(person.id.suffix(8)) }
        if native?.hazard != nil { return "Possible gas hazard" }
        if type == NativeInference.componentType { return "Unsupported inference" }
        return kind == "annotation" ? "Operator note" : text(payload?["sensor_id"], text(payload?["label"], text(value(raw, "key", "entity"))))
    }
    var frame: String { text(value(raw, "frame", "id"), "Unknown frame") + " @ " + text(value(raw, "frame", "revision"), "?") }
    var frameKey: String { tupleID([text(value(raw, "frame", "id"), ""), text(value(raw, "frame", "revision"), "")]) }
    var position: [Double]? {
        if type == NativeInference.componentType { return native?.hazard?.position }
        if kind == "annotation" {
            guard let array = payload?["position_mm"]?.array, array.count == 3 else { return nil }
            let values = array.compactMap(\.int)
            guard values.count == 3, values.allSatisfy({ (-1_000_000_000...1_000_000_000).contains($0) }) else { return nil }
            return values.map { Double($0) / 1000 }
        }
        guard let array = payload?["position_m"]?.array, array.count == 3 else { return nil }
        let values = array.compactMap(\.double)
        guard values.count == 3,
              values.allSatisfy({ $0.isFinite && abs($0) <= 1_000_000 }) else { return nil }
        return values
    }
    // Person records report X/Z only. Never fabricate a height for a 3D position.
    var planPositionXZ: [Double]? { native?.positionXZ ?? position.map { [$0[0], $0[2]] } }
    var origin: String { text(value(raw, "provenance", "origin"), "unknown") }
    var device: String { text(value(raw, "key", "source", "device")) }
    var sampleTime: Date? { dateValue(value(raw, "timing", "sample_time")?.string) }
    var evidence: [EvidenceItem] {
        if type == NativeInference.componentType {
            var seen = Set<String>()
            return (native?.evidence ?? []).map(EvidenceItem.init(native:)).filter { seen.insert($0.id).inserted }
        }
        if let one = EvidenceItem(payload?["evidence"], name: name) { return [one] }
        return (payload?["evidence"]?.array ?? []).enumerated().compactMap { EvidenceItem($0.element, name: "\(name) · evidence \($0.offset + 1)") }
    }
    func freshness(at now: Date, offline: Bool) -> String {
        if type == NativeInference.componentType {
            guard let native else { return "Unsupported result" }
            if offline { return "Last-known" }
            switch native.freshness(at: now) {
            case .fresh: return "Fresh"
            case .stale: return "Expired"
            case .unknown: return "Timing unknown"
            }
        }
        if kind == "annotation" {
            guard let expiry = payload?["expires_at_unix_ms"]?.double else { return "Expiry unknown" }
            _ = expiry
            if type == "wendy.annotation.note.v1" { return "Legacy assertion · age unverified" }
            return offline ? "Saved assertion · age unverified" : "Human assertion · gateway-relative time"
        }
        if offline { return "Saved" }
        guard let component = try? WorldComponent(raw: raw) else { return "Unknown" }
        switch component.freshness(at: now) {
        case .fresh: return "Fresh"
        case .stale: return "Expired"
        case .unknown: return "Unknown clock"
        }
    }
}

struct SitePlacement: Identifiable {
    let publication: JSONValue
    let geographic: GeographicPlacement
    init(publication: JSONValue) {
        self.publication = publication
        geographic = GeographicPlacement(document: value(publication, "component", "payload") ?? .object([:]))
    }
    var payload: JSONValue? { value(publication, "component", "payload") }
    var document: JSONValue? { payload }
    var id: String {
        tupleID([value(publication, "component", "key", "tenant"), value(publication, "component", "key", "site"), value(publication, "component", "key", "entity"), value(publication, "component", "key", "type"), value(publication, "component", "key", "source", "device"), value(publication, "component", "key", "source", "app"), value(publication, "component", "key", "source", "service"), value(publication, "component", "key", "source", "stream")].map { text($0, "") })
    }
    var revision: String { text(payload?["revision"]) }
    var site: String { text(payload?["site"]) }
    var latitude: Double? { geographic.latitude }
    var longitude: Double? { geographic.longitude }
    var uncertainty: Double? { geographic.anchorUncertaintyMetres }
    var placement: String { text(payload?["placement"], "Unspecified placement") }
    var validFrom: Date? { dateValue(payload?["valid_from"]?.string) }
    var validUntil: Date? { dateValue(payload?["valid_until"]?.string) }
    func isValid(at now: Date) -> Bool { geographic.state(at: now) == .usable }
}

struct MissionItem: Identifiable {
    let raw: JSONValue
    var id: String { tupleID([text(raw["audience"], ""), text(raw["task_id"], "")]) }
    var name: String { text(raw["task_id"]) }
    var status: JSONValue? { raw["status"] }
    var record: JSONValue? { status?["record"] }
    var phase: String { text(value(record, "status", "Phase"), "queued") }
    var assignee: String { text(status?["assignee"], text(raw["audience"])) }
    var result: JSONValue? { record?["result"] }
    var observedAt: Date? { status?["signed_at_unix_ms"]?.double.map { Date(timeIntervalSince1970: $0 / 1000) } }
    var evidence: [EvidenceItem] {
        (result?["samples"]?.array ?? []).compactMap { sample in
            EvidenceItem(sample["evidence"], name: text(sample["sensor_id"]))
        }
    }
}

struct EvidencePreview: Identifiable {
    let item: EvidenceItem
    let data: Data
    var retainUntil: Date? = nil
    var fromCache = false
    var id: String { item.id }
    func available(at now: Date) -> Bool { retainUntil.map { now < $0 } ?? true }
}

@MainActor
final class OperatorModel: ObservableObject {
    @Published var areaEntries: [AreaWorkspace] = []
    @Published var areaSelectedID: String?
    @Published var areaNotice: String?
    @Published var areaBusy = false
    @Published var areaOnline = false
    @Published var showAreasForTesting = false
    private var areaJournal: AreaJournal?
    private let areaDirectory: URL
    private var areaGeneration = UUID()
    @Published var snapshot: WorldSnapshot?
    @Published var sites: JSONValue = .object([:])
    @Published var tasks: JSONValue = .object([:])
    @Published var site = "No site connected"
    @Published var endpoint = "Import a simulation profile"
    @Published var state = "Disconnected"
    @Published private(set) var readMode: WorldReadMode = .full
    @Published var notice: String?
    @Published var fetchedAt: Date?
    @Published var selectedSiteID = ""
    @Published var selectedFrame = ""
    @Published var selectedObservation: String?
    @Published var enabledLayers: Set<String> = ["camera", "gas", "audio", "hazard", "person", "observation", "annotation"]
    @Published var missionContext: MissionContext?
    @Published var missionEntries: [MissionEntry] = []
    @Published var missionStopEntries: [MissionStopEntry] = []
    @Published var missionProposals: [JSONValue] = []
    @Published var reviewedProposal: MissionProposal?
    @Published var missionNotice: String?
    @Published var missionBusy = false
    @Published var missionStopping = false
    @Published var showMissionsForTesting = false
    @Published var inboxOrigins: [ProposalInboxOrigin] = []
    @Published var inboxDocuments: [ProposalInboxDocument] = []
    @Published var inboxRows: [RemoteProposal] = []
    @Published var inboxSelectedOrigin = ""
    @Published var inboxSelectedID = ""
    @Published var inboxReviewedID: String?
    @Published var inboxPrepared: PreparedProposalHandoff?
    @Published var inboxNotice: String?
    @Published var inboxBusy = false
    @Published var inboxOnline = false
    private var inboxClient: ProposalInboxClient?
    private var inboxJournal: ProposalInboxJournal?
    private let inboxDirectory: URL
    private var missionClient: MissionClient?
    private var missionJournal: MissionJournal?
    private let missionDirectory: URL
    @Published var annotationContext: AnnotationContext?
    @Published var annotationEntries: [AnnotationEntry] = []
    @Published var amendmentEntries: [AnnotationAmendmentEntry] = []
    @Published var annotationViews: [JSONValue] = []
    @Published var amendmentNotice: String?
    @Published var amendmentBusy = false
    @Published var annotationViewsUnavailable = true
    @Published var annotationSection = "Shared state"
    private var amendmentOutbox: AnnotationAmendmentOutbox?
    private var annotationViewsCache: AnnotationViewsCache?

    @Published var annotationNotice: String?
    @Published var annotationClockStatus = "Unknown. New text stays untimed."
    @Published var annotationBusy = false
    @Published var showNotesForTesting = false
    @Published var refreshing = false
    @Published var paused = false
    @Published var preview: EvidencePreview?
    @Published var evidenceLoading: String?
    var sceneActive = true
    private var client: WorldClient?
    private var evidenceClients: [WorldClient] = []
    private var nativeMediaCache: InspectionMediaCache?
    private let nativeMediaDirectory: URL
    private var credentials: WorldCredentials?
    let actions = WorldActions()
    private var generation = UUID()
    private var visibilityGeneration = 0
    private let cache: OfflineSnapshotCache
    private let annotationDirectory: URL
    private var annotationOutbox: AnnotationOutbox?
    #if DEBUG
    private var annotationTestStage: String?
    private var missionTestStage: String?
    #endif

    init(storageDirectory: URL? = nil) {
        let base = storageDirectory ?? FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask).first ?? FileManager.default.temporaryDirectory
        inboxDirectory = base.appendingPathComponent("WendyWorld/ProposalInbox", isDirectory: true)
        areaDirectory = base.appendingPathComponent("WendyWorld/Areas", isDirectory: true)
        cache = OfflineSnapshotCache(directory: base.appendingPathComponent("WendyWorld/ReadCache", isDirectory: true))
        missionDirectory = base.appendingPathComponent("WendyWorld/Missions", isDirectory: true)
        annotationDirectory = base.appendingPathComponent("WendyWorld/Annotations", isDirectory: true)
        nativeMediaDirectory = base.appendingPathComponent("WendyWorld/NativeMedia", isDirectory: true)
    }

    var observations: [Observation] { (snapshot?.components ?? []).map { Observation(raw: $0.raw) }.filter { !$0.type.contains("site") } }
    var visibleObservations: [Observation] { observations.filter { enabledLayers.contains($0.kind) && $0.frameKey == selectedFrame } }
    var placements: [SitePlacement] { (sites["publications"]?.array ?? []).map { SitePlacement(publication: $0) }.filter { $0.site == site }.sorted { $0.id < $1.id } }
    var placement: SitePlacement? { placements.first { $0.id == selectedSiteID } }
    var missions: [MissionItem] { (tasks["entries"]?.array ?? []).map { MissionItem(raw: $0) }.sorted { $0.name < $1.name } }
    var frames: [FrameChoice] {
        var names: [String: String] = [:]
        for observation in observations { names[observation.frameKey] = observation.frame }
        if let context = annotationContext { names[tupleID([context.frameID, context.frameRevision])] = context.frameID + " @ " + context.frameRevision }
        return names.map { FrameChoice(id: $0.key, title: $0.value) }.sorted { $0.id < $1.id }
    }
    var offline: Bool { state != "Connected" }
    var selected: Observation? { observations.first { $0.id == selectedObservation } }
    var canMission: Bool { missionClient != nil && missionJournal != nil }
    var canAnnotate: Bool { annotationOutbox != nil && credentials?.canAnnotate == true }
    var annotationAuthor: String { credentials?.principal ?? "Unavailable" }
    var annotationFrameKey: String { annotationContext.map { tupleID([$0.frameID, $0.frameRevision]) } ?? "" }

    /// Refresh is owned by the visible workspace. Hiding it revokes active
    /// mission participation and cancels refresh; reopening preserves journals.
    func run() async {
        visibilityGeneration += 1
        let visible = visibilityGeneration
        setSceneActive(true)
        defer { if visibilityGeneration == visible { setSceneActive(false) } }
        while !Task.isCancelled {
            if !paused && sceneActive { await refresh() }
            do { try await Task.sleep(for: .seconds(3)) } catch { return }
        }
    }

    /// Retains the original bounded model acceptance hooks inside the integrated
    /// app. Only explicit DEBUG launch arguments can enter these simulation tests.
    func runDebugStage(profile: URL, arguments: [String]) async -> Bool {
        #if DEBUG
        func value(_ flag: String) -> String? {
            guard let index = arguments.firstIndex(of: flag), index + 1 < arguments.count else { return nil }
            return arguments[index + 1]
        }
        #if os(macOS)
        if let stage = value("--inbox-test-stage") {
            annotationTestStage = "reopen"; showMissionsForTesting = true
            await runInboxTest(profile: profile, stage: stage, arguments: arguments)
            return true
        }
        if let stage = value("--area-test-stage") {
            annotationTestStage = "reopen"; showAreasForTesting = true
            await runAreaTest(profile: profile, stage: stage, arguments: arguments)
            return true
        }
        if let stage = value("--amendment-test-stage") {
            annotationTestStage = "reopen"; showNotesForTesting = true
            await runAmendmentTest(profile: profile, stage: stage, arguments: arguments)
            return true
        }
        #endif
        if let stage = value("--mission-test-stage"), ["draft", "send", "reopen", "reconcile", "stop", "proposal"].contains(stage) {
            missionTestStage = stage; showMissionsForTesting = true
            await runMissionTest(profile: profile, stage: stage, arguments: arguments)
            return true
        }
        if let stage = value("--annotation-test-stage"), ["prepare", "enqueue", "reopen", "sync"].contains(stage) {
            annotationTestStage = stage; showNotesForTesting = true
            await runAnnotationTest(profile: profile, stage: stage, text: value("--annotation-test-text") ?? "Native Apple offline model note")
            return true
        }
        #endif
        return false
    }

    func importProfile(_ url: URL) async {
        missionClient?.setMissionActive(false)
        let access = url.startAccessingSecurityScopedResource()
        defer { if access { url.stopAccessingSecurityScopedResource() } }
        do {
            let attributes = try url.resourceValues(forKeys: [.fileSizeKey, .isRegularFileKey])
            guard attributes.isRegularFile == true, (attributes.fileSize ?? Int.max) <= 256 << 10 else { throw CocoaError(.fileReadTooLarge) }
            let handle = try FileHandle(forReadingFrom: url)
            defer { try? handle.close() }
            let data = try handle.read(upToCount: (256 << 10) + 1) ?? Data()
            guard data.count <= 256 << 10 else { throw CocoaError(.fileReadTooLarge) }
            let object = try JSONValue.decode(data, limit: 256 << 10)
            let importedReadMode = try WorldReadMode(profile: object)
            let primaryData = try object["credentials"]?.encoded() ?? data
            let imported = try WorldCredentials(data: primaryData)
            let primary = WorldClient(credentials: imported)
            let importedOrigins = try ProposalInboxOrigin.profile(object["proposal_inbox"])
            guard importedOrigins.isEmpty || importedReadMode == .full else { throw ProposalInboxError.invalid }
            let importedInbox = importedOrigins.isEmpty ? nil : try ProposalInboxJournal(directory: inboxDirectory, credentials: imported, origins: importedOrigins)
            let importedAreas = importedReadMode == .full ? try AreaJournal(directory: areaDirectory, scopeID: imported.scopeID) : nil
            let nativeCacheScope = SHA256.hash(data: Data((imported.scopeID + ":" + importedReadMode.rawValue).utf8)).map { String(format: "%02x", $0) }.joined()
            let importedMediaCache = try InspectionMediaCache(directory: nativeMediaDirectory, scopeID: nativeCacheScope)
            var noteContext: AnnotationContext?
            var noteOutbox: AnnotationOutbox?
            if imported.canAnnotate {
                guard let raw = object["annotation_context"] else { throw AnnotationError.notAuthorized }
                let parsedContext = try AnnotationContext(json: raw)
                noteContext = parsedContext
                noteOutbox = try AnnotationOutbox(directory: annotationDirectory, credentials: imported, context: parsedContext)
            }
            var importedMission: MissionClient?
            var importedJournal: MissionJournal?
            let missionFields = ["mission_credentials", "mission_context", "mission_clock_trust", "mission_navigation_profile"]
            if missionFields.contains(where: { object[$0] != nil }) {
                guard let rawCredentials = object["mission_credentials"], let rawContext = object["mission_context"],
                      let rawPins = object["mission_clock_trust"] else { throw MissionError.invalidProfile }
                let missionCredentials = try MissionCredentials(data: rawCredentials.encoded())
                guard missionCredentials.tenant == imported.tenant, missionCredentials.site == imported.site else { throw MissionError.invalidProfile }
                let session = try MissionClient(credentials: missionCredentials, context: rawContext, pins: MissionClockPins(json: rawPins), navigationProfile: MissionNavigationProfile.imported(object["mission_navigation_profile"]))
                importedMission = session
                importedJournal = try MissionJournal(directory: missionDirectory, credentials: missionCredentials, context: session.context)
            }
            var media: [WorldClient] = []
            let sources = object["evidence_sources"]?.array ?? []
            guard sources.count <= 8 else { throw CocoaError(.validationMultipleErrors) }
            for source in sources {
                let sourceCredentials = try WorldCredentials(data: source.encoded())
                guard sourceCredentials.site == imported.site, sourceCredentials.tenant == imported.tenant else { throw CocoaError(.validationMissingMandatoryProperty) }
                media.append(WorldClient(credentials: sourceCredentials))
            }
            generation = UUID()
            refreshing = false
            evidenceLoading = nil
            inboxOrigins = importedOrigins; inboxDocuments = []; inboxRows = []; inboxSelectedOrigin = importedOrigins.first?.origin ?? ""; inboxSelectedID = ""; inboxReviewedID = nil; inboxPrepared = nil; inboxNotice = nil; inboxBusy = false; inboxOnline = false
            inboxClient = importedOrigins.isEmpty ? nil : ProposalInboxClient(credentials: imported); inboxJournal = importedInbox
            areaGeneration = UUID(); areaBusy = false; areaOnline = false; areaSelectedID = nil; areaEntries = []; areaNotice = nil
            areaJournal = importedAreas
            client = primary
            readMode = importedReadMode
            evidenceClients = media + [primary]
            nativeMediaCache = importedMediaCache
            credentials = imported
            importedMission?.setMissionActive(sceneActive)
            missionClient = importedMission; missionJournal = importedJournal; missionContext = importedMission?.context
            missionEntries = []; missionStopEntries = []; missionProposals = []; reviewedProposal = nil; missionNotice = nil; missionBusy = false; missionStopping = false
            annotationContext = noteContext; annotationOutbox = noteOutbox
            amendmentOutbox = try noteContext.map { try AnnotationAmendmentOutbox(directory: annotationDirectory, credentials: imported, context: $0) }
            annotationViewsCache = AnnotationViewsCache(directory: annotationDirectory, credentials: imported)
            annotationViews = (try? annotationViewsCache?.load()) ?? []; annotationViewsUnavailable = true
            amendmentEntries = []; amendmentNotice = nil; amendmentBusy = false
            let importedAmendments = amendmentOutbox
            guard await updateAmendmentSnapshot({ await importedAmendments?.snapshot() ?? [] }) else { return }
            amendmentNotice = nil; amendmentBusy = false
            annotationEntries = []; annotationNotice = nil; annotationBusy = false
            site = imported.site
            endpoint = imported.endpoint.absoluteString
            snapshot = nil; sites = .object([:]); tasks = .object([:]); fetchedAt = nil
            selectedObservation = nil; selectedFrame = ""; selectedSiteID = ""; preview = nil
            paused = false; notice = nil; state = "Connecting"
            #if DEBUG
            if annotationTestStage != nil || missionTestStage != nil { paused = true }
            #endif
            if let saved = try? cache.load(scopeID: imported.scopeID, readMode: importedReadMode) {
                snapshot = saved.snapshot; sites = saved.sites; tasks = saved.tasks; fetchedAt = saved.fetchedAt
                state = "Saved data"
                reconcileSelection()
            }
            let importedGeneration = generation
            let loadedInbox = await importedInbox?.documents() ?? []
            guard generation == importedGeneration else { return }
            inboxDocuments = loadedInbox; selectInboxOrigin(inboxSelectedOrigin)
            let loadedAreas = await importedAreas?.list() ?? []
            guard generation == importedGeneration else { return }
            areaEntries = loadedAreas; areaSelectedID = loadedAreas.first?.id
            if let importedJournal {
                let restored = await importedJournal.entries()
                guard generation == importedGeneration else { return }
                missionEntries = restored
                missionStopEntries = await importedJournal.stopEntries()
            }
            if let noteOutbox {
                let restored = await noteOutbox.entries()
                guard generation == importedGeneration else { return }
                annotationEntries = restored
                annotationClockStatus = (try? await noteOutbox.clockEvidence()) == nil ? "Unknown. New text stays untimed." : "Restored gateway-relative anchor. Its boot identity and holdover are checked again when saving."
                reconcileSelection()
            }
            #if DEBUG
            if missionTestStage != nil {
                state = snapshot == nil ? "Disconnected" : "Saved data"
                return
            }
            if let stage = annotationTestStage {
                if stage == "prepare" || stage == "sync" { await refresh() }
                else { state = "Saved data"; notice = "Annotation test: network refresh paused." }
                return
            }
            #endif
            await refresh()
            guard generation == importedGeneration else { return }

        } catch {
            notice = "Profile could not be opened. " + error.localizedDescription
        }
    }

    func refresh() async {
        guard !refreshing, let client, let credentials else { return }
        let active = generation
        let mode = readMode
        refreshing = true
        defer { if generation == active { refreshing = false } }
        do {
            if mode == .full, let annotationOutbox {
                let interval = try await annotationOutbox.prepareClock(using: client).interval()
                annotationClockStatus = "Last gateway-relative clock check: interval width \(interval.upperUnixMS - interval.lowerUnixMS) ms. Not verified UTC."
            }
            let latest = try await client.query()
            let nextSites = mode == .full ? try? await client.sites() : nil
            let nextTasks = mode == .full ? try? await client.taskRelay() : nil
            let nextAnnotationViews = mode == .full ? try? await client.annotationViews() : nil
            guard generation == active else { return }
            snapshot = latest
            if let nextAnnotationViews { annotationViews = nextAnnotationViews; annotationViewsUnavailable = false; try? annotationViewsCache?.save(nextAnnotationViews) }
            else { annotationViewsUnavailable = true }
            if mode == .nativeInference { sites = .object([:]); tasks = .object([:]) }
            else {
                if let nextSites { sites = nextSites }
                if let nextTasks { tasks = nextTasks }
            }
            fetchedAt = Date(); state = paused ? "Paused" : "Connected"
            notice = mode == .nativeInference ? "Native inference connection. Geographic placement and mission history are unavailable from this endpoint." : nextSites == nil || nextTasks == nil ? "Some sections are unavailable. Retained missions are always last-known." : nil
            reconcileSelection()
            if let fetchedAt, mode == .nativeInference || (sites["publications"]?.array != nil && tasks["entries"]?.array != nil) {
                do {
                    try cache.save(CachedWorldReadModel(snapshot: latest, sites: sites, tasks: tasks, fetchedAt: fetchedAt, scopeID: credentials.scopeID, readMode: mode))
                } catch {
                    notice = [notice, "The local snapshot could not be saved."].compactMap { $0 }.joined(separator: " ")
                }
            } else {
                notice = [notice, "The snapshot will be saved once site and mission sections are available."].compactMap { $0 }.joined(separator: " ")
            }
        } catch {
            guard generation == active else { return }
            state = "Offline"
            notice = snapshot == nil ? "The gateway is unavailable. No saved observations exist for this profile." : "The gateway is unavailable. Showing saved observations; they are not live."
        }
        if mode == .full && !paused && sceneActive { await syncAnnotations() }
    }

    @discardableResult
    func saveAnnotation(text: String, coordinates: [String], lifetimeMS: Int64 = 86_400_000) async -> Bool {
        guard let outbox = annotationOutbox, selectedFrame == annotationFrameKey, !annotationBusy, !amendmentBusy, !amendmentEntries.contains(where: {$0.state == .pending}) else {
            annotationNotice = "Choose the annotation context's frame before saving a note."
            return false
        }
        let active = generation
        do {
            let position = try AnnotationInput.millimetres(coordinates)
            let savedNote = try await outbox.saveDraft(text: text, positionMM: position, lifetimeMS: lifetimeMS)
            let entries = await outbox.entries()
            guard generation == active else { return false }
            annotationEntries = entries
            annotationNotice = savedNote.state == .untimed ? "Untimed text saved. Reconnecting will not date or publish it. Create a separate timed note when a valid clock is available." : "Timed draft saved on this device. Its original interval and evidence will be retained."
            if savedNote.state == .untimed { annotationClockStatus = "Unknown. New text stays untimed." }
            return true
        } catch {
            if generation == active { annotationNotice = error.localizedDescription }
            return false
        }
    }

    func syncAnnotations() async {
        guard !annotationBusy, !amendmentBusy, !amendmentEntries.contains(where: {$0.state == .pending}), let outbox = annotationOutbox, let client,
              annotationEntries.contains(where: { [.draft, .pending].contains($0.state) }) else { return }
        let active = generation
        annotationBusy = true
        defer { if generation == active { annotationBusy = false } }
        do {
            try await outbox.sync(using: client)
            if generation == active { annotationNotice = "Outbox checked. Published means the gateway durably acknowledged the exact signed note." }
        } catch {
            if generation == active { annotationNotice = "Notes remain on this device. " + error.localizedDescription }
        }
        let entries = await outbox.entries()
        if generation == active { annotationEntries = entries }
    }

    func reconcileSelection() {
        if !frames.contains(where: { $0.id == selectedFrame }) { selectedFrame = frames.first?.id ?? "" }
        if !placements.contains(where: { $0.id == selectedSiteID }) {
            let valid = placements.filter { $0.isValid(at: Date()) }
            selectedSiteID = valid.count == 1 ? valid.first!.id : ""
        }
        if selected == nil { selectedObservation = observations.first?.id }
    }

    func togglePause() {
        paused.toggle()
        if paused { state = "Paused" }
        else { actions.submit { await self.refresh() } }
    }

    func setSceneActive(_ active: Bool) {
        sceneActive = active
        missionClient?.setMissionActive(active)
        if !active && snapshot != nil { state = "Saved data" }
        if active && snapshot != nil && !paused { state = "Reconnecting" }
    }

    func openEvidence(_ item: EvidenceItem) async {
        guard evidenceLoading == nil else { return }
        evidenceLoading = item.id
        let active = generation
        defer { if generation == active { evidenceLoading = nil } }
        if let reference = item.nativeReference {
            preview = nil
            guard credentials?.scopes.contains("evidence-read") == true,
                  let mediaCache = nativeMediaCache, let client else {
                notice = "This profile cannot retrieve media."; return
            }
            do {
                if let saved = try await mediaCache.load(reference: reference) {
                    guard generation == active else { return }
                    preview = EvidencePreview(item: item, data: saved.data, retainUntil: saved.retainUntil, fromCache: true)
                    return
                }
                guard !offline else { notice = "This media is not saved on this device. Connect to download it."; return }
                let verified = try await client.downloadNativeEvidence(reference)
                guard generation == active else { return }
                try await mediaCache.store(verified, reference: reference)
                guard let stored = try await mediaCache.load(reference: reference), generation == active else { return }
                preview = EvidencePreview(item: item, data: stored.data, retainUntil: stored.retainUntil)
            } catch InspectionMediaCacheError.expired {
                if generation == active { notice = "This media has expired." }
            } catch {
                if generation == active { notice = "Media is unavailable. Try again when connected." }
            }
            return
        }
        for media in evidenceClients {
            if let verified = try? await media.downloadEvidence(digest: item.digest, maxBytes: item.size), verified.data.count == item.size, verified.contentType == item.contentType {
                guard generation == active else { return }
                preview = EvidencePreview(item: item, data: verified.data)
                return
            }
        }
        if generation == active { notice = "Evidence is unavailable from the configured capture nodes. Nothing was displayed without digest verification." }
    }

    @discardableResult
    func saveMissionDraft(id: String? = nil, x: String, durationMS: Int64, proposal: MissionProposal? = nil, prepared: PreparedProposalHandoff? = nil) async -> Bool {
        guard let journal = missionJournal, let context = missionContext else { return false }
        let active = generation
        do {
            let identifier = id ?? "native-" + UUID().uuidString.lowercased()
            let draft: MissionDraft
            if let proposal { draft = try proposal.unsignedDraft(id: identifier, durationMS: durationMS, context: context, prepared: prepared) }
            else {
                guard let coordinate = Double(x), coordinate.isFinite else { throw MissionError.invalidDraft }
                draft = try MissionDraft(id: identifier, target: [coordinate, 0.2, 0], durationMS: durationMS, context: context)
            }
            try await journal.saveDraft(draft)
            let entries = await journal.entries()
            guard generation == active else { return false }
            missionEntries = entries; missionNotice = "Unsigned inspection saved. Review it before signing and sending."
            return true
        } catch { if generation == active { missionNotice = error.localizedDescription }; return false }
    }

    func selectInboxOrigin(_ origin: String) {
        guard inboxOrigins.contains(where: { $0.origin == origin }) else { return }
        inboxSelectedOrigin = origin; inboxSelectedID = ""; inboxReviewedID = nil; inboxPrepared = nil; reviewedProposal = nil; inboxOnline = false
        do { inboxRows = try inboxDocuments.first(where: { $0.origin.origin == origin })?.rows(tenant: credentials?.tenant ?? "", site: credentials?.site ?? "") ?? [] }
        catch { inboxRows = []; inboxNotice = error.localizedDescription }
        inboxNotice = inboxRows.isEmpty ? "Load a complete inbox from the configured reader." : "Saved historical proposals. Current permission has not been checked."
    }
    func refreshInbox(pageSize: Int = 32) async {
        guard !inboxBusy, let reader = inboxClient, let journal = inboxJournal, let origin = inboxOrigins.first(where: { $0.origin == inboxSelectedOrigin }) else { return }
        let active = generation; inboxBusy = true; inboxOnline = false; inboxReviewedID = nil; inboxPrepared = nil; reviewedProposal = nil
        defer { if generation == active { inboxBusy = false } }
        do {
            let doc = try await reader.read(origin: origin, pageSize: pageSize)
            guard generation == active else { return }
            try await journal.install(doc)
            let documents = await journal.documents()
            guard generation == active else { return }
            inboxDocuments = documents
            if inboxSelectedOrigin == origin.origin {
                inboxRows = try doc.rows(tenant: reader.credentials.tenant, site: reader.credentials.site); inboxOnline = true
                inboxNotice = "Complete historical inbox saved. Gateway grant checks describe retrieval time, not permission to execute."
            }
        } catch { if generation == active, inboxSelectedOrigin == origin.origin { inboxNotice = "Refresh unavailable. Last complete history kept. " + error.localizedDescription } }
    }
    func reviewRemoteProposal(id: String) async {
        guard !inboxBusy, let row = inboxRows.first(where: { $0.id == id }), let live = missionClient else { inboxNotice = ProposalInboxError.wrongOrigin.localizedDescription; return }
        let active = generation; inboxBusy = true; inboxSelectedID = id; inboxReviewedID = nil; inboxPrepared = nil; reviewedProposal = nil
        defer { if generation == active { inboxBusy = false } }
        do {
            try row.origin.validateMissionOrigin(credentials: live.credentials, context: live.context)
            _ = try await live.catalog()
            let proposal = try await live.proposal(id: row.proposalID)
            try row.matchesLive(proposal, credentials: live.credentials, context: live.context)
            guard generation == active, inboxSelectedOrigin == row.origin.origin, inboxSelectedID == id else { return }
            reviewedProposal = proposal; inboxReviewedID = id
            inboxNotice = "Exact proposal rechecked at the configured origin. Review its target, then save an unsigned draft. Signing remains a separate action and rechecks the origin again."
        } catch { if generation == active, inboxSelectedOrigin == row.origin.origin, inboxSelectedID == id { inboxNotice = "Origin review refused. " + error.localizedDescription } }
    }
    func saveRemoteProposalDraft(id: String? = nil, durationMS: Int64) async -> Bool {
        guard let selected = inboxReviewedID, let row = inboxRows.first(where: { $0.id == selected }), let proposal = reviewedProposal,
              row.bodySHA256 == proposal.reference.bodySHA256, row.origin.origin == proposal.reference.audience else { inboxNotice = "Recheck the exact proposal at its configured origin first."; return false }
        let active = generation
        let saved = await saveMissionDraft(id: id, x: String(proposal.target[0]), durationMS: durationMS, proposal: proposal, prepared: inboxPrepared)
        if generation == active, inboxSelectedOrigin == row.origin.origin, inboxReviewedID == selected { inboxNotice = saved ? "Unsigned draft saved. Review and sign explicitly in the mission outbox below." : missionNotice }
        return saved
    }

    func importInboxHandoff(_ url: URL) {
        guard let row = inboxRows.first(where: { $0.id == inboxReviewedID }), let proposal = reviewedProposal, let context = missionContext,
              row.bodySHA256 == proposal.reference.bodySHA256, row.origin.origin == proposal.reference.audience else { inboxNotice = "Review the exact origin proposal before importing its prepared context."; return }
        let scoped = url.startAccessingSecurityScopedResource(); defer { if scoped { url.stopAccessingSecurityScopedResource() } }
        do {
            let attributes = try url.resourceValues(forKeys: [.fileSizeKey, .isRegularFileKey])
            guard url.isFileURL, attributes.isRegularFile == true, let size = attributes.fileSize, size > 0, size <= 512 << 10 else { throw MissionError.invalidDraft }
            let file = try FileHandle(forReadingFrom: url); defer { try? file.close() }
            let bytes = try file.read(upToCount: (512 << 10) + 1) ?? Data()
            guard bytes.count <= 512 << 10 else { throw MissionError.invalidDraft }
            let handoff = try PreparedProposalHandoff(data: bytes, proposal: proposal, context: context)
            inboxPrepared = handoff
            inboxNotice = "Prepared-reader handoff matches this exact proposal. Original deadline retained. Sensor truth and producer signatures are the prepared reader's responsibility; current approval remains separate."
        } catch { inboxPrepared = nil; inboxNotice = "Prepared context refused. " + error.localizedDescription }
    }

    #if DEBUG && os(macOS)
    private func runInboxTest(profile: URL, stage: String, arguments: [String]) async {
        func arg(_ name: String) -> String? { guard let i = arguments.firstIndex(of: name), i + 1 < arguments.count else { return nil }; return arguments[i + 1] }
        await importProfile(profile); paused = true; notice = "Proposal inbox demonstration: automatic world refresh paused."
        if let origin = arg("--inbox-origin") { selectInboxOrigin(origin) }
        let selected = arg("--inbox-proposal") ?? "inspect-pump", task = arg("--inbox-task") ?? "inbox-inspection"
        let qualified = inboxSelectedOrigin + ":" + selected
        switch stage {
        case "refresh": await refreshInbox(pageSize: Int(arg("--inbox-page-size") ?? "2") ?? 2)
        case "review": await reviewRemoteProposal(id: qualified)
        case "draft":
            await reviewRemoteProposal(id: qualified)
            if let path = arg("--inbox-handoff") { importInboxHandoff(URL(fileURLWithPath: path)) }
            _ = await saveRemoteProposalDraft(id: task, durationMS: 60000)
        case "send": await sendMission(id: task)
        case "reconcile": await reconcileMission(id: task)
        default: break
        }
        inboxSelectedID = qualified
        try? await Task.sleep(for: .milliseconds(250))
        guard let output = arg("--inbox-report"), output.hasPrefix("/") else { return }
        do {
            if let window = NSApplication.shared.windows.first(where: { $0.isVisible }), let view = window.contentView, let bitmap = view.bitmapImageRepForCachingDisplay(in: view.bounds) {
                view.cacheDisplay(in: view.bounds, to: bitmap); if let png = bitmap.representation(using: .png, properties: [:]) { try png.write(to: URL(fileURLWithPath: output + ".png")) }
            }
            let rows = try inboxRows.map { try JSONValue.decode($0.raw, limit: 128 << 10) }
            let missions = try missionEntries.map { entry in JSONValue.object(["id": .string(entry.id), "state": .string(entry.state.rawValue), "draft": try JSONValue.decode(JSONEncoder().encode(entry.draft)), "instruction_sha256": entry.instructionSHA256.map(JSONValue.string) ?? .null, "instruction": try entry.instruction.map { try JSONValue.decode($0) } ?? .null, "record": entry.record ?? .null, "error": entry.lastError.map(JSONValue.string) ?? .null]) }
            let report: JSONValue = .object(["stage": .string(stage), "origin": .string(inboxSelectedOrigin), "rows": .array(rows), "reviewed": inboxReviewedID.map(JSONValue.string) ?? .null, "notice": inboxNotice.map(JSONValue.string) ?? .null, "mission_notice": missionNotice.map(JSONValue.string) ?? .null, "missions": .array(missions), "online": .bool(inboxOnline), "scope": .string("Actual native model callbacks; no OS mouse input or executor motion")])
            try report.encoded().write(to: URL(fileURLWithPath: output), options: .atomic)
        } catch { notice = error.localizedDescription }
    }
    #endif

    func loadMissionProposals() async {
        guard let client = missionClient, !missionBusy else { return }
        let active = generation; missionBusy = true
        defer { if generation == active { missionBusy = false } }
        do {
            _ = try await client.catalog()
            let page = try await client.proposals()
            guard generation == active else { return }
            missionProposals = page["proposals"]?.array ?? []; missionNotice = "Proposals are suggestions. Reviewing a proposal does not authorize motion."
        } catch { if generation == active { missionNotice = error.localizedDescription } }
    }
    func reviewMissionProposal(id: String) async {
        guard let client = missionClient else { return }
        let active = generation
        do {
            let proposal = try await client.proposal(id: id)
            if generation == active { reviewedProposal = proposal; missionNotice = "Review the proposed target and author before saving a draft." }
        } catch { if generation == active { reviewedProposal = nil; missionNotice = error.localizedDescription } }
    }
    func sendMission(id: String) async {
        guard sceneActive, !missionBusy, let journal = missionJournal, let client = missionClient,
              let entry = missionEntries.first(where: { $0.id == id }) else { return }
        let active = generation; missionBusy = true
        defer { if generation == active { missionBusy = false } }
        do {
            if entry.state == .draft { try await journal.signAndSend(id: id, using: client) }
            else { try await journal.retry(id: id, using: client) }
            if generation == active { missionNotice = "The assignee acknowledged the exact signed inspection. Status remains last-known until reconciled." }
        } catch { if generation == active { missionNotice = error.localizedDescription } }
        let entries = await journal.entries(); if generation == active { missionEntries = entries }
    }
    func reconcileMission(id: String) async {
        guard !missionBusy, let journal = missionJournal, let client = missionClient else { return }
        let active = generation; missionBusy = true
        defer { if generation == active { missionBusy = false } }
        do { try await journal.reconcile(id: id, using: client); if generation == active { missionNotice = "Assignee status reconciled against the saved instruction." } }
        catch { if generation == active { missionNotice = error.localizedDescription } }
        let entries = await journal.entries(); if generation == active { missionEntries = entries }
    }
    func stopMission(id: String) async {
        guard sceneActive, !missionStopping, let journal = missionJournal, let client = missionClient else { return }
        let active = generation; missionStopping = true
        defer { if generation == active { missionStopping = false } }
        do {
            if missionEntries.contains(where: { $0.id == id && $0.instruction != nil }) { try await journal.stop(id: id, using: client) }
            else { try await journal.stopAny(taskID: id, using: client) }
            if generation == active { missionNotice = "The assignee acknowledged the signed stop. Reconcile to inspect the retained result." } }
        catch { if generation == active { missionNotice = "Stop outcome uncertain. " + error.localizedDescription } }
        let entries = await journal.entries(), stops = await journal.stopEntries()
        if generation == active { missionEntries = entries; missionStopEntries = stops }
    }

    #if DEBUG
    private func runMissionTest(profile: URL, stage: String, arguments: [String]) async {
        func argument(_ name: String) -> String? { arguments.firstIndex(of: name).flatMap { $0 + 1 < arguments.count ? arguments[$0 + 1] : nil } }
        let taskID = argument("--mission-test-id") ?? "native-runtime-test"
        var failure: String?
        var verifiedEvidence: [JSONValue] = []
        let invocationNonce = argument("--mission-test-nonce") ?? ""
        await importProfile(profile)
        // An explicit runtime invocation waits for the scene to settle before
        // acting. Ordinary launches never schedule these actions.
        for _ in 0..<25 {
            try? await Task.sleep(for: .milliseconds(200))
            if sceneActive { break }
        }
        if !sceneActive { failure = "The app is not in the foreground." }
        if invocationNonce.isEmpty || invocationNonce.utf8.count > 128 { failure = "An explicit bounded invocation nonce is required." }
        else if failure != nil { }
        else if !canMission { failure = notice ?? "Mission profile import failed." }
        else {
            if stage == "draft" {
                if !missionEntries.contains(where: { $0.id == taskID }) {
                    if !(await saveMissionDraft(id: taskID, x: argument("--mission-test-target-x") ?? "1.8", durationMS: Int64(argument("--mission-test-duration-ms") ?? "60000") ?? 0)) { failure = missionNotice }
                }
            } else if stage == "proposal" {
                await reviewMissionProposal(id: argument("--mission-test-proposal") ?? "")
                if let proposal = reviewedProposal {
                    if !(await saveMissionDraft(id: taskID, x: String(proposal.target[0]), durationMS: Int64(argument("--mission-test-duration-ms") ?? "60000") ?? 0, proposal: proposal)) { failure = missionNotice }
                } else { failure = missionNotice ?? "Proposal review failed." }
            } else if stage == "send" {
                if let entry = missionEntries.first(where: { $0.id == taskID }), [.draft, .signed, .uncertain].contains(entry.state) { await sendMission(id: taskID) }
                else { failure = "No pending mission action exists for this task." }
            }
            else if stage == "reconcile" { await reconcileMission(id: taskID) }
            else if stage == "stop" { await stopMission(id: taskID) }
            if stage == "send" {
                let entry = missionEntries.first(where: { $0.id == taskID })
                if entry?.instruction == nil || entry?.record == nil || ![MissionDeliveryState.delivered, .terminal].contains(entry?.state ?? .draft) {
                    failure = missionNotice ?? "The explicit confirmation did not obtain an admitted instruction."
                }
            }
            if let entry = missionEntries.first(where: { $0.id == taskID }), let error = entry.lastError { failure = error }
            if stage == "stop", let entry = missionStopEntries.last(where: { $0.taskID == taskID }), let error = entry.error { failure = error }
            if !missionEntries.contains(where: { $0.id == taskID }) && !missionStopEntries.contains(where: { $0.taskID == taskID }) && stage != "reopen" { failure = failure ?? "Task is absent from the durable journal." }
            if stage == "reconcile", failure == nil, let result = missionEntries.first(where: { $0.id == taskID })?.record?["result"] {
                for sample in result["samples"]?.array ?? [] {
                    guard let item = EvidenceItem(sample["evidence"], name: text(sample["sensor_id"])) else { continue }
                    await openEvidence(item)
                    guard let loaded = preview, loaded.item.digest == item.digest else { failure = "Returned inspection evidence could not be verified."; break }
                    verifiedEvidence.append(.object(["sha256": .string(item.digest), "size": .integer(Int64(loaded.data.count)), "content_type": .string(item.contentType)]))
                }
            }
        }
        do {
            let base = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask).first ?? FileManager.default.temporaryDirectory
            let directory = base.appendingPathComponent("WendyWorld/MissionTests", isDirectory: true)
            try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
            let entries: [JSONValue] = try missionEntries.map { entry in
                .object(["id": .string(entry.id), "draft": try JSONValue.decode(JSONEncoder().encode(entry.draft)), "state": .string(entry.state.rawValue),
                    "instruction_sha256": entry.instructionSHA256.map(JSONValue.string) ?? .null,
                    "instruction": try entry.instruction.map { try JSONValue.decode($0) } ?? .null,
                    "record": entry.record ?? .null, "lastError": entry.lastError.map(JSONValue.string) ?? .null,
                    "stop_state": entry.stopState.map(JSONValue.string) ?? .null,
                    "stop_instruction_sha256": entry.stopInstruction.map { .string(SHA256.hash(data: $0).map { String(format: "%02x", $0) }.joined()) } ?? .null])
            }
            #if os(macOS)
            let platform = "macos"
            #elseif os(visionOS)
            let platform = "visionos"
            #else
            let platform = "ios"
            #endif
            let report: JSONValue = .object(["schema": .string("wendy.apple.mission-model-test.v1"), "stage": .string(stage), "platform": .string(platform),
                "taskID": .string(taskID), "task_id": .string(taskID), "invocation_nonce": .string(invocationNonce), "run_id": missionContext.map { .string($0.runID) } ?? .null, "runID": missionContext.map { .string($0.runID) } ?? .null,
                "canMission": .bool(canMission), "contextSHA256": missionContext.map { .string($0.digest) } ?? .null,
                "verified_evidence": .array(verifiedEvidence), "entries": .array(entries), "stop_entries": .array(missionStopEntries.map { .object(["task_id": .string($0.taskID), "state": .string($0.state), "instruction_sha256": .string($0.instructionSHA256), "record": $0.record ?? .null, "error": $0.error.map(JSONValue.string) ?? .null]) }), "error": failure.map(JSONValue.string) ?? .null])
            let data = try report.encoded(pretty: true)
            guard data.count <= 4 << 20 else { throw MissionError.journalFull }
            let path = directory.appendingPathComponent(stage + ".json")
            try data.write(to: path, options: .atomic)
            try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: path.path)
            missionNotice = failure ?? "Mission model test finished: " + stage
        } catch { missionNotice = "Mission test report could not be saved: " + error.localizedDescription }
    }

    /// Explicit debug-only instrumentation of the actual app model. Default
    /// launches never enter this path, and only contributor profiles can write.
    private func runAnnotationTest(profile: URL, stage: String, text: String) async {
        var failure: String?
        await withTaskGroup(of: String?.self) { group in
            defer { group.cancelAll() }
            group.addTask { await self.annotationTestActions(profile: profile, stage: stage, text: text) }
            group.addTask {
                do { try await Task.sleep(for: .seconds(60)) } catch { return nil }
                return "Annotation model test exceeded 60 seconds."
            }
            if let first = await group.next() { failure = first }
        }
        do {
            let base = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask).first ?? FileManager.default.temporaryDirectory
            let directory = base.appendingPathComponent("WendyWorld/AnnotationTests", isDirectory: true)
            try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
            let entries: [JSONValue] = annotationEntries.map { entry in
                .object([
                    "id": .string(entry.id), "text": .string(entry.draft.text), "state": .string(entry.state.rawValue),
                    "authored_at_unix_ms": .integer(entry.draft.authoredAtUnixMS), "expires_at_unix_ms": .integer(entry.draft.expiresAtUnixMS),
                    "body_sha256": entry.bodySHA256.map(JSONValue.string) ?? .null,
                    "author_principal": .string(entry.draft.authorPrincipal)
                ])
            }
            let notes = observations.filter { $0.kind == "annotation" }
            let report: JSONValue = .object([
                "schema": .string("wendy.apple.annotation-model-test.v1"), "stage": .string(stage),
                "attemptedNetwork": .bool(stage == "prepare" || stage == "sync"),
                "canAnnotate": .bool(canAnnotate), "state": .string(state),
                "hasReadCache": .bool(snapshot != nil), "fetchedAtUnixMS": fetchedAt.map { .integer(Int64($0.timeIntervalSince1970 * 1000)) } ?? .null,
                "scopeID": annotationOutbox.map { .string($0.scopeID) } ?? .null,
                "entries": .array(entries), "notes": .array(notes.prefix(512).map(\.raw)), "noteCount": .integer(Int64(notes.count)),
                "pending": .integer(Int64(annotationEntries.filter { [.draft, .pending].contains($0.state) }.count)),
                "published": .integer(Int64(annotationEntries.filter { $0.state == .published }.count)),
                "error": failure.map(JSONValue.string) ?? .null
            ])
            let bytes = try report.encoded(pretty: true)
            guard bytes.count <= 4 << 20 else { throw AnnotationError.outboxFull }
            let path = directory.appendingPathComponent(stage + ".json")
            try bytes.write(to: path, options: .atomic)
            try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: path.path)
            annotationNotice = failure ?? "Annotation model test finished: \(stage)."
        } catch { annotationNotice = "Annotation test report could not be saved." }
    }

    private func annotationTestActions(profile: URL, stage: String, text: String) async -> String? {
        await importProfile(profile)
        guard !Task.isCancelled else { return "Test cancelled." }
        guard canAnnotate, annotationContext != nil else { return "A valid simulation contributor profile and context are required." }
        selectedFrame = annotationFrameKey
        if stage == "enqueue" {
            if !annotationEntries.contains(where: { $0.draft.text == text }) {
                guard await saveAnnotation(text: text, coordinates: ["1.250", "0", "2.500"]) else { return annotationNotice ?? "Draft saving failed." }
            }
        } else if stage == "sync" {
            await syncAnnotations()
            guard !Task.isCancelled else { return "Test cancelled." }
            await refresh()
        }
        return nil
    }
    #endif
}


extension OperatorModel {
    // The actor read can suspend after a successful prepare or send. Profile
    // import must win over any old-author snapshot completing afterward.
    func updateAmendmentSnapshot(_ load: () async -> [AnnotationAmendmentEntry]) async -> Bool {
        let active = generation
        let rows = await load()
        guard generation == active else { return false }
        amendmentEntries = rows
        return true
    }

    func prepareAmendment(originalID: String, action: String, text: String, coordinates: [String]) async {
        guard !amendmentBusy, !annotationBusy, let outbox = amendmentOutbox, let client,
              !annotationEntries.contains(where: {[.draft,.pending].contains($0.state)}),
              let original = annotationEntries.first(where: {$0.id == originalID && $0.state == .published}),
              let originalDigest = original.bodySHA256 else { amendmentNotice = "Resolve pending notes before preparing an amendment."; return }
        amendmentBusy = true; let active = generation; defer { if generation == active { amendmentBusy = false } }
        do {
            var predecessor: Data?
            if action == "correct" {
                guard !annotationViewsUnavailable, !offline, let view = annotationViews.first(where: {$0["original_sha256"]?.string == originalDigest}),
                      ["original","corrected"].contains(AnnotationViewPresentation.state(view, offline: false)),
                      let head = view["head_sha256"]?.string else { throw AnnotationError.invalidRecord }
                if head != originalDigest {
                    guard let saved = amendmentEntries.first(where: {$0.amendmentSHA256 == head && $0.state == .published}) else { throw AnnotationError.invalidRecord }
                    predecessor = saved.signedRecord
                }
            }
            try await outbox.prepare(original: original, predecessor: predecessor, action: action, text: text, positionMM: try AnnotationInput.millimetres(coordinates), using: client)
            guard generation == active else { return }
            guard await updateAmendmentSnapshot({ await outbox.snapshot() }) else { return }
            amendmentNotice = "Exact amendment saved before sending. Review it, then send the saved bytes. The original expiry is unchanged."
        } catch { if generation == active { amendmentNotice = error.localizedDescription } }
    }
    func syncAmendments() async {
        guard !amendmentBusy, !annotationBusy, let outbox = amendmentOutbox, let client else { return }
        amendmentBusy = true; let active = generation; defer { if generation == active { amendmentBusy = false } }
        var sendIssue: String?
        do { try await outbox.sync(using: client) }
        catch { sendIssue = "Saved bytes retained. " + error.localizedDescription }
        guard generation == active else { return }
        guard await updateAmendmentSnapshot({ await outbox.snapshot() }) else { return }
        if let sendIssue { amendmentNotice = sendIssue }
        else if !amendmentEntries.isEmpty && amendmentEntries.allSatisfy({ $0.state == .published }) {
            amendmentNotice = "Gateway acknowledged the exact saved amendments. Other replicas may still be disconnected."
        } else { amendmentNotice = "Saved bytes retained. Review each amendment's acknowledgement, denial or expiry below." }
        if let rows = try? await client.annotationViews() { guard generation == active else { return }; annotationViews = rows; annotationViewsUnavailable = false; try? annotationViewsCache?.save(rows) }
    }
}

#if DEBUG && os(macOS)
extension OperatorModel {
    private func runAmendmentTest(profile: URL, stage: String, arguments: [String]) async {
        func argument(_ name: String) -> String? { guard let i = arguments.firstIndex(of: name), i+1<arguments.count else {return nil};return arguments[i+1] }
        guard let output = argument("--amendment-report"), output.hasPrefix("/") else { return }
        var failure: String?
        await importProfile(profile); paused = stage == "reopen"
        do {
            if stage != "reopen" { await refresh() }
            switch stage {
            case "create", "create-short":
                guard await saveAnnotation(text: argument("--amendment-text") ?? "Inspect the west entry", coordinates: ["1.250","0","2.500"], lifetimeMS: stage == "create-short" ? 8_000 : 86_400_000) else { throw AnnotationError.invalidNote }
                await syncAnnotations(); await refresh(); annotationSection = "My notes"
            case "correct", "withdraw":
                guard let original = annotationEntries.first(where: {$0.state == .published}) else { throw AnnotationError.invalidOutbox }
                await prepareAmendment(originalID: original.id, action: stage == "correct" ? "correct" : "withdraw", text: argument("--amendment-text") ?? "Inspect the east entry instead", coordinates: ["2.500","0","1.250"])
                guard amendmentEntries.last?.state == .pending else { throw AnnotationError.invalidOutbox }
                annotationSection = "My notes"
            case "send": await syncAmendments(); await refresh(); annotationSection = "Shared state"
            case "view", "reopen": annotationSection = "Shared state"
            default: throw AnnotationError.invalidNote
            }
        } catch { failure = error.localizedDescription }
        do {
            try await Task.sleep(for: .milliseconds(700))
            if let window = NSApplication.shared.windows.first(where: {$0.isVisible}), let view = window.contentView,
               let bitmap = view.bitmapImageRepForCachingDisplay(in: view.bounds) {
                view.cacheDisplay(in: view.bounds, to: bitmap)
                if let png = bitmap.representation(using: .png, properties: [:]) { try png.write(to: URL(fileURLWithPath: output + ".png")) }
            }
            let report: [String: JSONValue] = ["stage":.string(stage),"error":failure.map(JSONValue.string) ?? .null,"notice":amendmentNotice.map(JSONValue.string) ?? .null,"original_count":.integer(Int64(annotationEntries.count)),"amendment_count":.integer(Int64(amendmentEntries.count)),"amendments":.array(try amendmentEntries.map {try JSONValue.decode(JSONEncoder().encode($0))}),"originals":.array(try annotationEntries.map {try JSONValue.decode(JSONEncoder().encode($0))}),"views":.array(annotationViews),"offline":.bool(offline),"connection_state":.string(state),"scope":.string("Actual native application callbacks; no OS mouse interaction or physical scene")]
            try JSONValue.object(report).encoded(pretty: true).write(to: URL(fileURLWithPath: output), options: .atomic)
        } catch { amendmentNotice = "Amendment test report failed: " + error.localizedDescription }
    }
}
#endif

extension OperatorModel {
    var currentArea: AreaWorkspace? { areaEntries.first {$0.id == areaSelectedID} }
    var canUseAreas: Bool { readMode == .full && areaJournal != nil }
    func selectArea(_ id:String) { areaGeneration=UUID();areaSelectedID=id;areaBusy=false;areaOnline=false;areaNotice="Saved complete snapshot. Refresh explicitly for a new read." }
    func saveArea(name:String,frameID:String,revision:String,minXZ:[Double],maxXZ:[Double],types:[String]) async {
        guard let areaJournal,canUseAreas,!areaBusy else{return}
        let g=generation,a=areaGeneration;areaBusy=true
        defer{if generation==g && areaGeneration==a{areaBusy=false}}
        do {
            let region=try AreaRegion(frameID:frameID,frameRevision:revision,minXZ:minXZ,maxXZ:maxXZ)
            let definition=try AreaDefinition(name:name,region:region,types:types)
            try await areaJournal.create(definition)
            let list=await areaJournal.list()
            guard generation==g && areaGeneration==a else{return}
            areaEntries=list;areaSelectedID=definition.id;areaOnline=false
            areaNotice="Workspace saved locally. It grants no authority and changes no node storage."
        } catch {if generation==g && areaGeneration==a{areaNotice=error.localizedDescription}}
    }
    func refreshArea(watching:Bool=false) async {
        guard let areaJournal,let client,let selected=currentArea,canUseAreas,!areaBusy else{return}
        let g=generation,a=areaGeneration;areaBusy=true;areaOnline=false
        areaNotice="Reading bounded pages. The previous complete snapshot remains visible."
        defer{if generation==g && areaGeneration==a{areaBusy=false}}
        do {
            let next:AreaSnapshot
            if watching,let previous=selected.snapshot {
                guard let changed=try await client.watchArea(previous) else {
                    if generation==g && areaGeneration==a {areaOnline=true;areaNotice="No selected change during this bounded watch. Saved deadlines are unchanged."};return
                };next=changed
            } else {next=try await client.readArea(selected.definition,pageSize:16)}
            guard generation==g && areaGeneration==a,areaSelectedID==selected.id else{return}
            try await areaJournal.install(next)
            let list=await areaJournal.list()
            guard generation==g && areaGeneration==a,areaSelectedID==selected.id else{return}
            areaEntries=list;areaOnline=true
            areaNotice="Complete read saved. Scope is this replica, exact frame and selected layers; not global completeness."
        } catch {
            if generation==g && areaGeneration==a{areaOnline=false;areaNotice="Refresh unavailable. Previous complete snapshot retained. "+error.localizedDescription}
        }
    }
    func removeArea() async {
        guard let areaJournal,let id=areaSelectedID,!areaBusy else{return}
        let g=generation,a=areaGeneration
        do {
            try await areaJournal.remove(id);let list=await areaJournal.list()
            guard generation==g && areaGeneration==a else{return}
            areaEntries=list;areaSelectedID=list.first?.id;areaOnline=false;areaNotice="Local workspace removed. Original World records are unchanged."
        } catch {if generation==g && areaGeneration==a{areaNotice=error.localizedDescription}}
    }
}

#if DEBUG && os(macOS)
extension OperatorModel {
    private func runAreaTest(profile:URL,stage:String,arguments:[String]) async {
        func argument(_ name:String)->String? {guard let i=arguments.firstIndex(of:name),i+1<arguments.count else{return nil};return arguments[i+1]}
        guard let output=argument("--area-report"),output.hasPrefix("/") else{return}
        await importProfile(profile);paused=true
        var failure:String?
        do {
            switch stage {
            case "create":
                let frame=argument("--area-frame") ?? "office-floor",revision=argument("--area-revision") ?? "office-frame-v1"
                let scale=550.0/9.0
                let drawn=try AreaRegion.fromCanvasDrag(frameID:frame,frameRevision:revision,viewportMin:[-2,-2],viewportMax:[7,7],size:[1000,600],start:[25+2*scale,25+2*scale],end:[25+7*scale,25+7*scale])
                await saveArea(name:argument("--area-name") ?? "West service area",frameID:frame,revision:revision,minXZ:drawn.minXZ,maxXZ:drawn.maxXZ,types:["wendy.hazard.v1"])
                guard currentArea != nil else{throw AreaError.invalid};await refreshArea()
            case "refresh":await refreshArea()
            case "watch":await refreshArea(watching:true)
            case "reopen":break
            default:throw AreaError.invalid
            }
            try await Task.sleep(for:.milliseconds(700))
            if let window=NSApplication.shared.windows.first(where:{$0.isVisible}),let view=window.contentView,let bitmap=view.bitmapImageRepForCachingDisplay(in:view.bounds){view.cacheDisplay(in:view.bounds,to:bitmap);if let png=bitmap.representation(using:.png,properties:[:]){try png.write(to:URL(fileURLWithPath:output+".png"))}}
        }catch{failure=error.localizedDescription}
        do {
            let snapshot=currentArea?.snapshot,rows=try snapshot?.records() ?? []
            let report:JSONValue = .object(["stage":.string(stage),"error":failure.map(JSONValue.string) ?? .null,"notice":areaNotice.map(JSONValue.string) ?? .null,"online_read":.bool(areaOnline),"workspace_count":.integer(Int64(areaEntries.count)),"workspace_id":currentArea.map{.string($0.id)} ?? .null,"record_count":.integer(Int64(rows.count)),"located_count":.integer(Int64(rows.filter(\.located).count)),"unlocated_count":.integer(Int64(rows.filter{!$0.located}.count)),"expired_count":.integer(Int64(rows.filter{snapshot?.expired($0) ?? true}.count)),"records":.array(rows.map{.object(["component":$0.component.raw,"spatial":$0.descriptor])}),"scope":.string("Actual native callbacks; loopback synthetic spatial fixtures; no OS mouse interaction or physical measurements")])
            try report.encoded(pretty:true).write(to:URL(fileURLWithPath:output),options:.atomic)
        }catch{areaNotice="Area report failed: "+error.localizedDescription}
    }
}
#endif
