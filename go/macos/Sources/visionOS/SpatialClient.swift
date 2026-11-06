import Foundation

/// Copies every ABI value before freeing C memory. Blocking Go calls run on
/// dedicated queues; a slow video read cannot delay Stop or the control lease.
enum SpatialClient {
    static let queue = DispatchQueue(label: "dev.wendy.spatial.backend", attributes: .concurrent)
    static func string(_ value: UnsafePointer<CChar>?) -> String { value.map(String.init(cString:)) ?? "" }
    static func check(_ status: WDStatus) throws {
        let message = string(status.message)
        if !message.isEmpty { throw SpatialFailure(message) }
    }
    static func finish(_ pointer: UnsafeMutablePointer<WDStatus>?) throws {
        guard let pointer else { throw SpatialFailure("No response from Wendy.") }
        defer { WDStatusFree(pointer) }
        try check(pointer.pointee)
    }
    static func cString<T>(_ text: String, _ body: (UnsafeMutablePointer<CChar>) throws -> T) rethrows -> T {
        try text.withCString { try body(UnsafeMutablePointer(mutating: $0)) }
    }
    static func work<T: Sendable>(_ operation: @escaping @Sendable () throws -> T) async throws -> T {
        // Once C starts, finish and return ownership even if the view cancels.
        try await BlockingWork.run(on: queue, unlessCancelled: false, operation)
    }
    static func initialize(directory: String) async throws {
        try await work { try cString(directory) { try finish(WDInitializeMobile($0)) } }
    }
    static func devices(cloud: Bool) async throws -> [SpatialDevice] {
        try await work {
            guard let result = cloud ? WDCloud() : WDNearby() else { throw SpatialFailure("Discovery returned no response.") }
            defer { WDDevicesFree(result) }
            try check(result.pointee.status)
            return UnsafeBufferPointer(start: result.pointee.items, count: result.pointee.count).map {
                SpatialDevice(id: string($0.id).isEmpty ? string($0.selector) : string($0.id),
                              name: string($0.name), selector: string($0.selector), lite: $0.lite != 0)
            }
        }
    }
    static func connect(_ selector: String) async throws -> UInt64 {
        try await work {
            try cString(selector) {
                guard let result = WDConnect($0, WDBeginConnection()) else { throw SpatialFailure("Connection returned no response.") }
                defer { WDConnectionFree(result) }
                try check(result.pointee.status)
                return result.pointee.session
            }
        }
    }
    static func disconnect() async { _ = try? await work { try finish(WDDisconnect()) } }
    static func signedIn() async throws -> Bool {
        try await work {
            guard let result = WDAccount() else { throw SpatialFailure("Account status unavailable.") }
            defer { WDAccountFree(result) }
            try check(result.pointee.status)
            return result.pointee.state == WD_ACCOUNT_SIGNED_IN
        }
    }
    static func login() async throws { try await work { try finish(WDLogin(0)) } }
    static func events() async throws -> [(UInt32, String)] {
        try await work {
            guard let result = WDPollEvents() else { return [] }
            defer { WDEventsFree(result) }
            return UnsafeBufferPointer(start: result.pointee.items, count: result.pointee.count).map { ($0.kind.rawValue, string($0.text)) }
        }
    }
    static func robots(session: UInt64) async throws -> [SpatialRobot] {
        try await work {
            guard let result = WDUnitreeRobots(session) else { throw SpatialFailure("No robot inventory response.") }
            defer { WDUnitreeRobotsFree(result) }
            try check(result.pointee.status)
            return UnsafeBufferPointer(start: result.pointee.items, count: result.pointee.count)
                .filter { $0.paired != 0 }.map { SpatialRobot(id: string($0.serial), name: string($0.name), connected: $0.connected != 0) }
        }
    }
    static func open(session: UInt64, serial: String) async throws -> UInt64 {
        try await SiteRobotSession.open(session: session, serial: serial)
    }
    static func close(_ handle: UInt64) async { _ = try? await work { WDSiteRobotClose(handle) } }
    static func stop(_ handle: UInt64, sequence: UInt64) async {
        _ = try? await work { WDSiteRobotStop(handle, sequence) }
    }
    static func poll(_ handle: UInt64) async throws -> SpatialReading {
        try await work {
            guard let result = WDSiteRobotPoll(handle) else { throw SpatialFailure("Robot observations unavailable.") }
            defer { WDSiteRobotStateFree(result) }
            try check(result.pointee.status)
            let s = result.pointee
            return SpatialReading(message: string(s.message), warning: string(s.control_warning),
                pose: s.pose != 0, ready: s.ready != 0, aligned: s.aligned != 0, moving: s.moving != 0,
                disconnected: s.disconnected != 0, position: SIMD3(Float(s.x), Float(s.floor_y), Float(s.z)), yaw: Float(s.yaw),
                route: UnsafeBufferPointer(start: s.route, count: s.route_count).map { SIMD3(Float($0.x), Float(s.floor_y), Float($0.z)) },
                hazards: UnsafeBufferPointer(start: s.hazards, count: s.hazard_count).map {
                    SpatialHazard(position: SIMD3(Float($0.x), Float(s.floor_y), Float($0.z)), size: Float($0.size), drop: $0.drop != 0)
                }, timing: SiteRobotTiming(s), search: RobotSearchReading(s), floorPolicy: SiteRobotFloorPolicy(s))
        }
    }
    static func align(_ handle: UInt64, sequence: UInt64, frame: String, center: SIMD3<Float>, yaw: Double) async throws {
        try await work {
            try cString(frame) { try finish(WDSiteRobotAlign(handle, sequence, $0, Double(center.x), Double(center.z), Double(center.y), yaw)) }
        }
    }
    static func destination(_ handle: UInt64, sequence: UInt64, frame: String, point: SIMD3<Float>, wearer: SIMD3<Float>, move: Bool) async throws {
        try await work {
            try cString(frame) { try finish(WDSiteRobotSummon(handle, sequence, $0, Double(point.x), Double(point.z), Double(wearer.x), Double(wearer.z), move ? 1 : 0)) }
        }
    }
    static func summary(session: UInt64) async throws -> String {
        try await work {
            guard let result = WDResources(session) else { throw SpatialFailure("Telemetry unavailable.") }
            defer { WDResourcesFree(result) }
            try check(result.pointee.status)
            let s = result.pointee
            var parts: [String] = []
            if s.cpu.available != 0 { parts.append(String(format: "CPU %.0f%%", s.cpu.value)) }
            if s.battery.available != 0 { parts.append(String(format: "Battery %.0f%%", s.battery.percent)) }
            if let thermal = s.thermals, s.thermal_count > 0 { parts.append(String(format: "%.1f °C", thermal.pointee.celsius)) }
            return parts.isEmpty ? "Connected · No resource measurements" : parts.joined(separator: " · ")
        }
    }
}
