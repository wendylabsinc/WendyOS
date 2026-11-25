import SwiftUI
import AppKit
import WorldClient

extension IncidentChangeModel {
    func prepareRisingInspection() async {
        guard state=="supported_rise",spatial.inspectionAvailable,!spatial.preparingInspection,let snap=snapshot else{return}
        spatial.preparingInspection=true;defer{spatial.preparingInspection=false}
        let id=UUID().uuidString.lowercased();spatial.inspectionMessage="Retaining this exact rising episode for review…"
        do{
            let raw=try JSONSerialization.data(withJSONObject:["request_id":id,"run_id":snap.run_id,"input_digest":snap.result.input_digest])
            try raw.write(to:session.appendingPathComponent("selection-request.json"),options:.atomic)
            let end=ContinuousClock.now.advanced(by:.seconds(15));var response:[String:Any]?
            while ContinuousClock.now<end{
                try Task.checkCancellation()
                if let data=try? Data(contentsOf:session.appendingPathComponent("selection-response.json")),data.count<=8192,let value=try? JSONSerialization.jsonObject(with:data)as?[String:Any],value["request_id"]as?String==id{response=value;break}
                try await Task.sleep(for:.milliseconds(100))
            }
            guard let response else{throw CocoaError(.fileReadUnknown)}
            if let error=response["error"]as?String{spatial.inspectionMessage=error;return}
            guard let hash=response["envelope_sha256"]as?String,incidentDigest(hash)else{throw CocoaError(.fileReadCorruptFile)}
            await spatial.missions.load(configURL:session.appendingPathComponent("mission-ui.json"),runID:snap.run_id,selectedEnvelope:hash,replay:false)
            spatial.inspectionMessage=spatial.missions.message;spatial.inspectionSheet=true
        }catch{spatial.inspectionMessage="Preparation was not confirmed. No inspection was signed."}
    }
    func risingInspectionAction(_ action:String)async->Bool{
        if action=="inspection-prepare"{await prepareRisingInspection();return spatial.missions.handoff != nil}
        return await spatial.inspectionAction(action)
    }
}

struct RisingInspectionCard:View {
    @State private var actions = TwinActions()

    @ObservedObject var model:IncidentChangeModel
    @ObservedObject var spatial:IncidentModel
    @ObservedObject var missions:DemoMissions
    var body:some View{
        Group {

        VStack(alignment:.leading,spacing:7){
            HStack{Text("INSPECT THIS RISING EPISODE").font(.system(size:11,weight:.bold,design:.monospaced));Spacer();Text(spatial.inspectionStatus?.phase.replacingOccurrences(of:"_",with:" ").uppercased() ?? "NO APPROVAL").font(.system(size:10,weight:.bold))}
            if let status=spatial.inspectionStatus{
                Text(status.knowledge).font(.caption).foregroundStyle(.secondary)
                if let hash=status.trigger_sha256{Text("RETAINED TRIGGER  "+hash.prefix(18)+"…").font(.system(size:10,design:.monospaced))}
                if let point=status.destination{Text(String(format:"Fixed reviewed destination %.2f, %.2f m",point[0],point[2])).font(.caption)}
                if status.samples>0{Text("\(status.samples) sensor samples · \(status.media_verified) verified media").font(.caption)}
            }
            if spatial.inspectionAvailable{
                HStack{
                    Button("Prepare rising inspection"){actions.submit { [self] in await model.prepareRisingInspection()}}.disabled(model.state != "supported_rise" || spatial.preparingInspection || missions.busy)
                    Button("Review / saved"){spatial.inspectionSheet=true}.disabled(!missions.enabled)
                }
                Text(spatial.inspectionMessage).font(.caption).foregroundStyle(.secondary)
            }
            Text("Exact original rise retained as historical context. Review and a separate operator signature bind one fixed destination; later evidence cannot retarget it.").font(.system(size:10)).foregroundStyle(.secondary)
        }.sheet(isPresented:$spatial.inspectionSheet){DemoMissionPanel(missions:missions).frame(minWidth:780,minHeight:650)}

        }.task { await actions.run() }
}
}


struct RisingTriggerReview:View {
    let trigger:MissionNativeTrigger
    private var selected:[String:Any] {
        let object=(try? JSONSerialization.jsonObject(with:trigger.canonical)) as? [String:Any]
        return (object?["component"] as? [String:Any])?["payload"] as? [String:Any] ?? [:]
    }
    var body:some View {
        let p=selected
        let raw=trigger.risingTemporalOriginalJSON
        let component=raw.flatMap{try? JSONSerialization.jsonObject(with:$0)} as? [String:Any]
        let claim=component?["payload"] as? [String:Any] ?? [:]
        let window=claim["window"] as? [String:Any] ?? [:]
        let traces=window["traces"] as? [[String:Any]] ?? []
        VStack(alignment:.leading,spacing:4) {
            Text("Retained rising episode · four sensors, twelve original readings").font(.caption).bold()
            Text("Original support until "+(p["valid_until"] as? String ?? "unknown")).font(.system(size:10,design:.monospaced)).textSelection(.enabled)
            Text("Exact temporal claim "+String((p["temporal_sha256"] as? String ?? "").prefix(20))+"…").font(.system(size:10,design:.monospaced)).textSelection(.enabled)
            DisclosureGroup("Original sensor windows") {
                ForEach(traces.indices,id:\.self){index in
                    let t=traces[index];let points=t["points"] as? [[String:Any]] ?? []
                    Text(String(format:"%@ · %.3f–%.3f simulation s · minimum rise %.2f ppm/s",t["sensor"] as? String ?? "?",points.first?["captured_s"] as? Double ?? 0,points.last?["captured_s"] as? Double ?? 0,t["min_rate_ppm_s"] as? Double ?? 0)).font(.system(size:10,design:.monospaced))
                }
            }.font(.caption)
            Text("Gateway-attested historical context. Signing does not renew its evidence or authorize automatic pursuit.").font(.caption).foregroundStyle(.secondary)
        }.frame(maxWidth:.infinity,alignment:.leading)
    }
}
