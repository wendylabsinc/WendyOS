import SwiftUI
import AppKit

private let ink = Color(red:0.14,green:0.22,blue:0.24)
private let teal = Color(red:0.12,green:0.47,blue:0.43)
private let paper = Color(red:0.97,green:0.97,blue:0.95)

struct DemoContent:View {
    var presentsEvidenceSheet = true
    @ObservedObject var model:DemoModel
    var capturedScene: NSImage? = nil
    var missionConfig: URL? = nil
    private let patrolSecondary = Color(red:0.35,green:0.42,blue:0.43)
    private var inspectionComplete:Bool { (model.snapshot?.total ?? 0)>0 && (model.snapshot?.completed ?? 0)>=(model.snapshot?.total ?? 0) }
    private var hasRobots:Bool { model.snapshot?.robots.isEmpty == false }
    private var hasJobs:Bool { model.snapshot?.jobs.isEmpty == false }
    private var hasEvents:Bool { model.snapshot?.events.isEmpty == false }
    var body:some View {
        VStack(spacing:0) {
            header
            Divider()
            HStack(spacing:0) {
                sidebar.frame(width:286)
                Divider()
                viewport
                Divider()
                inspector.frame(width:254)
            }
            Divider()
            footer
        }.background(paper).foregroundStyle(ink)
            .sheet(item:presentsEvidenceSheet ? $model.presentedEvidence : .constant(nil)) { selection in DemoNativeEvidenceSheet(selection:selection,media:model.nativeMedia) }
            .sheet(isPresented:Binding(get:{presentsEvidenceSheet && model.patrolEvidenceDigest != nil},set:{if !$0 {model.patrolEvidenceDigest=nil}})) {
                if let digest=model.patrolEvidenceDigest {PatrolEvidenceSheet(digest:digest,session:model.session)}
            }
    }
    private var header:some View {
        HStack(spacing:16) {
            Image(systemName:"square.stack.3d.up.fill").font(.system(size:24,weight:.medium)).foregroundStyle(teal)
            VStack(alignment:.leading,spacing:2) {
                Text("Wendy World").font(.system(size:20,weight:.semibold,design:.rounded))
                Text(model.fireResponseMode ? "SIMULATION LAB  /  ESP32 CAMERA + GO2 RESPONSE":model.handoffMode ? "SIMULATION LAB  /  AUTOMATIC FUSION HANDOFF":model.patrolMode ? "STUDIO 01  /  PASSIVE GO2 PATROL":model.liveNativeReplica ? "STUDIO 01  /  SHARED SENSOR FUSION":(model.trackingOnly ? "ANONYMOUS CAMERA TRACKING":(model.snapshot?.fusion == nil ? "STUDIO 01  /  COLLABORATIVE INSPECTION":"STUDIO 01  /  MULTIMODAL INSPECTION"))).font(.system(size:9,weight:.semibold)).tracking(1.6).foregroundStyle(.secondary)
            }
            Spacer()
            Label(model.statusBadge,systemImage:model.connected ? "circle.fill":"circle.dotted")
                .font(.system(size:10,weight:.semibold)).foregroundStyle(model.connected ? teal:.orange)
                .padding(.horizontal,12).padding(.vertical,7).background(teal.opacity(0.07),in:Capsule())
            Divider().frame(height:27)
            Text(clock(model.snapshot?.time_s ?? 0)).font(.system(size:14,weight:.medium,design:.monospaced))
#if canImport(WorldClient)
            if !model.readOnly {
            if model.trackingOnly {NativeZonesButton(model:model)}
            InvestigationLauncher()
            MissionHistoryLauncher()
            }
            if !model.readOnly, let missionConfig {
                DemoMissionLauncher(configURL:missionConfig,runID:model.state?.run_id,
                    selectedEnvelope:model.selectedOriginalEnvelopeSHA256,
                    replay:model.presentationReplay,onNavigationPreview:{ _ = model.setInspectionRoute($0) })
            }
#endif
            if let patrol=model.snapshot?.patrol,model.patrolMode {
                VStack(alignment:.trailing,spacing:3) {
                    Text(patrol.state.replacingOccurrences(of:"_",with:" ").capitalized).font(.system(size:12,weight:.semibold))
                    Text("Expires in "+clock(max(0,patrol.expires_at_s-(model.snapshot?.time_s ?? 0)))).font(.system(size:10,design:.monospaced)).foregroundStyle(.secondary)
                }
                Button { model.command(["paused","return_held","work_held","environment_held","world_held","tour_held"].contains(patrol.state) ? "patrol_resume":"patrol_pause") } label: {
                    Label(["work_held","environment_held","world_held","tour_held"].contains(patrol.state) ? "Resume":(patrol.state == "return_held" ? "Retry return":(patrol.state == "paused" ? "Resume":"Pause")),systemImage:["paused","return_held","work_held","environment_held","world_held","tour_held"].contains(patrol.state) ? "play.fill":"pause.fill")
                }.buttonStyle(.borderedProminent).tint(teal).disabled(!model.canCommand || patrol.terminal || (patrol.state=="world_held" && patrol.world_watch?.ready != true) || (patrol.state=="environment_held" && patrol.environment?.ready != true))
                if let rendezvous=patrol.return {
                    Button {model.command("patrol_recall")} label: {Label("Finish and return",systemImage:"arrow.uturn.backward")}
                        .buttonStyle(.bordered).disabled(!model.canCommand || patrol.terminal || rendezvous.requested || patrol.world_watch?.held == true || patrol.environment?.held == true)
                        .help("Cancel remaining observations and return to the signed rendezvous. While paused, Resume is still required.")
                }
                Button { model.command("patrol_stop") } label: { Label("Stop",systemImage:"stop.fill") }
                    .buttonStyle(.bordered).disabled(!model.canCommand || patrol.terminal)
                    .help("End this simulated patrol mission. Progress remains available.")
            } else if model.showsRobotControls && hasJobs {
            Button { model.command("start") } label:{ Label(inspectionComplete ? "Inspection complete":(model.snapshot?.authorized == true ? "Inspection running":"Start inspection"),systemImage:inspectionComplete ? "checkmark":"play.fill") }
                .buttonStyle(.borderedProminent).tint(teal)
                .disabled(!model.canCommand || model.snapshot?.authorized == true || model.snapshot?.stopped == true)
                .help("Authorize the simulated robot team. Does not control physical hardware.")
            Button { model.command("stop") } label:{ Image(systemName:"stop.fill").frame(width:18) }
                .buttonStyle(.bordered).tint(.red).disabled(!model.canCommand || model.snapshot?.authorized != true)
                .help("Stop every simulated robot locally. Restart the demo to begin again.")
            } else if model.trackingOnly { Label(model.presentationReplay ? "Recorded tracks" : "Tracking live",systemImage:"viewfinder").font(.system(size:12,weight:.medium)).foregroundStyle(teal) }
        }.padding(.leading,80).padding(.trailing,22).frame(height:78)
    }
    private var sidebar:some View {
        VStack(alignment:.leading,spacing:0) {
            VStack(alignment:.leading,spacing:7) {
                Text(model.fireResponseMode ? "ESP32 camera / Go2 response":model.snapshot?.fusion_selection != nil ? "One hazard. Three gateways.":model.patrolMode ? (model.snapshot?.patrol?.tour == nil ? "Autonomous room patrol":"Selected room inspection"):model.trackingOnly ? "From pixels to motion":"A shared view of the site").font(.system(size:18,weight:.semibold))
                Text(model.fireResponseMode ? "Camera frames pass through ESP32-S3 firmware. A confirmed synthetic flame cue selects an observation point for the signed Go2 mission.":model.snapshot?.fusion_selection != nil ? "The view follows fresh evidence and holds through small quality changes.":model.patrolMode ? (model.snapshot?.patrol?.tour == nil ? "Go2 revisits accessible rooms and records camera, LiDAR, gas and audio evidence. Findings never change its route.":"Go2 inspects each signed destination once, retains original observations, then returns. Blocked or interrupted stops remain incomplete."):model.liveNativeReplica ? "Camera, LiDAR, gas and audio shared between Gateway A and Gateway B.":(model.trackingOnly ? "Two camera views. Anonymous tracks with visible uncertainty.":(model.snapshot?.fusion == nil ? "Three robots map and inspect a furnished office.":"Camera, LiDAR, gas and audio observations meet in one live scene.")))
                    .font(.system(size:12)).foregroundStyle(.secondary).fixedSize(horizontal:false,vertical:true).lineSpacing(3)
            }.padding(22)
            if model.showsRobotControls && !model.patrolMode { Picker("Sidebar",selection:$model.panel) { Text("Layers").tag("Layers");Text("Network").tag("Network") }.pickerStyle(.segmented).padding(.horizontal,20).padding(.bottom,20) }
            ScrollView {
                VStack(alignment:.leading,spacing:22) {
                    if let selection=model.snapshot?.fusion_selection { FusionSelectionPanel(model:model,value:selection) }
                    else if model.panel=="Layers" || !model.showsRobotControls || model.patrolMode { layers } else { network }
                    if model.liveNativeReplica { SensorInputsPanel(model:model) }
                    if model.showsRobotControls { team }
                    if model.patrolMode,let patrol=model.snapshot?.patrol { patrolCoverage(patrol) }
                }.padding(.horizontal,20).padding(.bottom,24)
            }
        }.background(paper)
    }
    private var layers:some View {
        VStack(alignment:.leading,spacing:13) {
            if model.showsRobotControls && !model.patrolMode { HStack(spacing:6) {
                Button("Twin") { model.layers = [.building,.routes,.jobs,.labels,.observations,.hazards] }
                Button("Operations") { model.layers = [.building,.routes,.jobs,.labels,.links,.lidar,.observations,.hazards] }
                Button("Shared map") { model.layers = [.voxels,.routes,.links,.labels] }
            }.buttonStyle(.bordered).controlSize(.mini) }
            eyebrow("WORLD LAYERS")
            ForEach(model.availableLayers) { layer in
                Toggle(isOn:Binding(get:{model.layers.contains(layer)},set:{ model.setLayer(layer,enabled:$0) })) {
                    Label(layer.rawValue,systemImage:layer.icon).font(.system(size:12,weight:.medium))
                }.toggleStyle(.switch).controlSize(.mini).tint(teal).accessibilityLabel("Show \(layer.rawValue)")
            }
            if !model.patrolMode && (model.trackingOnly || model.showsRobotControls) {
                Divider().padding(.vertical,4)
                eyebrow("SELECTED REPLICA")
                if model.trackingOnly {
                Text(model.snapshot?.map_node == "hub" ? "Site hub":(model.snapshot?.map_node ?? "").capitalized).font(.system(size:13,weight:.semibold))
                Text("Track estimates list their source cameras. The actors are part of the simulated scene.").font(.system(size:11)).foregroundStyle(.secondary).fixedSize(horizontal:false,vertical:true)
                Text("The tracker receives images and calibration, not actor locations.").font(.system(size:10)).foregroundStyle(.secondary).fixedSize(horizontal:false,vertical:true)
            } else {
            Picker("Replica",selection:Binding(get:{model.snapshot?.map_node ?? "hub"},set:{model.command("select_map",target:$0)})) {
                Text("Site hub").tag("hub")
                ForEach(model.snapshot?.robots ?? []) { robot in Text(robot.name).tag(robot.id) }
            }.pickerStyle(.menu).disabled(!model.canCommand)
            Text("This participant's shared map and sensor fusion.").font(.system(size:10)).foregroundStyle(.secondary)
            HStack {
                Text("Occupied cells").font(.system(size:11)).foregroundStyle(.secondary)
                Spacer();Text("\(model.snapshot?.voxels.count ?? 0)").font(.system(size:13,weight:.semibold,design:.monospaced))
            }
            Text("Building geometry is the reference model. Shared voxels are accumulated LiDAR hits in the selected replica. Unobserved space stays unknown.")
                .font(.system(size:10)).foregroundStyle(.secondary).lineSpacing(3)
                }
            }
        }
    }
    private var network:some View {
        VStack(alignment:.leading,spacing:12) {
            eyebrow("LOCAL MESH")
            Text("Disable a link to watch the team reroute or continue on a cached path.")
                .font(.system(size:11)).foregroundStyle(.secondary).lineSpacing(3)
            ForEach(model.snapshot?.network.edges ?? []) { edge in edgeRow(edge) }
            Divider().padding(.vertical,3)
            eyebrow("INTERNET UPLINK")
            if let edge=model.snapshot?.network.internet { edgeRow(edge) }
            Text("This uplink is simulated. It starts off and carries no external traffic.")
                .font(.system(size:10)).foregroundStyle(.secondary).lineSpacing(3)
        }
    }
    private func edgeRow(_ edge:Edge)->some View {
        Toggle(isOn:Binding(get:{edge.enabled},set:{_ in model.command("toggle_edge",target:edge.id)})) {
            VStack(alignment:.leading,spacing:3) {
                Text("\(friendly(edge.a)) ↔ \(friendly(edge.b))").font(.system(size:11,weight:.medium))
                Text(edge.up ? "Connected · \(edge.packets) packets" : (edge.in_range == false ? "Out of radio range":"Link disabled"))
                    .font(.system(size:9)).foregroundStyle(edge.up ? teal:Color.secondary)
            }
        }.toggleStyle(.switch).controlSize(.mini).tint(teal).disabled(!model.canCommand)
    }
    private var team:some View {
        VStack(alignment:.leading,spacing:12) {
            Divider()
            eyebrow(model.fireResponseMode ? "RESPONSE ROBOT":model.patrolMode ? "PATROL ROBOT":"ROBOT TEAM")
            ForEach(model.snapshot?.robots ?? []) { robot in
                Button { model.selectedRobot=robot.id } label: {
                    HStack(spacing:10) {
                        RoundedRectangle(cornerRadius:10).fill(robotColor(robot).opacity(0.13)).overlay(Image(systemName:"sensor.tag.radiowaves.forward.fill").foregroundStyle(robotColor(robot))).frame(width:35,height:35)
                        VStack(alignment:.leading,spacing:3) { Text(robot.name).font(.system(size:12,weight:.semibold));Text(readable(robot.state)).font(.system(size:10)).foregroundStyle(.secondary) }
                        Spacer()
                        Circle().fill(robot.online ? teal:.orange).frame(width:6,height:6)
                    }.padding(10).background(model.selectedRobot==robot.id ? Color.white:Color.clear,in:RoundedRectangle(cornerRadius:12))
                        .overlay(RoundedRectangle(cornerRadius:12).stroke(model.selectedRobot==robot.id ? teal.opacity(0.2):.clear))
                }.buttonStyle(.plain)
            }
        }
    }
    private var viewport:some View {
        ZStack(alignment:.top) {
            if let capturedScene {
                Image(nsImage:capturedScene).resizable().interpolation(.high)
            } else {
                SceneView(model:model)
            }
            VStack(spacing:0) {
                HStack {
                    HStack(spacing:7) { Image(systemName:"building.2.crop.circle");Text(model.trackingOnly ? "CAMERA ANNEX":"STUDIO 01").tracking(1.2) }.font(.system(size:10,weight:.semibold))
                        .padding(.horizontal,13).padding(.vertical,9).background(.regularMaterial,in:Capsule())
                    Spacer()
                    HStack(spacing:2) {
                        ForEach(model.availableCameras) { camera in
                            Button { model.setCamera(camera) } label: { Text(camera.rawValue).font(.system(size:10,weight:.medium)).padding(.horizontal,10).padding(.vertical,7).background(model.camera==camera ? Color.white:.clear,in:Capsule()) }.buttonStyle(.plain)
                        }
                    }.padding(4).background(.regularMaterial,in:Capsule())
                }.padding(20)
                Spacer()
                HStack(alignment:.bottom) {
                    VStack(alignment:.leading,spacing:5) {
                        Text("DIGITAL TWIN").font(.system(size:9,weight:.bold)).tracking(1.5)
                        Text(model.trackingOnly ? "Reference actors + inferred tracks":"Reference geometry + live observations").font(.system(size:11))
                        HStack(spacing:12) { ForEach(model.snapshot?.robots ?? []) { r in HStack(spacing:4){Circle().fill(robotColor(r)).frame(width:5,height:5);Text(r.name).font(.system(size:9))} } }
                    }.padding(13).background(.regularMaterial,in:RoundedRectangle(cornerRadius:12))
                    Spacer()
                    Text("Drag to orbit · scroll to zoom").font(.system(size:9)).foregroundStyle(.secondary).padding(10).background(.regularMaterial,in:Capsule())
                }.padding(20)
            }
            if model.stale {
                Text(model.liveNativeReplica ? "Waiting for fresh sensor readings":"Waiting for a fresh simulator snapshot").font(.system(size:12,weight:.medium)).padding(16).background(.regularMaterial,in:Capsule()).frame(maxHeight:.infinity)
            }
        }.clipped()
    }
    private var inspector:some View {
        ScrollView {
            VStack(alignment:.leading,spacing:20) {
                if model.patrolMode && !model.fireResponseMode,let patrol=model.snapshot?.patrol { patrolInspector(patrol);Divider() }
                if model.fireResponseMode,let patrol=model.snapshot?.patrol,patrol.localizationHeld,
                   let health=patrol.localization_health {
                    VStack(alignment:.leading,spacing:4) {
                        Text(health.summary(held:true)).font(.system(size:11,weight:.medium))
                            .foregroundStyle(health.ready ? teal:Color.orange).fixedSize(horizontal:false,vertical:true)
                        Text(health.passiveSummary).font(.system(size:9)).foregroundStyle(.secondary)
                            .fixedSize(horizontal:false,vertical:true)
                    }.padding(9).frame(maxWidth:.infinity,alignment:.leading)
                        .background(teal.opacity(0.06),in:RoundedRectangle(cornerRadius:8))
                }
                if let selected=model.selectedOriginal {
                    VStack(alignment:.leading,spacing:8) {
                        HStack { Text("SELECTED OBSERVATION").font(.system(size:8,weight:.semibold)).foregroundStyle(.secondary);Spacer()
                            Button { model.clearOriginalSelection() } label: { Image(systemName:"xmark") }.buttonStyle(.plain).accessibilityLabel("Clear observation selection") }
                        Text(selected.title).font(.system(size:14,weight:.semibold))
                        Text("Simulation observation · "+selected.observationTime).font(.system(size:9)).foregroundStyle(.secondary)
                        Button { model.openSelectedEvidence() } label: { Label("Open evidence",systemImage:"photo.on.rectangle") }.buttonStyle(.bordered).tint(teal)
                    }.padding(12).background(teal.opacity(0.06),in:RoundedRectangle(cornerRadius:10))
                }
                if model.availablePerceptionPanels.count>1 {
                    HStack(spacing:4) {
                        ForEach(model.availablePerceptionPanels) {panel in
                            Button { model.setPerceptionPanel(panel) } label:{
                                Text(panel.rawValue).font(.system(size:12,weight:.semibold))
                                    .frame(maxWidth:.infinity).padding(.vertical,8)
                                    .foregroundStyle(model.perceptionPanel == panel ? Color.white:ink)
                                    .background(model.perceptionPanel == panel ? teal:.clear,in:Capsule())
                            }.buttonStyle(.plain).accessibilityLabel("Inspect \(panel.rawValue)")
                        }
                    }.padding(4).background(teal.opacity(0.08),in:Capsule())
                }
                if model.perceptionPanel == .hazard, let fusion=model.snapshot?.fusion { FusionPanel(model:model,fusion:fusion);Divider() }
                if model.fireResponseMode,let patrol=model.snapshot?.patrol { patrolInspector(patrol);Divider() }
                if model.perceptionPanel == .people, let tracking=model.snapshot?.tracking { TrackingPanel(model:model,tracking:tracking);Divider() }
                if model.showsRobotControls {
                if hasJobs && !model.patrolMode {
                VStack(alignment:.leading,spacing:10) {
                    eyebrow("INSPECTION")
                    HStack(alignment:.firstTextBaseline) {
                        Text("\(model.snapshot?.completed ?? 0)").font(.system(size:40,weight:.light,design:.rounded))
                        Text("/ \(model.snapshot?.total ?? 6)").font(.system(size:20,weight:.light)).foregroundStyle(.secondary)
                        Spacer();Image(systemName:"checkmark.seal").font(.system(size:25,weight:.light)).foregroundStyle(teal)
                    }
                    ProgressView(value:Double(model.snapshot?.completed ?? 0),total:Double(max(1,model.snapshot?.total ?? 6))).tint(teal)
                    Text(inspectionComplete ? "Inspection complete. All spaces have been visited.":(model.snapshot?.authorized == true ? "The team is inspecting assigned spaces." : "Start the team when you are ready.")).font(.system(size:11)).foregroundStyle(.secondary)
                }
                Divider()
                }
                if let robot=model.robot {
                    VStack(alignment:.leading,spacing:13) {
                        HStack { Text(robot.name).font(.system(size:22,weight:.semibold,design:.rounded));Spacer();Circle().fill(robotColor(robot)).frame(width:10,height:10) }
                        Text(readable(robot.state)).font(.system(size:11,weight:.medium)).foregroundStyle(teal)
                        metric(model.patrolMode ? "Physics position":"Position",String(format:"%.1f, %.1f m",robot.position[0],robot.position[1]))
                        metric("Speed",String(format:"%.2f m/s",sqrt(robot.velocity.reduce(0){$0+$1*$1})))
                        metric("LiDAR hits","\(robot.lidar_points.count)")
                        if !model.patrolMode {
                        metric("Local cells","\(robot.local_voxels)")
                        metric("Shared cells","\(robot.peer_voxels)")
                        VStack(alignment:.leading,spacing:5) { Text("Route to hub").font(.system(size:10)).foregroundStyle(.secondary);Text(robot.route.isEmpty ? "Isolated · cached path only":robot.route.map(friendly).joined(separator:" → ")).font(.system(size:11,weight:.medium)).fixedSize(horizontal:false,vertical:true) }
                        Button { model.command("toggle_available",target:robot.id) } label:{ Label(robot.available ? "Pause robot locally":"Resume robot locally",systemImage:robot.available ? "pause":"play") }.buttonStyle(.bordered).controlSize(.small).disabled(!model.canCommand)
                        Text("Local simulator control").font(.system(size:9)).foregroundStyle(.tertiary)
                        }
                    }
                    Divider()
                }
                if hasJobs && !model.patrolMode {
                VStack(alignment:.leading,spacing:12) {
                    eyebrow("SPACES")
                    ForEach(model.snapshot?.jobs ?? []) { job in
                        HStack(spacing:8) { Image(systemName:job.state == "completed" ? "checkmark.circle.fill":"circle.dashed").foregroundStyle(job.state == "completed" ? teal:Color.secondary);Text(job.label).font(.system(size:10));Spacer() }
                    }
                }
                Divider()
                }
                if hasEvents {
                VStack(alignment:.leading,spacing:9) {
                    eyebrow("LATEST ACTIVITY")
                    ForEach(Array((model.snapshot?.events ?? []).suffix(3).reversed().enumerated()),id:\.offset) { _,event in
                        VStack(alignment:.leading,spacing:4){Text(clock(event.time_s)).font(.system(size:9,design:.monospaced)).foregroundStyle(teal);Text(event.text).font(.system(size:10)).foregroundStyle(.secondary).fixedSize(horizontal:false,vertical:true)}
                    }
                }
                }
                }
            }.padding(20)
        }.background(paper)
    }
    private func patrolCoverage(_ patrol:PatrolState)->some View {
        VStack(alignment:.leading,spacing:12) {
            Divider();eyebrow("ROOM COVERAGE")
            Text(patrol.rooms.contains {$0.revisit_seconds != nil} ? "Signed revisit schedule · wall-clock deadlines continue while paused. Travel can make coverage overdue." : "A visit completes after two seconds of fresh sensor capture.")
                .font(.system(size:10)).foregroundStyle(patrolSecondary).fixedSize(horizontal:false,vertical:true)
            ForEach(patrol.rooms) { room in
                let now=model.snapshot?.time_s ?? 0
                let blocked=room.blocked(at:now)
                VStack(alignment:.leading,spacing:5) {
                    HStack {
                        Image(systemName:blocked ? "exclamationmark.triangle":(room.visits>0 ? "checkmark.circle":"circle.dashed"))
                            .foregroundStyle(blocked ? Color.orange:teal)
                        Text(room.name).font(.system(size:12,weight:patrol.current_room==room.id ? .semibold:.regular))
                        Spacer();Text("\(room.visits)").font(.system(size:11,design:.monospaced)).foregroundStyle(patrolSecondary)
                    }
                    Text(blocked ? "Blocked · retry in "+clock(max(0,(room.blocked_until_s ?? now)-now)):
                            room.last_observed_s.map {"Observed "+clock(max(0,now-$0))+" ago"} ?? "Not yet observed")
                        .font(.system(size:10)).foregroundStyle(blocked ? Color.orange:patrolSecondary)
                    if let interval=room.revisit_seconds, let due=room.next_due_s {
                        Text("Revisit every "+clock(Double(interval))+" · "+(due<=now ? "Overdue "+clock(now-due):"Due in "+clock(due-now)))
                            .font(.system(size:10,weight:.medium)).foregroundStyle(due<=now ? Color.orange:teal)
                    }
                    if blocked,let reason=room.blocked_reason {
                        Text(reason).font(.system(size:9)).foregroundStyle(patrolSecondary).fixedSize(horizontal:false,vertical:true)
                    }
                    if !room.evidence.isEmpty {
                        DisclosureGroup("\(room.evidence.count) evidence references") {
                            ForEach(room.evidence,id:\.self) {patrolEvidenceButton($0)}
                        }.font(.system(size:9)).foregroundStyle(patrolSecondary)
                    }
                }.padding(10).background(patrol.current_room==room.id ? teal.opacity(0.08):Color.clear,in:RoundedRectangle(cornerRadius:8))
            }
        }
    }
    private func patrolInspector(_ patrol:PatrolState)->some View {
        VStack(alignment:.leading,spacing:12) {
            eyebrow(patrol.tour == nil ? "PASSIVE PATROL":"FINITE INSPECTION TOUR")
            if let tour=patrol.tour {PatrolTourProgress(tour:tour)}
            if let offline=patrol.offline {
                Label("Prepared offline approval",systemImage:"shippingbox.fill").font(.system(size:12,weight:.semibold)).foregroundStyle(teal)
                Text("Original expiry · one activation").font(.caption2)
                Text("Kit "+String(offline.kit_id.suffix(12))+"\nPackage "+String(offline.package_sha256.prefix(12))).font(.system(size:9,design:.monospaced)).foregroundStyle(patrolSecondary)
                Text("Cached local authority. New remote revocations cannot be learned while disconnected.").font(.system(size:9)).foregroundStyle(patrolSecondary).fixedSize(horizontal:false,vertical:true)
            }
            Text(patrol.return?.requested == true ? (patrol.state == "completed" ? "Rendezvous reached":"Return rendezvous") : (patrol.current_room.flatMap {id in patrol.rooms.first {$0.id==id}?.name} ?? "Waiting for destination"))
                .font(.system(size:20,weight:.semibold,design:.rounded))
            if let policy=patrol.exclusions {Text("\(policy.rectangles.count) signed restricted areas · fixed for this mission").font(.system(size:11)).foregroundStyle(Color.red)}
            if let rendezvous=patrol.return {
                Text("APPROVED RENDEZVOUS").font(.caption2).foregroundStyle(teal)
                Text(String(format:"Map X %.2f · Y %.2f m",rendezvous.destination.x,rendezvous.destination.y)).font(.system(size:11,design:.monospaced))
                if let begin=rendezvous.begin_at_s, !rendezvous.requested {
                    Text("Scheduled return in "+clock(max(0,begin-(model.snapshot?.time_s ?? 0)))).font(.system(size:11))
                }
                Text(patrol.reason ?? (rendezvous.requested ? "Planning return to the approved point.":"Recall is available until the mission expires.")).font(.system(size:11)).foregroundStyle(patrol.state=="return_held" ? Color.orange:patrolSecondary).fixedSize(horizontal:false,vertical:true)
            }
            if let watch=patrol.world_watch {
                VStack(alignment:.leading,spacing:6){
                    Text("SHARED WORLD EVIDENCE").font(.caption2).foregroundStyle(teal)
                    Text(watch.held ? (watch.ready ? "Compatible evidence · Resume available":"Navigation held") : "Signed World condition satisfied").font(.system(size:14,weight:.semibold))
                    PatrolWorldMap(policy:watch.policy,selection:watch.selection)
                    Text("\(watch.transport.capitalized) · \(watch.selection["gateway"]?.string ?? "No compatible gateway") · original lifetime \(String(format:"%.1f",Double(watch.remaining_ms)/1000)) s").font(.system(size:10))
                    Text("Original capture "+(watch.original_unix_ms>0 ? Date(timeIntervalSince1970:Double(watch.original_unix_ms)/1000).formatted(date:.omitted,time:.standard):"unavailable")).font(.system(size:9,design:.monospaced)).foregroundStyle(patrolSecondary)
                    Text(watch.reason).font(.system(size:10)).foregroundStyle(watch.held ? Color.orange:patrolSecondary).fixedSize(horizontal:false,vertical:true)
                    Text("Purple: supported source cells. Orange: signed watch rectangle. Synthetic condition; never route or gas clearance.").font(.system(size:10)).foregroundStyle(patrolSecondary).fixedSize(horizontal:false,vertical:true)
                }.padding(10).background(teal.opacity(0.06),in:RoundedRectangle(cornerRadius:8))
            }
            if let environment=patrol.environment {
                VStack(alignment:.leading,spacing:7) {
                    Text("LOCAL ENVIRONMENTAL WATCH").font(.caption2).foregroundStyle(teal)
                    Text(environment.held ? (environment.ready ? "Clear interval observed · Resume available":"Navigation held") : "Signed local condition satisfied").font(.system(size:15,weight:.semibold))
                    if environment.last.sequence>0 {
                        Text(String(format:"%.1f + %.1f = %.1f ppm",environment.last.value_ppm,environment.last.uncertainty_ppm,environment.screening_ppm)).font(.system(size:13,weight:.medium,design:.monospaced))
                        Text(String(format:"Hold ≥ %.1f · Clear ≤ %.1f ppm",environment.policy.hold_ppm,environment.policy.clear_ppm)).font(.system(size:10))
                        Text(String(format:"Original sample #%llu · %.2f s",environment.last.sequence,Double(environment.last.simulation_ns)/1e9)).font(.system(size:9,design:.monospaced)).foregroundStyle(patrolSecondary)
                    } else {Text("No valid original local sample yet").font(.caption)}
                    if environment.held {
                        ProgressView(value:min(Double(environment.clear_elapsed_ms),Double(environment.policy.clear_for_ms)),total:Double(environment.policy.clear_for_ms)).tint(teal)
                        Text(String(format:"Original clear interval %.1f / %.1f s",Double(environment.clear_elapsed_ms)/1000,Double(environment.policy.clear_for_ms)/1000)).font(.system(size:10))
                    }
                    Text(environment.reason).font(.system(size:10)).foregroundStyle(environment.held ? Color.orange:patrolSecondary).fixedSize(horizontal:false,vertical:true)
                    Text("Synthetic gas-1 + declared uncertainty. Applies during patrol and return. No toxicology or physical safety claim.").font(.system(size:10)).foregroundStyle(patrolSecondary).fixedSize(horizontal:false,vertical:true)
                }.padding(10).background(teal.opacity(0.06),in:RoundedRectangle(cornerRadius:8))
            }
            if let work=patrol.work {
                VStack(alignment:.leading,spacing:7) {
                    Text("SIMULATED ACTUATOR WORK").font(.caption2).foregroundStyle(.orange)
                    Text(work.valid ? String(format:"%.2f / %.2f kJ used",work.used_j/1000,work.policy.limit_j/1000):(work.exhausted ? "Allowance exhausted":"Meter unavailable · navigation held")).font(.system(size:15,weight:.semibold))
                    ProgressView(value:min(work.used_j,work.policy.limit_j),total:work.policy.limit_j).tint(work.valid ? teal:Color.orange)
                    Text(String(format:"Last measured remaining %.2f kJ · Return reserve %.2f kJ",work.remaining_j/1000,work.policy.return_reserve_j/1000)).font(.system(size:10))
                    if let reason=work.reason {Text(reason).font(.system(size:10)).foregroundStyle(.orange)}
                    Text(String(format:"Original physics %.2f s · %.1f W positive joint power",Double(work.last.simulation_ns)/1e9,work.last.power_w)).font(.system(size:9,design:.monospaced)).foregroundStyle(patrolSecondary)
                    Text("Mechanical work only. No battery estimate or return guarantee. A reset cannot refill this mission.").font(.system(size:10)).foregroundStyle(patrolSecondary).fixedSize(horizontal:false,vertical:true)
                }.padding(10).background(Color.orange.opacity(0.06),in:RoundedRectangle(cornerRadius:8))
            }
            metric("Nominal speed",String(format:"%.2f m/s",patrol.nominal_speed_mps))
            metric("Observed rooms","\(patrol.rooms.filter {$0.visits>0}.count) / \(patrol.rooms.count)")
            if let health=patrol.localization_health {
                VStack(alignment:.leading,spacing:5) {
                    Text("LOCALIZATION OBSERVATIONS").font(.caption2).foregroundStyle(teal)
                    Text(health.summary(held:patrol.localizationHeld)).font(.system(size:11,weight:.medium))
                        .foregroundStyle(health.ready ? patrolSecondary:Color.orange).fixedSize(horizontal:false,vertical:true)
                    Text(health.observationAges).font(.system(size:9,design:.monospaced)).fixedSize(horizontal:false,vertical:true)
                    if health.variance.count == 3 {
                        Text(String(format:"Variance x %.4f m² · y %.4f m² · yaw %.4f rad²; each < %.4f",health.variance[0],health.variance[1],health.variance[2],health.variance_limit))
                            .font(.system(size:9,design:.monospaced)).fixedSize(horizontal:false,vertical:true)
                    }
                    Text(health.passiveSummary).font(.system(size:10)).fixedSize(horizontal:false,vertical:true)
                    Text("Fresh scans can refine AMCL while stopped. Observation updates never resume motion.")
                        .font(.system(size:9)).foregroundStyle(patrolSecondary).fixedSize(horizontal:false,vertical:true)
                }.padding(10).background(teal.opacity(0.06),in:RoundedRectangle(cornerRadius:8))
            }
            if let estimate=patrol.localization {
                Text(estimate.valid || patrol.localization_health != nil ? "AMCL estimate shown in purple. Ellipse represents 2σ position uncertainty.":"Navigation observations unavailable. Detailed health was not supplied.")
                    .font(.system(size:10)).foregroundStyle(estimate.valid ? patrolSecondary:Color.orange).fixedSize(horizontal:false,vertical:true)
            }
            Text("Observe and report. No person following or diversion toward findings.")
                .font(.system(size:10)).foregroundStyle(patrolSecondary).fixedSize(horizontal:false,vertical:true)
            Divider();eyebrow("RECENT FINDINGS")
            if patrol.recent_findings.isEmpty { Text("No findings reported.").font(.system(size:11)).foregroundStyle(patrolSecondary) }
            ForEach(Array(patrol.recent_findings.suffix(5).reversed())) { finding in
                VStack(alignment:.leading,spacing:5) {
                    Text(clock(finding.time_s)).font(.system(size:9,design:.monospaced)).foregroundStyle(teal)
                    Text(finding.text).font(.system(size:11)).fixedSize(horizontal:false,vertical:true)
                    if !finding.evidence.isEmpty {
                        DisclosureGroup("Evidence references") {
                            ForEach(finding.evidence,id:\.self) {patrolEvidenceButton($0)}
                        }.font(.system(size:9)).foregroundStyle(patrolSecondary)
                    }
                }
            }
        }
    }
    private func patrolEvidenceButton(_ digest:String)->some View {
        Button {model.patrolEvidenceDigest=digest} label: {
            Label("Open "+String(digest.prefix(12))+"…",systemImage:"doc.viewfinder")
                .font(.system(size:9,design:.monospaced))
        }.buttonStyle(.borderless).help(digest)
    }
    private var footer:some View {
        HStack(spacing:8) {
            Image(systemName:"info.circle").foregroundStyle(teal)
            Text(model.fireResponseMode ? "Recorded camera scene · actual ESP32-S3 firmware · synthetic flame cue · simulated Go2":model.patrolMode ? "Simulated Go2 · MuJoCo articulated Go2 · LiDAR + odometry navigation · viewer may disconnect":model.state?.mode == "local-inspection" ? "Synthetic office · MuJoCo 4 physics · local simulation · no physical hardware":(model.liveNativeReplica ? "Live simulation · MuJoCo 4 · Gateway A + Gateway B":(model.trackingOnly ? "MuJoCo rendered actors · image-based pose detection · anonymous estimates":"Synthetic office · MuJoCo 4 physics · simulated mesh · no physical hardware"))).font(.system(size:10))
            Spacer()
            Text(model.error ?? model.commandMessage).font(.system(size:10)).foregroundStyle(model.error == nil ? Color.secondary:.red).lineLimit(1)
        }.padding(.horizontal,22).frame(height:34)
    }
    private func eyebrow(_ text:String)->some View { Text(text).font(.system(size:9,weight:.semibold)).tracking(1.2).foregroundStyle(.secondary) }
    private func metric(_ label:String,_ value:String)->some View { HStack{Text(label).font(.system(size:10)).foregroundStyle(.secondary);Spacer();Text(value).font(.system(size:11,weight:.medium,design:.monospaced))} }
    private func friendly(_ value:String)->String { if value=="hub" {return "Site hub"};if value=="internet" {return "Internet"};return value.capitalized }
    private func readable(_ value:String)->String { value.replacingOccurrences(of:"_",with:" ").capitalized }
    private func robotColor(_ robot:Robot)->Color { Color(red:robot.color[0],green:robot.color[1],blue:robot.color[2]) }
    private func clock(_ seconds:Double)->String { String(format:"%02d:%02d",Int(seconds)/60,Int(seconds)%60) }
}
