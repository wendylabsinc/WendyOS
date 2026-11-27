import SwiftUI
import AppKit
@preconcurrency import SceneKit
import CryptoKit
import simd

struct CameraTwinSnapshot {
    struct Site {
        let id: String; let name: String; let modelRevision: String; let modelSHA256: String
        let metresPerModelUnit: Double
    }
    struct Camera: Identifiable {
        let id: String; let name: String; let state: StationaryCameraState
        let pose: StationaryCameraPoseState; let automaticAlignment: Bool
        func currentPose(now: Date = Date()) -> StationaryCameraPoseState.Pose? {
            guard state.fresh, state.connection == "streaming", let receipt = state.latest,
                  receipt.retainedUntil > now, (0..<3).contains(now.timeIntervalSince(receipt.receivedAt)), pose.fresh, pose.status == "located",
                  let value = pose.pose, value.validUntil > now,
                  pose.inputOrigin == receipt.origin, let evidence = pose.evidence, evidence.count == 2,
                  evidence.allSatisfy({ $0.sourceSession == receipt.sourceSession && $0.origin == receipt.origin }) else { return nil }
            return value
        }
    }
    let site: Site?; let modelAvailable: Bool; let cameras: [Camera]
    static func decode(_ data: Data) throws -> Self {
        guard data.count <= 8 << 20,
              let root = try JSONSerialization.jsonObject(with: data) as? [String: Any],
              root["schema"] as? String == "wendy.camera-twin.state.v1",
              let available = root["model_available"] as? Bool,
              let rows = root["cameras"] as? [[String: Any]], !rows.isEmpty, rows.count <= 16 else { throw StationaryCameraError.response }
        var site: Site?
        if let item = root["site"] as? [String: Any], let id = item["id"] as? String, !id.isEmpty {
            guard let name = item["name"] as? String, let revision = item["model_revision"] as? String,
                  let digest = item["model_sha256"] as? String, StationaryCameraReceipt.validDigest(digest),
                  [id, name, revision].allSatisfy({ $0.utf8.count <= 2048 }), !revision.isEmpty,
                  let scale = (item["metres_per_model_unit"] ?? root["metres_per_model_unit"]) as? Double,
                  scale.isFinite, scale > 0, scale <= 1000 else { throw StationaryCameraError.response }
            site = Site(id: id, name: name, modelRevision: revision, modelSHA256: digest, metresPerModelUnit: scale)
        }
        guard !available || site != nil else { throw StationaryCameraError.response }
        let cameras = try rows.map { row -> Camera in
            guard let id = row["id"] as? String, !id.isEmpty, id.utf8.count <= 128,
                  let name = row["name"] as? String, !name.isEmpty, name.utf8.count <= 1024,
                  let automatic = row["automatic_alignment"] as? Bool,
                  let rawState = row["state"], let rawPose = row["pose"] else { throw StationaryCameraError.response }
            let state = try StationaryCameraState.decode(JSONSerialization.data(withJSONObject: rawState))
            let pose = try StationaryCameraPoseState.decode(JSONSerialization.data(withJSONObject: rawPose))
            if let site, pose.pose != nil {
                guard pose.site.id == site.id, pose.site.modelRevision == site.modelRevision,
                      pose.site.modelSHA256 == site.modelSHA256 else { throw StationaryCameraError.response }
            }
            return Camera(id: id, name: name, state: state, pose: pose, automaticAlignment: automatic)
        }
        guard Set(cameras.map(\.id)).count == cameras.count else { throw StationaryCameraError.response }
        return Self(site: site, modelAvailable: available, cameras: cameras)
    }
}

