import Foundation
import SwiftUI

struct SiteRobotGeometryPacket: Sendable { var architecture, furniture: [Double] }
struct RobotMapPoint: Codable, Sendable { var X, Y: Double }
struct RobotMapVertex: Codable, Sendable { var X, Y, Z: Double }
struct RobotObservedSurface: Codable, Sendable {
    var position: RobotMapVertex
    var direction: RobotMapPoint
    var horizontal: Bool
    var stable: Bool
}
struct RobotVisualHint: Encodable, Sendable {
    var position: RobotMapPoint
    var yaw, at: Double
    var inliers: Int
    var residual: Double
}
struct RobotFusionRequest: Encodable, Sendable {
    var frame: String
    var architecture, furniture: [Double]
    var visual: RobotVisualHint?
    var reference: [RobotObservedSurface]
}
struct RobotScanPoint: Decodable, Equatable, Sendable {
    var X, Y, Z: Double
    var Class: Int
}
struct RobotFusionReport: Decodable, Sendable {
    struct Candidate: Decodable, Sendable { var X, Z, Yaw, FloorY, RMS, Coverage: Double }
    struct Tracking: Decodable, Sendable {
        struct Fit: Decodable, Sendable { var architecture, furniture, reference, directions: Int; var cost: Double; var valid: Bool }
        var fits: [Fit]?
        var independentViews: Int
        var novelFraction: Double
        var selected: Int
        var message: String
    }
    var message: String
    var candidates: [Candidate]?
    var tracking: Tracking
    var overlay: [RobotScanPoint]?
    var reference: [RobotObservedSurface]?
    var Accepted, Rejected: Int
    var waitingForObservations: Bool
    var visualUsed: Bool
    var observationRevision: UInt64
}

// Offer once per observation session. A failed match can offer immediately;
// sparse scans get time to arrive before suggesting a new viewpoint.
struct RobotRotationOffer {
    private(set) var offered = false
    private var waitingSince: Date?

    mutating func shouldPresent(eligible: Bool, scanReady: Bool, failedMatch: Bool = false, now: Date) -> Bool {
        guard eligible, !offered else { waitingSince = nil; return false }
        if failedMatch {
            offered = true
            return true
        }
        guard !scanReady else { waitingSince = nil; return false }
        if waitingSince == nil { waitingSince = now }
        guard let waitingSince, now.timeIntervalSince(waitingSince) >= 5 else { return false }
        offered = true
        return true
    }

    mutating func dismiss() { offered = true; waitingSince = nil }
}

enum RobotRotationState: Equatable {
    case idle, waitingForMeasurements
    case checking(Int), turning(Int), matching(Int)
    case stopped(String)

    var title: String? {
        switch self {
        case .idle: nil
        case .waitingForMeasurements: "Waiting for sensors"
        case .checking: "Checking space to turn"
        case .turning: "Looking around"
        case .matching: "Finding Go2 in the room"
        case .stopped: "Rotation stopped"
        }
    }

    var detail: String? {
        switch self {
        case .idle: nil
        case .waitingForMeasurements: "Go2 will stay still until fresh LiDAR and odometry arrive, then continue automatically."
        case .checking(let turn): "Checking clearance for turn \(turn) of 12."
        case .turning(let turn): "Turn \(turn) of 12. Keep Go2 in sight and its turning area clear."
        case .matching(let turn): "Comparing the view after turn \(turn) of 12 with your room."
        case .stopped(let reason): reason
        }
    }

    var canRetry: Bool { if case .stopped = self { true } else { false } }
}

// Only accepting the rotation prompt arms movement. Passive observations never do.
struct RobotPositionSearch {
    enum Outcome { case exhausted, timedOut }
    private(set) var outcome: Outcome?
    private(set) var active = false
    private(set) var remainingTurns = 0
    private(set) var direction: Int32 = 0
    private var deadline = Date.distantPast
    mutating func start(now: Date) { active = true; outcome = nil; remainingTurns = 12; direction = 0; deadline = now.addingTimeInterval(300) }
    mutating func resumeAfterCalibration(now: Date) { if active { deadline = now.addingTimeInterval(300) } }
    mutating func shouldRetryLocalization(pose: Bool, scanReady: Bool, measurementsReady: Bool, aligned: Bool,
                                         rematchAfterLook: Bool, attempts: Int, now: Date,
                                         calibrating: Bool = false) -> Bool {
        guard !calibrating else { return false }
        if active && now >= deadline { finish(.timedOut) }
        if outcome != nil || !measurementsReady { return false }
        // An explicit search must be able to acquire another view when room
        // landmarks are sparse. The backend still checks sensor freshness and
        // the entire turning footprint before allowing any motion.
        if active { return pose }
        return scanReady && (!aligned || rematchAfterLook) && attempts < 3
    }
    mutating func takeTurn(now: Date) -> Bool {
        guard active else { return false }
        guard now < deadline else { finish(.timedOut); return false }
        guard remainingTurns > 0 else { finish(.exhausted); return false }
        remainingTurns -= 1
        return true
    }
    mutating func recordDirection(_ value: Int32) { if active && direction == 0 && (value == 1 || value == -1) { direction = value } }
    mutating func cancel() { active = false; outcome = nil; remainingTurns = 0; direction = 0 }
    private mutating func finish(_ outcome: Outcome) { active = false; self.outcome = outcome }
}

