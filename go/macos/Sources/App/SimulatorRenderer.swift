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
                view.radius = schematic == nil ? 3 : (schematic == "esp32" ? 0.095 : 0.15)
                view.azimuth = schematic == "esp32" ? 0.12 : .pi/4
                view.polar = schematic == "esp32" ? 0.42 : 0.9
            }
            view.updateCamera()
            view.needsDisplay = true
        }
        static func board(lite: Bool) -> SCNNode {
            if lite { return esp32Board() }
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

        // Representative ESP32-C6 board, not a physical pinout or live GPIO display.
        private static func esp32Board() -> SCNNode {
            let root = SCNNode()
            func material(_ color: NSColor, metal: CGFloat = 0, roughness: CGFloat = 0.65) -> SCNMaterial {
                let result = SCNMaterial()
                result.lightingModel = .physicallyBased
                result.diffuse.contents = color
                result.metalness.contents = metal
                result.roughness.contents = roughness
                return result
            }
            let pcb = material(NSColor(red: 0.035, green: 0.09, blue: 0.08, alpha: 1))
            let plastic = material(NSColor(white: 0.055, alpha: 1), roughness: 0.85)
            let silver = material(NSColor(white: 0.65, alpha: 1), metal: 0.75, roughness: 0.35)
            let gold = material(NSColor(red: 0.65, green: 0.48, blue: 0.21, alpha: 1), metal: 0.7)
            let ceramic = material(NSColor(red: 0.32, green: 0.26, blue: 0.19, alpha: 1))
            @discardableResult
            func box(_ size: SIMD3<Float>, _ position: SIMD3<Float>, _ finish: SCNMaterial, bevel: CGFloat = 0.00015) -> SCNNode {
                let geometry = SCNBox(width: CGFloat(size.x), height: CGFloat(size.y), length: CGFloat(size.z), chamferRadius: bevel)
                geometry.materials = [finish]
                let node = SCNNode(geometry: geometry)
                node.simdPosition = position
                root.addChildNode(node)
                return node
            }
            func label(_ text: String, width: CGFloat, height: CGFloat, at position: SIMD3<Float>, dark: Bool = false) {
                let size = NSSize(width: width * 40000, height: height * 40000)
                let texture = NSImage(size: size, flipped: false) { rect in
                    let paragraph = NSMutableParagraphStyle()
                    paragraph.alignment = .center
                    (text as NSString).draw(in: rect, withAttributes: [
                        .font: NSFont.monospacedSystemFont(ofSize: size.height * 0.65, weight: .medium),
                        .foregroundColor: dark ? NSColor(white: 0.18, alpha: 1) : NSColor(white: 0.8, alpha: 1),
                        .paragraphStyle: paragraph,
                    ])
                    return true
                }
                let geometry = SCNPlane(width: width, height: height)
                let finish = SCNMaterial()
                finish.lightingModel = .constant
                finish.diffuse.contents = texture
                finish.isDoubleSided = true
                geometry.materials = [finish]
                let node = SCNNode(geometry: geometry)
                node.simdPosition = position
                root.addChildNode(node)
            }

            box(SIMD3(0.0254, 0.054, 0.0016), .zero, pcb, bevel: 0.0005)
            // Dark header housings, solder collars and square metal pins.
            for side: Float in [-1, 1] {
                box(SIMD3(0.0025, 0.041, 0.0025), SIMD3(side * 0.011, -0.001, -0.002), plastic)
                for index in 0..<16 {
                    let y = Float(index) * 0.00254 - 0.020
                    let pad = SCNNode(geometry: SCNTorus(ringRadius: 0.0008, pipeRadius: 0.00015))
                    pad.geometry?.materials = [silver]
                    pad.eulerAngles.x = .pi/2
                    pad.simdPosition = SIMD3(side * 0.011, y, 0.00085)
                    root.addChildNode(pad)
                    box(SIMD3(0.0006, 0.0006, 0.008), SIMD3(side * 0.011, y, -0.0024), gold, bevel: 0.00004)
                }
            }
            // Radio module shield and exposed antenna area.
            box(SIMD3(0.017, 0.023, 0.0008), SIMD3(0, 0.0125, 0.0012), pcb)
            box(SIMD3(0.016, 0.017, 0.0022), SIMD3(0, 0.0095, 0.0025), silver, bevel: 0.00035)
            label("ESP32-C6", width: 0.013, height: 0.0022, at: SIMD3(0, 0.012, 0.00362), dark: true)
            label("WIFI / BLE", width: 0.011, height: 0.0014, at: SIMD3(0, 0.008, 0.00362), dark: true)
            for index in 0..<6 {
                let x = Float(index) * 0.0022 - 0.0055
                box(SIMD3(0.00035, 0.004, 0.0001), SIMD3(x, 0.0215, 0.00165), gold, bevel: 0)
                if index < 5 {
                    box(SIMD3(0.0022, 0.00035, 0.0001), SIMD3(x + 0.0011, index.isMultiple(of: 2) ? 0.0233 : 0.0197, 0.00165), gold, bevel: 0)
                }
            }
            // USB-C shell with a recessed dark opening and connector tongue.
            box(SIMD3(0.009, 0.007, 0.0032), SIMD3(0, -0.024, 0.0024), silver, bevel: 0.0006)
            box(SIMD3(0.0076, 0.00015, 0.0022), SIMD3(0, -0.02755, 0.0024), plastic, bevel: 0.0005)
            box(SIMD3(0.0054, 0.00025, 0.0005), SIMD3(0, -0.02765, 0.0024), pcb, bevel: 0.0001)
            // Controller, regulator and small passives with metal end caps.
            box(SIMD3(0.004, 0.005, 0.0009), SIMD3(0, -0.008, 0.0013), plastic)
            for side: Float in [-1, 1] {
                for index in 0..<6 {
                    box(SIMD3(0.0008, 0.0003, 0.0002), SIMD3(side * 0.0023, -0.010 + Float(index) * 0.0008, 0.0011), silver, bevel: 0)
                }
                for index in 0..<5 {
                    let y = -0.011 + Float(index) * 0.0025
                    box(SIMD3(0.0016, 0.0008, 0.0006), SIMD3(side * 0.0065, y, 0.0012), index.isMultiple(of: 2) ? ceramic : plastic, bevel: 0.00005)
                    for end: Float in [-1, 1] {
                        box(SIMD3(0.0003, 0.00085, 0.00065), SIMD3(side * 0.0065 + end * 0.0007, y, 0.0012), silver, bevel: 0.00003)
                    }
                }
                box(SIMD3(0.0035, 0.0028, 0.001), SIMD3(side * 0.006, -0.0175, 0.0014), silver)
                box(SIMD3(0.002, 0.0016, 0.0008), SIMD3(side * 0.006, -0.0175, 0.0022), plastic)
            }
            label("WENDY LITE", width: 0.013, height: 0.0018, at: SIMD3(0, -0.003, 0.00085))
            label("BOOT", width: 0.004, height: 0.0011, at: SIMD3(-0.006, -0.0149, 0.00085))
            label("RESET", width: 0.004, height: 0.0011, at: SIMD3(0.006, -0.0149, 0.00085))
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
