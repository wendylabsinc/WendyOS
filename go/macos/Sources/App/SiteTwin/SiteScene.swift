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
    let robot: SiteRobotRenderState?
    let robotPoint: (SitePoint, Bool) -> Void
    let selectRobot: () -> Void

    public init(
        modelURL: URL,
        site: Site,
        cameraGeometry: [UUID: SiteCameraGeometry] = [:],
        placingCamera: UUID?,
        select: @escaping (UUID) -> Void,
        place: @escaping (UUID, SitePoint) -> Void,
        robot: SiteRobotRenderState? = nil,
        robotPoint: @escaping (SitePoint, Bool) -> Void = { _, _ in },
        selectRobot: @escaping () -> Void = {}
    ) {
        self.modelURL = modelURL
        self.site = site
        self.cameraGeometry = cameraGeometry
        self.placingCamera = placingCamera
        self.select = select
        self.place = place
        self.robot = robot
        self.robotPoint = robotPoint
        self.selectRobot = selectRobot
    }

    public var body: some View {
        SceneSurface(
            modelURL: modelURL,
            site: site,
            cameraGeometry: cameraGeometry,
            placingCamera: placingCamera,
            select: select,
            place: place,
            robot: robot,
            robotPoint: robotPoint,
            selectRobot: selectRobot
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
    var robot: SiteRobotRenderState?
    var robotPoint: (SitePoint, Bool) -> Void
    var selectRobot: () -> Void

    @MainActor final class Coordinator: NSObject {
        var parent: SceneSurface
        var loadedURL: URL?
        var markers = SCNNode()
        var model = SCNNode()
        weak var view: SCNView?
        var needsFraming = true
        var robotRoot = SCNNode()
        var scanRoot = SCNNode()
        var robotModel = SCNNode()
        var orders = SCNNode()
        var selection = SCNNode()
        var lastCameras: [SiteCamera]?
        var lastCameraGeometry: [UUID: SiteCameraGeometry] = [:]
        var lastRobot: SiteRobotRenderState?

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
                    robotRoot = SCNNode()
                    robotRoot.name = "robot:go2"
                    robotModel = Go2Model.make()
                    robotRoot.addChildNode(robotModel)
                    selection = SCNNode(geometry: SCNTorus(ringRadius: 0.46, pipeRadius: 0.012))
                    selection.geometry?.firstMaterial?.diffuse.contents = platformAccent
                    selection.geometry?.firstMaterial?.emission.contents = platformAccent
                    selection.position.y = 0.015
                    robotRoot.addChildNode(selection)
                    scene.rootNode.addChildNode(robotRoot)
                    orders = SCNNode()
                    scene.rootNode.addChildNode(orders)
                    scanRoot = SCNNode()
                    scanRoot.name = "localization:scan"
                    scene.rootNode.addChildNode(scanRoot)
                    lastCameras = nil
                    lastCameraGeometry = [:]
                    lastRobot = nil
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
            updateRobot()
            let currentGeometry = parent.cameraGeometry.filter {
                $0.value.pose.isCurrent(revision: parent.site.modelRevision, now: Date())
            }
            guard lastCameras != parent.site.cameras || lastCameraGeometry != currentGeometry else { return }
            lastCameras = parent.site.cameras
            lastCameraGeometry = currentGeometry
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
                    // Viewing cones are annotations, not camera selection targets.
                    cone.name = "camera-fov"
                    cone.categoryBitMask = 1 << 30
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

        private func updateRobot() {
            guard let state = parent.robot else { robotRoot.isHidden = true; orders.isHidden = true; scanRoot.isHidden = true; return }
            scanRoot.isHidden = state.scan.isEmpty
            if lastRobot?.scan != state.scan {
                scanRoot.childNodes.forEach { $0.removeFromParentNode() }
                for kind in 0...3 {
                    let points = state.scan.filter { $0.Class == kind }.map { SCNVector3($0.X,$0.Y,$0.Z) }
                    guard !points.isEmpty else { continue }
                    let source = SCNGeometrySource(vertices: points)
                    let indices = (0..<points.count).map { UInt32($0) }
                    let element = SCNGeometryElement(indices: indices, primitiveType: .point)
                    element.pointSize = kind == 0 ? 3 : 6
                    element.minimumPointScreenSpaceRadius = 2
                    element.maximumPointScreenSpaceRadius = 7
                    let geometry = SCNGeometry(sources: [source], elements: [element])
                    let material = SCNMaterial()
                    #if os(iOS)
                    let colors: [UIColor] = [.systemGray,.systemGreen,.systemOrange,.systemBlue]
                    #else
                    let colors: [NSColor] = [.systemGray,.systemGreen,.systemOrange,.systemBlue]
                    #endif
                    material.diffuse.contents = colors[kind]
                    material.lightingModel = .constant
                    material.transparency = kind == 0 ? 0.3 : 0.85
                    geometry.materials = [material]
                    scanRoot.addChildNode(SCNNode(geometry: geometry))
                }
            }
            orders.isHidden = false
            robotRoot.isHidden = state.position == nil
            if let position = state.position {
                SCNTransaction.begin()
                SCNTransaction.animationDuration = state.fresh && lastRobot?.fresh == true ? 0.1 : 0
                robotRoot.position = SCNVector3(position.x, position.y, position.z)
                robotRoot.eulerAngles = SCNVector3(0, state.yaw, 0)
                robotModel.opacity = state.fresh ? 1 : 0.45
                Go2Model.pose(robotModel, joints: state.joints)
                SCNTransaction.commit()
            }
            selection.isHidden = !state.selected
            selection.geometry?.firstMaterial?.diffuse.contents = state.fresh ? platformAccent : platformEstimated
            selection.geometry?.firstMaterial?.emission.contents = state.fresh ? platformAccent : platformEstimated
            guard lastRobot?.route != state.route || lastRobot?.destination != state.destination else { lastRobot = state; return }
            orders.childNodes.forEach { $0.removeFromParentNode() }
            if state.route.count > 1 {
                for index in 1..<state.route.count {
                    let a = state.route[index-1], b = state.route[index]
                    let delta = SIMD3<Float>(Float(b.x-a.x), Float(b.y-a.y), Float(b.z-a.z))
                    let length = simd_length(delta)
                    guard length > 0.001 else { continue }
                    let node = SCNNode(geometry: SCNCylinder(radius: 0.018, height: CGFloat(length)))
                    node.geometry?.firstMaterial?.diffuse.contents = platformAccent
                    node.geometry?.firstMaterial?.lightingModel = .constant
                    node.simdPosition = SIMD3(Float((a.x+b.x)/2), Float((a.y+b.y)/2+0.04), Float((a.z+b.z)/2))
                    node.simdOrientation = simd_quatf(from: SIMD3(0,1,0), to: delta/length)
                    orders.addChildNode(node)
                }
            }
            if let target = state.destination {
                for radius in [0.12, 0.24] {
                    let node = SCNNode(geometry: SCNTorus(ringRadius: radius, pipeRadius: 0.012))
                    node.geometry?.firstMaterial?.diffuse.contents = platformAccent
                    node.geometry?.firstMaterial?.lightingModel = .constant
                    node.position = SCNVector3(target.x, target.y+0.025, target.z)
                    orders.addChildNode(node)
                }
            }
            lastRobot = state
        }

        func tap(at point: CGPoint, immediate: Bool = false) {
            guard let view else { return }
            let hits = view.hitTest(
                point,
                options: [.searchMode: SCNHitTestSearchMode.all.rawValue,
                          .categoryBitMask: ~(1 << 30)]
            )
            if let camera = parent.placingCamera,
                let hit = hits.first(where: { isDescendant($0.node, of: model) })
            {
                let p = hit.worldCoordinates
                parent.place(camera, SitePoint(x: Double(p.x), y: Double(p.y), z: Double(p.z)))
            } else if parent.robot?.placing == true {
                // Placement owns the gesture even when the point is invalid.
                // A missed floor tap must not switch to camera controls.
                guard let hit = hits.first(where: { isDescendant($0.node, of: model) }),
                      abs(hit.worldNormal.y) > 0.85 else { return }
                let p = hit.worldCoordinates
                parent.robotPoint(SitePoint(x: Double(p.x), y: Double(p.y), z: Double(p.z)), false)
            } else if let hit = hits.first, isDescendant(hit.node, of: robotRoot) {
                parent.selectRobot()
            } else if let name = hits.first?.node.name, name.hasPrefix("sensor:"),
                let id = UUID(uuidString: String(name.dropFirst(7)))
            {
                parent.select(id)
            } else if parent.robot?.selected == true,
                      let hit = hits.first(where: { isDescendant($0.node, of: model) }),
                      abs(hit.worldNormal.y) > 0.85 {
                let p = hit.worldCoordinates
                parent.robotPoint(SitePoint(x: Double(p.x), y: Double(p.y), z: Double(p.z)), immediate)
            }
        }

        private func isDescendant(_ node: SCNNode, of root: SCNNode) -> Bool {
            var current: SCNNode? = node
            while let candidate = current {
                if candidate === root { return true }
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
            @objc func rightClicked(_ gesture: NSClickGestureRecognizer) {
                tap(at: gesture.location(in: view), immediate: true)
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
            let rightClick = NSClickGestureRecognizer(target: context.coordinator, action: #selector(Coordinator.rightClicked))
            rightClick.buttonMask = 2
            view.addGestureRecognizer(rightClick)
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
