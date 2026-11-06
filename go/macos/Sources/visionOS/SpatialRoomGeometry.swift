import Foundation
import simd

struct SpatialRoomPlane {
    let floor: Bool
    // Mesh vertices transformed from anchor space into world space.
    let vertices: [SIMD3<Float>]
    let indices: [UInt32]
}

/// The visible floor target and localization both use ARKit's measured mesh.
/// There is no assumed local plane axis or second extent transform.
enum SpatialRoomGeometry {
    static func geometry(_ planes: [SpatialRoomPlane], frameFromWorld: simd_float4x4) -> SiteRobotGeometryPacket? {
        guard planes.count <= 256 else { return nil }
        var vertices: [Double] = []
        var floorArea: Float = 0
        var walls = 0
        for plane in planes {
            guard !plane.indices.isEmpty, plane.indices.count % 3 == 0,
                  plane.indices.allSatisfy({ Int($0) < plane.vertices.count }) else { continue }
            let points = plane.vertices.map { point in
                let p = frameFromWorld * SIMD4(point, 1)
                return SIMD3(p.x, p.y, p.z)
            }
            guard points.allSatisfy(SpatialGeometry.finite) else { continue }
            var included = false
            for index in stride(from: 0, to: plane.indices.count, by: 3) {
                let triangle = (0..<3).map { points[Int(plane.indices[index + $0])] }
                let normal = simd_cross(triangle[1] - triangle[0], triangle[2] - triangle[0])
                let length = simd_length(normal)
                guard length > 0.000001 else { continue }
                let up = abs(normal.y) / length
                guard plane.floor ? up > 0.98 : up < 0.2 else { continue }
                if plane.floor { floorArea += length / 2 }
                included = true
                guard vertices.count + 9 <= 90_000 else { return nil }
                for point in triangle {
                    vertices.append(contentsOf: [Double(point.x), Double(point.y), Double(point.z)])
                }
            }
            if !plane.floor && included { walls += 1 }
        }
        guard floorArea >= 1, walls >= 2 else { return nil }
        return SiteRobotGeometryPacket(architecture: vertices, furniture: [])
    }

    static func transformed(_ packet: SiteRobotGeometryPacket, by matrix: simd_float4x4) -> SiteRobotGeometryPacket? {
        func convert(_ values: [Double]) -> [Double]? {
            guard values.count % 9 == 0, values.count <= 90_000 else { return nil }
            var result: [Double] = []
            result.reserveCapacity(values.count)
            for index in stride(from: 0, to: values.count, by: 3) {
                let p = matrix * SIMD4(Float(values[index]), Float(values[index + 1]), Float(values[index + 2]), 1)
                guard SpatialGeometry.finite(SIMD3(p.x, p.y, p.z)) else { return nil }
                result.append(contentsOf: [Double(p.x), Double(p.y), Double(p.z)])
            }
            return result
        }
        guard let architecture = convert(packet.architecture), let furniture = convert(packet.furniture) else { return nil }
        return SiteRobotGeometryPacket(architecture: architecture, furniture: furniture)
    }

    /// Keep one failed room packet locally so a device-only geometry failure
    /// can be replayed without recording camera images or robot credentials.
    static func recordFailure(_ packet: SiteRobotGeometryPacket, message: String) {
        struct Failure: Encodable {
            let at: Date
            let message: String
            let architecture: [Double]
        }
        guard let cache = FileManager.default.urls(for: .cachesDirectory, in: .userDomainMask).first else { return }
        let folder = cache.appendingPathComponent("Wendy", isDirectory: true)
        do {
            try FileManager.default.createDirectory(at: folder, withIntermediateDirectories: true)
            let data = try JSONEncoder().encode(Failure(at: .now, message: message, architecture: packet.architecture))
            try data.write(to: folder.appendingPathComponent("vision-room-geometry.json"), options: .atomic)
        } catch { /* Diagnostics must never interrupt Stop or change matching. */ }
    }

}
