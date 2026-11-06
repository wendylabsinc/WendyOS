import ARKit
import UIKit
import Foundation
import Observation
import RealityKit
import simd

struct SpatialMeshChunk: Sendable {
    let vertices: [SIMD3<Float>]
    let indices: [UInt32]
    var triangles: Int { indices.count / 3 }

    func validate() throws {
        guard !vertices.isEmpty, vertices.count <= 150_000, !indices.isEmpty,
              indices.count % 3 == 0, indices.count <= 450_000,
              vertices.allSatisfy({ SpatialGeometry.finite($0) && simd_length($0) < 1000 }),
              indices.allSatisfy({ Int($0) < vertices.count }) else {
            throw SpatialFailure("The room mesh contains invalid or oversized geometry.")
        }
    }
    static func capture(_ anchor: MeshAnchor, frameFromWorld: simd_float4x4) throws -> Self {
        try capture(source: anchor.geometry.vertices, faces: anchor.geometry.faces,
                    transform: frameFromWorld * anchor.originFromAnchorTransform)
    }
    static func capturePlane(_ anchor: PlaneAnchor) throws -> Self {
        try capture(source: anchor.geometry.meshVertices, faces: anchor.geometry.meshFaces,
                    transform: anchor.originFromAnchorTransform)
    }
    private static func capture(source: GeometrySource, faces: GeometryElement, transform: simd_float4x4) throws -> Self {
        guard source.format == .float3, source.count > 0, source.count <= 150_000,
              source.offset >= 0, source.stride >= 12,
              source.offset + (source.count - 1) * source.stride + 12 <= source.buffer.length,
              faces.primitive == .triangle, faces.count > 0, faces.count <= 150_000,
              [2, 4].contains(faces.bytesPerIndex), faces.count * 3 * faces.bytesPerIndex <= faces.buffer.length else {
            throw SpatialFailure("Unsupported room mesh format.")
        }
        let base = source.buffer.contents()
        let vertices = (0..<source.count).map { index -> SIMD3<Float> in
            let p = base.advanced(by: source.offset + index * source.stride)
            let world = transform * SIMD4<Float>(p.loadUnaligned(as: Float.self),
                p.advanced(by: 4).loadUnaligned(as: Float.self), p.advanced(by: 8).loadUnaligned(as: Float.self), 1)
            return SIMD3(world.x, world.y, world.z)
        }
        let buffer = faces.buffer.contents()
        let indices = (0..<(faces.count * 3)).map { index -> UInt32 in
            let p = buffer.advanced(by: index * faces.bytesPerIndex)
            return faces.bytesPerIndex == 2 ? UInt32(p.loadUnaligned(as: UInt16.self)) : p.loadUnaligned(as: UInt32.self)
        }
        let chunk = Self(vertices: vertices, indices: indices)
        try chunk.validate()
        return chunk
    }
    static func floor(_ anchor: MeshAnchor) throws -> Self? {
        guard let classes = anchor.geometry.classifications,
              classes.format == .uchar, classes.count == anchor.geometry.faces.count,
              classes.count > 0, classes.offset >= 0, classes.stride > 0,
              classes.offset + (classes.count - 1) * classes.stride < classes.buffer.length else { return nil }
        let all = try capture(anchor, frameFromWorld: matrix_identity_float4x4)
        let buffer = classes.buffer.contents()
        let floorFaces = (0..<classes.count).map { index in
            Int(buffer.load(fromByteOffset: classes.offset + index * classes.stride, as: UInt8.self)) == SurfaceClassification.floor.rawValue
        }
        return floorFacesOnly(all, classifiedFloor: floorFaces)
    }

