import Foundation
import SwiftUI

/// Shared connection and supervision settings for Mac, iPhone and Vision Pro.
/// Defaults belong to the Go controller; clients do not override them on open.
enum SiteRobotSession {
    private static let queue = DispatchQueue(label: "dev.wendy.robot-session", attributes: .concurrent)

    private struct Failure: LocalizedError {
        let message: String
        var errorDescription: String? { message }
    }

    private static func check(_ status: WDStatus) throws {
        if let message = status.message {
            let text = String(cString: message)
            if !text.isEmpty { throw Failure(message: text) }
        }
    }

    static func open(session: UInt64, serial: String) async throws -> UInt64 {
        try await BlockingWork.run(on: queue, unlessCancelled: false) {
            try serial.withCString { serial in
                guard let result = WDSiteRobotOpen(session, UnsafeMutablePointer(mutating: serial)) else {
                    throw Failure(message: "Missing robot connection.")
                }
                defer { WDUnitreeConnectionFree(result) }
                try check(result.pointee.status)
                return result.pointee.handle
            }
        }
    }

    static func setSetting(_ handle: UInt64, sequence: UInt64, setting: SiteRobotSetting, enabled: Bool) async throws {
        try await BlockingWork.run(on: queue, unlessCancelled: false) {
            let value: Int32 = enabled ? 1 : 0
            switch setting {
            case .arrivalTiming: try finish(WDSiteRobotArrivalTiming(handle, sequence, value))
            case .underBodyAllowance: try finish(WDSiteRobotUnderBodyAllowance(handle, sequence, value))
            case .unobservedFloor: try finish(WDSiteRobotAllowUnobservedFloor(handle, sequence, value))
            }
        }
    }
    private static func finish(_ result: UnsafeMutablePointer<WDStatus>?) throws {
        guard let result else { throw Failure(message: "Missing robot response.") }
        defer { WDStatusFree(result) }
        try check(result.pointee)
    }
    static func look(_ handle: UInt64, sequence: UInt64, direction: Int32) async throws {
        try await BlockingWork.run(on: queue, unlessCancelled: false) {
            try finish(WDSiteRobotLook(handle, sequence, direction))
        }
    }
    static func fuse(_ handle: UInt64, sequence: UInt64, request: RobotFusionRequest) async throws -> RobotFusionReport {
        let json = String(decoding: try JSONEncoder().encode(request), as: UTF8.self)
        return try await BlockingWork.run(on: queue, unlessCancelled: false) {
            try json.withCString {
                guard let result = WDSiteRobotFuse(handle, sequence, UnsafeMutablePointer(mutating: $0)) else {
                    throw Failure(message: "Missing localization report.")
                }
                defer { WDSiteRobotFusionFree(result) }
                try check(result.pointee.status)
                guard let json = result.pointee.json else { throw Failure(message: "Missing localization report.") }
                return try JSONDecoder().decode(RobotFusionReport.self, from: Data(String(cString: json).utf8))
            }
        }
    }
    static func confirmLocalization(_ handle: UInt64, sequence: UInt64, matchSequence: UInt64, frame: String, candidate: Int) async throws {
        try await BlockingWork.run(on: queue, unlessCancelled: false) {
            try frame.withCString {
                try finish(WDSiteRobotConfirmLocalization(handle, sequence, matchSequence, UnsafeMutablePointer(mutating: $0), Int32(candidate)))
            }
        }
    }
}

struct SiteRobotTiming: Codable, Sendable, Equatable {
    var enabled: Bool
    var usingArrival: Bool
    var unavailable: Bool

    init(enabled: Bool, usingArrival: Bool, unavailable: Bool) {
        self.enabled = enabled; self.usingArrival = usingArrival; self.unavailable = unavailable
    }

    init(_ state: WDSiteRobotState) {
        self.init(enabled: state.arrival_timing != 0, usingArrival: state.using_arrival_timing != 0,
                  unavailable: state.timing_unavailable != 0)
    }

    var supervisionMessage: String {
        enabled ? "Supervised · sensor age unverified. Keep Go2 in sight." : "Keep Go2 in sight and the route clear."
    }
}

struct SiteRobotArrivalTimingControls: View {
    let timing: SiteRobotTiming?
    let setEnabled: (Bool) -> Void

    var body: some View {
        if let timing, timing.unavailable || timing.enabled {
            Toggle("Supervised arrival timing", isOn: Binding(
                get: { timing.enabled }, set: setEnabled
            )).font(.caption)
            Text(timing.enabled
                 ? "Uses live packet arrivals. Sensor capture age is unverified. Stay with Go2 and keep Stop within reach."
                 : "This firmware rounds LiDAR timestamps. Supervised mode uses packet arrival times for this connection.")
                .font(.caption2).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
        }
    }
}


enum SiteRobotSetting: Sendable {
    case arrivalTiming, underBodyAllowance, unobservedFloor
}

struct SiteRobotFloorPolicy: Codable, Sendable, Equatable {
    var underBodyAllowance: Bool
    var allowUnobservedFloor: Bool

    init(underBodyAllowance: Bool, allowUnobservedFloor: Bool) {
        self.underBodyAllowance = underBodyAllowance
        self.allowUnobservedFloor = allowUnobservedFloor
    }

    init(_ state: WDSiteRobotState) {
        self.init(underBodyAllowance: state.under_body_allowance != 0,
                  allowUnobservedFloor: state.allow_unobserved_floor != 0)
    }

    var supervisionMessage: String? {
        if allowUnobservedFloor {
            return "Supervised navigation: unobserved floor allowed. Keep Go2 in sight and Stop within reach."
        }
        if underBodyAllowance { return "Hidden-floor departure enabled. Check beneath Go2." }
        return nil
    }
}

struct SiteRobotSupervisionControls: View {
    let timing: SiteRobotTiming?
    let floorPolicy: SiteRobotFloorPolicy?
    let setSetting: (SiteRobotSetting, Bool) -> Void

    var body: some View {
        Toggle("Supervised navigation through unobserved floor", isOn: Binding(
            get: { floorPolicy?.allowUnobservedFloor ?? false },
            set: { setSetting(.unobservedFloor, $0) }
        )).font(.caption).disabled(floorPolicy == nil)
        Text("Allows unobserved floor along the route. Go2 stops and tries another route around detected obstacles or drops. Lost telemetry stops movement. Stay with Go2 and keep Stop within reach.")
            .font(.caption2).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
        Toggle("Allow hidden floor beneath Go2", isOn: Binding(
            get: { floorPolicy?.underBodyAllowance ?? false },
            set: { setSetting(.underBodyAllowance, $0) }
        )).font(.caption).disabled(floorPolicy == nil)
        Text("Allows departure when the floor beneath Go2 is hidden from its sensors. Check that it is level and clear. Detected obstacles and drops still block movement.")
            .font(.caption2).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
        SiteRobotArrivalTimingControls(timing: timing) { setSetting(.arrivalTiming, $0) }
    }
}
