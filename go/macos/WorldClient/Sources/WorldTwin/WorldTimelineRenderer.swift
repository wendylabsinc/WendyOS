import AppKit
import Foundation
import SceneKit
import SwiftUI

struct PromoFailure: Error, LocalizedError {
    let message: String
    var errorDescription: String? { message }
}
struct PromoAction: Decodable {
    let kind: String
    let target: String?
    let enabled: Bool?
    let delta_x: Double?
    let zoom_delta: Double?
    let duration_s: Double?
}
struct PromoChapter: Decodable { let title: String; let detail: String?;let series:String? }
struct PromoPointer: Decodable { let x: Double; let y: Double; let label: String? }
struct PromoInspectionRoute: Decodable {
    let run_id: String; let original_envelope_sha256: String
    let observed: [Double]; let target: [Double]; let route: [[Double]]
}
struct PromoFrame: Decodable {
    let at_s: Double
    let state: DemoState
    let assets: [String: String]?
    let actions: [PromoAction]?
    let chapter: PromoChapter?
    let pointer: PromoPointer?
    let inspection_route: PromoInspectionRoute?
}
struct PromoMotion {
    let start: Double; let duration: Double; let dx: Double; let zoom: Double
    var progress: Double = 0
}
struct PromoFrameReceipt: Encodable {
    let frame: Int; let at_s: Double; let timeline_index: Int
    let state_sequence: Int; let simulation_time_s: Double
    let replica: String; let camera: String; let loaded_thumbnails: Int
    let tracking_thumbnails: Int; let visible_tracks: Int
    let tracks_layer_enabled: Bool; let camera_sources: [String: Bool]
    let selected_original: String?; let evidence_references: Int; let evidence_sheet: Bool
    let verified_media_preview: Bool
    let inspection_route_available: Bool; let inspection_route_visible: Bool
}
struct PromoReceipt: Encodable {
    let schema = "wendy.native-promo.render.v1"
    let status = "complete"
    let width = 1920; let height = 1080
    let fps: Int; let frame_count: Int; let run_id: String
    let visible_windows = 0; let live_commands = 0
    let scope = "Recorded simulator states rendered through the actual native model, scene renderer and UI"
    let frames: [PromoFrameReceipt]
    let timing_seconds: [String: Double]
}

