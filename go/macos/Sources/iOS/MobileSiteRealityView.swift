import SwiftUI
import RealityKit

/// Opt-in migration surface. The persisted site remains in its original metric
/// coordinate system; framing moves the viewer camera, never the saved model.
struct MobileSiteRealityView: View {
    let saved: StoredSite
    let select: (UUID) -> Void
    @State private var scene = MobileSiteRealityScene()
    @State private var error: String?
    @State private var loaded = false
    @State private var yaw = 0.5
    @State private var pitch = 0.8
    @State private var zoom = 1.0
    @GestureState private var drag = CGSize.zero
    @GestureState private var magnification = 1.0

    var body: some View {
        RealityView { content in
            content.camera = .virtual
            content.add(scene.root)
            do {
                try await scene.load(saved)
                scene.orbit(yaw: yaw, pitch: pitch, zoom: zoom)
                loaded = true
            } catch is CancellationError { }
            catch { self.error = error.localizedDescription }
        } update: { _ in
            scene.orbit(yaw: yaw - Double(drag.width) * 0.008,
                        pitch: pitch + Double(drag.height) * 0.008,
                        zoom: zoom / magnification)
        }
        .gesture(TapGesture().targetedToAnyEntity().onEnded { value in
            if let id = UUID(uuidString: value.entity.name) { select(id) }
        })
        .simultaneousGesture(DragGesture(minimumDistance: 8)
            .updating($drag) { value, state, _ in state = value.translation }
            .onEnded { value in
                yaw -= Double(value.translation.width) * 0.008
                pitch = min(1.45, max(-1.2, pitch + Double(value.translation.height) * 0.008))
            })
        .simultaneousGesture(MagnifyGesture()
            .updating($magnification) { value, state, _ in state = value.magnification }
            .onEnded { value in zoom = min(6, max(0.2, zoom / value.magnification)) })
        .overlay {
            if let error { ContentUnavailableView("Model unavailable", systemImage: "cube", description: Text(error)) }
            else if !loaded { ProgressView("Loading 3D model…") }
        }
        .overlay(alignment: .topTrailing) {
            Button("Reset view", systemImage: "arrow.counterclockwise") { yaw = 0.5; pitch = 0.8; zoom = 1 }
                .buttonStyle(.bordered).padding()
        }
        .overlay(alignment: .bottomLeading) {
            Text("Viewer preview · Live projection is available in the standard viewer.")
                .font(.caption).padding(10).background(.regularMaterial, in: RoundedRectangle(cornerRadius: 12)).padding()
        }
        .background(MobileSitesStyle.background)
        .tint(MobileLensStyle.green)
        .onGeometryChange(for: Bool.self) { $0.size.width <= $0.size.height } action: { portrait in
            scene.camera.camera.fieldOfViewOrientation = portrait ? .horizontal : .vertical
        }
        .accessibilityIdentifier("sites.reality.viewer")
        .onChange(of: saved.site) { _, site in scene.updateCameras(site) }
    }
}

@MainActor final class MobileSiteRealityScene {
    let root = Entity()
    let camera = PerspectiveCamera()
    private(set) var modelEntity: Entity?
    private(set) var markers = Entity()
    private(set) var center = SIMD3<Float>.zero
    private(set) var radius: Float = 1

    init() {
        root.addChild(camera)
        root.addChild(markers)
        camera.camera.fieldOfViewInDegrees = 50
        camera.camera.fieldOfViewOrientation = .horizontal
        camera.camera.near = 0.01
        camera.camera.far = 10_000
        let light = DirectionalLight()
        light.light.intensity = 2500
        light.look(at: .zero, from: SIMD3(3, 8, 5), relativeTo: nil)
        root.addChild(light)
        let fill = DirectionalLight()
        fill.light.intensity = 1000
        fill.look(at: .zero, from: SIMD3(-4, 3, -3), relativeTo: nil)
        root.addChild(fill)
    }

    func load(_ saved: StoredSite) async throws {
        try saved.site.validate()
        let entity = try await Entity(contentsOf: saved.modelURL)
        try Task.checkCancellation()
        entity.scale *= Float(saved.site.metresPerModelUnit)
        modelEntity?.removeFromParent()
        root.addChild(entity)
        modelEntity = entity
        let bounds = entity.visualBounds(relativeTo: root)
        guard (0..<3).allSatisfy({ bounds.center[$0].isFinite && bounds.extents[$0].isFinite }),
              simd_length(bounds.extents) > 0 else {
            entity.removeFromParent(); modelEntity = nil
            throw SiteError.invalidModel
        }
        center = bounds.center
        radius = max(0.25, simd_length(bounds.extents) / 2)
        updateCameras(saved.site)
    }

    func orbit(yaw: Double, pitch: Double, zoom: Double) {
        let elevation = Float(min(1.45, max(-1.2, pitch)))
        let azimuth = Float(yaw)
        let distance = radius * 3.4 * Float(min(6, max(0.2, zoom)))
        let offset = SIMD3<Float>(cos(elevation) * sin(azimuth), sin(elevation), cos(elevation) * cos(azimuth)) * distance
        camera.look(at: center, from: center + offset, relativeTo: root)
    }

    func updateCameras(_ site: Site) {
        markers.children.removeAll()
        for saved in site.cameras {
            guard let point = saved.position, point.isValid else { continue }
            let marker = ModelEntity(mesh: .generateSphere(radius: max(0.035, min(0.1, radius * 0.018))),
                materials: [UnlitMaterial(color: .systemGreen)])
            marker.name = saved.id.uuidString
            marker.position = SIMD3(Float(point.x), Float(point.y), Float(point.z))
            marker.components.set(InputTargetComponent())
            marker.generateCollisionShapes(recursive: false)
            markers.addChild(marker)
            if let geometry = saved.manualAlignment?.geometry(position: point, revision: site.modelRevision, now: Date()),
               let rays = geometry.lens?.boundaryRays {
                let origin = marker.position
                let ends = rays.map { ray -> SIMD3<Float> in
                    let direction = geometry.pose.quaternion.act(ray)
                    return origin + SIMD3(Float(direction.x), Float(direction.y), Float(direction.z)) * 1.2
                }
                for index in ends.indices {
                    line(from: ends[index], to: ends[(index + 1) % ends.count])
                    if index % 8 == 0 { line(from: origin, to: ends[index]) }
                }
            }
        }
    }

    private func line(from start: SIMD3<Float>, to end: SIMD3<Float>) {
        let length = simd_distance(start, end)
        guard length > 0.0001 else { return }
        let line = ModelEntity(mesh: .generateBox(size: SIMD3(0.008, 0.008, length)),
            materials: [UnlitMaterial(color: .systemOrange)])
        line.position = (start + end) / 2
        line.orientation = simd_quatf(from: SIMD3(0, 0, 1), to: (end - start) / length)
        markers.addChild(line)
    }
}
