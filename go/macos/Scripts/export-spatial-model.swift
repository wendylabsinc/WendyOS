// Run with: swift go/macos/Scripts/export-spatial-model.swift go/macos/Resources
import AppKit
import SceneKit
import Foundation

struct Visual: Decodable { let file: String; let color, position, rotation: [Float] }
struct Body: Decodable { let name, joint: String; let position, rotation: [Float]; let meshes: [Visual]; let children: [Body] }
let resources = URL(fileURLWithPath: CommandLine.arguments[1])
let directory = resources.appendingPathComponent("Go2")
func quaternion(_ q: [Float]) -> simd_quatf { simd_normalize(simd_quatf(ix: q[1], iy: q[2], iz: q[3], r: q[0])) }
func make(_ body: Body) throws -> SCNNode {
    let offset = SCNNode()
    offset.simdPosition = SIMD3(body.position[0], body.position[1], body.position[2])
    if body.name == "base" { offset.simdPosition.z = 0.31 }
    offset.simdOrientation = quaternion(body.rotation)
    let joint = SCNNode()
    joint.name = body.joint.isEmpty ? body.name : body.joint
    if !body.joint.isEmpty {
        let angle: Float = body.joint.contains("thigh") ? 0.8 : body.joint.contains("calf") ? -1.6 : 0
        joint.simdOrientation = simd_quatf(angle: angle, axis: body.joint.contains("hip") ? SIMD3(1, 0, 0) : SIMD3(0, 1, 0))
    }
    offset.addChildNode(joint)
    for visual in body.meshes {
        let source = try SCNScene(url: directory.appendingPathComponent(visual.file))
        let node = source.rootNode.clone()
        node.simdPosition = SIMD3(visual.position[0], visual.position[1], visual.position[2])
        node.simdOrientation = quaternion(visual.rotation)
        node.enumerateChildNodes { child, _ in
            guard let geometry = child.geometry else { return }
            let material = SCNMaterial()
            material.diffuse.contents = NSColor(srgbRed: CGFloat(visual.color[0]), green: CGFloat(visual.color[1]), blue: CGFloat(visual.color[2]), alpha: 1)
            material.lightingModel = .physicallyBased
            material.metalness.contents = 0.35; material.roughness.contents = 0.4
            geometry.materials = [material]
        }
        joint.addChildNode(node)
    }
    for child in body.children { joint.addChildNode(try make(child)) }
    return offset
}
let source = try JSONDecoder().decode(Body.self, from: Data(contentsOf: directory.appendingPathComponent("model.json")))
let scene = SCNScene()
let axes = SCNNode(); axes.eulerAngles.x = -.pi / 2
axes.addChildNode(try make(source)); scene.rootNode.addChildNode(axes)
let target = resources.appendingPathComponent("Spatial/Go2.usdz")
guard scene.write(to: target, options: nil, delegate: nil, progressHandler: nil) else { fatalError("Could not export Go2") }
print(target.path)
