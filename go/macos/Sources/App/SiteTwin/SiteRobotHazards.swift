@preconcurrency import SceneKit
import SwiftUI

struct SiteRobotHazard: Equatable, Sendable {
    var position: SitePoint
    var size, yaw: Double
    var drop: Bool

    var corners: [SitePoint] {
        let half = size / 2
        let c = cos(yaw), s = sin(yaw)
        return [(-half, -half), (half, -half), (half, half), (-half, half)].map { x, z in
            SitePoint(x: position.x + c*x + s*z, y: position.y + 0.035,
                      z: position.z - s*x + c*z)
        }
    }
}

@MainActor
enum SiteRobotHazardRenderer {
    // Two batched meshes keep dense scans from creating thousands of nodes.
    static func update(_ root: SCNNode, hazards: [SiteRobotHazard]) {
        root.childNodes.forEach { $0.removeFromParentNode() }
        for drop in [false, true] {
            var vertices: [SCNVector3] = []
            var indices: [UInt32] = []
            for hazard in hazards where hazard.drop == drop {
                guard hazard.position.isValid, hazard.size.isFinite, hazard.size > 0,
                      hazard.yaw.isFinite else { continue }
                let start = UInt32(vertices.count)
                vertices += hazard.corners.map { SCNVector3($0.x, $0.y, $0.z) }
                indices += [start, start+2, start+1, start, start+3, start+2]
            }
            guard !vertices.isEmpty else { continue }
            let geometry = SCNGeometry(sources: [SCNGeometrySource(vertices: vertices)],
                                       elements: [SCNGeometryElement(indices: indices, primitiveType: .triangles)])
            let material = SCNMaterial()
            #if os(iOS)
            let color: UIColor = drop ? .systemOrange : .systemRed
            #else
            let color: NSColor = drop ? .systemOrange : .systemRed
            #endif
            material.diffuse.contents = color
            material.lightingModel = .constant
            material.isDoubleSided = true
            material.transparency = 0.8
            material.writesToDepthBuffer = false
            // This is a floor projection, not measured object geometry. Keep
            // it visible when the imported scan covers that floor location.
            material.readsFromDepthBuffer = false
            geometry.materials = [material]
            let node = SCNNode(geometry: geometry)
            node.name = drop ? "navigation:drops" : "navigation:obstacles"
            // Same noninteractive category as the camera overlays.
            node.categoryBitMask = 1 << 30
            node.renderingOrder = 100
            root.addChildNode(node)
        }
    }
}
