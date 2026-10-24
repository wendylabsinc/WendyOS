#if os(macOS)
import SwiftUI
import SceneKit
import simd

// Uses the sandbox app's SceneKit approach: compiled MuJoCo meshes, body poses,
// an explicit Z-up conversion and a renderer whose lifetime follows the pane.
enum SimulatorGeometryBuilder {
    static func mesh(_ mesh: SimulatorMesh) -> SCNGeometry {
        let vertices = stride(from: 0, to: mesh.positions.count, by: 3).map {
            SIMD3(mesh.positions[$0], mesh.positions[$0+1], mesh.positions[$0+2])
        }
        var normals = [SIMD3<Float>](repeating: .zero, count: vertices.count)
        for i in stride(from: 0, to: mesh.indices.count, by: 3) {
            let a = Int(mesh.indices[i]), b = Int(mesh.indices[i+1]), c = Int(mesh.indices[i+2])
            let normal = simd_cross(vertices[b] - vertices[a], vertices[c] - vertices[a])
            normals[a] += normal; normals[b] += normal; normals[c] += normal
        }
        let sources = [SCNGeometrySource(vertices: vertices.map { SCNVector3($0) }),
            SCNGeometrySource(normals: normals.map { SCNVector3(simd_length_squared($0) > 1e-12 ? simd_normalize($0) : SIMD3(0, 0, 1)) })]
        return SCNGeometry(sources: sources, elements: [SCNGeometryElement(indices: mesh.indices, primitiveType: .triangles)])
    }
    static func node(_ geom: SimulatorGeometry, meshes: [Int: SCNGeometry]) -> SCNNode {
        let node = SCNNode(), shape = SCNNode()
        let x = CGFloat(geom.size.x), y = CGFloat(geom.size.y), z = CGFloat(geom.size.z)
        switch geom.shape {
        case WDSimPlane: shape.geometry = SCNPlane(width: x > 0 ? 2*x : 80, height: y > 0 ? 2*y : 80)
        case WDSimBox: shape.geometry = SCNBox(width: 2*x, height: 2*y, length: 2*z, chamferRadius: 0)
        case WDSimSphere: shape.geometry = SCNSphere(radius: x)
        case WDSimEllipsoid: shape.geometry = SCNSphere(radius: 1); shape.simdScale = geom.size
        case WDSimCylinder: shape.geometry = SCNCylinder(radius: x, height: 2*y); shape.eulerAngles.x = .pi/2
        case WDSimCapsule: shape.geometry = SCNCapsule(capRadius: x, height: 2*(y+x)); shape.eulerAngles.x = .pi/2
        case WDSimMesh: shape.geometry = meshes[geom.mesh]?.copy() as? SCNGeometry
        default: break // The Go boundary rejects unsupported geometry.
        }
        let material = SCNMaterial()
        material.lightingModel = .physicallyBased
        material.diffuse.contents = NSColor(red: CGFloat(geom.color.x), green: CGFloat(geom.color.y), blue: CGFloat(geom.color.z), alpha: CGFloat(geom.color.w))
        material.roughness.contents = 0.72; material.metalness.contents = 0.08
        material.isDoubleSided = geom.shape == WDSimPlane
        shape.geometry?.materials = [material]
        node.addChildNode(shape)
        node.simdPosition = geom.position; node.simdOrientation = geom.orientation
        return node
    }
}

