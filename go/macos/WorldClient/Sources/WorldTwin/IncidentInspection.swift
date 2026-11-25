import SwiftUI
import AppKit
import WorldClient

struct IncidentInspectionStatus: Decodable {
    let run_id, reader, phase, knowledge: String
    let snapshot_unix_ms: Int64
    let task_id, trigger_sha256: String?
    let observed, destination, robot_position: [Double]?
    let samples: Int
    let media_verified: Int
    let record_sha256: String?
    let source_capture, source_valid_until: String?
    let route: [[Double]]?
}

extension IncidentModel {
    var inspectionAvailable: Bool { FileManager.default.fileExists(atPath:session.appendingPathComponent("mission-ui.json").path) }
    func pollInspection() {
        if let entry=missions.entries.last(where:{$0.instruction != nil}),let run=snapshot?.run_id,
           let data=try? JSONSerialization.data(withJSONObject:["task_id":entry.id,"run_id":run,"trigger_sha256":entry.draft.trigger?.sha256 ?? ""]) {
            let file=session.appendingPathComponent("inspection-task.json")
            if (try? Data(contentsOf:file)) != data {try? data.write(to:file,options:.atomic)}
        }
        let file=session.appendingPathComponent("inspection-status.json")
        guard let size=try? file.resourceValues(forKeys:[.fileSizeKey]).fileSize, size<=32<<10,
              let data=try? Data(contentsOf:file),let value=try? JSONDecoder().decode(IncidentInspectionStatus.self,from:data),
              value.run_id==snapshot?.run_id,value.reader==snapshot?.reader,
              [value.observed,value.destination,value.robot_position].compactMap({$0}).allSatisfy({$0.count==3 && $0.allSatisfy{$0.isFinite && abs($0)<10000}}),
              value.samples>=0,value.samples<=4,value.media_verified>=0,value.media_verified<=3 else{return}
        let age=wallClock().timeIntervalSince1970-Double(value.snapshot_unix_ms)/1000
        guard age >= -0.5,value.snapshot_unix_ms>0 else{return}
        if let prior=inspectionPoseIdentity,value.snapshot_unix_ms<prior{return}
        if inspectionPoseIdentity != value.snapshot_unix_ms {
            inspectionPoseIdentity=value.snapshot_unix_ms
            inspectionPoseDeadline=clock().advanced(by:.seconds(max(0,2.5-age)))
        }
        inspectionStatus=value
    }
    func prepareInspection() async {
        guard inspectionAvailable,!preparingInspection,state=="consistent",let snap=snapshot else{return}
        preparingInspection=true;defer{preparingInspection=false}
        let id=UUID().uuidString.lowercased();inspectionMessage="Preparing the selected evidence for review…"
        do {
            let raw=try JSONSerialization.data(withJSONObject:["request_id":id,"run_id":snap.run_id,"input_digest":snap.result.input_digest])
            try raw.write(to:session.appendingPathComponent("selection-request.json"),options:.atomic)
            let end=ContinuousClock.now.advanced(by:.seconds(15));var response:[String:Any]?
            while ContinuousClock.now<end {
                try Task.checkCancellation()
                if let data=try? Data(contentsOf:session.appendingPathComponent("selection-response.json")),data.count<=8192,
                   let result=try? JSONSerialization.jsonObject(with:data) as? [String:Any],result["request_id"] as? String==id {response=result;break}
                try await Task.sleep(for:.milliseconds(150))
            }
            guard let response else{throw CocoaError(.fileReadUnknown)}
            if let error=response["error"] as? String{inspectionMessage=error;return}
            guard let hash=response["envelope_sha256"] as? String,incidentDigest(hash)else{throw CocoaError(.fileReadCorruptFile)}
            await missions.load(configURL:session.appendingPathComponent("mission-ui.json"),runID:snap.run_id,selectedEnvelope:hash,replay:false)
            inspectionMessage=missions.message;inspectionSheet=true
        }catch{inspectionMessage="Preparation was not confirmed. No inspection was signed."}
    }
    func inspectionAction(_ action:String) async -> Bool {
        switch action {
        case "inspection-pose-expiry-check":
            pollInspection()
            guard let deadline=inspectionPoseDeadline,deadline>clock() else{return false}
            let originalClock=clock,originalWall=wallClock,now=clock()
            defer{clock=originalClock;wallClock=originalWall}
            clock={now.advanced(by:.seconds(3))};pollInspection()
            guard inspectionPoseDeadline==deadline,deadline<clock() else{return false}
            wallClock={originalWall().addingTimeInterval(-3600)};pollInspection()
            return inspectionPoseDeadline==deadline && deadline<clock()
        case "inspection-prepare":await prepareInspection();return missions.handoff != nil
        case "inspection-review":await missions.review();inspectionMessage=missions.message;return missions.canSign
        case "inspection-sign":guard missions.canSign else{return false};await missions.signAndSend();inspectionMessage=missions.message;return missions.entries.contains{$0.id==missions.draft?.id && $0.instruction != nil}
        case "inspection-refresh":for entry in missions.entries{await missions.reconcile(entry.id)};inspectionMessage=missions.message;return true
        case "inspection-stop":guard let entry=missions.entries.last else{return false};await missions.stop(entry.id);return true
        default:return false
        }
    }
}

