import Foundation
import SwiftUI
import Darwin
import CryptoKit

// Same-host display clock used by the native bridge. It is not sensor or UTC time.
func demoMonotonicNS() -> Int64 {
    var scale = mach_timebase_info_data_t()
    mach_timebase_info(&scale)
    let ticks = mach_absolute_time(), divisor = UInt64(scale.denom), multiplier = UInt64(scale.numer)
    return Int64(clamping:(ticks / divisor) * multiplier + (ticks % divisor) * multiplier / divisor)
}

struct DemoGeometry: Decodable {
    let schema: String
    let run_id: String
    let model_sha256: String
    let geoms: [Geom]
    let presentation: Presentation?
    struct Presentation: Decodable { let name: String?; let subtitle: String?; let labels: [Label]? }
    struct Label: Decodable { let text: String; let position: [Double] }
    struct Geom: Decodable {
        let id: Int; let name: String; let type: String
        let size: [Double]; let rgba: [Double]; let position: [Double]; let rotation: [Double]
        let dynamic: Bool; let group: Int; let robot_id: String?
        let material: Material?; let material_name: String?
        let vertices: [[Double]]?; let faces: [[Int]]?
    }
    struct Material: Decodable {
        let texture_average_rgb: [Double]?
        let emission: Double?; let specular: Double?; let shininess: Double?; let reflectance: Double?
    }
}
struct DemoState: Decodable {
    let schema: String; let run_id: String; let sequence: Int
    let mode: String?
    let display_clock: String?
    let display_deadline_monotonic_ns: Int64?
    let observation_deadline_monotonic_ns: Int64?
    let worker_status: String; let exit_reason: String?; let last_command_sequence: Int
    var snapshot: Snapshot; let dynamic_geoms: [Transform]
    struct Transform: Decodable { let id: Int; let position: [Double]; let rotation: [Double] }
}
struct Snapshot: Decodable {
    let run_id: String; let time_s: Double; let phase: String
    let authorized: Bool; let stopped: Bool; let fault: String?
    let robots: [Robot]; let jobs: [Job]; let completed: Int; let total: Int
    let events: [Event]; let voxels: [Voxel]; let voxel_size: Double; let map_node: String
    let network: Network; let coordinator_age_s: Double?
    var fusion: FusionState?
    var fusion_selection: FusionSelection?
    var tracking: TrackingState?
    let patrol: PatrolState?
    let controls_available: Bool?
    let sensor_sources: [SensorSource]?
}
/// Patrol timestamps use snapshot.time_s; positions use MuJoCo XY floor, Z up.
struct PatrolState: Decodable {
    let tour:PatrolTourStatus?
    let offline:PatrolOfflineBinding?

    let world_watch:PatrolWorldStatus?
    let environment:Environment?
    struct Environment:Decodable {
        struct Sample:Decodable {
            let sequence:UInt64;let simulation_ns:Int64;let captured_unix_ms:Int64
            let value_ppm:Double;let uncertainty_ppm:Double;let valid:Bool
        }
        let policy:PatrolEnvironmentPolicy;let last:Sample;let screening_ppm:Double
        let clear_elapsed_ms:Int64;let ready:Bool;let held:Bool;let permanent:Bool;let reason:String
        var validGeometry:Bool {policy.valid && screening_ppm.isFinite && screening_ppm>=0 && clear_elapsed_ms>=0 && last.value_ppm.isFinite && last.uncertainty_ppm.isFinite && last.value_ppm>=0 && last.uncertainty_ppm>=0 && (last.sequence>0 || (!ready && held)) && (!permanent || !ready)}
    }
    let work:Work?
    struct Work:Decodable {
        struct Sample:Decodable {
            let run_id:String;let boot_id:String;let model_sha256:String;let method:String
            let captured_unix_ms:Int64;let simulation_ns:Int64;let sequence:UInt64
            let total_j:Double;let power_w:Double
            var validGeometry:Bool {!run_id.isEmpty && !boot_id.isEmpty && model_sha256.count==64 && method=="post-step-positive-joint-power-v1" && captured_unix_ms>0 && simulation_ns>0 && sequence>0 && total_j.isFinite && total_j>=0 && power_w.isFinite && power_w>=0}
        }
        let policy:PatrolWorkPolicy;let baseline:Sample;let last:Sample
        let used_j:Double;let remaining_j:Double;let valid:Bool;let permanent:Bool;let exhausted:Bool;let reason:String?
        var validGeometry:Bool {policy.valid && baseline.validGeometry && last.validGeometry && used_j.isFinite && used_j>=0 && remaining_j.isFinite && remaining_j>=0 && remaining_j<=policy.limit_j && baseline.run_id==last.run_id && baseline.boot_id==last.boot_id && baseline.model_sha256==last.model_sha256}
    }