struct RobotSearchReading: Codable, Sendable {
    var scanReady, measurementsReady: Bool
    var lookState: Int
    var lookDirection: Int32
    var observationRevision: UInt64

    init(_ state: WDSiteRobotState) {
        scanReady = state.scan_ready != 0; measurementsReady = state.measurements_ready != 0
        lookState = Int(state.look_state); lookDirection = state.look_direction
        observationRevision = state.observation_revision
    }

    init(scanReady: Bool, measurementsReady: Bool, lookState: Int = 0,
         lookDirection: Int32 = 0, observationRevision: UInt64 = 0) {
        self.scanReady = scanReady; self.measurementsReady = measurementsReady
        self.lookState = lookState; self.lookDirection = lookDirection; self.observationRevision = observationRevision
    }
}

struct RobotSearchPrompt: ViewModifier {
    @Binding var presented: Bool
    let canStart: Bool
    let start: () -> Void
    let dismiss: () -> Void

    func body(content: Content) -> some View {
        content.alert("Rotate Go2 to find its position?", isPresented: $presented) {
            Button("Start rotation", action: start).disabled(!canStart)
            Button("Not now", role: .cancel, action: dismiss)
        } message: {
            Text("No reliable position match was found. Go2 will check clearance, turn in 30° steps, and match its LiDAR observations after each turn. Keep Go2 in sight and its turning area clear.")
        }
    }
}

struct RobotRotationProgress: View {
    let state: RobotRotationState
    let canRetry, canPlace: Bool
    let retry, place, stop: () -> Void

    var body: some View {
        if let title = state.title {
            VStack(alignment: .leading, spacing: 8) {
                HStack(spacing: 8) {
                    if state.canRetry { Image(systemName: "exclamationmark.circle").foregroundStyle(.orange) }
                    else { ProgressView().controlSize(.small) }
                    Text(title).font(.caption.weight(.semibold))
                }
                if let detail = state.detail {
                    Text(detail).font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                }
                if state.canRetry {
                    HStack {
                        Button("Try again", action: retry).accessibilityLabel("Try rotation again").disabled(!canRetry)
                        Button("Place manually", action: place).disabled(!canPlace)
                    }.font(.caption).buttonStyle(.bordered)
                } else {
                    Button("Cancel search", role: .cancel, action: stop).font(.caption).buttonStyle(.bordered)
                }
            }.padding(10).frame(maxWidth: .infinity, alignment: .leading)
                .background(.quaternary, in: RoundedRectangle(cornerRadius: 10))
                .accessibilityElement(children: .contain)
        }
    }
}

protocol RobotSearching: Sendable {
    func look(_ handle: UInt64, sequence: UInt64, direction: Int32) async throws
    func fuse(_ handle: UInt64, sequence: UInt64, request: RobotFusionRequest) async throws -> RobotFusionReport
    func confirm(_ handle: UInt64, sequence: UInt64, matchSequence: UInt64, frame: String, candidate: Int) async throws
}

struct RobotSearchClient: RobotSearching {
    func look(_ handle: UInt64, sequence: UInt64, direction: Int32) async throws {
        try await SiteRobotSession.look(handle, sequence: sequence, direction: direction)
    }
    func fuse(_ handle: UInt64, sequence: UInt64, request: RobotFusionRequest) async throws -> RobotFusionReport {
        try await SiteRobotSession.fuse(handle, sequence: sequence, request: request)
    }
    func confirm(_ handle: UInt64, sequence: UInt64, matchSequence: UInt64, frame: String, candidate: Int) async throws {
        try await SiteRobotSession.confirmLocalization(handle, sequence: sequence, matchSequence: matchSequence, frame: frame, candidate: candidate)
    }
}
