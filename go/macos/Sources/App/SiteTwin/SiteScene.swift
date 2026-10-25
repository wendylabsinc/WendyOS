@preconcurrency import SceneKit
import SwiftUI

/// Shows imported geometry, manual markers and current reported camera poses.
/// Lens cones require calibration; they do not imply measured coverage.
@MainActor
public struct SiteScene: View {
    let modelURL: URL
    let site: Site
    let cameraGeometry: [UUID: SiteCameraGeometry]
    let placingCamera: UUID?
    let select: (UUID) -> Void
    let place: (UUID, SitePoint) -> Void

    public init(
        modelURL: URL,
        site: Site,
        cameraGeometry: [UUID: SiteCameraGeometry] = [:],
        placingCamera: UUID?,
        select: @escaping (UUID) -> Void,
        place: @escaping (UUID, SitePoint) -> Void
    ) {
        self.modelURL = modelURL
        self.site = site
        self.cameraGeometry = cameraGeometry
        self.placingCamera = placingCamera
        self.select = select
        self.place = place
    }

    public var body: some View {
        SceneSurface(
            modelURL: modelURL,
            site: site,
            cameraGeometry: cameraGeometry,
            placingCamera: placingCamera,
            select: select,
            place: place
        )
        .accessibilityLabel("3D model of \(site.name)")
    }
}

private struct SceneSurface {
    var modelURL: URL
    var site: Site
    var cameraGeometry: [UUID: SiteCameraGeometry]
    var placingCamera: UUID?
    var select: (UUID) -> Void
    var place: (UUID, SitePoint) -> Void

    @MainActor final class Coordinator: NSObject {
        var parent: SceneSurface
        var loadedURL: URL?
        var markers = SCNNode()
        var model = SCNNode()
        weak var view: SCNView?
        var needsFraming = true

        init(_ parent: SceneSurface) { self.parent = parent }

        func update(_ parent: SceneSurface, view: SCNView) {
            self.parent = parent
            self.view = view
            if loadedURL != parent.modelURL {
                do {
                    let imported = try SCNScene(
                        url: parent.modelURL,
                        options: [
                            .checkConsistency: true, .convertToYUp: true,
                            .convertUnitsToMeters: 1.0,
                        ]
                    )
                    let scene = SCNScene()
                    model = imported.rootNode.clone()
                    scene.rootNode.addChildNode(model)
                    markers = SCNNode()
                    scene.rootNode.addChildNode(markers)
                    view.scene = scene
                    loadedURL = parent.modelURL
                    needsFraming = true
                    view.allowsCameraControl = true
                    view.autoenablesDefaultLighting = true
                    view.antialiasingMode = .multisampling4X
                } catch {
                    // The import path validates SceneKit loading before persisting.
                    // If a saved model becomes unreadable, never render a demo scene.
                    view.scene = nil
                    return
                }
            }
            let scale = Float(parent.site.metresPerModelUnit)
            model.scale = SCNVector3(scale, scale, scale)
            frameOverviewIfNeeded()
            markers.childNodes.forEach { $0.removeFromParentNode() }
            for camera in parent.site.cameras {
                let reported = parent.cameraGeometry[camera.id]
                let current = reported?.pose.isCurrent(revision: parent.site.modelRevision, now: Date()) == true ? reported : nil
                guard let position = current?.pose.position ?? camera.position else { continue }
                let geometry = SCNSphere(radius: 0.08)
                geometry.firstMaterial?.diffuse.contents = platformAccent
                geometry.firstMaterial?.lightingModel = .constant
                let node = SCNNode(geometry: geometry)
                node.name = "sensor:\(camera.id.uuidString)"
                node.position = SCNVector3(position.x, position.y, position.z)
                markers.addChildNode(node)
                if let current, let rays = current.lens?.boundaryRays {
                    // Two metres is a drawing length, not sensor range or visible
                    // coverage. Surface occlusion and detection range are unknown.
                    var vertices = [SCNVector3(0, 0, 0)]
                    for ray in rays {
                        let p = current.pose.quaternion.act(ray * 2)
                        vertices.append(SCNVector3(p.x, p.y, p.z))
                    }
                    var indices: [Int32] = []
                    for i in rays.indices {
                        indices.append(contentsOf: [Int32(i+1), Int32((i+1) % rays.count + 1)])
                        if i % 8 == 0 { indices.append(contentsOf: [0, Int32(i+1)]) }
                    }
                    let lines = SCNGeometry(sources: [SCNGeometrySource(vertices: vertices)],
                        elements: [SCNGeometryElement(indices: indices, primitiveType: .line)])
                    lines.firstMaterial?.diffuse.contents = current.lens?.measured == false ? platformEstimated : platformAccent
                    lines.firstMaterial?.lightingModel = .constant
                    let cone = SCNNode(geometry: lines)
                    cone.name = node.name
                    node.addChildNode(cone)
                }
            }
        }

