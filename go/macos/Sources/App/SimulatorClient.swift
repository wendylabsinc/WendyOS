#if os(macOS)
import Foundation
import simd

struct SimulatorOperationError: LocalizedError {
    let message: String
    let recovery: WDSimulatorRecovery
    var errorDescription: String? { message }
}

struct LocalSimulator: Identifiable, Equatable {
    let name: String
    let profile: String
    let state: String
    let address: String
    let version: String
    let error: String
    let diskBytes: Int64
    var id: String { name }
    var running: Bool { state == "running" }
    var robot: Bool { ["go2", "g1", "so101", "rosmaster-r2"].contains(profile) }
    var lite: Bool { profile.hasPrefix("wendy-lite") }
    var title: String { SimulatorProfile.all.first { $0.id == profile }?.title ?? profile }
}
struct SimulatorProfile: Identifiable {
    let id: String
    let title: String
    static let all = [
        Self(id: "generic", title: "WendyOS"),
        Self(id: "go2", title: "Unitree Go2"),
        Self(id: "g1", title: "Unitree G1"),
        Self(id: "so101", title: "SO-101 arm"),
        Self(id: "rosmaster-r2", title: "Rosmaster R2"),
        Self(id: "wendy-lite", title: "ESP32-C6 · WASM"),
        Self(id: "wendy-lite-native", title: "ESP32-C6 · Native")
    ]
}
struct SimulatorMesh {
    let id: Int
    let positions: [Float]
    let indices: [UInt32]
}
struct SimulatorGeometry {
    let body: Int
    let mesh: Int
    let shape: WDSimulatorShape
    let size: SIMD3<Float>
    let position: SIMD3<Float>
    let orientation: simd_quatf
    let color: SIMD4<Float>
}
struct SimulatorScene {
    let id: String
    let robotBody: Int
    let bodyCount: Int
    let geometries: [SimulatorGeometry]
    let meshes: [SimulatorMesh]
}
struct SimulatorPose {
    let sceneID: String
    let mode: String
    let time: Double
    let epoch: UInt64
    let generation: UInt64
    let valid: Bool
    let positions: [SIMD3<Float>]
    let orientations: [simd_quatf]
}

