import SwiftUI
import RealityKit
import UIKit
import simd
import QuartzCore

@MainActor
enum SpatialDrawing {
    static func ball(_ position: SIMD3<Float>, radius: Float, color: UIColor) -> ModelEntity {
        let entity = ModelEntity(mesh: .generateSphere(radius: radius), materials: [SimpleMaterial(color: color, isMetallic: false)])
        entity.position = position
        return entity
    }
    static func line(_ start: SIMD3<Float>, _ end: SIMD3<Float>, color: UIColor, width: Float = 0.025) -> Entity {
        let length = simd_distance(start, end)
        guard length > 0.001, length.isFinite else { return Entity() }
        let entity = ModelEntity(mesh: .generateBox(width: width, height: width, depth: length), materials: [SimpleMaterial(color: color, isMetallic: false)])
        entity.look(at: end, from: (start + end) / 2, relativeTo: nil)
        return entity
    }
    static func robot(at position: SIMD3<Float>, yaw: Float) -> Entity {
        let root = Entity()
        root.position = position
        root.orientation = simd_quatf(angle: yaw, axis: [0, 1, 0])
        if let original = SpatialAssets.robot {
            root.addChild(original.clone(recursive: true))
            return root
        }
        let body = ModelEntity(mesh: .generateBox(size: [0.48, 0.18, 0.27], cornerRadius: 0.04), materials: [SimpleMaterial(color: .white.withAlphaComponent(0.65), isMetallic: true)])
        body.position.y = 0.32; root.addChild(body)
        for x: Float in [-0.18, 0.18] {
            for z: Float in [-0.12, 0.12] {
                let leg = ModelEntity(mesh: .generateBox(size: [0.045, 0.25, 0.045], cornerRadius: 0.015), materials: [SimpleMaterial(color: .darkGray, isMetallic: false)])
                leg.position = [x, 0.15, z]; root.addChild(leg)
            }
        }
        root.addChild(ball([0.27, 0.34, 0], radius: 0.055, color: .cyan))
        return root
    }

}

/// Own scene objects outside SwiftUI observation: changing the entity cache
/// during RealityView.update must not schedule another view update.
@MainActor
final class SpatialLabelRenderer {
    let root = Entity()
    private var markers: [String: Entity] = [:]
    private var cards: [String: Entity] = [:]

    func update(_ labels: [SpatialLabel], card: (String) -> Entity?, face: (Entity) -> Void) {
        let ids = Set(labels.map(\.id))
        for id in Set(markers.keys).union(cards.keys).subtracting(ids) {
            markers.removeValue(forKey: id)?.removeFromParent()
            cards.removeValue(forKey: id)?.removeFromParent()
        }
        for label in labels {
            let marker: Entity
            if let existing = markers[label.id] { marker = existing }
            else {
                let created = SpatialDrawing.ball(label.position, radius: 0.025, color: .cyan)
                created.name = "label:\(label.id)"
                created.components.set(SpatialPinTarget())
                created.components.set(InputTargetComponent())
                created.generateCollisionShapes(recursive: false)
                markers[label.id] = created; root.addChild(created); marker = created
            }
            marker.position = label.position
            let current = card(label.id)
            if cards[label.id] !== current {
                cards.removeValue(forKey: label.id)?.removeFromParent()
                if let current { cards[label.id] = current; root.addChild(current) }
            }
            if let current {
                current.position = label.position + SIMD3(0, 0.12, 0)
                face(current)
            }
        }
    }

    func clear() {
        root.children.removeAll(); markers.removeAll(); cards.removeAll()
    }
}

struct SpatialFloorTarget: Component {
    @MainActor static func material() -> UnlitMaterial {
        var material = UnlitMaterial(color: .cyan.withAlphaComponent(0.35))
        material.faceCulling = .none
        return material
    }

    @MainActor static func mesh(_ chunk: SpatialMeshChunk, updating previous: ModelEntity?) async throws -> ModelEntity {
        try chunk.validate()
        var descriptor = MeshDescriptor(name: "Detected floor")
        descriptor.positions = MeshBuffer(chunk.vertices)
        descriptor.primitives = .triangles(chunk.indices)
        let resource = try MeshResource.generate(from: [descriptor])
        let shape = try await ShapeResource.generateStaticMesh(from: resource)
        let entity = previous ?? ModelEntity()
        entity.model = ModelComponent(mesh: resource, materials: [material()])
        entity.name = "floor"
        entity.components.set(Self())
        entity.components.set(InputTargetComponent())
        entity.components.set(CollisionComponent(shapes: [shape]))
        return entity
    }
}