struct PromoContent: View {
    @ObservedObject var model: DemoModel
    let scene: NSImage
    let chapter: PromoChapter?
    let pointer: PromoPointer?
    let pointerAge: Double
    let progress: Double
    private let teal = Color(red: 0.12, green: 0.47, blue: 0.43)
    var body: some View {
        ZStack(alignment: .topLeading) {
            DemoContent(presentsEvidenceSheet: false, model: model, capturedScene: scene).preferredColorScheme(.light)
            VStack {
                HStack(spacing: 7) {
                    Circle().fill(teal).frame(width: 5, height: 5)
                    Text("MUJOCO 4 · RECORDED SIMULATION")
                        .font(.system(size: 9, weight: .semibold)).tracking(1.1)
                }.foregroundStyle(teal).padding(.horizontal, 13).padding(.vertical, 7)
                    .background(Color.white.opacity(0.96), in: Capsule())
                Spacer()
                if let chapter {
                    VStack(alignment: .leading, spacing: 7) {
                        Text("WENDY WORLD / "+(chapter.series ?? "DISTRIBUTED FUSION"))
                            .font(.system(size: 9, weight: .semibold)).tracking(1.6).foregroundStyle(teal)
                        Text(chapter.title).font(.system(size: 27, weight: .semibold, design: .rounded))
                            .foregroundStyle(Color(red: 0.14, green: 0.22, blue: 0.24))
                        if let detail = chapter.detail {
                            Text(detail).font(.system(size: 13)).foregroundStyle(Color(red: 0.35, green: 0.42, blue: 0.43))
                                .fixedSize(horizontal: false, vertical: true)
                        }
                        GeometryReader { geometry in
                            Capsule().fill(teal.opacity(0.12)).overlay(alignment: .leading) {
                                Capsule().fill(teal).frame(width: geometry.size.width * progress)
                            }
                        }.frame(height: 3).padding(.top, 5)
                    }.padding(22).frame(maxWidth: .infinity, alignment: .leading)
                        .background(Color.white.opacity(0.97), in: RoundedRectangle(cornerRadius: 17))
                        .overlay(RoundedRectangle(cornerRadius: 17).stroke(teal.opacity(0.15)))
                        .shadow(color: .black.opacity(0.09), radius: 18, y: 7)
                }
            }.padding(.leading, 318).padding(.trailing, 285).padding(.top, 86).padding(.bottom, 63)
            if let selection = model.presentedEvidence {
                DemoNativeEvidenceSheet(selection:selection,media:model.nativeMedia)
                    .clipShape(RoundedRectangle(cornerRadius:17))
                    .overlay(RoundedRectangle(cornerRadius:17).stroke(teal.opacity(0.15)))
                    .shadow(color:.black.opacity(0.2),radius:24,y:8)
                    .position(x:1000,y:470)
            }
            if let digest=model.patrolEvidenceDigest {
                PatrolEvidenceSheet(digest:digest,session:model.session)
                    .clipShape(RoundedRectangle(cornerRadius:17))
                    .overlay(RoundedRectangle(cornerRadius:17).stroke(teal.opacity(0.15)))
                    .shadow(color:.black.opacity(0.2),radius:24,y:8)
                    .position(x:1000,y:470)
            }
            if let pointer, pointerAge >= 0 && pointerAge < 2.8 {
                let pulse = min(1, pointerAge / 0.65)
                ZStack {
                    Circle().stroke(teal.opacity(1 - pulse), lineWidth: 3)
                        .frame(width: 18 + pulse * 43, height: 18 + pulse * 43)
                    Circle().fill(Color.white).frame(width: 17, height: 17)
                        .overlay(Circle().stroke(teal, lineWidth: 3))
                        .shadow(color: .black.opacity(0.18), radius: 5, y: 2)
                }.position(x: pointer.x, y: pointer.y)
                if let label = pointer.label {
                    Text(label).font(.system(size: 12, weight: .semibold)).foregroundStyle(.white)
                        .padding(.horizontal, 13).padding(.vertical, 9)
                        .background(teal, in: Capsule())
                        .position(x: min(1740, max(150, pointer.x + 100)), y: min(1020, pointer.y + 37))
                }
            }
        }.frame(width: 1920, height: 1080).environment(\.colorScheme, .light)
    }
}

