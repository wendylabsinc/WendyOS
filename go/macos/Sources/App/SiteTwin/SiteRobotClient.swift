import Foundation
import SwiftUI

struct SiteRobotReading: Sendable {
    var message: String
    var controlWarning: String = ""
    var disconnected = false
    var pose, ready, aligned, moving: Bool
    var arrivalTiming, usingArrivalTiming, timingUnavailable, scanReady, measurementsReady: Bool
    var underBodyAllowance: Bool
    var allowUnobservedFloor: Bool = false
    var x, z, yaw, floorY: Double
    var observationRevision: UInt64 = 0
    var lookState: Int = 0
    var lookDirection: Int32 = 0
    var route: [SitePoint]
    var joints: [Double]
    var hazards: [SiteRobotHazard] = []
    var hazardsReady = false

    // Joint and pose samples animate the scene without rebuilding its controls.
    func hasSameControls(as other: Self) -> Bool {
        disconnected == other.disconnected && controlWarning == other.controlWarning && lookDirection == other.lookDirection && lookState == other.lookState && message == other.message && pose == other.pose && ready == other.ready &&
        aligned == other.aligned && moving == other.moving && arrivalTiming == other.arrivalTiming &&
        usingArrivalTiming == other.usingArrivalTiming && timingUnavailable == other.timingUnavailable &&
        allowUnobservedFloor == other.allowUnobservedFloor && scanReady == other.scanReady && measurementsReady == other.measurementsReady && underBodyAllowance == other.underBodyAllowance &&
        floorY == other.floorY && route == other.route && hazardsReady == other.hazardsReady
    }
}
struct SiteRobotCandidate: Sendable {
    var position: SitePoint
    var yaw, rms, coverage: Double

}
/// Progress of a Linux "find this object" mission, as the twin reports it.
struct RobotFindStatus: Decodable, Sendable, Equatable {
    var State = ""
    var Phase = ""
    var Message = ""
    var Query = ""
    var WantsView: UInt64 = 0
    var Viewpoints = 0
    var Views = 0
    var Remembered = false
    var Seen = false
    var Located = false
    var X = 0.0
    var Z = 0.0
    var Box: [Double] = []
    var BoxView: UInt64 = 0
    var Barked = false
    var BarkFailure = ""
    var searching: Bool { State == "searching" }
}
protocol SiteRobotObserving: Sendable {
    func open(session: UInt64, serial: String) async throws -> UInt64
    func poll(_ handle: UInt64) async throws -> SiteRobotReading
    func close(_ handle: UInt64) async
}

protocol SiteRobotTurning: Sendable {
    func look(_ handle: UInt64, sequence: UInt64, direction: Int32) async throws
    func stop(_ handle: UInt64, sequence: UInt64) async
}

protocol SiteRobotSearching: Sendable {
    func confirmLocalization(_ handle: UInt64, sequence: UInt64, matchSequence: UInt64, frame: String, candidate: Int) async throws
    func resetLocalization(_ handle: UInt64, sequence: UInt64, frame: String) async throws
    func fuse(_ handle: UInt64, sequence: UInt64, request: RobotFusionRequest) async throws -> RobotFusionReport
    func search(_ handle: UInt64, sequence: UInt64, request: RobotFusionRequest) async throws
    func searchStatus(_ handle: UInt64) async throws -> SiteRobotClient.RemoteReply
}