struct SpatialPinTarget: Component {}

struct SpatialOverlayView: View {
    @Bindable var model: SpatialModel
    @State private var graphics = Entity()
    @State private var observations = SpatialObservationRenderer()
    @State private var actions = AsyncActions()
    @State private var labels = SpatialLabelRenderer()
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        RealityView { content, _ in
            content.add(model.tracking.root)
            content.add(model.tracking.floors)
            model.tracking.root.addChild(graphics)
            model.tracking.root.addChild(labels.root)
            model.tracking.root.addChild(model.tracking.scan.preview)
        } update: { _, attachments in
            graphics.children.removeAll()
            model.tracking.floors.isEnabled = model.selectingAlignment || model.registrationStep == 1 || model.registrationStep == 3
            if model.replay == nil, model.alignedFrame == model.tracking.frameID,
               let reading = model.reading, CACurrentMediaTime() - model.readingAt < 0.5 {
                observations.update(reading, showRobot: false)
                graphics.addChild(observations.root)
                if let card = attachments.entity(for: "robot"), reading.pose {
                    card.position = reading.position + SIMD3(0, 0.85, 0)
                    faceWearer(card); graphics.addChild(card)
                }
            }
            if let candidate = model.robotSearch.candidate, model.tracking.tracked, model.replay == nil {
                let position = SIMD3<Float>(Float(candidate.X), Float(candidate.FloorY), Float(candidate.Z))
                graphics.addChild(SpatialDrawing.robot(at: position, yaw: Float(candidate.Yaw)))
                graphics.addChild(SpatialDrawing.ball(position + SIMD3(0, 0.65, 0), radius: 0.07, color: .yellow))
            }
            if model.selectingAlignment, let center = model.center {
                graphics.addChild(SpatialDrawing.ball(center + SIMD3(0, 0.03, 0), radius: 0.07, color: .cyan))
                if let front = model.front { graphics.addChild(SpatialDrawing.line(center + SIMD3(0, 0.04, 0), front + SIMD3(0, 0.04, 0), color: .green)) }
            }
            if let preview = model.preview {
                graphics.addChild(SpatialDrawing.ball(preview.point + SIMD3(0, 0.06, 0), radius: 0.09, color: .green))
            }
            labels.update(model.labels, card: { attachments.entity(for: $0) }, face: faceWearer)
            for point in model.roomReferences where model.registrationStep != nil {
                graphics.addChild(SpatialDrawing.ball(point + SIMD3(0, 0.025, 0), radius: 0.05, color: .yellow))
            }
        } attachments: {
            ForEach(model.labels) { label in
                Attachment(id: label.id) {
                    VStack(alignment: .leading, spacing: 4) {
                        Label(label.name, systemImage: label.lite ? "cpu" : "server.rack").font(.headline)
                        Text("Manually placed").font(.caption).foregroundStyle(.secondary)
                        if model.connected?.id == label.id {
                            TimelineView(.periodic(from: .now, by: 1)) { _ in
                                Text(CACurrentMediaTime() - model.telemetryAt < 8 ? model.telemetry : "Telemetry unavailable or stale").font(.caption)
                            }
                        }
                    }.padding(14).glassBackgroundEffect()
                }
            }
            Attachment(id: "robot") {
                VStack(spacing: 8) {
                    SpatialEyebrow(title: "Robot · live observations")
                    Label(model.robot?.name ?? "Go2", systemImage: "pawprint.fill").font(.title3.bold())
                    Text(model.reading?.message ?? "Waiting for observations").font(.caption).frame(maxWidth: 260)
                    HStack(spacing: 16) {
                        Button {
                            model.assistant.prefill("Explain this robot's current state and anything I should check.", session: model.session)
                            openWindow(id: "assistant")
                        } label: { Label("Ask Wendy", systemImage: "waveform") }
                        .tint(SpatialStyle.amber)
                        Button("Come here") { actions.submit { await model.callRobot() } }.disabled(!model.canPreview || model.busy)
                        Button("Stop", role: .destructive) { actions.submit { await model.stop() } }.tint(.red)
                    }
                }.padding(24).glassBackgroundEffect(in: RoundedRectangle(cornerRadius: 28))
            }
        }
        .simultaneousGesture(SpatialTapGesture().targetedToEntity(where: .has(SpatialFloorTarget.self)).onEnded { value in
            let point = value.convert(value.location3D, from: .local, to: model.tracking.root)
            model.selectFloor(point)
        })
        .simultaneousGesture(DragGesture().targetedToEntity(where: .has(SpatialPinTarget.self)).onChanged { value in
            guard value.entity.name.hasPrefix("label:") else { return }
            let id = String(value.entity.name.dropFirst(6))
            guard let index = model.labels.firstIndex(where: { $0.id == id }), model.tracking.tracked else { return }
            let point = value.convert(value.location3D, from: .local, to: model.tracking.root)
            if SpatialGeometry.finite(point) { model.labels[index].position = point }
        })
        .task { await actions.run() }
        .task {
            model.immersive = true
            await model.tracking.run()
            if !Task.isCancelled { model.error = model.tracking.message }
            await model.invalidateAlignment()
            model.registration = nil; model.registrationStep = nil; model.labels = []
        }
        .onDisappear {
            labels.clear(); labels.root.removeFromParent(); graphics.removeFromParent()
            model.immersive = false; model.alignedFrame = nil; model.preview = nil
        }
    }
    private func faceWearer(_ entity: Entity) {
        if let wearer = model.tracking.wearer {
            entity.look(at: wearer, from: entity.position, relativeTo: model.tracking.root, forward: .positiveZ)
        }
    }
}