@MainActor struct WorldTimelineRenderer {
    static func require(_ condition: Bool, _ message: String) throws {
        if !condition { throw PromoFailure(message: message) }
    }
    static func relative(_ name: String, under root: URL) throws -> URL {
        let parts = name.split(separator: "/", omittingEmptySubsequences: false)
        try require(!name.hasPrefix("/") && !name.contains("\\") && !parts.isEmpty
                    && parts.allSatisfy { !$0.isEmpty && $0 != "." && $0 != ".." }, "invalid relative asset path")
        return root.appendingPathComponent(name)
    }
    static func indicator(_ action: PromoAction, model: DemoModel, renderer: SceneRenderer) -> PromoPointer? {
        if action.kind == "select_scene", let name=action.target,
           let node=renderer.scene.rootNode.childNode(withName:name,recursively:true) {
            let point=renderer.view.projectPoint(node.worldPosition)
            return PromoPointer(x:287+Double(point.x),y:79+966-Double(point.y),label:"Select observation")
        }
        if action.kind == "open_evidence" { return PromoPointer(x:1660,y:185,label:"Open supporting evidence") }
        if action.kind == "close_evidence" { return PromoPointer(x:1397,y:182,label:"Return to the twin") }
        if action.kind == "evidence_details" { return PromoPointer(x:870,y:525,label:"Inspect capture provenance") }
        if !model.showsRobotControls {
            switch action.kind {
            case "camera":
                return PromoPointer(x: action.target == "Floor plan" ? 1605 : 1535, y: 117, label: action.target)
            case "layer":
                guard let target = action.target,
                      let index = model.availableLayers.firstIndex(where: { $0.rawValue == target }) else { return nil }
                let width = (target as NSString).size(withAttributes:[.font:NSFont.systemFont(ofSize:12,weight:.medium)]).width
                return PromoPointer(x:89+Double(width),y:215+29*Double(index),label:target)
            default: return nil
            }
        }
        switch action.kind {
        case "camera":
            let positions: [String: Double] = ["Overview": 1452, "Floor plan": 1524, "Follow robot": 1604]
            let x = positions[action.target ?? ""] ?? 1490
            return PromoPointer(x: x, y: 116, label: action.target)
        case "layer":
            guard let index = DemoLayer.allCases.firstIndex(where: { $0.rawValue == action.target }) else { return nil }
            let positions: [Double] = [115, 132, 143, 153, 125, 137, 110, 145, 121]
            guard index<positions.count else{return nil}
            return PromoPointer(x: positions[index], y: 286 + Double(index) * 29, label: action.target)
        case "panel": return PromoPointer(x: action.target == "Network" ? 188 : 112, y: 196, label: action.target)
        case "follow_input": return PromoPointer(x: 980, y: 450, label: "Orbit the recorded view")
        default: return nil
        }
    }
    static func apply(_ action: PromoAction, model: DemoModel, renderer: SceneRenderer,
                      at time: Double, motions: inout [PromoMotion]) throws {
        switch action.kind {
        case "camera":
            guard let preset = CameraPreset(rawValue: action.target ?? "") else { throw PromoFailure(message: "invalid camera") }
            model.setCamera(preset)
            if preset == .robot {
                motions.append(PromoMotion(start: time + 0.5, duration: 12, dx: 45, zoom: -8))
            } else { motions.removeAll() }
        case "layer":
            guard let layer = DemoLayer(rawValue: action.target ?? ""), let enabled = action.enabled else { throw PromoFailure(message: "invalid layer") }
            model.setLayer(layer, enabled: enabled)
        case "panel":
            try require(["Layers", "Network"].contains(action.target ?? ""), "invalid panel")
            model.panel = action.target!
        case "select_robot":
            try require(model.snapshot?.robots.contains { $0.id == action.target } == true, "invalid robot")
            model.selectedRobot = action.target!
        case "select_scene":
            renderer.update(model)
            guard let name=action.target, let node=renderer.scene.rootNode.childNode(withName:name,recursively:true),
                  renderer.select(node:node) else { throw PromoFailure(message:"recorded scene selection unavailable") }
        case "open_patrol_evidence":
            guard let digest=action.target,model.patrolMode else {throw PromoFailure(message:"patrol evidence unavailable")}
            _=try PatrolEvidenceFile.load(digest,session:model.session)
            model.patrolEvidenceDigest=digest
        case "close_patrol_evidence": model.patrolEvidenceDigest=nil
        case "open_evidence":
            try require(model.selectedOriginal != nil,"recorded original is not selected")
            model.openSelectedEvidence()
        case "close_evidence":
            model.presentedEvidence=nil
            model.nativeMedia.dismiss()
        case "evidence_details":
            try require(model.presentedEvidence != nil,"recorded evidence is not open")
            model.nativeMedia.showsSourceDetails = action.enabled ?? true
        case "follow_input":
            let dx = action.delta_x ?? 0, zoom = action.zoom_delta ?? 0, duration = action.duration_s ?? 0
            try require(model.camera == .robot && dx.isFinite && zoom.isFinite && duration.isFinite
                        && abs(dx) <= 500 && abs(zoom) <= 500 && duration >= 0 && duration <= 20, "invalid camera gesture")
            renderer.update(model)
            if duration == 0 { renderer.followInput(horizontal: dx, zoom: zoom) }
            else { motions.append(PromoMotion(start: time, duration: duration, dx: dx, zoom: zoom)) }
        default: throw PromoFailure(message: "unknown presentation action")
        }
    }
    static func render(_ request: WorldTimelineRenderRequest) async throws {
        do {
            try await renderFrames(request)
        } catch {
            try? writeFailure(error, output: request.output)
            throw error
        }
    }

    static func writeFailure(_ error: Error, output: URL) throws {
        struct Failure: Encodable {
            let schema = "wendy.native-promo.render-error.v1"
            let status: String
            let error: String
        }
        let cancelled = error is CancellationError
        let message = cancelled ? "Export cancelled when its workspace task ended." : error.localizedDescription
        let result = Failure(status: cancelled ? "cancelled" : "failed", error: String(message.prefix(1024)))
        try FileManager.default.createDirectory(at: output, withIntermediateDirectories: true)
        try JSONEncoder().encode(result).write(to: output.appendingPathComponent("render-error.json"), options: .atomic)
    }

