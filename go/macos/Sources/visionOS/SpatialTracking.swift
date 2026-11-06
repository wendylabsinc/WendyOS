import ARKit
import Observation
import RealityKit
import QuartzCore

@MainActor @Observable
final class SpatialTracking {
    let root = Entity()
    let floors = Entity()
    let scan = SpatialRoomScan()
    var scanningSupported: Bool { SceneReconstructionProvider.isSupported }
    private var reconstruction = SceneReconstructionProvider(modes: [.classification])
    var running = false
    private var starting = false
    var tracked = false
    var wearer: SIMD3<Float>?
    var wearerForward: SIMD3<Float>?
    var message = "Open AR overlays to track your surroundings."
    var epoch = UUID()
    var frameID: String { epoch.uuidString }
    var alignmentInvalidated = 0
    private var session = ARKitSession()
    private var world = WorldTrackingProvider()
    private var planes = PlaneDetectionProvider(alignments: [.horizontal, .vertical])
    private var reference: UUID?
    private var worldFromFrame: simd_float4x4?
    private var roomPlanes: [UUID: SpatialRoomPlane] = [:]
    var roomGeometry: SiteRobotGeometryPacket? {
        guard tracked, let worldFromFrame else { return nil }
        return SpatialRoomGeometry.geometry(Array(roomPlanes.values), frameFromWorld: simd_inverse(worldFromFrame))
    }
    private var planeEntities: [UUID: ModelEntity] = [:]
    private var meshFloorEntities: [UUID: ModelEntity] = [:]
    var floorTargetCount = 0
    var floorSelectionMessage: String {
        floorTargetCount == 0
            ? "No floor target detected yet. Look down and around the floor until cyan surfaces appear."
            : "Look at the matching point on the cyan floor and pinch."
    }
    private func updateFloorCount() {
        floorTargetCount = planeEntities.count + meshFloorEntities.count
        // The mesh supplies measured targets while plane detection catches up.
        for entity in meshFloorEntities.values { entity.isEnabled = planeEntities.isEmpty }
    }