enum CameraTwinSharedObservations {
    static func decode(_ data: Data, twin: CameraTwinSnapshot, now: Date = Date()) throws -> CameraTwinPeopleBatch {
        guard data.count <= 1 << 20 else { throw StationaryCameraError.size }
        let decoder = JSONDecoder()
        let fractional = Date.ISO8601FormatStyle(includingFractionalSeconds: true)
        let wholeSeconds = Date.ISO8601FormatStyle(includingFractionalSeconds: false)
        decoder.dateDecodingStrategy = .custom { decoder in
            let raw = try decoder.singleValueContainer().decode(String.self)
            if let date = (try? fractional.parse(raw)) { return date }
            guard let date = (try? wholeSeconds.parse(raw)) else { throw StationaryCameraError.response }; return date
        }
        var value = try decoder.decode(CameraTwinPeopleBatch.self, from: data)
        guard value.schema == "wendy.camera-twin.observations.v1", value.observations.count <= 64,
              value.modelSHA256 == twin.site?.modelSHA256, value.floorY.isFinite, abs(value.floorY) <= 1000,
              Set(value.observations.map(\.id)).count == value.observations.count else { throw StationaryCameraError.response }
        value.observations = try value.observations.filter { observation in
            guard !observation.id.isEmpty, observation.id.utf8.count <= 256, observation.provenance == "inferred",
                  observation.position.count == 3, observation.position.allSatisfy({ $0.isFinite && abs($0) <= 1000 }),
                  abs(observation.position[1] - value.floorY) < 0.001,
                  ["measured", "simulated"].contains(observation.inputOrigin),
                  !observation.inputs.isEmpty, observation.inputs.count <= 8,
                  Set(observation.inputs.map(\.cameraID)).count == observation.inputs.count,
                  observation.validUntil > observation.observedAt else { throw StationaryCameraError.response }
            guard observation.validUntil > now, observation.observedAt <= now else { return false }
            for input in observation.inputs {
                guard let camera = twin.cameras.first(where: { $0.id == input.cameraID }),
                      let pose = camera.currentPose(now: now), camera.pose.coordinateFrame == value.coordinateFrame,
                      let receipt = ([camera.state.latest].compactMap { $0 } + camera.state.recent).first(where: { $0.id == input.receiptID }),
                      input.sha256 == receipt.sha256, input.sourceSession == receipt.sourceSession,
                      input.sourceSession == camera.state.latest?.sourceSession, input.sequence == receipt.sequence,
                      input.origin == receipt.origin, input.origin == observation.inputOrigin,
                      abs(input.receivedAt.timeIntervalSince(receipt.receivedAt)) < 0.002,
                      abs(input.poseObservedAt.timeIntervalSince(pose.observedAt)) < 0.002,
                      input.receivedAt <= now, observation.validUntil <= receipt.receivedAt.addingTimeInterval(1.252),
                      observation.validUntil <= pose.validUntil else { return false }
            }
            return true
        }
        return value
    }
}

struct CameraTwinPersonDisplay: Identifiable {
    let id: String; let position: SIMD3<Double>; let observedAt: Date; let validUntil: Date
}