    private static func renderFrames(_ request: WorldTimelineRenderRequest) async throws {
        let timeline = request.timeline, out = request.output
        let duration = request.duration, fps = request.fps
        try require(duration > 0 && duration <= 60 && (1...24).contains(fps), "duration/fps outside bounds")
        try require(!FileManager.default.fileExists(atPath: out.appendingPathComponent("render.json").path)
                    && !FileManager.default.fileExists(atPath: out.appendingPathComponent("frames").path), "render output already exists")
        let geometry = try JSONDecoder().decode(DemoGeometry.self, from: Data(contentsOf: timeline.appendingPathComponent("geometry.json")))
        let lines = try String(contentsOf: timeline.appendingPathComponent("frames.jsonl"), encoding: .utf8).split(separator: "\n")
        try require(!lines.isEmpty && lines.count <= 3600, "invalid timeline length")
        let frames = try lines.map { try JSONDecoder().decode(PromoFrame.self, from: Data($0.utf8)) }
        try require(frames[0].at_s >= 0 && frames[0].at_s < 1, "timeline must begin within the first second")
        for index in frames.indices {
            let frame = frames[index]
            try require(frame.at_s.isFinite && frame.at_s >= 0 && frame.at_s <= 60
                        && frame.state.run_id == geometry.run_id
                        && frame.state.snapshot.run_id == geometry.run_id, "timeline scope differs")
            if index > 0 {
                try require(frame.at_s > frames[index - 1].at_s
                            && frame.state.sequence >= frames[index - 1].state.sequence
                            && frame.state.snapshot.time_s >= frames[index - 1].state.snapshot.time_s,
                            "timeline order regressed")
            }
        }
        let session = out.appendingPathComponent("playback", isDirectory: true)
        let images = out.appendingPathComponent("frames", isDirectory: true)
        try FileManager.default.createDirectory(at: session, withIntermediateDirectories: true)
        try FileManager.default.createDirectory(at: images, withIntermediateDirectories: true)
        let model = DemoModel(session: session, capture: nil, readOnly: true)
        model.presentationReplay = true
        model.geometry = geometry
        let renderer = SceneRenderer()
        model.renderer = renderer
        renderer.view.frame = CGRect(x: 0, y: 0, width: 1378, height: 966)
        renderer.view.rendersContinuously = false
        let offscreen = SCNRenderer(device: nil, options: nil)
        offscreen.scene = renderer.scene
        offscreen.pointOfView = renderer.camera
        var inputIndex = -1, chapter: PromoChapter?, pointer: PromoPointer?
        var pointerAt = -10.0, motions: [PromoMotion] = []
        var installed = Set<String>()
        var receipt: [PromoFrameReceipt] = []
        var timings: [String: Double] = ["state_and_camera": 0, "scene_render": 0, "native_ui": 0, "png_write": 0]
        let count = Int(ceil(duration * Double(fps)))
        for number in 0..<count {
            try Task.checkCancellation()
            await Task.yield()
            try autoreleasepool {
                var checkpoint = ProcessInfo.processInfo.systemUptime
                let time = Double(number) / Double(fps)
                while inputIndex + 1 < frames.count && (inputIndex < 0 || frames[inputIndex + 1].at_s <= time) {
                    inputIndex += 1
                    let frame = frames[inputIndex]
                    for name in installed {
                        try? FileManager.default.removeItem(at: relative(name, under: session))
                    }
                    installed.removeAll()
                    for (name, source) in frame.assets ?? [:] {
                        let destination = try relative(name, under: session)
                        let bytes = try Data(contentsOf: relative(source, under: timeline))
                        try require(bytes.count <= 4 << 20, "camera asset too large")
                        try FileManager.default.createDirectory(at: destination.deletingLastPathComponent(), withIntermediateDirectories: true)
                        try bytes.write(to: destination, options: .atomic)
                        installed.insert(name)
                    }
                    model.state = frame.state
                    model.stale = false
                    model.commandMessage = "Recorded simulator state · presentation replay"
                    model.updateEvidence()
                    if let value = frame.chapter { chapter = value }
                    if let value = frame.pointer { pointer = value; pointerAt = frame.at_s }
                    for action in frame.actions ?? [] {
                        try apply(action, model: model, renderer: renderer, at: frame.at_s, motions: &motions)
                        if frame.pointer == nil, let value = indicator(action, model: model,renderer:renderer) { pointer = value; pointerAt = frame.at_s }
                    }
                    if let preview = frame.inspection_route {
                        let route = try DemoInspectionRoute(runID:preview.run_id,
                            originalEnvelopeSHA256:preview.original_envelope_sha256,
                            observed:preview.observed,target:preview.target,route:preview.route)
                        try require(model.setInspectionRoute(route),"recorded route does not match selected original")
                    }
                }
                renderer.update(model)
                for index in motions.indices {
                    let linear = min(1, max(0, (time - motions[index].start) / motions[index].duration))
                    let eased = linear * linear * (3 - 2 * linear)
                    let delta = eased - motions[index].progress
                    renderer.followInput(horizontal: motions[index].dx * delta, zoom: motions[index].zoom * delta)
                    motions[index].progress = eased
                }
                motions.removeAll { $0.progress >= 1 }
                SCNTransaction.flush()
                var finished = ProcessInfo.processInfo.systemUptime
                timings["state_and_camera", default: 0] += finished - checkpoint; checkpoint = finished
                let scene = offscreen.snapshot(atTime: time, with: CGSize(width: 1378, height: 966), antialiasingMode: .multisampling4X)
                finished = ProcessInfo.processInfo.systemUptime
                timings["scene_render", default: 0] += finished - checkpoint; checkpoint = finished
                let content = PromoContent(model: model, scene: scene, chapter: chapter, pointer: pointer,
                                           pointerAge: time - pointerAt, progress: time / duration)
                let host = NSHostingView(rootView: content)
                host.frame = CGRect(x: 0, y: 0, width: 1920, height: 1080)
                host.layoutSubtreeIfNeeded()
                guard let bitmap = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: 1920, pixelsHigh: 1080,
                    bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true, isPlanar: false,
                    colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0) else {
                    throw PromoFailure(message: "native bitmap unavailable")
                }
                bitmap.size = host.bounds.size
                host.cacheDisplay(in: host.bounds, to: bitmap)
                finished = ProcessInfo.processInfo.systemUptime
                timings["native_ui", default: 0] += finished - checkpoint; checkpoint = finished
                guard let png = bitmap.representation(using: .png, properties: [:]) else { throw PromoFailure(message: "native PNG unavailable") }
                try png.write(to: images.appendingPathComponent(String(format: "%06d.png", number)))
                timings["png_write", default: 0] += ProcessInfo.processInfo.systemUptime - checkpoint
                guard let state = model.state else { throw PromoFailure(message: "no recorded state") }
                receipt.append(PromoFrameReceipt(frame: number, at_s: time, timeline_index: inputIndex,
                    state_sequence: state.sequence, simulation_time_s: state.snapshot.time_s,
                    replica: state.snapshot.map_node, camera: model.camera.rawValue,
                    loaded_thumbnails: model.evidenceImages.count + model.trackingImages.count,
                    tracking_thumbnails: model.trackingImages.count,
                    visible_tracks: renderer.diagnostics().first { $0.name == "Tracks" }?.child_nodes ?? 0,
                    tracks_layer_enabled: model.layers.contains(.tracks),
                    camera_sources: Dictionary(uniqueKeysWithValues: (state.snapshot.tracking?.camera_sources ?? []).map { ($0.id, $0.enabled) }),
                    selected_original:model.selectedOriginalEnvelopeSHA256,
                    evidence_references:model.selectedOriginal?.evidence.count ?? 0,
                    evidence_sheet:model.presentedEvidence != nil,
                    verified_media_preview:model.nativeMedia.preview != nil,
                    inspection_route_available:model.visibleInspectionRoute != nil,
                    inspection_route_visible:renderer.scene.rootNode.childNode(withName:"inspection-route-preview",recursively:true) != nil))
                if number % fps == 0 { FileHandle.standardOutput.write(Data("Rendered \(number)/\(count)\n".utf8)) }
            }
        }
        let result = PromoReceipt(fps: fps, frame_count: count, run_id: geometry.run_id, frames: receipt, timing_seconds: timings)
        let encoder = JSONEncoder(); encoder.outputFormatting = [.sortedKeys, .prettyPrinted]
        try encoder.encode(result).write(to: out.appendingPathComponent("render.json"), options: .atomic)
    }
}
