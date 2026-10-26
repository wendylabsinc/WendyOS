@preconcurrency import SceneKit
import SwiftUI

public struct SiteRobotRenderState: Equatable {
    var position: SitePoint?
    var yaw = 0.0
    var joints: [Double] = []
    var selected = true
    var fresh = false
    var moving = false
    var route: [SitePoint] = []
    var destination: SitePoint?
    var placing = false
    var scan: [RobotScanPoint] = []
}

/// Original Go2 meshes and kinematic hierarchy, not a generic quadruped icon.
/// Model coordinates are X forward, Y left, Z up. Site coordinates are Y up.
@MainActor
enum Go2Model {
    private struct Mesh: Decodable {
        let file: String
        let color, position, rotation: [Float]
    }
    private struct Body: Decodable {
        let name, joint: String
        let position, rotation: [Float]
        let meshes: [Mesh]
        let children: [Body]
    }
    private static var cached: SCNNode?
    static let jointNames = ["FR", "FL", "RR", "RL"].flatMap { leg in
        ["hip", "thigh", "calf"].map { "\(leg)_\($0)_joint" }
    }

    static func make() -> SCNNode {
        if let cached { return cached.clone() }
        let root = SCNNode()
        root.name = "go2-model"
        guard let url = Bundle.main.url(forResource: "model", withExtension: "json", subdirectory: "Go2"),
              let data = try? Data(contentsOf: url),
              let body = try? JSONDecoder().decode(Body.self, from: data) else { return root }
        let axes = SCNNode()
        axes.eulerAngles.x = -.pi / 2
        root.addChildNode(axes)
        axes.addChildNode(makeBody(body, directory: url.deletingLastPathComponent()))
        pose(root, joints: [])
        cached = root
        return root.clone()
    }

    private static func makeBody(_ body: Body, directory: URL) -> SCNNode {
        let offset = SCNNode()
        offset.simdPosition = SIMD3(body.position[0], body.position[1], body.position[2])
        // Stand at the model's actual support height instead of floating at
        // the MJCF initial free-body height.
        if body.name == "base" { offset.simdPosition.z = 0.31 }
        offset.simdOrientation = quaternion(body.rotation)
        let joint = SCNNode()
        joint.name = body.joint.isEmpty ? body.name : body.joint
        offset.addChildNode(joint)
        for mesh in body.meshes {
            guard let scene = try? SCNScene(url: directory.appendingPathComponent(mesh.file), options: nil) else { continue }
            let node = scene.rootNode.clone()
            node.simdPosition = SIMD3(mesh.position[0], mesh.position[1], mesh.position[2])
            node.simdOrientation = quaternion(mesh.rotation)
            node.enumerateChildNodes { child, _ in
                guard let geometry = child.geometry else { return }
                let material = SCNMaterial()
                #if os(macOS)
                material.diffuse.contents = NSColor(srgbRed: CGFloat(mesh.color[0]), green: CGFloat(mesh.color[1]), blue: CGFloat(mesh.color[2]), alpha: 1)
                #else
                material.diffuse.contents = UIColor(red: CGFloat(mesh.color[0]), green: CGFloat(mesh.color[1]), blue: CGFloat(mesh.color[2]), alpha: 1)
                #endif
                material.lightingModel = .physicallyBased
                material.metalness.contents = 0.25
                material.roughness.contents = 0.6
                geometry.materials = [material]
            }
            joint.addChildNode(node)
        }
        for child in body.children { joint.addChildNode(makeBody(child, directory: directory)) }
        return offset
    }
    private static func quaternion(_ q: [Float]) -> simd_quatf {
        simd_normalize(simd_quatf(ix: q[1], iy: q[2], iz: q[3], r: q[0]))
    }
    static func pose(_ root: SCNNode, joints: [Double]) {
        for (index, name) in jointNames.enumerated() {
            guard let node = root.childNode(withName: name, recursively: true) else { continue }
            let fallback: Float = name.contains("thigh") ? 0.8 : name.contains("calf") ? -1.6 : 0
            let angle = joints.count == 12 && joints[index].isFinite ? Float(joints[index]) : fallback
            node.simdOrientation = simd_quatf(angle: angle, axis: name.contains("hip") ? SIMD3(1,0,0) : SIMD3(0,1,0))
        }
    }

    static func previewScene() -> SCNScene {
        let scene = SCNScene()
        scene.rootNode.addChildNode(make())
        let camera = SCNNode()
        camera.name = "preview-camera"
        camera.camera = SCNCamera()
        camera.camera?.fieldOfView = 36
        camera.position = SCNVector3(1.2, 0.85, 1.1)
        camera.look(at: SCNVector3(0, 0.23, 0))
        scene.rootNode.addChildNode(camera)
        return scene
    }
}

@MainActor
struct Go2Preview: View {
    @State private var scene = Go2Model.previewScene()
    var body: some View {
        SceneView(scene: scene, pointOfView: scene.rootNode.childNode(withName: "preview-camera", recursively: false),
                  options: [.autoenablesDefaultLighting, .allowsCameraControl])
            .frame(height: 135)
            .accessibilityLabel("Interactive 3D Unitree Go2 model, standing pose preview")
    }
}
