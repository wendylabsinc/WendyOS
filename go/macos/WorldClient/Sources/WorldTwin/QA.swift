import Foundation
#if canImport(WorldClient)
import WorldClient
#endif

struct LayerDiagnostic: Encodable { let name:String; let enabled:Bool; let hidden:Bool; let child_nodes:Int }
struct CameraDiagnostic:Encodable { let mode:String;let yaw:Double;let distance:Double;let elevation:Double;let position:[Double];let target:[Double] }
struct FusionDiagnostic:Encodable {
    let state:String;let sample_time_s:Double;let observations:Int;let loaded_thumbnails:Int;let localized_hazard:Bool
    let replica_node:String?;let local_observations:Int?;let received_observations:Int?;let missing_observations:Int?
}
struct TrackingDiagnostic:Encodable { let participant_id:String;let live_tracks:Int;let cameras:Int;let loaded_thumbnails:Int;let selected_track:String? }
struct QARequest: Decodable { let id:String; let action:String; let target:String?; let enabled:Bool?;let delta_x:Double?;let zoom_delta:Double? }
struct QAResult: Encodable {
    let schema = "wendy.macos-demo.app-qa.v1"
    let id:String; let action:String; let accepted:Bool; let error:String?
    let kind = "application_model_check_not_OS_input"
    let run_id:String; let state_sequence:Int; let worker_command_sequence:Int
    let camera:String; let selected_robot:String; let selected_replica:String
    let layers:[LayerDiagnostic]; let command_message:String; let capture:String?; let capture_receipt:String?
    let camera_state:CameraDiagnostic?
    let fusion:FusionDiagnostic?
    let tracking:TrackingDiagnostic?
    let inspector_panel:String?
}

