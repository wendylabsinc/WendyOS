import Foundation

struct UnitreeRobot: Identifiable, Equatable {
    let serial, name, address: String
    let paired, connected: Bool
    var id: String { serial }
    static let manual = Self(serial: "", name: "Unitree Go2", address: "", paired: false, connected: false)
}
struct UnitreeSnapshot { let robots: [UnitreeRobot]; let warning: String }

final class UnitreeClient: Sendable {
    static let shared = UnitreeClient()
    private let work = DispatchQueue(label: "dev.wendy.unitree", attributes: .concurrent)
    private let motion = DispatchQueue(label: "dev.wendy.unitree.motion", qos: .userInteractive)
    private func perform<T: Sendable>(control: Bool = false, unlessCancelled: Bool = true, _ body: @escaping @Sendable () throws -> T) async throws -> T {
        // Stop/close and connection-handle cleanup must complete even when the
        // caller has been cancelled. Connection attempts cancel through Go.
        try await BlockingWork.run(on: control ? motion : work, unlessCancelled: unlessCancelled, body)
    }
    func robots(session: UInt64 = 0) async throws -> UnitreeSnapshot {
        try await perform {
            guard let p = WDUnitreeRobots(session) else { throw WendyError(message: "Missing Unitree discovery result.") }
            defer { WDUnitreeRobotsFree(p) }
            try GoClient.check(p.pointee.status)
            return UnitreeSnapshot(robots: UnsafeBufferPointer(start: p.pointee.items, count: p.pointee.count).map {
                UnitreeRobot(serial: GoClient.string($0.serial), name: GoClient.string($0.name), address: GoClient.string($0.address), paired: $0.paired != 0, connected: $0.connected != 0)
            }, warning: GoClient.string(p.pointee.warning))
        }
    }
    // Temporary C strings are freed on all paths and never appear in results.
    func connect(session: UInt64, serial: String, name: String, address: String, region: String, email: String, password: String) async throws -> UInt64 {
        let attempt = WDUnitreeAttemptCreate()
        defer { WDUnitreeAttemptFree(attempt) }
        return try await withTaskCancellationHandler {
            try Task.checkCancellation()
            return try await perform(unlessCancelled: false) {
                let values = [serial, name, address, region, email, password].map { strdup($0)! }
                defer { values.forEach { free($0) } }
                if session != 0 {
                    try GoClient.finish(WDUnitreePair(attempt, session, values[0], values[1], values[2], values[3], values[4], values[5]))
                    return 0
                }
                guard let p = WDUnitreeConnect(attempt, values[0], values[2], values[3], values[4], values[5]) else { throw WendyError(message: "Missing Unitree connection result.") }
                defer { WDUnitreeConnectionFree(p) }
                try GoClient.check(p.pointee.status)
                return p.pointee.handle
            }
        } onCancel: {
            WDUnitreeAttemptCancel(attempt)
        }
    }
    func startVideo(handle: UInt64) async throws {
        try await perform { try GoClient.finish(WDUnitreeVideoStart(handle)) }
    }
    func nextVideo(handle: UInt64) async throws -> Data {
        try await perform {
            guard let p = WDUnitreeVideoNext(handle) else { throw WendyError(message: "Missing Go2 camera frame.") }
            defer { WDUnitreeVideoFrameFree(p) }
            try GoClient.check(p.pointee.status)
            guard let bytes = p.pointee.bytes, p.pointee.count > 0 else { return Data() }
            return Data(bytes: bytes, count: p.pointee.count)
        }
    }
    func stopVideo(handle: UInt64) async {
        try? await perform(unlessCancelled: false) { WDUnitreeVideoStop(handle) }
    }
    func forget(session: UInt64, serial: String) async throws {
        try await perform { try GoClient.withCString(serial) { try GoClient.finish(WDUnitreeForget(session, $0)) } }
    }
    func move(handle: UInt64, forward: Double = 0, lateral: Double = 0, yaw: Double = 0) async throws {
        try await perform(control: true, unlessCancelled: forward != 0 || lateral != 0 || yaw != 0) { try GoClient.finish(WDUnitreeMove(handle, forward, lateral, yaw)) }
    }
    func close(handle: UInt64) async throws {
        try await perform(control: true, unlessCancelled: false) { try GoClient.finish(WDUnitreeClose(handle)) }
    }
}
