import Foundation
@preconcurrency import SceneKit

enum SiteRobotGeometry {
    struct ChairFootprint: Sendable {
        var minX, maxX, minZ, maxZ: Double
        func contains(x: Double, z: Double) -> Bool {
            x >= minX && x <= maxX && z >= minZ && z <= maxZ
        }
    }

    private static func isChair(_ node: SCNNode) -> Bool {
        var ancestor: SCNNode? = node
        while let current = ancestor {
            if current.name?.hasPrefix("Chair") == true { return true }
            ancestor = current.parent
        }
        return false
    }

    static func chairFootprints(_ url: URL, scale: Double) throws -> [ChairFootprint] {
        guard scale.isFinite, scale > 0 else { throw WendyError(message: "Invalid room scale.") }
        let scene = try SCNScene(url: url, options: [.checkConsistency: true, .convertToYUp: true, .convertUnitsToMeters: 1.0])
        scene.rootNode.scale = SCNVector3(scale, scale, scale)
        var result: [ChairFootprint] = []
        scene.rootNode.enumerateChildNodes { node, _ in
            guard node.geometry != nil, isChair(node) else { return }
            let (low, high) = node.boundingBox
            var points: [SCNVector3] = []
            for x in [low.x, high.x] {
                for y in [low.y, high.y] {
                    for z in [low.z, high.z] { points.append(node.convertPosition(SCNVector3(x, y, z), to: nil)) }
                }
            }
            // The seat box does not bound the backrest. Exclude references in
            // its horizontal footprint at every height, with 15 cm for overhang.
            result.append(ChairFootprint(minX: points.map { Double($0.x) }.min()! - 0.15,
                                         maxX: points.map { Double($0.x) }.max()! + 0.15,
                                         minZ: points.map { Double($0.z) }.min()! - 0.15,
                                         maxZ: points.map { Double($0.z) }.max()! + 0.15))
        }
        return result
    }

    static func load(_ url: URL, scale: Double) async throws -> [Double] {
        try await BlockingWork.run(on: .global(qos: .userInitiated)) {
            try triangles(url, scale: scale)
        }
    }

    static func semantic(_ url: URL, scale: Double) async throws -> SiteRobotGeometryPacket {
        try await BlockingWork.run(on: .global(qos: .userInitiated)) {
            SiteRobotGeometryPacket(architecture: try triangles(url, scale: scale), furniture: try triangles(url, scale: scale, furnitureOnly: true))
        }
    }

    static func triangles(_ url: URL, scale: Double, furnitureOnly: Bool = false) throws -> [Double] {
        guard scale.isFinite, scale > 0 else { throw WendyError(message: "Invalid room scale.") }
        let scene = try SCNScene(url: url, options: [.checkConsistency: true, .convertToYUp: true, .convertUnitsToMeters: 1.0])
        scene.rootNode.scale = SCNVector3(scale, scale, scale)
        var result: [Double] = []
        var failure: Error?
        scene.rootNode.enumerateChildNodes { node, stop in
            do {
                guard let geometry = node.geometry else { return }
                // RoomPlan exports movable objects separately from architecture.
                // Keep these separate so the matcher can give them weaker weight.
                var isFurniture = false
                var ancestor: SCNNode? = node
                while let current = ancestor {
                    if current.name == "Object_grp" { isFurniture = true }
                    ancestor = current.parent
                }
                guard isFurniture == furnitureOnly else { return }
                // A RoomPlan chair box may describe only the seat. Its omitted
                // back, open legs and movable position make it an unreliable
                // height/direction target. This affects localization only;
                // navigation obstacles still come from the live LiDAR grid.
                if isFurniture && isChair(node) { return }
                // Doors can move as well; retain the surrounding wall/opening.
                if node.name?.hasPrefix("Door") == true { return }
                guard let source = geometry.sources(for: .vertex).first,
                      source.usesFloatComponents, source.componentsPerVector >= 3,
                      [4, 8].contains(source.bytesPerComponent), source.vectorCount <= 1_000_000,
                      source.dataOffset >= 0, source.dataStride >= source.bytesPerComponent*3 else {
                    throw WendyError(message: "This room uses unsupported vertex data.")
                }
                let positions = try source.data.withUnsafeBytes { bytes -> [SCNVector3] in
                    try (0..<source.vectorCount).map { i in
                        let offset = source.dataOffset + i*source.dataStride
                        guard offset <= bytes.count-source.bytesPerComponent*3 else { throw WendyError(message: "Incomplete room vertices.") }
                        func component(_ axis: Int) -> Double {
                            let at = offset+axis*source.bytesPerComponent
                            return source.bytesPerComponent == 4 ? Double(bytes.loadUnaligned(fromByteOffset: at, as: Float.self)) : bytes.loadUnaligned(fromByteOffset: at, as: Double.self)
                        }
                        return node.convertPosition(SCNVector3(component(0), component(1), component(2)), to: nil)
                    }
                }
                for element in geometry.elements {
                    guard element.primitiveType == .triangles else { continue }
                    guard [1,2,4].contains(element.bytesPerIndex), element.primitiveCount <= 100_000,
                          result.count/9+element.primitiveCount <= 100_000,
                          element.data.count >= element.primitiveCount*3*element.bytesPerIndex else {
                        throw WendyError(message: "This room mesh is too complex for local scan matching.")
                    }
                    try element.data.withUnsafeBytes { bytes in
                        for i in 0..<(element.primitiveCount*3) {
                            let offset = i*element.bytesPerIndex
                            let index: Int
                            switch element.bytesPerIndex {
                            case 1: index = Int(bytes.loadUnaligned(fromByteOffset: offset, as: UInt8.self))
                            case 2: index = Int(bytes.loadUnaligned(fromByteOffset: offset, as: UInt16.self))
                            default: index = Int(bytes.loadUnaligned(fromByteOffset: offset, as: UInt32.self))
                            }
                            guard positions.indices.contains(index) else { throw WendyError(message: "Invalid room triangle.") }
                            let p = positions[index]
                            result.append(contentsOf: [Double(p.x), Double(p.y), Double(p.z)])
                        }
                    }
                }
            } catch { failure = error; stop.pointee = true }
        }
        if let failure { throw failure }
        guard !result.isEmpty || furnitureOnly else { throw WendyError(message: "The room has no triangle geometry to match.") }
        return result
    }
}