    func run() async {
        guard WorldTrackingProvider.isSupported, PlaneDetectionProvider.isSupported else {
            message = "AR tracking requires Apple Vision Pro. Use the tabletop view in Simulator."
            return
        }
        guard !starting, !running else { return }
        starting = true
        session = ARKitSession(); world = WorldTrackingProvider()
        planes = PlaneDetectionProvider(alignments: [.horizontal, .vertical])
        reconstruction = SceneReconstructionProvider(modes: [.classification])
        epoch = UUID(); reference = nil; worldFromFrame = nil
        planeEntities.removeAll(); meshFloorEntities.removeAll(); floorTargetCount = 0
        roomPlanes.removeAll(); floors.children.removeAll()
        defer {
            scan.interrupt()
            session.stop(); starting = false; running = false; tracked = false; wearer = nil; wearerForward = nil
            worldFromFrame = nil; reference = nil; alignmentInvalidated += 1
            root.isEnabled = false; floors.children.removeAll()
            planeEntities.removeAll(); meshFloorEntities.removeAll(); floorTargetCount = 0
        }
        var stage = "Starting AR"
        do {
            let authorization = await session.requestAuthorization(for: [.worldSensing])
            guard authorization[.worldSensing] == .allowed else { throw SpatialFailure("Allow World Sensing in Settings to place overlays.") }
            var providers: [any DataProvider] = [world, planes]
            if scanningSupported { providers.append(reconstruction) }
            try await session.run(providers)
            stage = "Finding your room"
            message = "Look around while Vision Pro establishes world tracking."
            // run() starts the providers; it does not guarantee a tracked pose.
            // Anchoring the launch origin before localization can fail in ARKit.
            _ = try await readyPose()
            if let previous = await world.allAnchors {
                // Placements are session-only. Clear anchors left by a killed app
                // before adding one, so they cannot exhaust ARKit's anchor limit.
                for anchor in previous {
                    try Task.checkCancellation()
                    try await world.removeAnchor(anchor)
                }
            }
            stage = "Anchoring your room"
            for attempt in 0..<3 {
                var transform = matrix_identity_float4x4
                let pose = try await readyPose()
                // Place the reference near the wearer, with gravity-aligned axes
                // and the AR origin's floor height. Do not reuse a distant origin.
                transform.columns.3 = SIMD4(pose.columns.3.x, 0, pose.columns.3.z, 1)
                let anchor = WorldAnchor(originFromAnchorTransform: transform)
                reference = anchor.id
                do {
                    try await world.addAnchor(anchor)
                    break
                } catch let failure as WorldTrackingProvider.Error where failure.code == .addWorldAnchorFailed && attempt < 2 {
                    try? await world.removeAnchor(forID: anchor.id)
                    reference = nil
                    try await Task.sleep(for: .milliseconds(500))
                }
            }
            if let reference, let anchor = await world.allAnchors?.first(where: { $0.id == reference }), anchor.isTracked {
                worldFromFrame = anchor.originFromAnchorTransform
                root.transform = Transform(matrix: anchor.originFromAnchorTransform)
            }
            starting = false; running = true
            await withTaskGroup(of: Void.self) { group in
                group.addTask { await self.monitorAnchors() }
                group.addTask { await self.monitorPlanes() }
                group.addTask { await self.monitorHead() }
                if self.scanningSupported { group.addTask { await self.monitorReconstruction() } }
                group.addTask { await self.monitorEvents() }
                await group.next()
                group.cancelAll()
                if let reference = self.reference { try? await self.world.removeAnchor(forID: reference) }
                self.session.stop()
            }
        } catch is CancellationError {
            message = "AR closed."
        } catch {
            let reason = (error as? LocalizedError)?.failureReason
            let detail = [error.localizedDescription, reason].compactMap { $0 }.filter { !$0.isEmpty }.joined(separator: " ")
            let code = (error as? WorldTrackingProvider.Error).map { " [\($0.code)]" } ?? ""
            message = "\(stage): \(detail)\(code) Close AR and try again after looking around the room."
        }
    }
    private func readyPose() async throws -> simd_float4x4 {
        let deadline = ContinuousClock.now.advanced(by: .seconds(15))
        while ContinuousClock.now < deadline {
            try Task.checkCancellation()
            if world.state == .running,
               let head = world.queryDeviceAnchor(atTimestamp: CACurrentMediaTime()), head.isTracked {
                return head.originFromAnchorTransform
            }
            if world.state == .stopped { throw SpatialFailure("World tracking stopped before the room was ready.") }
            try await Task.sleep(for: .milliseconds(100))
        }
        throw SpatialFailure("Vision Pro has not established a tracked position yet. Keep the room well lit and look at nearby surfaces.")
    }
    private func monitorAnchors() async {
        for await update in world.anchorUpdates {
            guard !Task.isCancelled else { return }
            guard update.anchor.id == reference else {
                // A process killed before cleanup may leave its session anchor.
                // This app does not persist placements across AR sessions.
                if update.event == .added { try? await world.removeAnchor(forID: update.anchor.id) }
                continue
            }
            let valid = update.event != .removed && update.anchor.isTracked
            if !valid { invalidate() }
            worldFromFrame = valid ? update.anchor.originFromAnchorTransform : nil
            if let transform = worldFromFrame { root.transform = Transform(matrix: transform) }
        }
    }
    private func monitorHead() async {
        while !Task.isCancelled {
            if let frame = worldFromFrame, world.state == .running,
               let head = world.queryDeviceAnchor(atTimestamp: CACurrentMediaTime()), head.isTracked {
                let point = simd_inverse(frame) * head.originFromAnchorTransform.columns.3
                wearer = SIMD3(point.x, point.y, point.z)
                let direction = simd_inverse(frame) * head.originFromAnchorTransform * SIMD4<Float>(0, 0, -1, 0)
                wearerForward = SIMD3(direction.x, direction.y, direction.z)
                tracked = true; root.isEnabled = true
                message = "Tracking your surroundings"
            } else { invalidate() }
            do { try await Task.sleep(for: .milliseconds(50)) } catch { return }
        }
    }
    private func monitorEvents() async {
        for await event in session.events {
            guard !Task.isCancelled else { return }
            switch event {
            case .authorizationChanged(_, let status):
                if status != .allowed { invalidate(); message = "World Sensing permission was removed."; return }
            case .dataProviderStateChanged(_, let state, let error):
                if state == .paused || state == .stopped { invalidate() }
                if let error { message = error.localizedDescription; return }
            @unknown default: break
            }
        }
    }
    private func monitorPlanes() async {
        // Tracking startup may finish after ARKit first reports the room.
        // Seed existing anchors before listening for subsequent changes.
        for anchor in planes.allAnchors { await updatePlane(anchor, removed: false) }
        for await update in planes.anchorUpdates {
            guard !Task.isCancelled else { return }
            await updatePlane(update.anchor, removed: update.event == .removed)
        }
    }
    private func updatePlane(_ anchor: PlaneAnchor, removed: Bool) async {
        let isFloor = anchor.surfaceClassification == .floor
        guard !removed, isFloor || anchor.surfaceClassification == .wall else {
            roomPlanes.removeValue(forKey: anchor.id)
            planeEntities.removeValue(forKey: anchor.id)?.removeFromParent()
            updateFloorCount(); return
        }
        do {
            let chunk = try SpatialMeshChunk.capturePlane(anchor)
            if roomPlanes.count < 256 || roomPlanes[anchor.id] != nil {
                roomPlanes[anchor.id] = SpatialRoomPlane(floor: isFloor, vertices: chunk.vertices, indices: chunk.indices)
            }
            if isFloor, let floor = SpatialMeshChunk.floorFacesOnly(chunk, classifiedFloor: Array(repeating: true, count: chunk.triangles)) {
                let entity = try await SpatialFloorTarget.mesh(floor, updating: planeEntities[anchor.id])
                guard !Task.isCancelled else { return }
                if entity.parent == nil { floors.addChild(entity) }
                planeEntities[anchor.id] = entity
            } else {
                planeEntities.removeValue(forKey: anchor.id)?.removeFromParent()
            }
            updateFloorCount()
        } catch { /* Keep the previous measured target if an update is malformed. */ }
    }
    private func monitorReconstruction() async {
        for anchor in reconstruction.allAnchors {
            guard !Task.isCancelled else { return }
            await updateMeshFloor(anchor, removed: false)
        }
        for await update in reconstruction.anchorUpdates {
            guard !Task.isCancelled else { return }
            await updateMeshFloor(update.anchor, removed: update.event == .removed)
            guard scan.capturing, tracked, let worldFromFrame else { continue }
            if update.event == .removed { scan.remove(update.anchor.id) }
            else { await scan.receive(update.anchor, frameFromWorld: simd_inverse(worldFromFrame)) }
        }
    }
    private func updateMeshFloor(_ anchor: MeshAnchor, removed: Bool) async {
        if removed {
            meshFloorEntities.removeValue(forKey: anchor.id)?.removeFromParent()
            updateFloorCount()
        } else if meshFloorEntities.count < 128 || meshFloorEntities[anchor.id] != nil {
            do {
                if let chunk = try SpatialMeshChunk.floor(anchor) {
                    let entity = try await SpatialFloorTarget.mesh(chunk, updating: meshFloorEntities[anchor.id])
                    guard !Task.isCancelled else { return }
                    if entity.parent == nil { floors.addChild(entity) }
                    meshFloorEntities[anchor.id] = entity
                } else {
                    meshFloorEntities.removeValue(forKey: anchor.id)?.removeFromParent()
                }
                updateFloorCount()
            } catch { /* A malformed mesh must not replace a valid floor target. */ }
        }
    }
    func startScan() async {
        guard tracked, scanningSupported, !scan.saving, let worldFromFrame else { scan.message = "Open AR and allow World Sensing on Vision Pro to scan."; return }
        scan.start(frame: frameID)
        let frame = frameID
        for anchor in reconstruction.allAnchors {
            guard !Task.isCancelled, tracked, frameID == frame, scan.capturing else { break }
            await scan.receive(anchor, frameFromWorld: simd_inverse(worldFromFrame))
        }
    }
    func invalidate() {
        scan.interrupt()
        if tracked { alignmentInvalidated += 1 }
        tracked = false; wearer = nil; wearerForward = nil; root.isEnabled = false
        message = "Tracking unavailable. Align Go2 again after tracking resumes."
    }
    func framePoint(worldPoint: SIMD3<Float>) -> SIMD3<Float>? {
        guard tracked, let worldFromFrame else { return nil }
        let p = simd_inverse(worldFromFrame) * SIMD4(worldPoint, 1)
        return SIMD3(p.x, p.y, p.z)
    }
    func placementPoint() -> SIMD3<Float>? {
        guard tracked, let worldFromFrame,
              let head = world.queryDeviceAnchor(atTimestamp: CACurrentMediaTime()), head.isTracked else { return nil }
        let p = simd_inverse(worldFromFrame) * head.originFromAnchorTransform * SIMD4<Float>(0, -0.15, -0.8, 1)
        return SIMD3(p.x, p.y, p.z)
    }
}