    let exclusions:PatrolRestrictionPolicy?
    let `return`: Rendezvous?
    let reason: String?
    struct Rendezvous: Decodable {
        struct Point: Decodable {let x:Double;let y:Double;let yaw:Double}
        let destination:Point;let before_expiry_seconds:Int;let begin_at_s:Double?
        let requested:Bool;let trigger:String?
        var valid:Bool {destination.x.isFinite && destination.y.isFinite && destination.yaw.isFinite && abs(destination.yaw)<=Double.pi && (0...86400).contains(before_expiry_seconds) && (begin_at_s?.isFinite ?? true)}
    }
    let mission_id: String; let state: String; let current_room: String?
    let expires_at_s: Double; let nominal_speed_mps: Double
    let rooms: [Room]; let recent_findings: [Finding]; let localization: Localization?
    let localization_health: LocalizationHealth?
    struct Room: Decodable, Identifiable {
        let id: String; let name: String; let polygon: [[Double]]
        let last_observed_s: Double?; let visits: Int
        let revisit_seconds: Int?; let next_due_s: Double?
        let blocked_until_s: Double?; let blocked_reason: String?; let evidence: [String]
        func blocked(at time: Double) -> Bool { (blocked_until_s ?? 0)>time }
    }
    struct Finding: Decodable, Identifiable {
        let id: String; let time_s: Double; let text: String; let evidence: [String]
    }
    struct Localization: Decodable {
        let position: [Double]; let yaw: Double; let covariance: [Double]; let valid: Bool
        var validGeometry: Bool {
            position.count==3 && position.allSatisfy { $0.isFinite } && yaw.isFinite &&
            covariance.count==4 && covariance.allSatisfy { $0.isFinite } &&
            covariance[0]>=0 && covariance[3]>=0 && abs(covariance[1]-covariance[2])<0.00001 &&
            covariance[0]*covariance[3]-covariance[1]*covariance[2]>=(-0.000001)
        }
    }
    /// Display diagnostics only. Receipt ages are monotonic wall time, not simulation time.
    struct LocalizationHealth: Decodable {
        let schema: String; let ready: Bool; let reasons: [String]
        let sensor_age_s: Double?; let pose_age_s: Double?; let costmap_age_s: Double?
        let variance: [Double]; let variance_limit: Double
        let passive: Passive; let motion_resumed: Bool
        struct Passive: Decodable {
            let attempts: Int; let pending: Bool
            let last_scan_stamp: Double?; let last_attempt_age_s: Double?
            private enum CodingKeys: String, CodingKey { case attempts,pending,last_scan_stamp,last_attempt_age_s }
            init(from decoder: Decoder) throws {
                let c = try decoder.container(keyedBy:CodingKeys.self)
                attempts = try c.decode(Int.self,forKey:.attempts)
                pending = try c.decode(Bool.self,forKey:.pending)
                last_scan_stamp = try c.decode(Double?.self,forKey:.last_scan_stamp)
                last_attempt_age_s = try c.decode(Double?.self,forKey:.last_attempt_age_s)
            }
        }
        private enum CodingKeys: String, CodingKey {
            case schema,ready,reasons,sensor_age_s,pose_age_s,costmap_age_s,variance,variance_limit,passive,motion_resumed
        }
        static let reasonText: [String: String] = [
            "policy_unconfirmed":"Navigation policy unconfirmed",
            "sensors_unready":"Sensor pipeline not ready",
            "scan_invalid":"Laser scan invalid",
            "sensor_stale":"Scan or odometry observations stale",
            "pose_stale":"AMCL pose stale", "costmap_stale":"Costmap stale",
            "pose_invalid":"AMCL pose invalid", "covariance_invalid":"AMCL uncertainty invalid",
            "covariance_high":"AMCL uncertainty exceeds the navigation limit",
            "navigation_unavailable":"Navigation service unavailable",
            "planner_unavailable":"Route planner unavailable"
        ]
        init(from decoder: Decoder) throws {
            let c = try decoder.container(keyedBy: CodingKeys.self)
            schema = try c.decode(String.self,forKey:.schema)
            ready = try c.decode(Bool.self,forKey:.ready)
            reasons = try c.decode([String].self,forKey:.reasons)
            sensor_age_s = try c.decode(Double?.self,forKey:.sensor_age_s)
            pose_age_s = try c.decode(Double?.self,forKey:.pose_age_s)
            costmap_age_s = try c.decode(Double?.self,forKey:.costmap_age_s)
            variance = try c.decode([Double].self,forKey:.variance)
            variance_limit = try c.decode(Double.self,forKey:.variance_limit)
            passive = try c.decode(Passive.self,forKey:.passive)
            motion_resumed = try c.decode(Bool.self,forKey:.motion_resumed)
            func age(_ value: Double?) -> Bool { value.map { $0.isFinite && (0...1e12).contains($0) } ?? true }
            guard schema == "wendy.nav.localization-health.v1", !motion_resumed,
                  reasons.count <= Self.reasonText.count, Set(reasons).count == reasons.count,
                  reasons.allSatisfy({ Self.reasonText[$0] != nil }), ready == reasons.isEmpty,
                  age(sensor_age_s),age(pose_age_s),age(costmap_age_s),
                  variance_limit == 0.0225, variance.isEmpty || variance.count == 3,
                  variance.allSatisfy({ $0.isFinite && abs($0) <= 1e12 }),
                  (variance.allSatisfy({ $0 >= 0 }) || reasons.contains("covariance_invalid")),
                  (0...1_000_000_000).contains(passive.attempts),
                  age(passive.last_scan_stamp),age(passive.last_attempt_age_s),
                  !passive.pending || passive.attempts > 0 else { throw DemoError.invalidFile }
            if ready {
                guard sensor_age_s.map({ $0 < 0.6 }) == true,
                      pose_age_s.map({ $0 < 3 }) == true,costmap_age_s.map({ $0 < 3 }) == true,
                      variance.count == 3,variance.allSatisfy({ $0 >= 0 && $0 < variance_limit })
                else { throw DemoError.invalidFile }
            }
        }
        func summary(held: Bool) -> String {
            if ready { return held ? "Observations recovered. Resume remains explicit." : "Navigation observations ready." }
            return reasons.compactMap { Self.reasonText[$0] }.joined(separator: ". ") + "."
        }
        var observationAges: String {
            func age(_ v: Double?) -> String { v.map { String(format:"%.2f s",$0) } ?? "unknown" }
            return "Scan/odometry \(age(sensor_age_s)) · pose \(age(pose_age_s)) · costmap \(age(costmap_age_s))"
        }
        var passiveSummary: String {
            "Passive observation requests: \(passive.attempts)" + (passive.pending ? " · awaiting response" : " · idle")
        }
    }
    var localizationHeld: Bool { ["paused","return_held","work_held","environment_held","world_held","tour_held"].contains(state) }
    var terminal: Bool { ["stopped","expired","completed","failed","interrupted"].contains(state) }
    var valid: Bool {
        !mission_id.isEmpty && expires_at_s.isFinite && nominal_speed_mps.isFinite &&
        nominal_speed_mps>=0 && rooms.count<=128 && recent_findings.count<=128 &&
        Set(rooms.map(\.id)).count==rooms.count && Set(recent_findings.map(\.id)).count==recent_findings.count &&
        rooms.allSatisfy { !$0.id.isEmpty && $0.visits>=0 && $0.polygon.count>=3 && $0.polygon.count<=128 &&
            $0.polygon.allSatisfy { $0.count==2 && $0.allSatisfy { $0.isFinite } } &&
            (($0.revisit_seconds == nil && $0.next_due_s == nil) || ($0.revisit_seconds.map {(10...86400).contains($0)} == true && $0.next_due_s?.isFinite == true)) && ($0.last_observed_s?.isFinite ?? true) && ($0.blocked_until_s?.isFinite ?? true) && $0.evidence.count<=128 } &&
        recent_findings.allSatisfy { $0.time_s.isFinite && $0.evidence.count<=128 } &&
        (tour?.valid ?? true) && (world_watch?.valid ?? true) && (environment?.validGeometry ?? true) && (offline?.valid ?? true) && (work?.validGeometry ?? true) && (localization?.validGeometry ?? true) && (self.return?.valid ?? true) && (exclusions?.valid ?? true)
    }
}
struct SensorSource: Codable, Identifiable {
    let id: String; let enabled: Bool; let kind: String?; let label: String?
    var title: String { label ?? id.replacingOccurrences(of:"-",with:" ").capitalized }
    enum CodingKeys: String, CodingKey { case id,enabled,kind,label }
    init(from decoder: Decoder) throws {
        let c=try decoder.container(keyedBy:CodingKeys.self)
        id=try c.decode(String.self,forKey:.id);enabled=try c.decode(Bool.self,forKey:.enabled)
        kind=try c.decodeIfPresent(String.self,forKey:.kind);label=try c.decodeIfPresent(String.self,forKey:.label)
        guard !id.isEmpty,id.count<=128,id.allSatisfy({$0.isLetter || $0.isNumber || $0=="-" || $0=="_"}),
              (kind?.count ?? 0)<=64,(label?.count ?? 0)<=160 else {throw DemoError.invalidFile}
    }
}
struct Robot: Decodable, Identifiable {
    let id: String; let name: String; let color: [Double]; let position: [Double]; let velocity: [Double]
    let online: Bool; let available: Bool; let state: String; let job_id: String
    let path: [[Double]]; let lidar_points: [[Double]]; let local_voxels: Int; let peer_voxels: Int
    let route: [String]
}
struct Job: Decodable, Identifiable { let id: String; let label: String; let position: [Double]; let state: String? }
struct Event: Decodable { let time_s: Double; let text: String }
struct Voxel: Decodable { let position: [Double]; let source: String }
struct Network: Decodable {
    let edges: [Edge]; let internet: Edge; let gossip_rounds: Int; let peer_voxels: [String:Int]
}
struct Edge: Decodable, Identifiable {
    let id: String; let a: String; let b: String; let enabled: Bool; let up: Bool
    let in_range: Bool?; let packets: Int
}
struct CommandReceipt: Decodable { let schema: String; let run_id: String; let sequence: Int; let action: String; let accepted: Bool; let error: String?;let target:String? }
struct DemoCommand: Encodable { let schema = "wendy.macos-demo.command.v1"; let run_id: String; let sequence: Int; let action: String; let target: String }
struct ReadyReceipt: Encodable { let schema = "wendy.macos-demo.app-ready.v1"; let run_id: String; let renderer = "SceneKit"; let synthetic = true }
struct CaptureRequest: Decodable { let id: String }
struct AppCaptureReceipt: Encodable {
    let schema = "wendy.macos-demo.app-capture.v1"
    let run_id: String; let mode: String?; let state_sequence: Int; let renderer_sequence: Int
    let display_clock = "mach_absolute_time_ns"
    let capture_started_monotonic_ns: Int64; let capture_finished_monotonic_ns: Int64
    let display_deadline_monotonic_ns: Int64?; let observation_deadline_monotonic_ns: Int64?
    let fresh_at_start: Bool?; let fresh_at_end: Bool?
    let native_observation_fresh_at_start: Bool?; let native_observation_fresh_at_end: Bool?
    let fusion_state: String?; let localized_hazard: Bool; let hazard_visible: Bool; let current_native_hazard_captured: Bool
    let original_envelope_sha256: String?; let sample_time_s: Double
    let layers: [LayerDiagnostic]; let camera: CameraDiagnostic?; let loaded_thumbnails: Int
    let loaded_thumbnail_ids: [String]; let sensor_sources: [SensorSource]?
    let native_tracks: [NativeTrackCapture]
    let inspector_panel: String?
    let png_sha256: String; let png_bytes: Int
}
struct NativeTrackCapture: Encodable {
    let id: String; let original_envelope_sha256: String?
    let display_deadline_monotonic_ns: Int64?
    let visible: Bool; let fresh_at_start: Bool; let fresh_at_end: Bool
}

