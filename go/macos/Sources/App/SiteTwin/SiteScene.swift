@preconcurrency import SceneKit
import SwiftUI

/// Shows imported geometry, manual markers and current reported camera poses.
/// Lens cones require calibration; they do not imply measured coverage.
@MainActor
public struct SiteScene: View {
    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    let modelURL: URL
    let site: Site
    let cameraGeometry: [UUID: SiteCameraGeometry]
    let colorSources: [UUID: SiteCameraColorSource]
    let colorStatus: ([UUID: String]) -> Void
    let people: [SitePersonTrack]
    let selectingPeopleFloor: Bool
    let selectPeopleFloor: (SitePoint) -> Void
    let placingCamera: UUID?
    let select: (UUID) -> Void
    let place: (UUID, SitePoint) -> Void
    let robot: SiteRobotRenderState?
    let robotPoint: (SitePoint, Bool) -> Void
    let selectRobot: () -> Void
    let fog: SiteFogConfiguration
    let fogSummary: (SiteFogSummary) -> Void

    public init(
        modelURL: URL,
        site: Site,
        cameraGeometry: [UUID: SiteCameraGeometry] = [:],
        colorSources: [UUID: SiteCameraColorSource] = [:],
        colorStatus: @escaping ([UUID: String]) -> Void = { _ in },
        people: [SitePersonTrack] = [],
        selectingPeopleFloor: Bool = false,
        selectPeopleFloor: @escaping (SitePoint) -> Void = { _ in },
        placingCamera: UUID?,
        select: @escaping (UUID) -> Void,
        place: @escaping (UUID, SitePoint) -> Void,
        robot: SiteRobotRenderState? = nil,
        robotPoint: @escaping (SitePoint, Bool) -> Void = { _, _ in },
        selectRobot: @escaping () -> Void = {},
        fog: SiteFogConfiguration = .init(),
        fogSummary: @escaping (SiteFogSummary) -> Void = { _ in }
    ) {
        self.modelURL = modelURL
        self.site = site
        self.cameraGeometry = cameraGeometry
        self.colorSources = colorSources
        self.colorStatus = colorStatus
        self.people = people
        self.selectingPeopleFloor = selectingPeopleFloor
        self.selectPeopleFloor = selectPeopleFloor
        self.placingCamera = placingCamera
        self.select = select
        self.place = place
        self.robot = robot
        self.robotPoint = robotPoint
        self.selectRobot = selectRobot
        self.fog = fog
        self.fogSummary = fogSummary
    }

    public var body: some View {
        TimelineView(.animation(minimumInterval: 0.25, paused: !fog.enabled && colorSources.isEmpty && people.isEmpty)) { context in
            SceneSurface(
                modelURL: modelURL,
                site: site,
                cameraGeometry: cameraGeometry,
                colorSources: colorSources,
                colorStatus: colorStatus,
                people: people,
                reduceMotion: reduceMotion,
                selectingPeopleFloor: selectingPeopleFloor,
                selectPeopleFloor: selectPeopleFloor,
                placingCamera: placingCamera,
                select: select,
                place: place,
                robot: robot,
                robotPoint: robotPoint,
                selectRobot: selectRobot,
                fog: fog,
                fogSummary: fogSummary,
                refreshDate: context.date
            )
            .accessibilityLabel("3D model of \(site.name)")
        }
        .task(id: colorSources.values.map(\.id).sorted { $0.uuidString < $1.uuidString }) {
            let leases = colorSources.values.map { ($0, $0.acquire()) }
            defer { for (source, lease) in leases { source.release(lease) } }
            while !Task.isCancelled {
                do { try await Task.sleep(for: .seconds(60)) } catch { return }
            }
        }
    }
}

private struct SceneSurface {
    var modelURL: URL
    var site: Site
    var cameraGeometry: [UUID: SiteCameraGeometry]
    var colorSources: [UUID: SiteCameraColorSource]
    var colorStatus: ([UUID: String]) -> Void
    var people: [SitePersonTrack]
    var reduceMotion: Bool
    var selectingPeopleFloor: Bool
    var selectPeopleFloor: (SitePoint) -> Void
    var placingCamera: UUID?
    var select: (UUID) -> Void
    var place: (UUID, SitePoint) -> Void
    var robot: SiteRobotRenderState?
    var robotPoint: (SitePoint, Bool) -> Void
    var selectRobot: () -> Void
    var fog: SiteFogConfiguration
    var fogSummary: (SiteFogSummary) -> Void
    // Forces a native-view update even when telemetry stops arriving.
    var refreshDate: Date