    static func floorFacesOnly(_ chunk: Self, classifiedFloor: [Bool]) -> Self? {
        guard classifiedFloor.count == chunk.triangles, (try? chunk.validate()) != nil else { return nil }
        var indices: [UInt32] = []
        for face in classifiedFloor.indices where classifiedFloor[face] {
            let triangle = Array(chunk.indices[(face * 3)..<(face * 3 + 3)])
            let a = chunk.vertices[Int(triangle[0])], b = chunk.vertices[Int(triangle[1])], c = chunk.vertices[Int(triangle[2])]
            let normal = simd_cross(b - a, c - a), length = simd_length(normal)
            guard length > 0.000001, abs(normal.y) / length > 0.98 else { continue }
            indices.append(contentsOf: triangle)
        }
        guard !indices.isEmpty else { return nil }
        return Self(vertices: chunk.vertices, indices: indices)
    }

    @MainActor func entity(color: UIColor) async throws -> ModelEntity {
        try validate()
        var mesh = MeshDescriptor(name: "Room surface")
        mesh.positions = MeshBuffer(vertices)
        mesh.primitives = .triangles(indices)
        let resource = try await MeshResource(from: [mesh])
        return ModelEntity(mesh: resource, materials: [SimpleMaterial(color: color, isMetallic: false)])
    }
}

@MainActor @Observable
final class SpatialRoomScan {
    let preview = Entity()
    var capturing = false
    var saving = false
    var frame: String?
    var interrupted = false
    var message = "Look around to capture walls, floor and furniture. Geometry only; no camera photos."
    private(set) var chunks: [UUID: SpatialMeshChunk] = [:]
    private var entities: [UUID: Entity] = [:]
    private var generation = 0
    var triangleCount: Int { chunks.values.reduce(0) { $0 + $1.triangles } }
    var canSave: Bool { !saving && !chunks.isEmpty }

    func start(frame: String) {
        guard !saving else { return }
        generation += 1; chunks = [:]; entities = [:]; preview.children.removeAll()
        self.frame = frame; capturing = true; interrupted = false; preview.isEnabled = true
        message = "Look around slowly. Cyan surfaces show what has been captured."
    }
    func pause() { capturing = false; preview.isEnabled = false }
    func interrupt() {
        if !chunks.isEmpty || capturing {
            pause(); interrupted = true
            message = "Tracking stopped. You can save this partial scan or start a new one."
        }
    }
    func remove(_ id: UUID) {
        guard capturing else { return }
        chunks.removeValue(forKey: id); entities.removeValue(forKey: id)?.removeFromParent()
    }
    func receive(_ anchor: MeshAnchor, frameFromWorld: simd_float4x4) async {
        guard capturing else { return }
        let ticket = generation
        do {
            let chunk = try SpatialMeshChunk.capture(anchor, frameFromWorld: frameFromWorld)
            guard triangleCount - (chunks[anchor.id]?.triangles ?? 0) + chunk.triangles <= 300_000,
                  chunks.count < 1024 || chunks[anchor.id] != nil else {
                pause(); message = "Scan limit reached. Save this room before starting another."; return
            }
            let entity = try await chunk.entity(color: .systemTeal.withAlphaComponent(0.16))
            guard capturing, generation == ticket else { return }
            chunks[anchor.id] = chunk
            entities.removeValue(forKey: anchor.id)?.removeFromParent()
            entities[anchor.id] = entity; preview.addChild(entity)
        } catch { message = error.localizedDescription }
    }
    func save(name: String) async throws -> (SpatialSite, URL) {
        guard canSave else { throw SpatialFailure("Capture some room surfaces first.") }
        pause(); saving = true
        defer { saving = false }
        let captured = Array(chunks.values)
        let folder = try SpatialSite.importFolder()
        let url = folder.appendingPathComponent(UUID().uuidString).appendingPathExtension("usdz")
        try await SpatialClient.work { try SpatialUSDZ.data(chunks: captured).write(to: url, options: .atomic) }
        let title = String(name.trimmingCharacters(in: .whitespacesAndNewlines).prefix(200))
        let site = SpatialSite(name: title.isEmpty ? "Vision Pro room" : title, modelURL: url, metresPerUnit: 1, cameras: [])
        let archive = try await SpatialClient.work { try site.writeArchive() }
        message = "Room saved. Unseen or occluded surfaces may be missing."
        return (site, archive)
    }
}
