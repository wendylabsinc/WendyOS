import AppKit
import SceneKit

/// Overlay labels repeat on every frame. Retain their immutable geometry while
/// giving each displayed label its own nodes, transform and billboard constraint.
@MainActor final class SceneLabelCache {
    private struct Template {
        let text: SCNText
        let plate: SCNPlane
        let pivot: SCNMatrix4
    }

    private let capacity: Int
    private var templates: [String: Template] = [:]
    private var insertionOrder: [String] = []
    var count: Int { templates.count }

    init(capacity: Int = 128) {
        self.capacity = max(1, min(capacity, 128))
    }

    func label(_ text: String, position: SCNVector3, size: CGFloat) -> SCNNode {
        let template: Template
        if let cached = templates[text] {
            template = cached
        } else {
            template = makeTemplate(text)
            // Keep arbitrary long labels out of the cache as well as limiting
            // entry count. They still render normally for this frame.
            if text.utf8.count <= 512 {
                if templates.count == capacity {
                    templates.removeValue(forKey: insertionOrder.removeFirst())
                }
                templates[text] = template
                insertionOrder.append(text)
            }
        }
        let textNode = SCNNode(geometry: template.text)
        textNode.castsShadow = false
        textNode.pivot = template.pivot
        textNode.renderingOrder = 101
        let plateNode = SCNNode(geometry: template.plate)
        plateNode.position.z = -0.01
        plateNode.renderingOrder = 100
        plateNode.castsShadow = false
        let node = SCNNode()
        node.addChildNode(plateNode)
        node.addChildNode(textNode)
        node.scale = SCNVector3(Float(size), Float(size), Float(size))
        node.position = position
        node.constraints = [SCNBillboardConstraint()]
        return node
    }

    private func makeTemplate(_ text: String) -> Template {
        func material(_ color: NSColor) -> SCNMaterial {
            let result = SCNMaterial()
            result.diffuse.contents = color
            result.emission.contents = color
            result.lightingModel = .constant
            result.readsFromDepthBuffer = false
            result.writesToDepthBuffer = false
            return result
        }
        let geometry = SCNText(string: text, extrusionDepth: 0)
        geometry.font = NSFont.systemFont(ofSize: 1, weight: .medium)
        geometry.flatness = 0.15
        geometry.firstMaterial = material(NSColor(red: 0.16, green: 0.25, blue: 0.26, alpha: 1))
        let (minimum, maximum) = SCNNode(geometry: geometry).boundingBox
        let pivot = SCNMatrix4MakeTranslation((minimum.x + maximum.x) / 2, (minimum.y + maximum.y) / 2, 0)
        let plate = SCNPlane(width: CGFloat(maximum.x - minimum.x) + 1.5, height: CGFloat(maximum.y - minimum.y) + 0.9)
        plate.cornerRadius = 0.45
        plate.firstMaterial = material(NSColor(red: 0.98, green: 0.98, blue: 0.96, alpha: 0.94))
        return Template(text: geometry, plate: plate, pivot: pivot)
    }
}