/// A proposed route supplied by the mission handoff. World XYZ stays separate
/// from the original observation; this preview never commands the robot.
struct DemoInspectionRoute: Equatable {
    let runID: String
    let originalEnvelopeSHA256: String
    let observed: [Double]
    let target: [Double]
    let route: [[Double]]
    init(runID: String, originalEnvelopeSHA256: String, observed: [Double], target: [Double], route: [[Double]]) throws {
        func point(_ p: [Double]) -> Bool { p.count == 3 && p.allSatisfy { $0.isFinite && abs($0) <= 1_000_000 } }
        guard !runID.isEmpty,runID.utf8.count <= 128,
              originalEnvelopeSHA256.count == 64,originalEnvelopeSHA256.allSatisfy({ "0123456789abcdef".contains($0) }),
              point(observed),point(target),!route.isEmpty,route.count <= 4096,
              route.allSatisfy({ point($0) && $0[1] == target[1] }),route.last == target else { throw DemoError.invalidFile }
        self.runID=runID;self.originalEnvelopeSHA256=originalEnvelopeSHA256
        self.observed=observed;self.target=target;self.route=route
    }
}

enum DemoLayer: String, CaseIterable, Identifiable {
    case building = "Building", lidar = "Live LiDAR", voxels = "Shared voxels", routes = "Planned routes"
    case links = "Mesh links", jobs = "Inspections", labels = "Labels"
    case observations = "Observations", hazards = "Hazards", tracks = "Tracks"
    case coverage = "Room coverage", localization = "Localization"
    var id: String { rawValue }
    var icon: String { switch self {
        case .building: return "building.2"; case .lidar: return "sensor.tag.radiowaves.forward"
        case .voxels: return "square.stack.3d.up"; case .routes: return "point.topleft.down.to.point.bottomright.curvepath"
        case .links: return "point.3.connected.trianglepath.dotted"; case .jobs: return "checkmark.circle"
        case .labels: return "textformat"
        case .coverage:return "square.grid.2x2"; case .localization:return "location.circle"
        case .observations: return "viewfinder"; case .hazards: return "waveform.path.ecg"; case .tracks:return "figure.walk"
    } }
}
enum PerceptionPanel:String,CaseIterable,Identifiable {
    case hazard="Hazard", people="People"
    var id:String {rawValue}
}
enum CameraPreset: String, CaseIterable, Identifiable { case perspective = "Overview", top = "Floor plan", robot = "Follow robot"; var id: String { rawValue } }