@MainActor
enum SpatialTwinPicking {
    static func prepare(_ entity: Entity) async throws {
        // A convex hull encloses empty room space and can intercept taps below
        // or above the visible floor. Hit-test the same triangles we render.
        entity.components.remove(CollisionComponent.self)
        entity.components.remove(InputTargetComponent.self)
        if let model = entity.components[ModelComponent.self] {
            let shape = try await ShapeResource.generateStaticMesh(from: model.mesh)
            entity.components.set(CollisionComponent(shapes: [shape]))
            entity.components.set(InputTargetComponent())
        }
        for child in entity.children { try await prepare(child) }
    }

    static func contains(_ entity: Entity, in root: Entity) -> Bool {
        var current: Entity? = entity
        while let node = current {
            if node === root { return true }
            current = node.parent
        }
        return false
    }
}

struct SpatialTabletop: View {
    @Bindable var model: SpatialModel
    @State private var stage = Entity()
    @State private var metricSpace = Entity()
    @State private var imported: Entity?
    @State private var graphics = Entity()
    @State private var observations = SpatialObservationRenderer()
    @State private var loadError: String?
    @State private var scale: Float = 0.15
    @State private var center = SIMD3<Float>.zero

    var body: some View {
        RealityView { content, _ in
            stage.addChild(metricSpace)
            metricSpace.addChild(graphics)
            content.add(stage)
        } update: { _, attachments in
            metricSpace.scale = SIMD3(repeating: scale)
            metricSpace.position = -center * scale
            graphics.children.removeAll()
            if let site = model.site, model.replay == nil {
                if let imported, imported.parent !== metricSpace {
                    metricSpace.addChild(imported)
                    imported.scale = SIMD3(repeating: site.metresPerUnit)
                }
                imported?.isEnabled = true
                for camera in site.cameras {
                    if let p = camera.position {
                        graphics.addChild(SpatialDrawing.ball(SIMD3(Float(p.x), Float(p.y), Float(p.z)), radius: 0.08, color: .orange))
                    }
                }
                for point in model.siteReferences where model.registrationStep != nil {
                    graphics.addChild(SpatialDrawing.ball(point, radius: 0.08, color: .yellow))
                }
                if let registration = model.registration, registration.frame == model.tracking.frameID, model.tracking.tracked {
                    if let wearer = model.registeredWearer {
                        let floor = model.siteReferences.first?.y ?? wearer.y - 1.6
                        let foot = SIMD3(wearer.x, floor + 0.03, wearer.z)
                        graphics.addChild(SpatialDrawing.ball(foot, radius: 0.13, color: .systemTeal))
                        graphics.addChild(SpatialDrawing.line(foot, wearer, color: .systemTeal, width: 0.035))
                        if let forward = model.tracking.wearerForward {
                            let direction = registration.direction(forward)
                            let planar = SIMD3(direction.x, 0, direction.z)
                            if simd_length(planar) > 0.1 {
                                graphics.addChild(SpatialDrawing.line(foot, foot + simd_normalize(planar) * 0.6, color: .systemTeal, width: 0.06))
                            }
                        }
                        if let label = attachments.entity(for: "you") {
                            label.position = wearer + SIMD3(0, 0.2, 0)
                            label.scale = SIMD3(repeating: 1 / max(scale, 0.01))
                            label.components.set(BillboardComponent())
                            graphics.addChild(label)
                        }
                    }
                    if let reading = model.reading, model.alignedFrame == model.tracking.frameID,
                       CACurrentMediaTime() - model.readingAt < 0.5 {
                        observations.update(reading)
                        observations.root.transform = Transform(matrix: registration.siteFromFrame)
                        graphics.addChild(observations.root)
                        if reading.pose, reading.aligned, let label = attachments.entity(for: "go2") {
                            label.position = registration.point(reading.position + SIMD3(0, 0.8, 0))
                            label.scale = SIMD3(repeating: 1 / max(scale, 0.01))
                            label.components.set(BillboardComponent())
                            graphics.addChild(label)
                        }
                    }
                }
            } else {
                imported?.isEnabled = false
                if let reading = model.displayedReading, model.replay != nil || CACurrentMediaTime() - model.readingAt < 0.5 {
                    observations.update(reading)
                    graphics.addChild(observations.root)
                }
            }
        } attachments: {
            Attachment(id: "go2") {
                Label(model.robot?.name ?? "Go2", systemImage: "pawprint.fill").font(.headline)
                    .foregroundStyle(SpatialStyle.amber).padding(12).glassBackgroundEffect(in: Capsule())
            }
            Attachment(id: "you") {
                Label("You", systemImage: "person.fill").font(.headline)
                    .foregroundStyle(SpatialStyle.cyan).padding(12).glassBackgroundEffect(in: Capsule())
            }
        }
        .ornament(attachmentAnchor: .scene(.bottom)) {
            HStack(spacing: 24) {
                VStack(alignment: .leading, spacing: 4) {
                    SpatialEyebrow(title: model.replay != nil ? "Recorded observations" : "Digital twin")
                    Text(model.replay != nil ? model.replay!.robot : model.site?.name ?? "Live robot observations").font(.headline)
                    if let loadError { Text(loadError).font(.caption).foregroundStyle(.orange) }
                    else if model.site != nil, model.replay == nil {
                        TimelineView(.periodic(from: .now, by: 0.5)) { _ in
                            Text(model.robotMapStatus).font(.caption).foregroundStyle(.secondary)
                                .frame(maxWidth: 320, alignment: .leading).fixedSize(horizontal: false, vertical: true)
                        }
                    }
                }
                Divider().frame(height: 32)
                Button { scale = max(0.01, scale / 1.25) } label: { Image(systemName: "minus.magnifyingglass") }
                    .accessibilityLabel("Zoom out")
                Text("\(Int(scale * 100))%").font(.subheadline.monospacedDigit()).frame(width: 48)
                Button { scale = min(1, scale * 1.25) } label: { Image(systemName: "plus.magnifyingglass") }
                    .accessibilityLabel("Zoom in")
            }.padding(20).glassBackgroundEffect(in: RoundedRectangle(cornerRadius: 28))
        }
        .gesture(SpatialTapGesture().targetedToAnyEntity().onEnded { value in
            guard model.replay == nil, let imported,
                  SpatialTwinPicking.contains(value.entity, in: imported) else { return }
            let point = value.convert(value.location3D, from: .local, to: metricSpace)
            model.selectSitePoint(point)
        })
        .task(id: model.site?.modelURL) {
            imported?.removeFromParent(); imported = nil; loadError = nil
            guard let site = model.site else { return }
            do {
                let entity = try await Entity(contentsOf: site.modelURL)
                guard !Task.isCancelled, site.modelURL == model.site?.modelURL else { return }
                entity.scale = SIMD3(repeating: site.metresPerUnit)
                try await SpatialTwinPicking.prepare(entity)
                guard !Task.isCancelled, site.modelURL == model.site?.modelURL else { return }
                let bounds = entity.visualBounds(relativeTo: nil)
                center = bounds.center
                let size = max(bounds.extents.x, bounds.extents.y, bounds.extents.z)
                scale = min(1, 0.9 / max(size, 0.1))
                imported = entity
            } catch { loadError = error.localizedDescription }
        }
        .onAppear {
            if model.replay != nil || model.site == nil {
                center = model.replay?.samples.first?.reading.position ?? model.center ?? model.reading?.position ?? .zero
                scale = 0.15
            }
        }
        .onChange(of: model.replay != nil) { _, replay in
            if replay { center = model.replay?.samples.first?.reading.position ?? .zero; scale = 0.15 }
            else if let imported {
                let bounds = imported.visualBounds(relativeTo: metricSpace)
                center = bounds.center; scale = min(1, 0.9 / max(bounds.extents.x, bounds.extents.y, bounds.extents.z, 0.1))
            }
        }
        .onChange(of: model.alignedFrame) { _, _ in
            if model.site == nil { center = model.center ?? .zero }
        }
    }
}