final class SiteRobotClient: SiteRobotObserving, SiteRobotTurning, SiteRobotSearching {
    static let shared = SiteRobotClient()
    private let queue = DispatchQueue(label: "dev.wendy.site-robot", attributes: .concurrent)
    private func perform<T: Sendable>(_ body: @escaping @Sendable () throws -> T) async throws -> T {
        try await BlockingWork.run(on: queue, unlessCancelled: false, body)
    }
    struct RemoteReply: Decodable, Sendable {
        var SearchState: String
        var SearchMessage: String
        var SearchTurns: Int
        var SearchMoves: Int?
        var SearchPhase: String?
        var Fusion: RobotFusionReport?
    }
    func resetLocalization(_ handle: UInt64, sequence: UInt64, frame: String) async throws {
        struct Command: Encodable { var Operation = "reset-localization"; var Sequence: UInt64; var Frame: String }
        let _: RemoteReply = try await remoteRequest(handle, data: JSONEncoder().encode(Command(Sequence: sequence, Frame: frame)))
    }
    func search(_ handle: UInt64, sequence: UInt64, request: RobotFusionRequest) async throws {
        struct Command: Encodable { var Operation = "search"; var Sequence: UInt64; var Fusion: RobotFusionRequest }
        let _: RemoteReply = try await remoteRequest(handle, data: JSONEncoder().encode(Command(Sequence: sequence, Fusion: request)))
    }
    func searchStatus(_ handle: UInt64) async throws -> RemoteReply {
        try await remoteRequest(handle, data: Data(#"{"Operation":"search-poll"}"#.utf8))
    }
    private func remoteRequest<Reply: Decodable & Sendable>(_ handle: UInt64, data: Data) async throws -> Reply {
        let json = String(decoding: data, as: UTF8.self)
        return try await perform {
            try GoClient.withCString(json) {
                guard let p = WDSiteRobotRequest(handle, $0) else { throw WendyError(message: "Missing Linux twin response.") }
                defer { WDSiteRobotFusionFree(p) }
                try GoClient.check(p.pointee.status)
                return try JSONDecoder().decode(Reply.self, from: Data(GoClient.string(p.pointee.json).utf8))
            }
        }
    }
    private struct FindReply: Decodable, Sendable { var Find: RobotFindStatus }
    /// Starts a Linux find mission. Linux plans, looks and moves; this client
    /// renders progress and supplies the camera frames Linux asks for.
    func find(_ handle: UInt64, sequence: UInt64, frame: String, query: String, bark: Bool, model: String, port: UInt32) async throws {
        struct Command: Encodable { var Operation = "find"; var Sequence: UInt64; var Frame, Query: String; var Bark: Bool; var Model: String; var ModelPort: UInt32 }
        let _: FindReply = try await remoteRequest(handle, data: JSONEncoder().encode(
            Command(Sequence: sequence, Frame: frame, Query: query, Bark: bark, Model: model, ModelPort: port)))
    }
    func findStatus(_ handle: UInt64) async throws -> RobotFindStatus {
        let reply: FindReply = try await remoteRequest(handle, data: Data(#"{"Operation":"find-poll"}"#.utf8))
        return reply.Find
    }
    func findView(_ handle: UInt64, view: UInt64, jpeg: Data) async throws {
        struct Command: Encodable { var Operation = "find-view"; var View: UInt64; var Image: Data }
        let _: FindReply = try await remoteRequest(handle, data: JSONEncoder().encode(Command(View: view, Image: jpeg)))
    }
    func open(session: UInt64, serial: String) async throws -> UInt64 {
        try await perform {
            try GoClient.withCString(serial) { serial in
                guard let p = WDSiteRobotOpen(session, serial) else { throw WendyError(message: "Missing robot connection.") }
                defer { WDUnitreeConnectionFree(p) }
                try GoClient.check(p.pointee.status)
                return p.pointee.handle
            }
        }
    }
    func poll(_ handle: UInt64) async throws -> SiteRobotReading {
        try await perform {
            guard let p = WDSiteRobotPoll(handle) else { throw WendyError(message: "Missing robot state.") }
            defer { WDSiteRobotStateFree(p) }
            try GoClient.check(p.pointee.status)
            let s = p.pointee
            return SiteRobotReading(message: GoClient.string(s.message), controlWarning: GoClient.string(s.control_warning), disconnected: s.disconnected != 0, pose: s.pose != 0, ready: s.ready != 0,
                aligned: s.aligned != 0, moving: s.moving != 0,
                arrivalTiming: s.arrival_timing != 0, usingArrivalTiming: s.using_arrival_timing != 0,
                timingUnavailable: s.timing_unavailable != 0, scanReady: s.scan_ready != 0, measurementsReady: s.measurements_ready != 0,
                underBodyAllowance: s.under_body_allowance != 0, allowUnobservedFloor: s.allow_unobserved_floor != 0, x: s.x, z: s.z, yaw: s.yaw, floorY: s.floor_y, observationRevision: s.observation_revision, lookState: Int(s.look_state), lookDirection: s.look_direction,
                route: UnsafeBufferPointer(start: s.route, count: s.route_count).map { SitePoint(x: $0.x, y: s.floor_y, z: $0.z) },
                joints: Array(UnsafeBufferPointer(start: s.joints, count: s.joint_count)),
                hazards: UnsafeBufferPointer(start: s.hazards, count: s.hazard_count).map {
                    SiteRobotHazard(position: SitePoint(x: $0.x, y: s.floor_y, z: $0.z), size: $0.size, yaw: $0.yaw, drop: $0.drop != 0)
                }, hazardsReady: s.hazards_ready != 0)
        }
    }
    func nextVideo(_ handle: UInt64) async throws -> Data {
        try await perform {
            guard let p = WDSiteRobotVideoNext(handle) else { throw WendyError(message: "Missing Go2 camera frame.") }
            defer { WDUnitreeVideoFrameFree(p) }
            try GoClient.check(p.pointee.status)
            guard let bytes = p.pointee.bytes, p.pointee.count > 0 else { return Data() }
            return Data(bytes: bytes, count: p.pointee.count)
        }
    }
    func align(_ handle: UInt64, sequence: UInt64, frame: String, position: SitePoint, yaw: Double, geometry: SiteRobotGeometryPacket? = nil) async throws {
        if let geometry {
            struct Command: Encodable {
                var Operation = "align"
                var Sequence: UInt64
                var Frame: String
                var X, Z, Floor, Yaw: Double
                var Fusion: RobotFusionRequest
            }
            let command = Command(Sequence: sequence, Frame: frame, X: position.x, Z: position.z, Floor: position.y, Yaw: yaw,
                Fusion: RobotFusionRequest(frame: frame, architecture: geometry.architecture, furniture: geometry.furniture, visual: nil, reference: []))
            let _: RemoteReply = try await remoteRequest(handle, data: JSONEncoder().encode(command))
            return
        }
        try await perform { try GoClient.withCString(frame) { try GoClient.finish(WDSiteRobotAlign(handle, sequence, $0, position.x, position.z, position.y, yaw)) } }
    }
    func destination(_ handle: UInt64, sequence: UInt64, frame: String, point: SitePoint, move: Bool) async throws {
        try await perform { try GoClient.withCString(frame) { try GoClient.finish(WDSiteRobotDestination(handle, sequence, $0, point.x, point.z, move ? 1 : 0)) } }
    }
    func look(_ handle: UInt64, sequence: UInt64, direction: Int32) async throws {
        try await perform { try GoClient.finish(WDSiteRobotLook(handle, sequence, direction)) }
    }
    func stop(_ handle: UInt64, sequence: UInt64) async { try? await perform { WDSiteRobotStop(handle, sequence) } }
    func localize(_ handle: UInt64, sequence: UInt64, frame: String, vertices: [Double]) async throws -> [SiteRobotCandidate] {
        try await perform {
            try GoClient.withCString(frame) { frame in
                var mesh = vertices
                return try mesh.withUnsafeMutableBufferPointer { values in
                    guard let p = WDSiteRobotLocalize(handle, sequence, frame, values.baseAddress, values.count/3) else { throw WendyError(message: "No localization response.") }
                    defer { WDSiteRobotCandidatesFree(p) }
                    try GoClient.check(p.pointee.status)
                    return UnsafeBufferPointer(start: p.pointee.items, count: p.pointee.count).map {
                        SiteRobotCandidate(position: SitePoint(x: $0.x, y: $0.floor_y, z: $0.z), yaw: $0.yaw, rms: $0.rms, coverage: $0.coverage)
                    }
                }
            }
        }
    }
    func fuse(_ handle: UInt64, sequence: UInt64, request: RobotFusionRequest) async throws -> RobotFusionReport {
        let bytes = try JSONEncoder().encode(request)
        guard let json = String(data: bytes, encoding: .utf8) else { throw SiteLocalizationError.invalidMap }
        return try await perform {
            try GoClient.withCString(json) { request in
                guard let p = WDSiteRobotFuse(handle, sequence, request) else { throw WendyError(message: "Missing localization report.") }
                defer { WDSiteRobotFusionFree(p) }
                try GoClient.check(p.pointee.status)
                return try JSONDecoder().decode(RobotFusionReport.self, from: Data(GoClient.string(p.pointee.json).utf8))
            }
        }
    }
    func confirmLocalization(_ handle: UInt64, sequence: UInt64, matchSequence: UInt64, frame: String, candidate: Int) async throws {
        try await perform { try GoClient.withCString(frame) { try GoClient.finish(WDSiteRobotConfirmLocalization(handle, sequence, matchSequence, $0, Int32(candidate))) } }
    }
    func arrivalTiming(_ handle: UInt64, sequence: UInt64, enabled: Bool) async throws {
        try await perform { try GoClient.finish(WDSiteRobotArrivalTiming(handle, sequence, enabled ? 1 : 0)) }
    }
    func close(_ handle: UInt64) async { try? await perform { WDSiteRobotClose(handle) } }
    func underBodyAllowance(_ handle: UInt64, sequence: UInt64, enabled: Bool) async throws {
        try await perform { try GoClient.finish(WDSiteRobotUnderBodyAllowance(handle, sequence, enabled ? 1 : 0)) }
    }
    func allowUnobservedFloor(_ handle: UInt64, sequence: UInt64, enabled: Bool) async throws {
        try await perform { try GoClient.finish(WDSiteRobotAllowUnobservedFloor(handle, sequence, enabled ? 1 : 0)) }
    }
}

struct SiteRobotCameraView: View {
    let handle: UInt64
    var onFrame: @MainActor (CameraCalibrationFrame) -> Void = { _ in }
    var onDecoder: @MainActor (H264AnnexBDecoder) -> Void = { _ in }
    @State private var decoder = H264AnnexBDecoder()
    @State private var lastFrame: Date?
    @State private var error: String?

    var body: some View {
        ZStack(alignment: .topLeading) {
            Color.black
            CameraDisplayView(decoder: decoder)
            Label("Go2 camera", systemImage: "video.fill")
                .font(.caption.weight(.semibold)).foregroundStyle(.white)
                .padding(8).background(.black.opacity(0.6), in: RoundedRectangle(cornerRadius: 8)).padding(8)
            TimelineView(.periodic(from: .now, by: 1)) { timeline in
                if handle == 0 {
                    Text("Connect Go2 to view its camera.").font(.caption).foregroundStyle(.white)
                        .frame(maxWidth: .infinity, maxHeight: .infinity)
                } else if let error {
                    Text(error).font(.caption).padding().foregroundStyle(.white).background(.black.opacity(0.7))
                        .frame(maxWidth: .infinity, maxHeight: .infinity)
                } else if lastFrame == nil {
                    ProgressView("Opening Go2 camera…").tint(.white).foregroundStyle(.white)
                        .frame(maxWidth: .infinity, maxHeight: .infinity)
                } else if timeline.date.timeIntervalSince(lastFrame!) > 3 {
                    Text("Camera feed interrupted").font(.caption).padding().foregroundStyle(.white).background(.black.opacity(0.7))
                        .frame(maxWidth: .infinity, maxHeight: .infinity)
                }
            }
        }
        .clipShape(RoundedRectangle(cornerRadius: 12))
        .accessibilityLabel("Go2 front camera")
        .task(id: handle) {
            decoder.reset(); lastFrame = nil; error = nil
            guard handle != 0 else { return }
            onDecoder(decoder)
            decoder.calibrationFrames.setEnabled(true)
            defer { decoder.calibrationFrames.setEnabled(false) }
            do {
                while !Task.isCancelled {
                    let bytes = try await SiteRobotClient.shared.nextVideo(handle)
                    try Task.checkCancellation()
                    if !bytes.isEmpty {
                        decoder.feed(bytes); lastFrame = Date()
                        if let frame = decoder.calibrationFrames.latest { onFrame(frame) }
                    }
                }
            } catch {
                if !Task.isCancelled { self.error = error.localizedDescription }
            }
        }
    }
}