@MainActor final class DemoModel: ObservableObject {
    let actions = TwinActions()

    let readOnly: Bool
    let session: URL
    @Published var geometry: DemoGeometry?
    @Published var state: DemoState?
    @Published var layers: Set<DemoLayer> = [.building,.routes,.jobs,.labels,.observations,.hazards,.tracks]
    @Published var evidenceImages: [String:NSImage] = [:]
    @Published var trackingImages: [String:NSImage] = [:]
    @Published var selectedTrack:String?
    @Published private(set) var selectedOriginal: DemoOriginalSelection?
    @Published var presentedEvidence: DemoOriginalSelection?
    @Published var patrolEvidenceDigest:String?
    @Published private(set) var inspectionRoute: DemoInspectionRoute?
    @Published private(set) var inspectionRouteRevision = 0
    let nativeMedia: DemoNativeMedia
    @Published private var requestedPerceptionPanel:PerceptionPanel = .hazard
    private let thumbnails = EvidenceThumbnails()
    @Published var selectedRobot = "atlas"
    @Published var panel = "Layers"
    @Published var camera: CameraPreset = .perspective
    @Published var cameraRevision = 0
    @Published var pending: Int?
    @Published var commandMessage = "Ready when you are"
    @Published var presentationReplay = false
    @Published var error: String?
    @Published var stale = true
    @Published var latestRead = Date.distantPast
    var renderer: SceneRenderer?
    private var timer: Timer?
    private var polling = false
    private var scheduledInterval: TimeInterval = 0
    private let monotonicNow: @MainActor () -> Int64
    @Published private(set) var nativeObservationCurrent = false
    private var lastSequence = -1
    private var latestTick = ContinuousClock.now
    private var pendingAction = ""
 private var pendingTarget = ""
 #if canImport(WorldClient)
 lazy var zones=DemoZones(model:self)
 #endif
    private var pendingSince = ContinuousClock.now
    private var lastCommand = 0
    private var readyWritten = false
    private var observedCapture = ""
    let capturePath: String?
    var captureDone = false
    lazy var qa = DemoQA(model:self)