struct SimulatorRenderer: NSViewRepresentable {
    let scene: SimulatorScene?
    let pose: SimulatorPose?
    let schematic: String?
    let follow: Bool
    let recenter: Int
    func makeCoordinator() -> Coordinator { Coordinator() }
    func makeNSView(context: Context) -> SimulatorOrbitView {
        let view = SimulatorOrbitView()
        view.scene = SCNScene()
        view.backgroundColor = NSColor(red: 0.06, green: 0.08, blue: 0.1, alpha: 1)
        view.autoenablesDefaultLighting = true
        view.antialiasingMode = .multisampling4X
        view.preferredFramesPerSecond = 30
        view.rendersContinuously = false
        view.scene?.rootNode.addChildNode(view.cameraNode)
        view.cameraNode.camera = SCNCamera()
        view.cameraNode.camera?.zNear = 0.005; view.cameraNode.camera?.zFar = 200
        view.pointOfView = view.cameraNode
        view.updateCamera()
        context.coordinator.update(view, scene: scene, pose: pose, schematic: schematic, follow: follow, recenter: recenter)
        return view
    }
    func updateNSView(_ view: SimulatorOrbitView, context: Context) {
        context.coordinator.update(view, scene: scene, pose: pose, schematic: schematic, follow: follow, recenter: recenter)
    }
    static func dismantleNSView(_ view: SimulatorOrbitView, coordinator: Coordinator) {
        view.rendersContinuously = false; view.isPlaying = false
        view.delegate = nil; view.scene = nil
        coordinator.bodies = []; coordinator.root = nil
    }
    @MainActor final class Coordinator {
        var loadedID: String?
        var root: SCNNode?
        var bodies: [SCNNode] = []
        var lastRecenter = -1
        func update(_ view: SimulatorOrbitView, scene: SimulatorScene?, pose: SimulatorPose?, schematic: String?, follow: Bool, recenter: Int) {
            let id = scene?.id ?? schematic
            if loadedID != id {
                root?.removeFromParentNode(); bodies = []
                let world = SCNNode(); world.eulerAngles.x = -.pi/2
                view.scene?.rootNode.addChildNode(world); root = world
                if let scene {
                    let meshes = Dictionary(uniqueKeysWithValues: scene.meshes.map { ($0.id, SimulatorGeometryBuilder.mesh($0)) })
                    bodies = (0..<scene.bodyCount).map { _ in let node = SCNNode(); world.addChildNode(node); return node }
                    for geom in scene.geometries { bodies[geom.body].addChildNode(SimulatorGeometryBuilder.node(geom, meshes: meshes)) }
                } else if let schematic {
                    world.addChildNode(Self.board(lite: schematic == "esp32"))
                }
                loadedID = id; lastRecenter = -1
            }
            if let scene, let pose, pose.sceneID == scene.id, pose.positions.count == bodies.count {
                SCNTransaction.begin(); SCNTransaction.animationDuration = 0
                for i in bodies.indices { bodies[i].simdPosition = pose.positions[i]; bodies[i].simdOrientation = pose.orientations[i] }
                SCNTransaction.commit()
                if follow, bodies.indices.contains(scene.robotBody) {
                    view.target = bodies[scene.robotBody].simdWorldPosition
                }
            }
            if recenter != lastRecenter {
                lastRecenter = recenter
                view.target = .zero
                if let scene, bodies.indices.contains(scene.robotBody) { view.target = bodies[scene.robotBody].simdWorldPosition }
                view.radius = schematic == nil ? 3 : 0.15
                view.azimuth = .pi/4; view.polar = 0.9
            }
            view.updateCamera()
            view.needsDisplay = true
        }
        static func board(lite: Bool) -> SCNNode {
            let root = SCNNode()
            func box(_ size: SIMD3<Float>, _ position: SIMD3<Float>, _ color: NSColor) {
                let node = SCNNode(geometry: SCNBox(width: CGFloat(size.x), height: CGFloat(size.y), length: CGFloat(size.z), chamferRadius: 0.0006))
                node.geometry?.firstMaterial?.diffuse.contents = color
                node.simdPosition = position; root.addChildNode(node)
            }
            box(SIMD3(0.027, 0.055, 0.002), .zero, .systemGreen)
            box(SIMD3(0.016, 0.020, 0.003), SIMD3(0, 0.008, 0.0025), .lightGray)
            box(SIMD3(0.009, 0.007, 0.004), SIMD3(0, -0.026, 0.003), .gray)
            for side: Float in [-1, 1] {
                for pin in 0..<10 { box(SIMD3(0.002, 0.002, 0.003), SIMD3(side*0.012, Float(pin)*0.0045-0.02, 0.0025), .systemYellow) }
            }
            if !lite { box(SIMD3(0.010, 0.012, 0.003), SIMD3(0, -0.012, 0.0025), .darkGray) }
            return root
        }
    }
}

final class SimulatorOrbitView: SCNView {
    let cameraNode = SCNNode()
    var target = SIMD3<Float>.zero
    var radius: Float = 3
    var azimuth: Float = .pi/4
    var polar: Float = 0.9
    override var acceptsFirstResponder: Bool { true }
    override func mouseDragged(with event: NSEvent) {
        azimuth -= Float(event.deltaX)*0.01
        polar = min(1.5, max(0.08, polar-Float(event.deltaY)*0.01))
        updateCamera()
    }
    override func scrollWheel(with event: NSEvent) {
        radius = min(60, max(0.04, radius*exp(Float(event.scrollingDeltaY)*0.015)))
        updateCamera()
    }
    func updateCamera() {
        cameraNode.simdPosition = target + SIMD3(radius*sin(polar)*sin(azimuth), radius*cos(polar), radius*sin(polar)*cos(azimuth))
        cameraNode.look(at: SCNVector3(target), up: SCNVector3(0, 1, 0), localFront: SCNVector3(0, 0, -1))
        needsDisplay = true
    }
}

@MainActor final class SimulatorRenderModel: ObservableObject {
    @Published var scene: SimulatorScene?
    @Published var pose: SimulatorPose?
    @Published var error: String?
    func stream(name: String) async {
        scene = nil; pose = nil; error = nil
        while !Task.isCancelled {
            do {
                if scene == nil {
                    let next = try await SimulatorClient.shared.scene(name: name)
                    try Task.checkCancellation(); scene = next
                }
                let next = try await SimulatorClient.shared.pose(name: name)
                try Task.checkCancellation()
                if next.sceneID != scene?.id { scene = nil; pose = nil; continue }
                guard next.positions.count == scene?.bodyCount else { throw WendyError(message: "Scene and pose body counts do not match.") }
                pose = next; error = nil
                try await Task.sleep(for: .milliseconds(33))
            } catch is CancellationError { return }
            catch {
                guard !Task.isCancelled else { return }
                self.error = error.localizedDescription
                try? await Task.sleep(for: .seconds(2))
            }
        }
    }
}
#endif