/// Explicit local diagnostics only. No action runs until the owner writes a new request.
@MainActor final class DemoQA {
    private weak var model:DemoModel?
    private var seen=Set<String>()
    private var asyncActionRunning = false
    private var waiting:QARequest?
    private var begun=ContinuousClock.now
    private var investigationAction=false
    init(model:DemoModel) { self.model=model }
    func poll() {
        guard let model, !asyncActionRunning, !investigationAction else{return}
        if let request=waiting {
            if model.state?.worker_status == "closed" {finish(request,accepted:false,error:"worker_closed")}
            else if model.pending == nil { finish(request,accepted:model.error == nil && model.commandMessage == "Control applied") }
            else if ContinuousClock.now-begun > .seconds(6) { finish(request,accepted:false,error:"acknowledgement_missing") }
            return
        }
        guard model.state != nil,model.renderer?.loaded == true else{return}
        guard seen.count<128 else{return}
        let path=model.session.appendingPathComponent("qa-request.json")
        guard let size=try? path.resourceValues(forKeys:[.fileSizeKey]).fileSize,size<=4096,
              let data=try? Data(contentsOf:path),data.count<=4096,
              let request=try? JSONDecoder().decode(QARequest.self,from:data),
              !request.id.isEmpty,request.id.count<=64,
              request.id.utf8.allSatisfy({ (48...57).contains($0) || (65...90).contains($0) || (97...122).contains($0) || $0==45 }),
              !seen.contains(request.id) else{return}
        seen.insert(request.id)
        guard request.action.hasPrefix("investigation_") || request.action.hasPrefix("history_") || model.connected else {finish(request,accepted:false,error:"scene_not_current");return}
        switch request.action {
        #if canImport(WorldClient)
        case "zone_open":
            model.zones.showing = true
            asyncActionRunning = true
            model.actions.submit { [self] in  await model.zones.refresh(); asyncActionRunning = false; finish(request, accepted: model.zones.context != nil) }
        case "zone_draw":
            guard let raw = request.target?.data(using: .utf8), let coordinates = try? JSONDecoder().decode([Double].self, from: raw),
                  coordinates.count == 4, coordinates.allSatisfy({ $0.isFinite && $0 >= 0 && $0 <= 400 }) else { finish(request, accepted: false, error: "invalid_rectangle"); return }
            model.zones.draw(from: CGPoint(x: coordinates[0], y: coordinates[1]), to: CGPoint(x: coordinates[2], y: coordinates[3]), size: CGSize(width: 400, height: 320))
            finish(request, accepted: true)
        case "zone_publish":
            guard model.zones.canPublish else { finish(request, accepted: false, error: "zone_authority_unavailable"); return }
            let prior = Set(model.zones.rows.map(\.id))
            asyncActionRunning = true
            model.actions.submit { [self] in  await model.zones.publish(); asyncActionRunning = false; finish(request, accepted: !Set(model.zones.rows.map(\.id)).subtracting(prior).isEmpty) }
        case "zone_select", "zone_edit", "zone_delete":
            guard let target = request.target, let row = model.zones.rows.first(where: { $0.digest == target }) else { finish(request, accepted: false, error: "revision_unavailable"); return }
            if request.action == "zone_edit" { model.zones.edit(row); finish(request, accepted: !row.deleted); return }
            asyncActionRunning = true
            model.actions.submit { [self] in
                if request.action == "zone_select" {
                    await model.zones.select(row)
                    asyncActionRunning = false
                    if model.pending != nil { waiting = request; begun = .now }
                    else { finish(request, accepted: false, error: "selection_not_sent") }
                } else {
                    await model.zones.delete(row); asyncActionRunning = false
                    finish(request, accepted: !model.zones.rows.contains(where: { $0.id == row.id }))
                }
            }
        case "zone_capture":
            finish(request, accepted: model.zones.capture(model.session.appendingPathComponent("qa-zone-" + request.id + ".png")))
        #endif
        case "toggle_layer":
            guard let target=request.target,let layer=DemoLayer(rawValue:target),let enabled=request.enabled else {finish(request,accepted:false,error:"invalid_layer");return}
            model.setLayer(layer,enabled:enabled);model.renderer?.update(model);finish(request,accepted:true)
        case "camera":
            guard let target=request.target,let camera=CameraPreset(rawValue:target),model.availableCameras.contains(camera) else{finish(request,accepted:false,error:"invalid_camera");return}
            model.setCamera(camera);model.renderer?.update(model);finish(request,accepted:true)
        case "select_robot":
            guard let target=request.target,model.snapshot?.robots.contains(where:{$0.id==target}) == true else{finish(request,accepted:false,error:"invalid_robot");return}
            model.selectedRobot=target;model.renderer?.update(model);finish(request,accepted:true)
        case "select_track":
            guard let target=request.target,model.snapshot?.tracking?.live(at:model.snapshot?.time_s ?? 0).contains(where:{$0.id==target}) == true else{finish(request,accepted:false,error:"invalid_track");return}
            model.selectedTrack=target;model.renderer?.update(model);finish(request,accepted:true)
        case "panel":
            guard let target=request.target,["Layers","Network"].contains(target) else{finish(request,accepted:false,error:"invalid_panel");return}
            model.panel=target;finish(request,accepted:true)
        case "inspector":
            guard let target=request.target,let panel=PerceptionPanel(rawValue:target),model.setPerceptionPanel(panel) else{finish(request,accepted:false,error:"invalid_inspector");return}
            finish(request,accepted:true)
        case "follow_input":
            let dx=request.delta_x ?? 0,zoom=request.zoom_delta ?? 0
            guard model.camera == .robot,dx.isFinite,zoom.isFinite,abs(dx)<=500,abs(zoom)<=500 else{finish(request,accepted:false,error:"invalid_follow_input");return}
            model.renderer?.followInput(horizontal:dx,zoom:zoom);finish(request,accepted:true)
        case "command":
            // target is a fixed action; selection/edge target is carried in a second colon field.
            let parts=(request.target ?? "").split(separator:":",maxSplits:1,omittingEmptySubsequences:false).map(String.init)
            let action=parts.first ?? ""
            guard ["start","stop","toggle_edge","toggle_available","select_map","toggle_camera","toggle_sensor","patrol_pause","patrol_resume","patrol_recall","patrol_stop","handoff_input","handoff_result","handoff_quality","handoff_duplicate","handoff_fixture"].contains(action),model.canCommand else {finish(request,accepted:false,error:"control_unavailable");return}
            waiting=request;begun = .now;model.command(action,target:parts.count==2 ? parts[1]:"")
            if model.pending == nil {finish(request,accepted:false,error:"control_not_sent")}
        #if canImport(WorldClient)
        case "history_select","history_refresh","history_capture":
            investigationAction=true
            model.actions.submit { [self] in
                let history=DemoMissionHistory.shared
                var accepted=true
                if request.action=="history_select" {await history.select(task:request.target ?? "")}
                else if request.action=="history_refresh" {await history.refresh();accepted=history.document != nil}
                else {accepted=history.capture(model.session.appendingPathComponent("history-\(request.id).png"))}
                investigationAction=false;finish(request,accepted:accepted,error:accepted ? nil:"history_action_failed")
            }
        case "investigation_prepare", "investigation_retain", "investigation_open", "investigation_retry", "investigation_release", "investigation_capture":
            investigationAction=true
            model.actions.submit { [self] in
                let investigations=DemoInvestigations.shared
                var accepted=true
                switch request.action {
                case "investigation_prepare":
                    do {
                        guard let name=request.target,!name.contains("/"),name.hasSuffix(".json") else {throw InvestigationError.invalid}
                        let raw=try DemoInvestigations.privateRead(model.session.appendingPathComponent(name).path)
                        await investigations.prepareHistorical(try JSONValue.decode(raw))
                        accepted=investigations.draftResult != nil
                    }catch{accepted=false}
                case "investigation_retain":
                    if let name=request.target {investigations.draftName=name}
                    if let seconds=request.delta_x,seconds.isFinite,seconds>=2,seconds<=3600 {investigations.deadline=Date().addingTimeInterval(seconds)}
                    await investigations.saveAndRetain();accepted=investigations.selected != nil
                case "investigation_open":
                    await investigations.refresh()
                    if let entry=investigations.entries.first(where:{$0.id==request.target || $0.name==request.target}) {await investigations.open(entry)}else{accepted=false}
                case "investigation_retry","investigation_release":
                    if let entry=investigations.selected,let hold=entry.holds.first(where:{$0.origin.audience==request.target}) {await investigations.act(entry,hold:hold,release:request.action=="investigation_release")}else{accepted=false}
                default:
                    await investigations.refresh()
                    accepted=investigations.capture(model.session.appendingPathComponent("investigation-\(request.id).png"))
                }
                investigationAction=false;finish(request,accepted:accepted,error:accepted ? nil:"investigation_action_failed")
            }
        #endif
        case "capture": finish(request,accepted:true,capture:true)
        default: finish(request,accepted:false,error:"unknown_action")
        }
    }
    private func finish(_ request:QARequest,accepted:Bool,error:String?=nil,capture:Bool=false) {
        guard let model,model.state != nil else{return}
        waiting=nil
        let image="qa-\(request.id).png"
        let captured=capture && model.capture(path:model.session.appendingPathComponent(image).path)
        guard let state=model.state else{return}
        let result=QAResult(id:request.id,action:request.action,accepted:accepted && (!capture || captured),
            error:error ?? (capture && !captured ? "capture_failed":nil),run_id:state.run_id,state_sequence:state.sequence,
            worker_command_sequence:state.last_command_sequence,camera:model.camera.rawValue,selected_robot:model.selectedRobot,
            selected_replica:state.snapshot.map_node,layers:model.renderer?.diagnostics() ?? [],command_message:model.commandMessage,
            capture:captured ? image:nil,capture_receipt:captured ? image+".json":nil,camera_state:model.renderer?.cameraDiagnostics(),
            fusion:state.snapshot.fusion.map { FusionDiagnostic(state:$0.state,sample_time_s:$0.sample_time_s,
                observations:$0.observations.count,loaded_thumbnails:model.evidenceImages.count,localized_hazard:$0.localizedHazard,
                replica_node:$0.replica_node,local_observations:$0.replica_node == nil ? nil:$0.observations.filter { $0.delivery == "local" }.count,
                received_observations:$0.replica_node == nil ? nil:$0.observations.filter { $0.delivery == "received" }.count,
                missing_observations:$0.replica_node == nil ? nil:$0.missing_observations.count) },
            tracking:state.snapshot.tracking.map { TrackingDiagnostic(participant_id:$0.participant_id,live_tracks:$0.live(at:state.snapshot.time_s).count,cameras:$0.cameras.count,loaded_thumbnails:model.trackingImages.count,selected_track:model.selectedTrack) },
            inspector_panel:model.perceptionPanel?.rawValue)
        let path=model.session.appendingPathComponent("qa-result-\(request.id).json")
        try? model.exclusiveJSON(result,path:path)
    }
}