    init(session: URL, capture: String?, monotonicNow: @escaping @MainActor () -> Int64 = demoMonotonicNS,
         viewerProfile: URL? = nil, mediaCacheDirectory: URL? = nil, readOnly: Bool = false) {
        self.readOnly = readOnly; self.presentationReplay = readOnly
        self.session = session; self.capturePath = capture; self.monotonicNow = monotonicNow
        let storage = mediaCacheDirectory ?? FileManager.default.urls(for:.applicationSupportDirectory,in:.userDomainMask)[0].appendingPathComponent("WendyWorld/NativeMedia")
        nativeMedia = DemoNativeMedia(profile:viewerProfile,cacheDirectory:storage)
    }
    var snapshot: Snapshot? { state?.snapshot }
    var robot: Robot? { snapshot?.robots.first { $0.id == selectedRobot } }
    var connected: Bool { !stale && state?.worker_status == "running" && (!(liveNativeReplica || handoffMode) || observationFresh(at:monotonicNow())) }
    var trackingOnly:Bool { snapshot?.tracking != nil && snapshot?.fusion == nil && snapshot?.robots.isEmpty == true }
    var selectedOriginalEnvelopeSHA256: String? {
        selectedOriginal?.runID == state?.run_id ? selectedOriginal?.originalEnvelopeSHA256 : nil
    }
    var visibleInspectionRoute: DemoInspectionRoute? {
        guard let route=inspectionRoute,route.runID==state?.run_id,
              route.originalEnvelopeSHA256==selectedOriginalEnvelopeSHA256 else{return nil}
        return route
    }
    @discardableResult func setInspectionRoute(_ value: DemoInspectionRoute?) -> Bool {
        if let value {
            guard value.runID==state?.run_id,value.originalEnvelopeSHA256==selectedOriginalEnvelopeSHA256 else{return false}
        }
        if inspectionRoute != value { inspectionRoute=value;inspectionRouteRevision += 1;renderer?.update(self) }
        return true
    }
    @discardableResult func selectTrack(_ id: String) -> Bool {
        refreshNativeDisplay()
        guard let track=snapshot?.tracking?.live(at:snapshot?.time_s ?? 0).first(where:{$0.id==id}) else { return false }
        selectedTrack=id; setPerceptionPanel(.people)
        if let original=track.originalEnvelopeSHA256,let run=state?.run_id {
            if original != selectedOriginalEnvelopeSHA256 { setInspectionRoute(nil) }
            selectedOriginal=DemoOriginalSelection(originalEnvelopeSHA256:original,runID:run,title:"Person evidence",
                observationTime:track.last_observed,evidence:track.nativeEvidence,observationID:nil)
        } else { selectedOriginal=nil;setInspectionRoute(nil) }
        return true
    }
    @discardableResult func selectHazard(observationID: String? = nil) -> Bool {
        refreshNativeDisplay()
        guard let fusion=snapshot?.fusion,fusion.localizedHazard,let original=fusion.original_envelope_sha256,
              let run=state?.run_id else { return false }
        let observation=observationID.flatMap { id in fusion.observations.first(where:{$0.id==id}) }
        if observationID != nil && observation == nil { return false }
        if original != selectedOriginalEnvelopeSHA256 { setInspectionRoute(nil) }
        selectedTrack=nil; setPerceptionPanel(.hazard)
        selectedOriginal=DemoOriginalSelection(originalEnvelopeSHA256:original,runID:run,
            title:observation.map { $0.label+" evidence" } ?? "Hazard evidence",
            observationTime:String(format:"%.3f s",fusion.sample_time_s),evidence:fusion.nativeEvidence,observationID:observationID)
        return true
    }
    func clearOriginalSelection() { selectedOriginal=nil; selectedTrack=nil;setInspectionRoute(nil) }
    func openSelectedEvidence() { presentedEvidence=selectedOriginal }
    var availablePerceptionPanels:[PerceptionPanel] {
        PerceptionPanel.allCases.filter {$0 == .hazard ? snapshot?.fusion != nil:snapshot?.tracking != nil}
    }
    var perceptionPanel:PerceptionPanel? {
        availablePerceptionPanels.contains(requestedPerceptionPanel) ? requestedPerceptionPanel:availablePerceptionPanels.first
    }
    @discardableResult func setPerceptionPanel(_ panel:PerceptionPanel)->Bool {
        guard availablePerceptionPanels.contains(panel) else{return false}
        requestedPerceptionPanel=panel;return true
    }
    static func isPatrolMode(_ mode:String?)->Bool { mode == "go2-patrol" || mode == "go2-fire-response" }
    static func isFireWaiting(_ value:DemoState)->Bool {
        value.mode == "go2-fire-response" && value.snapshot.phase == "waiting_for_fire" &&
        value.snapshot.patrol == nil && value.snapshot.controls_available == false &&
        !value.snapshot.authorized && value.snapshot.stopped
    }
    var patrolMode:Bool { Self.isPatrolMode(state?.mode) }
    var fireResponseMode:Bool { state?.mode == "go2-fire-response" }
    var handoffMode:Bool { state?.mode == "fusion-handoff" }
    var liveNativeReplica:Bool { state?.mode == "live-native-replica" }
    var hasRobots:Bool { snapshot?.robots.isEmpty == false }
    var showsRobotControls:Bool { hasRobots && !liveNativeReplica }
    var sensorSources:[SensorSource] { snapshot?.sensor_sources ?? [] }
    var availableCameras:[CameraPreset] { showsRobotControls ? CameraPreset.allCases:[.perspective,.top] }
    var availableLayers:[DemoLayer] {
        if patrolMode {return [.building,.coverage,.routes,.lidar,.observations,.hazards,.localization,.labels]}
        if trackingOnly {return visibleInspectionRoute == nil ? [.building,.tracks,.labels]:[.building,.tracks,.routes,.labels]}
        if !showsRobotControls {return [.building,.labels,.observations,.hazards,.tracks,.routes].filter {
            switch $0 {
            case .building,.labels:return true
            case .tracks:return snapshot?.tracking != nil
            case .routes:return visibleInspectionRoute != nil
            default:return snapshot?.fusion != nil
            }
        }}
        return DemoLayer.allCases.filter {$0 != .coverage && $0 != .localization && ($0 != .tracks || snapshot?.tracking != nil)}
    }
    var statusBadge:String {
        if readOnly || presentationReplay {return "SIMULATION REPLAY"}
        if liveNativeReplica {return connected ? "LIVE SIMULATION":"WAITING FOR SENSORS"}
        return connected ? "LIVE SIMULATION":"WAITING FOR SIMULATOR"
    }
    var canCommand: Bool { !readOnly && !presentationReplay && connected && pending == nil && error == nil &&
        (!fireResponseMode || snapshot?.patrol != nil) &&
        (!liveNativeReplica || snapshot?.controls_available == true) }
    var pollInterval: TimeInterval { (liveNativeReplica || handoffMode) ? 0.025:(patrolMode ? 0.033:0.15) }
    private func observationFresh(at now: Int64) -> Bool {
        guard let state, state.display_clock == "mach_absolute_time_ns",
              let deadline = state.observation_deadline_monotonic_ns else { return false }
        return now >= 0 && now < deadline
    }
    private func hazardFresh(at now: Int64) -> Bool {
        guard observationFresh(at:now), let deadline = state?.display_deadline_monotonic_ns else { return false }
        return now < deadline
    }
    func refreshNativeDisplay() {
        guard (liveNativeReplica || handoffMode), !presentationReplay else { return }
        let now = monotonicNow(), current = observationFresh(at:now)
        if nativeObservationCurrent != current { nativeObservationCurrent = current }
        guard var value=state else {return}
        var changed=false
        if let fusion = value.snapshot.fusion, fusion.state == "hazard", !hazardFresh(at:now) {
            value.snapshot.fusion = fusion.expiredDisplay()
            if handoffMode {value.snapshot.fusion_selection=value.snapshot.fusion_selection?.expiredDisplay()}
            evidenceImages = [:]
            changed=true
        }
        if handoffMode, !current, let selection=value.snapshot.fusion_selection, !selection.displayExpired {
            value.snapshot.fusion_selection=selection.expiredDisplay()
            value.snapshot.fusion=value.snapshot.fusion?.expiredDisplay()
            evidenceImages=[:];changed=true
        }
        if var tracking=value.snapshot.tracking {
            let count=tracking.tracks.count,cameras=tracking.cameras.count
            tracking.expireNative(at:now,clockKnown:value.display_clock=="mach_absolute_time_ns")
            if tracking.tracks.count != count || tracking.cameras.count != cameras {
                value.snapshot.tracking=tracking
                let sources=Set(tracking.cameras.map(\.sensor_id))
                trackingImages=trackingImages.filter { sources.contains($0.key) }
                changed=true
            }
        }
        if changed {
            state=value
            renderer?.update(self, force:true)
        }
    }
    func start() {
        polling = true
        poll()
        configureTimer()
    }
    private func configureTimer() {
        guard polling, scheduledInterval != pollInterval else { return }
        timer?.invalidate(); scheduledInterval = pollInterval
        timer = Timer.scheduledTimer(withTimeInterval: pollInterval, repeats: true) { [weak self] _ in
            MainActor.assumeIsolated { self?.poll() }
        }
    }
    private func read<T: Decodable>(_ name: String, limit: Int, as type: T.Type) throws -> T {
        let path = session.appendingPathComponent(name)
        let size = try path.resourceValues(forKeys: [.fileSizeKey]).fileSize ?? 0
        guard size > 0, size <= limit else { throw DemoError.invalidFile }
        let data = try Data(contentsOf: path)
        guard data.count <= limit else { throw DemoError.invalidFile }
        return try JSONDecoder().decode(type, from: data)
    }
    func poll() {
        defer {
            refreshNativeDisplay()
            configureTimer()
            if pending != nil,ContinuousClock.now-pendingSince > .seconds(5) {
                error = "Control acknowledgement missing. No automatic retry."
            }
            #if canImport(WorldClient)
            if zones.active != nil { renderer?.update(self) }
            #endif
            if !readOnly { qa.poll() }
        }
        do {
            if geometry == nil {
                let value = try read("geometry.json", limit: 64 << 20, as: DemoGeometry.self)
                guard value.schema == "wendy.macos-demo.geometry.v1", value.geoms.count <= 30000 else { throw DemoError.invalidFile }
                geometry = value
            }
            let value = try read("state.json", limit: 16 << 20, as: DemoState.self)
            guard value.schema == "wendy.macos-demo.state.v1", value.run_id == geometry?.run_id,
                  value.snapshot.run_id == value.run_id, value.sequence >= lastSequence,
                  value.snapshot.patrol?.valid != false,
                  !Self.isPatrolMode(value.mode) || value.snapshot.patrol != nil || Self.isFireWaiting(value),
                  value.snapshot.fusion?.replica_node == nil || value.snapshot.fusion?.replica_node == value.snapshot.map_node,
                  value.snapshot.fusion_selection?.agrees(with:value.snapshot.fusion,mapNode:value.snapshot.map_node) ?? true,
                  value.snapshot.tracking == nil || value.snapshot.tracking?.participant_id == value.snapshot.map_node,
                  (value.snapshot.sensor_sources?.count ?? 0)<=16,
                  Set((value.snapshot.sensor_sources ?? []).map(\.id)).count == (value.snapshot.sensor_sources?.count ?? 0) else { throw DemoError.invalidFile }
            if value.sequence > lastSequence {
                let enteringPatrol = Self.isPatrolMode(value.mode) && value.mode != state?.mode
                let leavingFireWait = state.map(Self.isFireWaiting) == true && !Self.isFireWaiting(value)
                state = value
                if enteringPatrol {
                    selectedRobot=value.snapshot.robots.first?.id ?? "go2"
                    layers=[.building,.coverage,.routes,.labels,.observations,.hazards,.localization]
                }
                if enteringPatrol || leavingFireWait {
                    commandMessage = readOnly ? "Retained simulation replay · robot controls disabled" : Self.isFireWaiting(value) ? "Watching camera inputs; Go2 waits for a confirmed cue":fireResponseMode ? "The signed Go2 mission continues when this viewer closes":"Patrol continues when this viewer closes"
                }
                lastSequence = value.sequence; latestRead = Date(); latestTick = .now
                lastCommand = max(lastCommand, value.last_command_sequence)
                updateEvidence()
                if !availableCameras.contains(camera) {setCamera(.perspective)}
            }
            stale = !readOnly && ContinuousClock.now - latestTick > .seconds(2)
            refreshNativeDisplay()
            if let number = pending {
                let name = String(format: "receipts/%06d.json", number)
                if FileManager.default.fileExists(atPath: session.appendingPathComponent(name).path) {
                    let receipt = try read(name, limit: 4096, as: CommandReceipt.self)
                    guard receipt.schema == "wendy.macos-demo.command-result.v1", receipt.run_id == value.run_id,
                          receipt.sequence == number, receipt.action == pendingAction, pendingAction != "zone_select" || receipt.target==pendingTarget else { throw DemoError.invalidFile }
                    if value.last_command_sequence >= number {
                        #if canImport(WorldClient)
                        if pendingAction=="zone_select"{zones.acknowledge(sequence:number,target:pendingTarget,accepted:receipt.accepted,tracking:value.snapshot.tracking)}
                        #endif
                        pending = nil
                        commandMessage = receipt.accepted ? "Control applied" : "Control refused: \(receipt.error ?? "unavailable")"
                    }
                }
            }
            if !readOnly, !readyWritten, renderer?.loaded == true {
                try writeJSON(ReadyReceipt(run_id: value.run_id), to: "app-ready.json")
                readyWritten = true
            }
            if renderer?.loaded == true, !captureDone, let capturePath { captureDone = capture(path: capturePath) }
            let trigger = session.appendingPathComponent("capture-request.json")
            if !readOnly, let data = try? Data(contentsOf: trigger), data.count <= 4096,
               let obj = try? JSONDecoder().decode(CaptureRequest.self, from: data),
               case let token = obj.id, token != observedCapture, token.count <= 64,
               token.allSatisfy({ $0.isLetter || $0.isNumber || $0 == "-" }) {
                observedCapture = token
                _ = capture(path: session.appendingPathComponent("app-capture-\(token).png").path)
            }
            if value.worker_status == "closed" { if !readOnly { qa.poll() };polling = false;timer?.invalidate() }
        } catch {
            stale = true
            if geometry != nil { self.error = liveNativeReplica ? "Live sensor readings are unavailable.":"The local simulator snapshot is unavailable." }
        }
    }
    func command(_ action: String, target: String = "") {
        guard canCommand, let state, lastCommand < 4096 else { return }
        if let selection=snapshot?.fusion_selection {
            let ids=Set(selection.candidates.map(\.gateway))
            guard (["handoff_input","handoff_result","handoff_quality","handoff_duplicate"].contains(action) && ids.contains(target)) || (action=="handoff_fixture" && target=="fixture") else{return}
        } else if patrolMode {
            guard let patrol=snapshot?.patrol,!patrol.terminal,
                  ["patrol_pause","patrol_resume","patrol_stop","patrol_recall"].contains(action),target.isEmpty else{return}
            if action=="patrol_resume" && !["paused","return_held","work_held","environment_held","world_held","tour_held"].contains(patrol.state) {return}
            if action=="patrol_resume" && patrol.world_watch.map({!$0.ready || $0.permanent}) == true {return}
            if action=="patrol_recall" && patrol.world_watch?.held == true {return}
            if action=="patrol_resume" && patrol.environment.map({!$0.ready || $0.permanent}) == true {return}
            if action=="patrol_recall" && patrol.environment?.held == true {return}
            if action=="patrol_recall" && (patrol.return == nil || patrol.return?.requested == true) {return}
            if action=="patrol_pause" && patrol.state == "paused" {return}
        } else if liveNativeReplica {
            guard action=="toggle_sensor",sensorSources.contains(where:{$0.id==target}) else{return}
        } else if trackingOnly {
            guard (action=="toggle_camera" && snapshot?.tracking?.camera_sources.contains(where:{$0.id==target}) == true)||(action=="zone_select" && target.utf8.count<=2048) else{return}
        } else if !hasRobots {
            return
        }
        let number = lastCommand + 1
        do {
            try exclusiveJSON(DemoCommand(run_id: state.run_id, sequence: number, action: action, target: target),
                              path:session.appendingPathComponent(String(format:"commands/%06d.json",number)))
            lastCommand = number; pending = number; pendingAction = action; pendingTarget=target;pendingSince = .now
            commandMessage = liveNativeReplica ? "Waiting for sensor acknowledgement…":"Waiting for simulator…"
        } catch { self.error = "Could not send the control. No automatic retry." }
    }
    func setCamera(_ preset: CameraPreset) { guard availableCameras.contains(preset) else{return};camera = preset; cameraRevision += 1 }
    func setLayer(_ layer:DemoLayer, enabled:Bool) { if enabled { layers.insert(layer) } else { layers.remove(layer) } }
    func updateEvidence() {
        var images: [String:NSImage] = [:]
        for observation in (snapshot?.fusion?.observations ?? []).filter({ $0.kind == "camera" }).prefix(8) {
            if let path=observation.evidence_path, let image=thumbnails.load(path, session:session) { images[observation.id]=image }
        }
        evidenceImages=images
        var tracked:[String:NSImage]=[:]
        for camera in (snapshot?.tracking?.cameras ?? []).prefix(8) {
            if let path=camera.evidence_path,let image=thumbnails.load(path,session:session,expectedSHA256:camera.image_sha256) { tracked[camera.sensor_id]=image }
        }
        trackingImages=tracked
    }
    func close() {
        polling = false; timer?.invalidate()
        nativeMedia.dismiss()
        if !readOnly && !patrolMode { try? Data().write(to: session.appendingPathComponent("stop"), options: .atomic) }
    }
    private func writeJSON<T: Encodable>(_ obj: T, to name: String) throws {
        guard !readOnly else { throw DemoError.invalidFile }
        try JSONEncoder().encode(obj).write(to: session.appendingPathComponent(name), options: .atomic)
    }
    func exclusiveJSON<T: Encodable>(_ obj: T, path: URL) throws {
        guard !readOnly else { throw DemoError.invalidFile }
        let data = try JSONEncoder().encode(obj)
        guard data.count <= 4096 else { throw DemoError.invalidFile }
        let tmp = path.deletingLastPathComponent().appendingPathComponent(".command-\(UUID().uuidString)")
        let fd = Darwin.open(tmp.path, O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW, S_IRUSR | S_IWUSR)
        guard fd >= 0 else { throw DemoError.invalidFile }
        defer { Darwin.close(fd); unlink(tmp.path) }
        let written = data.withUnsafeBytes { Darwin.write(fd, $0.baseAddress, $0.count) }
        guard written == data.count, fsync(fd) == 0, link(tmp.path, path.path) == 0 else { throw DemoError.invalidFile }
    }
    func capture(path: String) -> Bool {
        guard let window = NSApplication.shared.windows.first(where: { $0.isVisible }), let view = window.contentView,
              let scn = renderer?.view else { return false }
        return capture(path:path, size:view.bounds.size, renderScene:{ scn.snapshot() })
    }
    // The offscreen check calls this same path with an isolated SceneKit renderer.
    func capture(path: String, size: CGSize, renderScene: () -> NSImage) -> Bool {
        refreshNativeDisplay()
        guard let renderer, let capturedState = state else { return false }
        renderer.update(self, force:true)
        let start = monotonicNow()
        let freshStart = liveNativeReplica ? hazardFresh(at:start):nil
        let observationStart = liveNativeReplica ? observationFresh(at:start):nil
        let layers = renderer.diagnostics(), cameraState = renderer.cameraDiagnostics()
        let fusion = capturedState.snapshot.fusion
        let visible = fusion?.localizedHazard == true && layers.contains { $0.name == DemoLayer.hazards.rawValue && $0.enabled && !$0.hidden && $0.child_nodes > 0 }
        let renderedSequence = renderer.renderedSequence
        let thumbnailIDs = evidenceImages.keys.sorted()
        // Render this same app content with its own Metal snapshot in place of the SCNView.
        // SwiftUI lays out the chrome over that image, avoiding flipped-coordinate compositing
        // and keeping the live view and its camera untouched.
        let host = NSHostingView(rootView:DemoContent(model:self,capturedScene:renderScene()).preferredColorScheme(.light))
        host.frame = CGRect(origin:.zero,size:size)
        host.layoutSubtreeIfNeeded()
        guard let bitmap=host.bitmapImageRepForCachingDisplay(in:host.bounds) else { return false }
        host.cacheDisplay(in:host.bounds,to:bitmap)
        guard let png=bitmap.representation(using:.png,properties:[:]) else { return false }
        let end = monotonicNow()
        let freshEnd = liveNativeReplica ? hazardFresh(at:end):nil
        let observationEnd = liveNativeReplica ? observationFresh(at:end):nil
        let nativeTracks = liveNativeReplica ? (capturedState.snapshot.tracking?.live(at:capturedState.snapshot.time_s) ?? []).map { track in
            NativeTrackCapture(id:track.id,original_envelope_sha256:track.originalEnvelopeSHA256,
                display_deadline_monotonic_ns:track.displayDeadline,
                visible:self.layers.contains(.tracks) && renderer.world.childNode(withName:"track:"+track.id,recursively:true) != nil,
                fresh_at_start:track.nativeFresh(at:start),fresh_at_end:end>=start && track.nativeFresh(at:end))
        }:[]
        let receipt = AppCaptureReceipt(run_id:capturedState.run_id,mode:capturedState.mode,
            state_sequence:capturedState.sequence,renderer_sequence:renderedSequence,
            capture_started_monotonic_ns:start,capture_finished_monotonic_ns:end,
            display_deadline_monotonic_ns:capturedState.display_deadline_monotonic_ns,
            observation_deadline_monotonic_ns:capturedState.observation_deadline_monotonic_ns,
            fresh_at_start:freshStart,fresh_at_end:freshEnd,
            native_observation_fresh_at_start:observationStart,native_observation_fresh_at_end:observationEnd,
            fusion_state:fusion?.state,localized_hazard:fusion?.localizedHazard == true,hazard_visible:visible,
            current_native_hazard_captured:visible && freshStart == true && freshEnd == true && end >= start,
            original_envelope_sha256:fusion?.original_envelope_sha256,sample_time_s:capturedState.snapshot.time_s,
            layers:layers,camera:cameraState,loaded_thumbnails:thumbnailIDs.count,
            loaded_thumbnail_ids:thumbnailIDs,sensor_sources:capturedState.snapshot.sensor_sources,
            native_tracks:nativeTracks,
            inspector_panel:perceptionPanel?.rawValue,
            png_sha256:SHA256.hash(data:png).map { String(format:"%02x",$0) }.joined(),png_bytes:png.count)
        refreshNativeDisplay()
        do {
            try png.write(to:URL(fileURLWithPath:path),options:.atomic)
            try JSONEncoder().encode(receipt).write(to:URL(fileURLWithPath:path+".json"),options:.atomic)
            return true
        } catch { return false }
    }
}
enum DemoError: Error { case invalidFile }
