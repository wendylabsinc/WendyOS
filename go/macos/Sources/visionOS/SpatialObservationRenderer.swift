import RealityKit
import UIKit
import simd

/// Keep meshes and the robot hierarchy alive between telemetry updates. Only
/// transforms and visibility change at the 10 Hz observation rate.
@MainActor
final class SpatialObservationRenderer {
    let root = Entity()
    private var robot: Entity?
    private var detailedRobot = false
    private var routes: [ModelEntity] = []
    private var hazards: [ModelEntity] = []
    private let unitBox = MeshResource.generateBox(size: 1)
    private let routeMaterial = SimpleMaterial(color: UIColor(red: 0.38, green: 0.87, blue: 0.92, alpha: 0.85), isMetallic: false)
    private let obstacleMaterial = SimpleMaterial(color: UIColor.orange.withAlphaComponent(0.45), isMetallic: false)
    private let dropMaterial = SimpleMaterial(color: UIColor.red.withAlphaComponent(0.55), isMetallic: false)

    func update(_ reading: SpatialReading, showRobot: Bool = true) {
        root.removeFromParent()
        root.transform = .identity
        root.isEnabled = reading.pose && reading.aligned
        guard root.isEnabled else { return }
        if showRobot {
            if robot == nil || (!detailedRobot && SpatialAssets.robot != nil) {
                robot?.removeFromParent()
                robot = SpatialDrawing.robot(at: .zero, yaw: 0)
                detailedRobot = SpatialAssets.robot != nil
                root.addChild(robot!)
            }
            robot?.position = reading.position
            robot?.orientation = simd_quatf(angle: reading.yaw, axis: [0, 1, 0])
        }
        robot?.isEnabled = showRobot
        let height = SIMD3<Float>(0, 0.025, 0)
        let segments = min(max(0, reading.route.count - 1), 512)
        while routes.count < segments {
            let node = ModelEntity(mesh: unitBox, materials: [routeMaterial])
            routes.append(node); root.addChild(node)
        }
        for (index, node) in routes.enumerated() {
            node.isEnabled = index < segments
            guard node.isEnabled else { continue }
            let start = reading.route[index] + height, end = reading.route[index + 1] + height
            let length = simd_distance(start, end)
            node.isEnabled = length.isFinite && length > 0.001
            if node.isEnabled {
                node.scale = [0.025, 0.025, length]
                node.look(at: end, from: (start + end) / 2, relativeTo: nil)
            }
        }
        let count = min(reading.hazards.count, 256)
        while hazards.count < count {
            let node = ModelEntity(mesh: unitBox, materials: [obstacleMaterial])
            hazards.append(node); root.addChild(node)
        }
        for (index, node) in hazards.enumerated() {
            node.isEnabled = index < count
            guard node.isEnabled else { continue }
            let hazard = reading.hazards[index]
            node.isEnabled = hazard.size.isFinite && hazard.size > 0 && SpatialGeometry.finite(hazard.position)
            if node.isEnabled {
                node.position = hazard.position + height
                node.scale = [hazard.size, 0.035, hazard.size]
                node.model?.materials = [hazard.drop ? dropMaterial : obstacleMaterial]
            }
        }
    }
}