// Separate queues keep downloads and boots from blocking device UI or rendering.
final class SimulatorClient: Sendable {
    static let shared = SimulatorClient()
    private let lifecycle = DispatchQueue(label: "dev.wendy.simulator.lifecycle", qos: .userInitiated)
    private let reads = DispatchQueue(label: "dev.wendy.simulator.reads", qos: .userInitiated, attributes: .concurrent)
    private func perform<T: Sendable>(mutation: Bool = false, _ work: @escaping @Sendable () throws -> T) async throws -> T {
        try await BlockingWork.run(on: mutation ? lifecycle : reads, work)
    }
    func list() async throws -> [LocalSimulator] {
        try await perform {
            guard let p = WDSimulators() else { throw WendyError(message: "Missing simulator list.") }
            defer { WDSimulatorsFree(p) }
            try GoClient.check(p.pointee.status)
            return UnsafeBufferPointer(start: p.pointee.items, count: p.pointee.count).map {
                LocalSimulator(name: GoClient.string($0.name), profile: GoClient.string($0.profile), state: GoClient.string($0.state), address: GoClient.string($0.address), version: GoClient.string($0.version), error: GoClient.string($0.error), diskBytes: $0.disk_bytes)
            }
        }
    }
    func create(name: String, profile: String, image: String, disk: Int) async throws {
        try await perform(mutation: true) {
            try GoClient.withCString(name) { n in
                try GoClient.withCString(profile) { p in
                    try GoClient.withCString(image) { i in try GoClient.finish(WDSimulatorCreate(n, p, i, Int32(disk))) }
                }
            }
        }
    }
    func act(name: String, action: WDSimulatorAction) async throws {
        try await perform(mutation: true) {
            try GoClient.withCString(name) {
                guard let p = WDSimulatorPerform($0, action) else { throw WendyError(message: "Missing simulator result.") }
                defer { WDSimulatorOperationFree(p) }
                if let message = p.pointee.status.message {
                    throw SimulatorOperationError(message: String(cString: message), recovery: p.pointee.recovery)
                }
            }
        }
    }
    func scene(name: String) async throws -> SimulatorScene {
        try await perform {
            try GoClient.withCString(name) {
                guard let p = WDSimulatorScene($0) else { throw WendyError(message: "Missing simulation scene.") }
                defer { WDSimulatorSceneFree(p) }
                let v = p.pointee
                try GoClient.check(v.status)
                let geometries = UnsafeBufferPointer(start: v.geoms, count: v.geom_count).map { g in
                    SimulatorGeometry(body: Int(g.body), mesh: Int(g.mesh), shape: g.shape,
                        size: SIMD3(g.size.0, g.size.1, g.size.2), position: SIMD3(g.position.0, g.position.1, g.position.2),
                        orientation: simd_quatf(vector: SIMD4(g.quaternion.0, g.quaternion.1, g.quaternion.2, g.quaternion.3)),
                        color: SIMD4(g.rgba.0, g.rgba.1, g.rgba.2, g.rgba.3))
                }
                let meshes = UnsafeBufferPointer(start: v.meshes, count: v.mesh_count).map {
                    SimulatorMesh(id: Int($0.id), positions: Array(UnsafeBufferPointer(start: $0.positions, count: $0.position_count)), indices: Array(UnsafeBufferPointer(start: $0.indices, count: $0.index_count)))
                }
                return SimulatorScene(id: GoClient.string(v.id), robotBody: Int(v.robot_body), bodyCount: v.body_count, geometries: geometries, meshes: meshes)
            }
        }
    }
    func pose(name: String) async throws -> SimulatorPose {
        try await perform {
            try GoClient.withCString(name) {
                guard let p = WDSimulatorPose($0) else { throw WendyError(message: "Missing simulation pose.") }
                defer { WDSimulatorPoseFree(p) }
                let v = p.pointee
                try GoClient.check(v.status)
                let positions = UnsafeBufferPointer(start: v.positions, count: v.body_count * 3)
                let rotations = UnsafeBufferPointer(start: v.quaternions, count: v.body_count * 4)
                return SimulatorPose(sceneID: GoClient.string(v.scene_id), mode: GoClient.string(v.mode), time: v.time,
                    epoch: v.epoch, generation: v.generation, valid: v.valid != 0,
                    positions: (0..<v.body_count).map { SIMD3(positions[$0*3], positions[$0*3+1], positions[$0*3+2]) },
                    orientations: (0..<v.body_count).map { simd_quatf(vector: SIMD4(rotations[$0*4], rotations[$0*4+1], rotations[$0*4+2], rotations[$0*4+3])) })
            }
        }
    }
}

@MainActor final class SimulatorModel: ObservableObject {
    @Published var items: [LocalSimulator] = []
    @Published var selection: String?
    @Published var error: String?
    @Published var operation: String?
    @Published private(set) var preparingSimulator: String?
    @Published var refreshing = false
    @Published var recovery: WDSimulatorRecovery = WDSimRecoveryNone
    @Published var recoveryTarget: LocalSimulator?
    private let client = SimulatorClient.shared
    var selected: LocalSimulator? { items.first { $0.id == selection } }
    func refresh() async {
        guard !refreshing else { return }
        refreshing = true
        defer { refreshing = false }
        do {
            items = try await client.list()
            if !items.contains(where: { $0.id == selection }) { selection = items.first?.id }
        } catch { self.error = error.localizedDescription }
    }
    func act(_ item: LocalSimulator, _ action: WDSimulatorAction, label: String) async {
        guard operation == nil else { return }
        if action == WDSimStart || action == WDSimUpdateRuntime || action == WDSimUpdateAgent {
            preparingSimulator = item.name
        }
        operation = "\(label) \(item.name)…"; error = nil; recovery = WDSimRecoveryNone; recoveryTarget = nil
        defer { operation = nil; preparingSimulator = nil }
        do { try await client.act(name: item.name, action: action) }
        catch {
            self.error = error.localizedDescription
            if let failure = error as? SimulatorOperationError {
                recovery = failure.recovery; recoveryTarget = item
            }
        }
        await refresh()
    }
    func create(name: String, profile: String, image: String, disk: Int) async {
        guard operation == nil else { return }
        operation = "Creating \(name)…"; error = nil; recovery = WDSimRecoveryNone; recoveryTarget = nil
        defer { operation = nil }
        do { try await client.create(name: name, profile: profile, image: image, disk: disk); selection = name }
        catch { self.error = error.localizedDescription }
        await refresh()
    }
}
#endif