struct IncidentInspectionCard: View {
    @State private var actions = TwinActions()

    @ObservedObject var model:IncidentModel
    @ObservedObject var missions:DemoMissions
    var body:some View {
        Group {

        VStack(alignment:.leading,spacing:8) {
            Text("INSPECTION").font(.system(size:11,weight:.bold,design:.monospaced)).tracking(1)
            if let status=model.inspectionStatus {
                Text(status.phase.replacingOccurrences(of:"_",with:" ").capitalized).font(.headline)
                Text(status.knowledge).font(.caption).foregroundStyle(.secondary)
                if let hash=status.trigger_sha256 {Text("Selected evidence "+hash.prefix(12)).font(.system(size:10,design:.monospaced))}
                if let point=status.destination {Text(String(format:"Fixed destination %.2f, %.2f m",point[0],point[2])).font(.caption)}
                if status.samples>0 {Text("\(status.samples) retained sensor samples · \(status.media_verified) verified media").font(.caption)}
            }
            if model.inspectionAvailable {
                Button("Prepare inspection") {actions.submit { [self] in await model.prepareInspection()}}.disabled(model.state != "consistent" || model.preparingInspection || missions.busy)
                Button("Review / saved inspections") {model.inspectionSheet=true}.disabled(!missions.enabled)
                Text(model.inspectionMessage).font(.caption).foregroundStyle(.secondary).fixedSize(horizontal:false,vertical:true)
            }
            Text("Preparation and review do not move the robot. A separate operator signature authorizes this simulated mission.").font(.caption).foregroundStyle(.secondary).fixedSize(horizontal:false,vertical:true)
        }.padding(12).frame(maxWidth:.infinity,alignment:.leading).background(Color.white).clipShape(RoundedRectangle(cornerRadius:10))
        .sheet(isPresented:$model.inspectionSheet){DemoMissionPanel(missions:missions)}

        }.task { await actions.run() }
}
}

struct IncidentTriggerReview: View {
    let trigger: MissionNativeTrigger
    private var payload: [String:Any] {
        ((try? JSONSerialization.jsonObject(with:trigger.canonical)) as? [String:Any])?["component"] as? [String:Any] ?? [:]
    }
    var body:some View {
        let selected=payload["payload"] as? [String:Any] ?? [:]
        let supports=selected["supports"] as? [[String:Any]] ?? []
        VStack(alignment:.leading,spacing:4) {
            Text("Retained association · independent gas and visual evidence").font(.caption).bold()
            ForEach(supports.indices,id:\.self) { index in
                let support=supports[index]
                Text((support["kind"] as? String ?? "Evidence").capitalized+" · "+String((support["sha256"] as? String ?? "").prefix(12))+" · originally valid until "+(support["valid_until"] as? String ?? "unknown"))
                    .font(.system(size:10,design:.monospaced)).textSelection(.enabled)
            }
            Text("Historical review context. These lifetimes are not renewed by signing; the cameras do not detect gas.").font(.caption).foregroundStyle(.secondary)
        }.frame(maxWidth:.infinity,alignment:.leading)
    }
}