    @MainActor final class Coordinator: NSObject {
        var parent: SceneSurface
        var loadedURL: URL?
        var markers = SCNNode()
        var peopleRoot = SCNNode()
        var personMarkers: [UUID: SitePersonMarker] = [:]
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
        var lastMarkerScale: Float?
        var lastRobot: SiteRobotRenderState?
        let fogRenderer = SiteFogRenderer()
        var colorRenderers: [UUID: SiteCameraColorRenderer] = [:]
        var lastColorStatus: [UUID: String] = [:]
        var lastFogSummary: SiteFogSummary?

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
                    personMarkers = [:]
                    peopleRoot = SCNNode()
                    scene.rootNode.addChildNode(peopleRoot)
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
            updatePeople()
            colorRenderers = colorRenderers.filter { parent.colorSources[$0.key] != nil }
            var activeColors: [SiteCameraColorRenderer] = []
            var statuses: [UUID: String] = [:]
            for id in parent.colorSources.keys.sorted(by: { $0.uuidString < $1.uuidString }) {
                guard activeColors.count < 8 else {
                    statuses[id] = "Eight cameras are already contributing color."
                    continue
                }
                let renderer = colorRenderers[id] ?? SiteCameraColorRenderer()
                colorRenderers[id] = renderer
                if renderer.update(model: model, revision: parent.site.modelRevision,
                    source: parent.colorSources[id], geometry: parent.cameraGeometry[id], device: view.device, now: Date()) {
                    activeColors.append(renderer)
                }
                statuses[id] = renderer.status
            }
            if lastColorStatus != statuses {
                lastColorStatus = statuses
                let currentStatuses = statuses
                DispatchQueue.main.async { [weak self] in
                    guard let self, self.lastColorStatus == currentStatuses else { return }
                    self.parent.colorStatus(currentStatuses)
                }
            }
            let summary = fogRenderer.update(model: model, configuration: parent.fog, robot: parent.robot,
                colors: activeColors)
            if summary != lastFogSummary {
                lastFogSummary = summary
                let configuration = parent.fog
                // SwiftUI is currently updating the native view. Publish the HUD
                // on the next run-loop turn, rejecting superseded contexts.
                DispatchQueue.main.async { [weak self] in
                    guard let self, self.parent.fog == configuration,
                          self.lastFogSummary == summary else { return }
                    self.parent.fogSummary(summary)
                }
            }
            let currentGeometry = parent.cameraGeometry.filter {
                $0.value.pose.isCurrent(revision: parent.site.modelRevision, now: Date())
            }
            guard lastCameras != parent.site.cameras || lastCameraGeometry != currentGeometry || lastMarkerScale != scale else { return }
            lastCameras = parent.site.cameras
            lastCameraGeometry = currentGeometry
            lastMarkerScale = scale
            SCNTransaction.flush()
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
                if let current {
                    node.simdPosition = SiteCameraWallPlacement.displayPosition(
                        node.simdPosition, forward: SIMD3<Float>(current.pose.forward), in: model)
                }
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

        private func updatePeople() {
            let now = Date()
            let visible = parent.people.filter {
                (0...SitePeopleTracker.lifetime).contains(now.timeIntervalSince($0.observedAt))
            }
            let ids = Set(visible.map(\.id))
            for id in Array(personMarkers.keys) where !ids.contains(id) {
                personMarkers.removeValue(forKey: id)?.node.removeFromParentNode()
            }
            for person in visible {
                if let marker = personMarkers[person.id] {
                    marker.update(person, now: now, reduceMotion: parent.reduceMotion)
                } else {
                    let marker = SitePersonMarker(person: person, now: now, reduceMotion: parent.reduceMotion)
                    personMarkers[person.id] = marker
                    peopleRoot.addChildNode(marker.node)
                }
            }
        }

        func tap(at point: CGPoint, immediate: Bool = false) {
            guard let view else { return }
            let hits = view.hitTest(
                point,
                options: [.searchMode: SCNHitTestSearchMode.all.rawValue,
                          .categoryBitMask: ~(1 << 30)]
            )
            if parent.selectingPeopleFloor {
                guard let hit = hits.first(where: { isDescendant($0.node, of: model) }),
                      abs(hit.worldNormal.y) > 0.95 else { return }
                let p = hit.worldCoordinates
                parent.selectPeopleFloor(SitePoint(x: Double(p.x), y: Double(p.y), z: Double(p.z)))
            } else if let camera = parent.placingCamera,
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

/// Keeps camera annotations clear of nearby RoomPlan walls. This is a display
/// adjustment only; localization and persisted calibration retain the fitted pose.
@MainActor
enum SiteCameraWallPlacement {
    static func displayPosition(_ position: SIMD3<Float>, forward: SIMD3<Float>, in model: SCNNode) -> SIMD3<Float> {
        let clearance: Float = 0.09 // Eight-centimetre marker plus one centimetre.
        let maximumCorrection: Float = 0.5
        var result = position
        var nearest = Float.infinity
        model.enumerateChildNodes { wall, _ in
            // RoomPlan's WallN meshes use local +Z for the room-facing surface.
            // Do not attach cameras to furniture, doors or unclassified meshes.
            guard let name = wall.name, name.hasPrefix("Wall"),
                  !name.dropFirst(4).isEmpty, name.dropFirst(4).allSatisfy(\.isNumber),
                  wall.geometry != nil else { return }
            let normal = simd_normalize(SIMD3<Float>(wall.convertVector(SCNVector3(0, 0, 1), to: nil)))
            guard abs(normal.y) < 0.2, simd_dot(normal, forward) > 0.2 else { return }
            let surface = SIMD3<Float>(wall.convertPosition(SCNVector3(0, 0, wall.boundingBox.max.z), to: nil))
            let distance = simd_dot(position - surface, normal)
            guard distance < clearance, distance >= -maximumCorrection else { return }
            // Intersect the actual mesh rather than its bounding box, preserving
            // openings and avoiding attachment above a partial-height wall.
            let start = wall.convertPosition(SCNVector3(position + normal * (maximumCorrection + clearance)), from: nil)
            let end = wall.convertPosition(SCNVector3(position - normal * 0.01), from: nil)
            let hits = wall.hitTestWithSegment(from: start, to: end, options: [
                SCNHitTestOption.backFaceCulling.rawValue: false,
                SCNHitTestOption.clipToZRange.rawValue: false,
                SCNHitTestOption.searchMode.rawValue: SCNHitTestSearchMode.all.rawValue,
            ])
            for hit in hits where simd_dot(simd_normalize(SIMD3<Float>(hit.worldNormal)), normal) > 0.9 {
                let candidate = SIMD3<Float>(hit.worldCoordinates) + normal * clearance
                let correction = simd_distance(position, candidate)
                if correction < nearest {
                    nearest = correction
                    result = candidate
                }
            }
        }
        return result
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