        func frameOverviewIfNeeded() {
            guard needsFraming, let view, view.bounds.width > 0, view.bounds.height > 0,
                  let scene = view.scene else { return }
            let (low, high) = model.boundingBox
            let scale = Float(parent.site.metresPerModelUnit)
            let minimum = SIMD3<Float>(Float(low.x), Float(low.y), Float(low.z)) * scale
            let maximum = SIMD3<Float>(Float(high.x), Float(high.y), Float(high.z)) * scale
            let center = (minimum + maximum) * 0.5
            let radius = max(simd_length(maximum - minimum) * 0.5, 0.5)
            let halfVertical = Float.pi * 25 / 180
            let aspect = Float(view.bounds.width / view.bounds.height)
            let halfAngle = min(halfVertical, atan(tan(halfVertical) * aspect))
            let distance = radius / sin(halfAngle) * 1.2
            let camera = SCNNode()
            camera.camera = SCNCamera()
            camera.camera?.fieldOfView = 50
            camera.camera?.automaticallyAdjustsZRange = true
            camera.simdPosition = center + simd_normalize(SIMD3<Float>(0.8, 1.2, 0.8)) * distance
            let target = SCNVector3(center.x, center.y, center.z)
            camera.look(at: target)
            scene.rootNode.addChildNode(camera)
            view.pointOfView = camera
            view.defaultCameraController.target = target
            needsFraming = false
        }

        func tap(at point: CGPoint) {
            guard let view else { return }
            let hits = view.hitTest(
                point,
                options: [.searchMode: SCNHitTestSearchMode.all.rawValue]
            )
            if let camera = parent.placingCamera,
                let hit = hits.first(where: { !isMarker($0.node) })
            {
                let p = hit.worldCoordinates
                parent.place(camera, SitePoint(x: Double(p.x), y: Double(p.y), z: Double(p.z)))
            } else if let name = hits.first?.node.name, name.hasPrefix("sensor:"),
                let id = UUID(uuidString: String(name.dropFirst(7)))
            {
                parent.select(id)
            }
        }

        private func isMarker(_ node: SCNNode) -> Bool {
            var current: SCNNode? = node
            while let candidate = current {
                if candidate === markers { return true }
                current = candidate.parent
            }
            return false
        }

        #if os(iOS)
            @objc func clicked(_ gesture: UITapGestureRecognizer) {
                tap(at: gesture.location(in: view))
            }
        #else
            @objc func clicked(_ gesture: NSClickGestureRecognizer) {
                tap(at: gesture.location(in: view))
            }
        #endif
    }
}

#if os(iOS)
    private var platformAccent: UIColor { .systemGreen }
    private var platformEstimated: UIColor { .systemOrange }
    extension SceneSurface: UIViewRepresentable {
        func makeCoordinator() -> Coordinator { Coordinator(self) }
        func makeUIView(context: Context) -> SCNView {
            let view = SiteSCNView()
            view.didLayout = { [weak coordinator = context.coordinator] in coordinator?.frameOverviewIfNeeded() }
            view.backgroundColor = .secondarySystemBackground
            view.addGestureRecognizer(
                UITapGestureRecognizer(
                    target: context.coordinator,
                    action: #selector(Coordinator.clicked)
                )
            )
            context.coordinator.update(self, view: view)
            return view
        }
        func updateUIView(_ view: SCNView, context: Context) {
            context.coordinator.update(self, view: view)
        }
    }
#else
    private var platformAccent: NSColor { .systemGreen }
    private var platformEstimated: NSColor { .systemOrange }
    extension SceneSurface: NSViewRepresentable {
        func makeCoordinator() -> Coordinator { Coordinator(self) }
        func makeNSView(context: Context) -> SCNView {
            let view = SiteSCNView()
            view.didLayout = { [weak coordinator = context.coordinator] in coordinator?.frameOverviewIfNeeded() }
            view.backgroundColor = .windowBackgroundColor
            view.addGestureRecognizer(
                NSClickGestureRecognizer(
                    target: context.coordinator,
                    action: #selector(Coordinator.clicked)
                )
            )
            context.coordinator.update(self, view: view)
            return view
        }
        func updateNSView(_ view: SCNView, context: Context) {
            context.coordinator.update(self, view: view)
        }
    }
#endif

@MainActor
private final class SiteSCNView: SCNView {
    var didLayout: (() -> Void)?
    #if os(iOS)
    override func layoutSubviews() {
        super.layoutSubviews()
        didLayout?()
    }
    #else
    override func layout() {
        super.layout()
        didLayout?()
    }
    #endif
}