extension StationaryCameraModel {
    var twinFresh: Bool { ProcessInfo.processInfo.systemUptime - twinUpdatedUptime < 3 }
    var visibleTwinCameras: [CameraTwinSnapshot.Camera] { twinFresh ? (twin?.cameras ?? []) : [] }
    func chooseCamera(_ id: String) async {
        guard twin?.cameras.contains(where: { $0.id == id }) == true, id != selectedCameraID else { return }
        selectedCameraID = id; showLive()
        if let camera = twin?.cameras.first(where: { $0.id == id }) { apply(camera.state); applyPose(camera.pose) }
        await refresh()
    }
    func refreshTwin() async {
        guard profile != nil else { return }
        twinGeneration &+= 1
        let generation = twinGeneration
        do {
            let bytes = try await get(path: "/v1/twin", limit: 8 << 20, contentType: "application/json")
            let next = try CameraTwinSnapshot.decode(bytes)
            guard !Task.isCancelled, generation == twinGeneration else { return }
            var nextURL = roomURL, nextDigest = roomDigest
            if next.modelAvailable, let site = next.site, roomDigest != site.modelSHA256 {
                let data = try await get(path: "/v1/site/model", limit: 128 << 20, contentType: "application/vnd.usdz+zip")
                guard !Task.isCancelled, generation == twinGeneration,
                      SHA256.hash(data: data).map({ String(format: "%02x", $0) }).joined() == site.modelSHA256 else { throw StationaryCameraError.evidence }
                let folder = FileManager.default.temporaryDirectory.appendingPathComponent("wendy-world-room-" + UUID().uuidString)
                try FileManager.default.createDirectory(at: folder, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
                let file = folder.appendingPathComponent("model.usdz")
                try data.write(to: file, options: .atomic)
                nextURL = file; nextDigest = site.modelSHA256
            } else if !next.modelAvailable { nextURL = nil; nextDigest = nil }
            var images: [String: NSImage] = [:]
            for camera in next.cameras where camera.state.fresh {
                guard !Task.isCancelled, generation == twinGeneration else { return }
                if let receipt = camera.state.latest, receipt.retainedUntil > Date(),
                   let image = try? await load(receipt, cameraID: camera.id) { images[camera.id] = image }
            }
            guard !Task.isCancelled, generation == twinGeneration else { return }
            let oldSite = twin?.site
            if oldSite?.id != next.site?.id || oldSite?.modelSHA256 != next.site?.modelSHA256 || oldSite?.modelRevision != next.site?.modelRevision {
                floorY = nil; peopleTracks = []; lastPeopleBatch = nil
                await peopleEngine.reset()
            }
            guard !Task.isCancelled, generation == twinGeneration else { return }
            if let old = roomURL, old != nextURL { try? FileManager.default.removeItem(at: old.deletingLastPathComponent()) }
            // Publish a snapshot only with the images verified against its own receipts.
            twinImages = images; roomURL = nextURL; roomDigest = nextDigest; twin = next
            twinUpdatedUptime = ProcessInfo.processInfo.systemUptime; twinMessage = nil
            if !next.cameras.contains(where: { $0.id == selectedCameraID }) { selectedCameraID = next.cameras.first?.id }
        } catch StationaryCameraError.http(404) {
            guard generation == twinGeneration else { return }
            twin = nil; twinImages = [:]; peopleTracks = []; roomURL = nil
        } catch {
            guard generation == twinGeneration else { return }
            twinUpdatedUptime = -.infinity; twinImages = [:]; peopleTracks = []
            twinMessage = "The room connection is unavailable. Live spatial overlays are hidden."
        }
    }
    func setAutomaticAlignment(_ enabled: Bool) async {
        do {
            let body = try JSONSerialization.data(withJSONObject: ["enabled": enabled])
            _ = try await get(path: "/v1/pose/automatic", limit: 4096, contentType: "application/json", method: "POST", body: body)
            await refreshTwin()
        } catch { twinMessage = "Automatic alignment could not be changed. Refresh to check its state." }
    }
    func refreshPeople() async {
        guard !peopleProcessing else { return }
        peopleProcessing = true
        defer { peopleProcessing = false }
        guard showPeople, twinFresh else {
            peopleTracks = []; await peopleEngine.reset()
            if var batch = lastPeopleBatch {
                batch.observations = []
                _ = try? await get(path: "/v1/twin/observations", limit: 1 << 20, contentType: "application/json", method: "POST", body: batch.encoded(), statuses: [200,202])
                lastPeopleBatch = nil
            }
            peopleMessage = showPeople ? "Choose a horizontal floor surface." : "People tracking is off."
            return
        }
        guard let floorY else {
            await refreshSharedPeople()
            return
        }
        let generation = twinGeneration
        let context = (twin?.site?.id ?? "") + ":" + (twin?.site?.modelRevision ?? "") + ":" + (twin?.site?.modelSHA256 ?? "") + ":" + String(floorY)
        let frames: [CameraTwinPeopleFrame] = visibleTwinCameras.prefix(8).compactMap { camera in
            guard let receipt = camera.state.latest, let pose = camera.currentPose(),
                  let image = twinImages[camera.id]?.cgImage(forProposedRect: nil, context: nil, hints: nil) else { return nil }
            let evidence = CameraTwinPeopleEvidence(cameraID: camera.id, receiptID: receipt.id, sha256: receipt.sha256,
                sourceSession: receipt.sourceSession, sequence: receipt.sequence, receivedAt: receipt.receivedAt,
                origin: receipt.origin, poseObservedAt: pose.observedAt)
            let lens = CameraTwinPeopleLens(width: receipt.width, height: receipt.height, fx: pose.lens.fx, fy: pose.lens.fy,
                cx: pose.lens.cx, cy: pose.lens.cy, distortion: pose.lens.distortion)
            let calibration = CameraTwinPeopleCalibration(coordinateFrame: camera.pose.coordinateFrame,
                siteID: camera.pose.site.id, modelRevision: camera.pose.site.modelRevision, modelSHA256: camera.pose.site.modelSHA256,
                worldFromCamera: pose.worldFromCamera, lens: lens, poseObservedAt: pose.observedAt, poseValidUntil: pose.validUntil)
            return CameraTwinPeopleFrame(image: image, receipt: evidence, calibration: calibration)
        }
        let sourceContext = frames.map { $0.receipt.cameraID + ":" + $0.receipt.sourceSession + ":" + $0.receipt.origin }.joined(separator: "|")
        let result = await peopleEngine.process(frames: frames, floorY: floorY)
        guard !Task.isCancelled, showPeople, twinFresh,
              generation == twinGeneration,
              context == (twin?.site?.id ?? "") + ":" + (twin?.site?.modelRevision ?? "") + ":" + (twin?.site?.modelSHA256 ?? "") + ":" + String(self.floorY ?? .nan) else { return }
        let currentSourceContext = visibleTwinCameras.prefix(8).compactMap { camera -> String? in
            guard camera.currentPose() != nil, let receipt = camera.state.latest, twinImages[camera.id] != nil else { return nil }
            return camera.id + ":" + receipt.sourceSession + ":" + receipt.origin
        }.joined(separator: "|")
        guard sourceContext == currentSourceContext else { peopleTracks = []; return }
        peopleMessage = frames.isEmpty ? "Align a fresh camera to track people." : "Anonymous estimates require visible feet."

        if let batch = result.observations {
            lastPeopleBatch = batch
            do {
                _ = try await get(path: "/v1/twin/observations", limit: 1 << 20, contentType: "application/json", method: "POST", body: batch.encoded(), statuses: [200,202])
            } catch { peopleMessage += " · shared publication unavailable" }
        }
        await refreshSharedPeople()
    }
    func refreshSharedPeople() async {
        guard showPeople, twinFresh, let twin else { peopleTracks = []; return }
        let generation = twinGeneration
        do {
            let data = try await get(path: "/v1/twin/observations", limit: 1 << 20, contentType: "application/json")
            let batch = try CameraTwinSharedObservations.decode(data, twin: twin)
            guard showPeople, twinFresh, generation == twinGeneration else { return }
            peopleTracks = batch.observations.filter { $0.validUntil > Date() }.map {
                CameraTwinPersonDisplay(id: $0.id, position: SIMD3($0.position[0],$0.position[1],$0.position[2]), observedAt: $0.observedAt, validUntil: $0.validUntil)
            }
            peopleMessage = "\(peopleTracks.count) shared anonymous estimates" + (floorY == nil ? " · choose floor to contribute" : " · feet must be visible")
        } catch {
            guard generation == twinGeneration else { return }
            peopleTracks = []; peopleMessage = "Shared people observations are unavailable."
        }
    }
    func selectFloor(_ y: Double) {
        guard y.isFinite, abs(y) <= 1000 else { return }
        floorY = y; choosingFloor = false; peopleTracks = []; lastPeopleContext = nil
    }
}

struct CameraTwinToolbar: View {
    @ObservedObject var model: StationaryCameraModel
    var body: some View {
        HStack(spacing: 16) {
            Picker("View", selection: $model.showRoom) { Text("Room").tag(true); Text("Camera").tag(false) }
                .pickerStyle(.segmented).frame(width: 170)
            Toggle("Camera color", isOn: $model.showCameraColor).toggleStyle(.checkbox).disabled(model.roomURL == nil)
            Toggle("People", isOn: $model.showPeople).toggleStyle(.checkbox).disabled(model.roomURL == nil)
            if model.showPeople {
                Button(model.floorY == nil ? "Choose floor" : "Change floor") { model.choosingFloor = true; model.showRoom = true }
                    .buttonStyle(.bordered)
            }
            Spacer(minLength: 0)
            Text("\(model.visibleTwinCameras.filter { $0.currentPose() != nil }.count) aligned")
                .font(.system(size: 11)).foregroundStyle(.secondary)
        }.font(.system(size: 11))
    }
}

struct CameraTwinRoomPanel: View {
    @ObservedObject var model: StationaryCameraModel
    var body: some View {
        ZStack(alignment: .topLeading) {
            RoundedRectangle(cornerRadius: 14).fill(Color(red: 0.89, green: 0.92, blue: 0.90))
            if let capture = model.roomCapture {
                Image(nsImage: capture).resizable().scaledToFill().clipped().clipShape(RoundedRectangle(cornerRadius: 14))
            } else if let url = model.roomURL {
                CameraTwinScene(url: url, scale: model.twin?.site?.metresPerModelUnit ?? 1,
                    cameras: model.visibleTwinCameras, images: model.twinImages,
                    selected: model.selectedCameraID, color: model.showCameraColor,
                    people: model.showPeople && model.twinFresh ? model.peopleTracks : [],
                    choosingFloor: model.choosingFloor, chooseFloor: { model.selectFloor($0) })
                    .clipShape(RoundedRectangle(cornerRadius: 14))
            } else {
                VStack(spacing: 10) {
                    Image(systemName: "square.3.layers.3d").font(.system(size: 32, weight: .light))
                    Text("Open a room scan to place your cameras").font(.system(size: 16, weight: .medium))
                    Text("Scan on iPhone in Wendy, share the site, then import it when launching World.")
                        .font(.system(size: 12)).foregroundStyle(.secondary)
                }.frame(maxWidth: .infinity, maxHeight: .infinity)
            }
            VStack(alignment: .leading, spacing: 6) {
                Text(model.choosingFloor ? "Click a horizontal floor surface" : "SCAN COORDINATES · METRES")
                    .font(.system(size: 9, weight: .semibold)).tracking(1)
                if model.showCameraColor { Text("Experimental image projection onto saved geometry").font(.system(size: 10)) }
                if model.showPeople { Text(model.peopleMessage).font(.system(size: 10)) }
                if let message = model.twinMessage { Text(message).font(.system(size: 10)).foregroundStyle(.orange) }
            }.padding(12).background(.regularMaterial, in: RoundedRectangle(cornerRadius: 9)).padding(12)
            VStack {
                Spacer()
                HStack {
                    Text("Drag to orbit · Scroll to zoom").font(.system(size: 10))
                    Spacer()
                    Text("Cones show viewing angle, not sensing range").font(.system(size: 10))
                }.padding(10).background(.regularMaterial)
            }.clipShape(RoundedRectangle(cornerRadius: 14))
        }.frame(minHeight: 250)
    }
}

enum CameraTwinGeometry {
    static func matrix(_ values: [Double]) -> simd_float4x4? {
        guard values.count == 16, values.allSatisfy(\.isFinite) else { return nil }
        return simd_float4x4(columns: (SIMD4(values[0..<4].map(Float.init)), SIMD4(values[4..<8].map(Float.init)), SIMD4(values[8..<12].map(Float.init)), SIMD4(values[12..<16].map(Float.init))))
    }
    // Same Brown-Conrady optical convention as Wendy's SiteCameraLens.
    static func ray(x: Double, y: Double, lens: StationaryCameraPoseState.Lens) -> SIMD3<Float>? {
        guard lens.fx > 0, lens.fy > 0, lens.distortion.count == 5 else { return nil }
        let xd = (x - lens.cx) / lens.fx, yd = (y - lens.cy) / lens.fy
        var u = xd, v = yd
        let d = lens.distortion
        for _ in 0..<40 {
            let r = u*u + v*v, radial = 1 + d[0]*r + d[1]*r*r + d[4]*r*r*r
            guard radial.isFinite, radial > 0.01 else { return nil }
            let nextU = (xd - 2*d[2]*u*v - d[3]*(r+2*u*u)) / radial
            let nextV = (yd - d[2]*(r+2*v*v) - 2*d[3]*u*v) / radial
            u = nextU; v = nextV
        }
        let r = u*u + v*v, radial = 1 + d[0]*r + d[1]*r*r + d[4]*r*r*r
        let errorX = (u*radial + 2*d[2]*u*v + d[3]*(r+2*u*u) - xd)*lens.fx
        let errorY = (v*radial + d[2]*(r+2*v*v) + 2*d[3]*u*v - yd)*lens.fy
        guard u.isFinite, v.isFinite, abs(u) < 100, abs(v) < 100,
              errorX.isFinite, errorY.isFinite, hypot(errorX,errorY) < 0.1 else { return nil }
        return SIMD3(Float(u), Float(v), 1)
    }
    static func lines(_ points: [SIMD3<Float>], edges: [Int32], color: NSColor) -> SCNNode {
        let source = SCNGeometrySource(vertices: points.map(SCNVector3.init))
        let element = SCNGeometryElement(indices: edges, primitiveType: .line)
        let shape = SCNGeometry(sources: [source], elements: [element])
        let material = SCNMaterial(); material.lightingModel = .constant; material.diffuse.contents = color
        shape.materials = [material]; return SCNNode(geometry: shape)
    }
}

struct CameraTwinScene: NSViewRepresentable {
    let url: URL; let scale: Double; let cameras: [CameraTwinSnapshot.Camera]; let images: [String: NSImage]
    let selected: String?; let color: Bool; let people: [CameraTwinPersonDisplay]
    let choosingFloor: Bool; let chooseFloor: (Double) -> Void
    @MainActor static func capture(url: URL, scale: Double, cameras: [CameraTwinSnapshot.Camera], images: [String: NSImage], selected: String?, color: Bool, people: [CameraTwinPersonDisplay]) -> NSImage? {
        let input = CameraTwinScene(url: url, scale: scale, cameras: cameras, images: images, selected: selected, color: color, people: people, choosingFloor: false, chooseFloor: { _ in })
        let view = SCNView(frame: NSRect(x: 0,y: 0,width: 1100,height: 360))
        let coordinator = Coordinator(); coordinator.view = view; coordinator.update(input)
        guard let scene = view.scene, let camera = view.pointOfView else { return nil }
        scene.background.contents = NSColor(red: 0.89,green: 0.92,blue: 0.90,alpha: 1)
        let renderer = SCNRenderer(device: nil, options: nil); renderer.scene = scene; renderer.pointOfView = camera
        return renderer.snapshot(atTime: 0, with: NSSize(width: 1100,height: 360), antialiasingMode: .multisampling4X)
    }
    func makeCoordinator() -> Coordinator { Coordinator() }
    func makeNSView(context: Context) -> SCNView {
        let view = SCNView(); view.allowsCameraControl = true; view.autoenablesDefaultLighting = false
        view.backgroundColor = NSColor(red: 0.89, green: 0.92, blue: 0.90, alpha: 1)
        view.antialiasingMode = .multisampling4X
        let tap = NSClickGestureRecognizer(target: context.coordinator, action: #selector(Coordinator.clicked(_:)))
        view.addGestureRecognizer(tap); context.coordinator.view = view
        return view
    }
    func updateNSView(_ view: SCNView, context: Context) { context.coordinator.update(self) }
    @MainActor final class Coordinator: NSObject {
        weak var view: SCNView?
        var scene = SCNScene(), room = SCNNode(), annotations = SCNNode(), projection = SCNNode()
        var loadedURL: URL?; var colorKey = ""; var lastColorTime: TimeInterval = -.infinity
        var choosingFloor = false; var chooseFloor: ((Double) -> Void)?
        func update(_ input: CameraTwinScene) {
            guard let view else { return }
            choosingFloor = input.choosingFloor; chooseFloor = input.chooseFloor
            if loadedURL != input.url {
                guard let source = try? SCNScene(url: input.url, options: [.checkConsistency: true, .convertToYUp: true, .convertUnitsToMeters: 1.0]) else { return }
                scene = SCNScene(); room = SCNNode(); annotations = SCNNode(); projection = SCNNode()
                room.addChildNode(source.rootNode.clone())
                room.enumerateChildNodes { node, _ in
                    node.camera = nil; node.light = nil; node.removeAllActions()
                    if let original = node.geometry, let geometry = original.copy() as? SCNGeometry {
                        geometry.materials = original.materials.map { source in
                            guard source.diffuse.contents == nil || source.diffuse.contents is NSColor,
                                  let material = source.copy() as? SCNMaterial else { return source }
                            material.lightingModel = .lambert
                            material.specular.contents = NSColor.black
                            return material
                        }
                        node.geometry = geometry
                    }
                }
                room.simdScale = SIMD3(repeating: Float(input.scale))
                scene.rootNode.addChildNode(room); scene.rootNode.addChildNode(annotations); scene.rootNode.addChildNode(projection)
                let bounds = room.boundingBox
                let low = SIMD3<Float>(bounds.min) * Float(input.scale), high = SIMD3<Float>(bounds.max) * Float(input.scale)
                let center = (low + high)/2
                let direction = simd_normalize(SIMD3<Float>(0.65,0.85,0.8))
                let right = simd_normalize(simd_cross(SIMD3<Float>(0,1,0),direction)), up = simd_cross(direction,right)
                let aspect = Float(view.bounds.height > 1 ? view.bounds.width/view.bounds.height : 1100.0/360.0)
                let tangent = tan(Float(42.0 * Double.pi / 360.0))
                var distance: Float = 1
                for x in [low.x,high.x] { for y in [low.y,high.y] { for z in [low.z,high.z] {
                    let point = SIMD3(x,y,z)-center
                    let projected = max(abs(simd_dot(point,right))/(tangent*max(0.5,aspect)), abs(simd_dot(point,up))/tangent)
                    distance = max(distance,simd_dot(point,direction)+projected*1.14)
                } } }
                let camera = SCNNode(); camera.camera = SCNCamera(); camera.camera?.zFar = 10000
                camera.camera?.projectionDirection = .vertical; camera.camera?.fieldOfView = 42
                camera.simdPosition = center + direction*distance
                camera.look(at: SCNVector3(center)); scene.rootNode.addChildNode(camera)
                let ambient = SCNNode(); ambient.light = SCNLight(); ambient.light?.type = .ambient
                ambient.light?.intensity = 260; ambient.light?.color = NSColor.white
                scene.rootNode.addChildNode(ambient)
                let key = SCNNode(); key.light = SCNLight(); key.light?.type = .directional
                key.light?.intensity = 680; key.light?.color = NSColor(red:1,green:0.97,blue:0.92,alpha:1)
                key.eulerAngles = SCNVector3(-0.8,-0.65,0); scene.rootNode.addChildNode(key)
                let fill = SCNNode(); fill.light = SCNLight(); fill.light?.type = .directional
                fill.light?.intensity = 180; fill.light?.color = NSColor(red:0.8,green:0.89,blue:1,alpha:1)
                fill.eulerAngles = SCNVector3(-0.4,2.5,0); scene.rootNode.addChildNode(fill)
                view.scene = scene; view.pointOfView = camera; view.defaultCameraController.target = SCNVector3(center)
                loadedURL = input.url; colorKey = ""
            }
            annotations.childNodes.forEach { $0.removeFromParentNode() }
            for camera in input.cameras {
                guard let pose = camera.currentPose(), let receipt = camera.state.latest,
                      let transform = CameraTwinGeometry.matrix(pose.worldFromCamera) else { continue }
                let tint: NSColor = pose.lens.usesSharedOrAssumedLens ? .systemOrange : .systemTeal
                let node = SCNNode(); node.simdTransform = transform
                let marker = SCNNode(geometry: SCNSphere(radius: camera.id == input.selected ? 0.10 : 0.07))
                marker.geometry?.firstMaterial?.diffuse.contents = tint; node.addChildNode(marker)
                var corners: [SIMD3<Float>] = [.zero]
                for (x,y) in [(0.0,0.0),(Double(receipt.width),0),(Double(receipt.width),Double(receipt.height)),(0,Double(receipt.height))] {
                    if let ray = CameraTwinGeometry.ray(x: x, y: y, lens: pose.lens) { corners.append(ray * 0.75) }
                }
                if corners.count == 5 { node.addChildNode(CameraTwinGeometry.lines(corners, edges: [0,1,0,2,0,3,0,4,1,2,2,3,3,4,4,1], color: tint)) }
                annotations.addChildNode(node)
            }
            for person in input.people where person.validUntil > Date() && (0...1.25).contains(Date().timeIntervalSince(person.observedAt)) {
                let node = SCNNode(geometry: SCNCapsule(capRadius: 0.15, height: 0.7))
                node.simdPosition = SIMD3<Float>(person.position) + SIMD3(0,0.35,0)
                node.geometry?.firstMaterial?.diffuse.contents = NSColor.systemIndigo.withAlphaComponent(0.8)
                annotations.addChildNode(node)
                let halo = SCNNode(geometry: SCNTorus(ringRadius: 0.24, pipeRadius: 0.015))
                halo.simdPosition = SIMD3<Float>(person.position) + SIMD3(0,0.015,0)
                halo.geometry?.firstMaterial?.diffuse.contents = NSColor.systemIndigo; annotations.addChildNode(halo)
            }
            let eligible = input.color ? input.cameras.filter { $0.currentPose() != nil && input.images[$0.id] != nil } : []
            let key = eligible.map { $0.id + ($0.state.latest?.sha256 ?? "") + ($0.pose.pose?.observedAt.description ?? "") }.joined()
            if key != colorKey, key.isEmpty || ProcessInfo.processInfo.systemUptime - lastColorTime > 1.5 {
                projection.childNodes.forEach { $0.removeFromParentNode() }
                for camera in eligible.prefix(8) { if let image = input.images[camera.id] { project(camera: camera, image: image) } }
                colorKey = key; lastColorTime = ProcessInfo.processInfo.systemUptime
            }
            SCNTransaction.flush()
        }
        @objc func clicked(_ gesture: NSClickGestureRecognizer) {
            guard choosingFloor, let view else { return }
            let hits = view.hitTest(gesture.location(in: view), options: [.rootNode: room, .backFaceCulling: false, .searchMode: SCNHitTestSearchMode.all.rawValue])
            if let hit = hits.first(where: { abs($0.worldNormal.y) > 0.8 }) { chooseFloor?(Double(hit.worldCoordinates.y)) }
        }
        // Raycast the saved mesh before placing each color sample. This preserves
        // first-surface occlusion; discontinuous cells are omitted instead of bridging walls.
        func project(camera: CameraTwinSnapshot.Camera, image: NSImage) {
            guard let pose = camera.currentPose(), let receipt = camera.state.latest,
                  let matrix = CameraTwinGeometry.matrix(pose.worldFromCamera) else { return }
            let origin = matrix.columns.3.xyz
            let columns = 28, rows = 21
            var points = [SIMD3<Float>?](repeating: nil, count: (columns+1)*(rows+1))
            for y in 0...rows { for x in 0...columns {
                guard let ray = CameraTwinGeometry.ray(x: Double(x)*Double(receipt.width)/Double(columns), y: Double(y)*Double(receipt.height)/Double(rows), lens: pose.lens) else { continue }
                let direction = simd_normalize((matrix * SIMD4(ray, 0)).xyz)
                let hits = room.hitTestWithSegment(from: room.convertPosition(SCNVector3(origin + direction*0.03), from: nil), to: room.convertPosition(SCNVector3(origin + direction*15), from: nil), options: [SCNHitTestOption.backFaceCulling.rawValue: false, SCNHitTestOption.searchMode.rawValue: SCNHitTestSearchMode.closest.rawValue])
                if let hit = hits.first { points[y*(columns+1)+x] = SIMD3<Float>(hit.worldCoordinates) - direction*0.008 }
            } }
            var vertices: [SCNVector3] = [], uv: [CGPoint] = [], indices: [Int32] = []
            for y in 0..<rows { for x in 0..<columns {
                let corners = [(x,y),(x+1,y),(x+1,y+1),(x,y+1)]
                let positions = corners.compactMap { points[$0.1*(columns+1)+$0.0] }
                guard positions.count == 4, positions.dropFirst().allSatisfy({ simd_distance(positions[0], $0) < 1 }) else { continue }
                let start = Int32(vertices.count)
                vertices += positions.map(SCNVector3.init)
                uv += corners.map { CGPoint(x: Double($0.0)/Double(columns), y: 1-Double($0.1)/Double(rows)) }
                indices += [start,start+1,start+2,start,start+2,start+3]
            } }
            guard !vertices.isEmpty else { return }
            let geometry = SCNGeometry(sources: [SCNGeometrySource(vertices: vertices), SCNGeometrySource(textureCoordinates: uv)], elements: [SCNGeometryElement(indices: indices, primitiveType: .triangles)])
            let material = SCNMaterial(); material.diffuse.contents = image; material.lightingModel = .constant
            material.isDoubleSided = true; material.transparency = 0.88; material.writesToDepthBuffer = false
            geometry.materials = [material]; projection.addChildNode(SCNNode(geometry: geometry))
        }
    }
}

private extension SIMD4 where Scalar == Float {
    var xyz: SIMD3<Float> { SIMD3(x,y,z) }
}
