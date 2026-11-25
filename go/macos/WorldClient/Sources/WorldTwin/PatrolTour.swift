import SwiftUI
import AppKit

struct PatrolTourPolicy:Codable {
    struct Stop:Codable,Identifiable {let room_id:String;let position:[Double];let yaw:Double;var id:String{room_id};var valid:Bool{!room_id.isEmpty && room_id.count<=128 && position.count==3 && position[1]==0.3 && position.allSatisfy(\.isFinite) && abs(position[0])<=9 && abs(position[2])<=7 && yaw.isFinite && abs(yaw)<=Double.pi}}
    let method:String;let stops:[Stop]
    var valid:Bool{method=="inspect-selected-rooms-once-and-return-v1" && !stops.isEmpty && stops.count<=8 && Set(stops.map(\.room_id)).count==stops.count && stops.allSatisfy(\.valid)}
}
struct PatrolTourStatus:Decodable {
    struct Visit:Decodable {let room_id:String;let sample_simulation_ns:Int64;let manifest_sha256:String;let evidence:[String]}
    let policy:PatrolTourPolicy;let visits:[Visit];let outcome:String
    var valid:Bool {policy.valid && visits.count<=policy.stops.count && ["pending","inspecting","incomplete","returning","completed"].contains(outcome) && visits.enumerated().allSatisfy {i,v in v.room_id==policy.stops[i].room_id && v.sample_simulation_ns>0 && v.manifest_sha256.count==64 && v.evidence.count>=4 && v.evidence.count<=8 && v.evidence.contains(v.manifest_sha256)} && (outcome != "completed" || visits.count==policy.stops.count)}
}
struct PatrolReviewRouter:View {
    var onClose: () -> Void = {}
    let tour:Bool;let inputURL:URL;let outputURL:URL
    var body:some View {if tour{PatrolTourReview(inputURL:inputURL,outputURL:outputURL,onClose:onClose)}else{PatrolScheduleReview(onClose:onClose,inputURL:inputURL,outputURL:outputURL)}}
}
@MainActor final class PatrolTourReviewModel:ObservableObject {
    let actions = TwinActions()
    @Published var completed = false
    struct Leg:Decodable {let reachable:Bool;let length:Double?;let route:[[Double]]?;let reason:String?}
    let inputURL:URL;let outputURL:URL
    @Published var input:PatrolScheduleReviewInput?
    @Published var selected:[String]=[]
    @Published var legs:[Leg]=[]
    @Published var reviewed=false
    @Published var busy=false
    @Published var error:String?
    private var generation=0
    private var timer:Timer?
    private var lastQA=""
    var options:[PatrolScheduleReviewInput.RendezvousOption] {guard let input else{return []};return (input.rendezvous_options ?? []).filter {p in input.rooms.contains {$0.id==p.id}}}
    var policy:PatrolTourPolicy {PatrolTourPolicy(method:"inspect-selected-rooms-once-and-return-v1",stops:selected.compactMap {id in options.first {$0.id==id}.map {PatrolTourPolicy.Stop(room_id:id,position:[$0.x,0.3,-$0.y],yaw:$0.yaw)}})}
    var canApprove:Bool {!busy && reviewed && policy.valid && legs.count==selected.count+1}
    init(inputURL:URL,outputURL:URL){self.inputURL=inputURL;self.outputURL=outputURL;do{let i=try PatrolScheduleReviewInput.load(inputURL);guard i.rendezvous_options?.first?.id=="start" else{throw DemoError.invalidFile};input=i}catch{self.error="Cannot review this commissioned room inventory."}}
    func start(){timer=Timer.scheduledTimer(withTimeInterval:0.15,repeats:true){[weak self] _ in MainActor.assumeIsolated { self?.actions.submit { [weak self] in await self?.pollQA() } }}}
    func stop(){timer?.invalidate();timer=nil}
    func select(_ id:String)->Bool {guard options.contains(where:{$0.id==id}),!busy else{return false};if selected.contains(id){selected.removeAll {$0==id}}else{guard selected.count<8 else{return false};selected.append(id)};changed();return true}
    func move(_ id:String,by:Int)->Bool {guard !busy,let index=selected.firstIndex(of:id),selected.indices.contains(index+by) else{return false};selected.swapAt(index,index+by);changed();return true}
    private func changed(){generation+=1;reviewed=false;legs=[];error=nil}
    func preview() async {
        guard !busy,policy.valid,let input,let raw=input.navigation_url,let base=URL(string:raw),base.scheme=="http",base.host=="127.0.0.1",let start=input.rendezvous_options?.first else{error="Select available rooms before preview.";return}
        busy=true;reviewed=false;legs=[];error=nil;let revision=generation
        defer{busy=false}
        do{
            var prior:[String:Double]=["x":start.x,"y":start.y,"yaw":start.yaw];var result:[Leg]=[]
            let points=policy.stops.map {["x":$0.position[0],"y":(-$0.position[2]),"yaw":$0.yaw]} + [["x":start.x,"y":start.y,"yaw":start.yaw]]
            for point in points {
                var target:[String:Any]=point;target["start"]=prior
                var request=URLRequest(url:base.appendingPathComponent(input.exclusions==nil ? "route":"policy/preview"));request.httpMethod="POST";request.timeoutInterval=30;request.setValue("application/json",forHTTPHeaderField:"Content-Type")
                if let restrictions=input.exclusions {request.httpBody=try JSONSerialization.data(withJSONObject:["policy":JSONSerialization.jsonObject(with:JSONEncoder().encode(restrictions)),"target":target,"return_target":point])}else{request.httpBody=try JSONSerialization.data(withJSONObject:target)}
                let (data,response)=try await URLSession.shared.data(for:request);guard let http=response as? HTTPURLResponse,http.statusCode==200,data.count<=512*1024 else{throw DemoError.invalidFile}
                let leg:Leg
                if input.exclusions != nil {struct Preview:Decodable{let restricted:Leg};leg=try JSONDecoder().decode(Preview.self,from:data).restricted}else{leg=try JSONDecoder().decode(Leg.self,from:data)}
                guard leg.length.map {$0.isFinite && $0>=0} ?? true,leg.route.map {$0.count<=20000 && $0.allSatisfy {$0.count>=2 && $0.allSatisfy(\.isFinite)}} ?? true else{throw DemoError.invalidFile}
                result.append(leg);prior=point
            }
            guard generation==revision else{return};legs=result
        }catch{self.error="Route preview unavailable. Check current localization, then preview again."}
    }
    func approve()->Bool {
        guard canApprove,let input,let start=input.rendezvous_options?.first else{error="Review the selected stops and complete route preview first.";return false}
        do{
            var document:[String:Any]=["tour":try JSONSerialization.jsonObject(with:JSONEncoder().encode(policy)),"return":["position":[start.x,0.3,-start.y],"yaw":start.yaw,"before_expiry_seconds":0]]
            if let x=input.exclusions{document["exclusions"]=try JSONSerialization.jsonObject(with:JSONEncoder().encode(x))}
            if let x=input.environment{document["environment"]=try JSONSerialization.jsonObject(with:JSONEncoder().encode(x))}
            if let x=input.work{document["work"]=try JSONSerialization.jsonObject(with:JSONEncoder().encode(x))}
            if let x=input.world_watch{document["world_watch"]=try x.document()}
            let data=try JSONSerialization.data(withJSONObject:document,options:[.sortedKeys,.prettyPrinted]);guard data.count<=16384 else{throw DemoError.invalidFile};try data.write(to:outputURL,options:.withoutOverwriting);try FileManager.default.setAttributes([.posixPermissions:0o600],ofItemAtPath:outputURL.path);return true
        }catch{self.error="Approval was not saved: \(error.localizedDescription)";return false}
    }
    private func pollQA() async {
        let request=inputURL.deletingLastPathComponent().appendingPathComponent("tour-qa-request.json")
        guard let raw=try? Data(contentsOf:request),raw.count<=4096,let q=(try? JSONSerialization.jsonObject(with:raw)) as? [String:String],let id=q["id"],id != lastQA,id.range(of:"^[A-Za-z0-9_-]{1,64}$",options:.regularExpression) != nil else{return};lastQA=id
        var accepted=false
        switch q["action"] {
        case "select":accepted=select(q["target"] ?? "")
        case "up":accepted=move(q["target"] ?? "",by:-1)
        case "preview":await preview();accepted=legs.count==selected.count+1
        case "review":reviewed=true;accepted=true
        case "approve":accepted=approve()
        case "capture":
            let host=NSHostingView(rootView:PatrolTourReview(model:self,active:false).environment(\.colorScheme,.light));host.appearance=NSAppearance(named:.aqua);host.frame=CGRect(x:0,y:0,width:1460,height:940);host.layoutSubtreeIfNeeded()
            if let bitmap=host.bitmapImageRepForCachingDisplay(in:host.bounds){host.cacheDisplay(in:host.bounds,to:bitmap);if let data=bitmap.representation(using:.png,properties:[:]){try? data.write(to:request.deletingLastPathComponent().appendingPathComponent("tour-qa-"+id+".png"));accepted=true}}
        default:break
        }
        let receipt:[String:Any]=["id":id,"action":q["action"] ?? "","accepted":accepted,"selected":selected,"preview_legs":legs.count,"kind":"native_application_model_not_OS_input"]
        if let data=try? JSONSerialization.data(withJSONObject:receipt,options:.sortedKeys){try? data.write(to:request.deletingLastPathComponent().appendingPathComponent("tour-qa-"+id+".json"),options:.atomic)}
        if q["action"]=="approve" && accepted {completed = true}
    }
}
@MainActor struct PatrolTourReview:View {
    @State private var actions = TwinActions()

    var onClose: () -> Void = {}
    @StateObject private var model:PatrolTourReviewModel;let active:Bool
    init(inputURL:URL,outputURL:URL,onClose:@escaping () -> Void = {}){self.onClose=onClose;_model=StateObject(wrappedValue:PatrolTourReviewModel(inputURL:inputURL,outputURL:outputURL));active=true}
    init(model:PatrolTourReviewModel,active:Bool){_model=StateObject(wrappedValue:model);self.active=active}
    var body:some View {
        Group {

        VStack(alignment:.leading,spacing:18){
            HStack{Image(systemName:"list.number").font(.system(size:32)).foregroundStyle(.teal);VStack(alignment:.leading){Text("Inspect selected rooms").font(.system(size:30,weight:.semibold));Text("ARTICULATED GO2 SIMULATION · FINITE SIGNED TOUR").font(.caption.monospaced()).foregroundStyle(.secondary)};Spacer();Text("One observation per stop\nThen return and settle").multilineTextAlignment(.trailing).foregroundStyle(.teal)}
            Text("Choose an ordered inspection. The reviewed destinations are fixed for this mission. Fresh camera, LiDAR, gas and audio originals are captured only after arrival and settling. A blocked or interrupted stop remains incomplete.").font(.callout)
            Divider()
            HStack(alignment:.top,spacing:25){
                VStack(alignment:.leading,spacing:12){Text("AVAILABLE ROOMS").font(.caption.bold()).foregroundStyle(.secondary);ForEach(model.options){room in Button{_ = model.select(room.id)}label:{HStack{Image(systemName:model.selected.contains(room.id) ? "checkmark.circle.fill":"circle");VStack(alignment:.leading){Text(room.name).font(.headline);Text(String(format:"X %.2f · Y %.2f m · yaw %.2f",room.x,room.y,room.yaw)).font(.caption.monospaced())};Spacer()}.padding(14).background(model.selected.contains(room.id) ? Color.teal.opacity(0.10):Color.white,in:RoundedRectangle(cornerRadius:12))}.buttonStyle(.plain).disabled(model.busy)};Spacer();Text("Unavailable rooms have no commissioned reachable candidate. Preview is a planning snapshot; navigation rechecks live conditions.").font(.caption).foregroundStyle(.secondary)}.frame(width:330)
                Divider()
                VStack(alignment:.leading,spacing:12){Text("SIGNED ORDER & OBSERVATION POINTS").font(.caption.bold()).foregroundStyle(.secondary);ForEach(Array(model.selected.enumerated()),id:\.element){index,id in HStack{Text(String(index+1)).font(.title3.bold()).foregroundStyle(.teal);Text(model.options.first{$0.id==id}?.name ?? id);Spacer();Button{_ = model.move(id,by:-1)}label:{Image(systemName:"arrow.up")}.disabled(index==0 || model.busy);Button{_ = model.move(id,by:1)}label:{Image(systemName:"arrow.down")}.disabled(index==model.selected.count-1 || model.busy)}.padding(12).background(Color.white,in:RoundedRectangle(cornerRadius:10))};if model.selected.isEmpty{Text("Select a room to begin.").padding(20).foregroundStyle(.secondary)};Text("Return to the starting rendezvous after all selected observations. Early Finish and return cancels remaining stops and produces an incomplete inspection.").font(.callout).foregroundStyle(.secondary);Button(model.busy ? "Planning route legs…":"Preview complete tour and return"){actions.submit { [self] in await model.preview()}}.buttonStyle(.borderedProminent).disabled(model.busy || !model.policy.valid);ForEach(Array(model.legs.enumerated()),id:\.offset){i,leg in Label((i<model.selected.count ? "Stop \(i+1)":"Return")+(leg.reachable ? String(format:" · %.1f m",leg.length ?? 0):" · Blocked, inspection will hold"),systemImage:leg.reachable ? "checkmark.circle":"exclamationmark.triangle").font(.callout).foregroundStyle(leg.reachable ? Color.teal:.orange)};Spacer()}.frame(width:370)
                VStack(alignment:.leading,spacing:12){Text("ROUTE PREVIEW").font(.caption.bold()).foregroundStyle(.secondary);PatrolTourMap(context:model.input?.policy_context,policy:model.policy,legs:model.legs).frame(width:540,height:390).background(Color.white,in:RoundedRectangle(cornerRadius:14));Text("Teal: ordered observation points · Blue: route previews · Orange: approved return. The preview does not authorize motion.").font(.caption).foregroundStyle(.secondary);if let i=model.input {Text("Original deadline: \(i.duration_seconds ?? 3600) seconds. LiDAR, odometry and localization remain mandatory. Stop is permanent for this mission.").font(.callout);if let w=i.work{Text(String(format:"Signed work allowance %.0f J, return reserve %.0f J",w.limit_j,w.return_reserve_j)).font(.caption)};if let p=i.environment{Text(String(format:"Local gas hold ≥ %.1f ppm; explicit recovery",p.hold_ppm)).font(.caption)};if let p=i.world_watch{Text("World watch: original compatible evidence required · "+p.method).font(.caption)};if let p=i.exclusions{Text("Restricted policy: "+p.sha256).font(.caption.monospaced())}};Text("Simulation only. Arrival or inspection does not establish that a room is safe.").font(.caption).foregroundStyle(.orange);Spacer()}
            }
            if let error=model.error{Text(error).foregroundStyle(.red)}
            Divider();Toggle("I reviewed the ordered stops, fixed destinations, return and retained mission conditions",isOn:$model.reviewed)
            HStack{Button("Cancel"){onClose()};Spacer();Text("No motion occurs until this separate review is approved.").font(.caption).foregroundStyle(.secondary);Button(model.input?.offline_prepare == true ? "Approve and prepare offline tour":"Approve and start inspection"){if model.approve(){onClose()}}.buttonStyle(.borderedProminent).disabled(!model.canApprove)}
        }.padding(26).background(Color(red:0.96,green:0.97,blue:0.95)).foregroundStyle(Color(red:0.10,green:0.20,blue:0.20)).frame(minWidth:1400,minHeight:900).preferredColorScheme(.light).onAppear{if active{model.start()}}.onDisappear{if active{model.stop()}}.task { if active { await model.actions.run() } }.onChange(of:model.completed) { _, done in if done { onClose() } }

        }.task { await actions.run() }
}
}
struct PatrolTourMap:View {
    let context:PatrolPolicyContext?;let policy:PatrolTourPolicy;let legs:[PatrolTourReviewModel.Leg]
    var body:some View {Canvas {c,size in
        guard let g=context?.grid else{return};let width=Double(g.width)*g.resolution,height=Double(g.height)*g.resolution
        func point(_ x:Double,_ y:Double)->CGPoint{CGPoint(x:(x-g.origin[0])/width*size.width,y:size.height-(y-g.origin[1])/height*size.height)}
        for y in 0..<g.height {for x in 0..<g.width where g.data[y*g.width+x]>50 {let a=point(g.origin[0]+Double(x)*g.resolution,g.origin[1]+Double(y+1)*g.resolution);c.fill(Path(CGRect(x:a.x,y:a.y,width:size.width/Double(g.width)+0.4,height:size.height/Double(g.height)+0.4)),with:.color(.gray.opacity(0.4)))}}
        for (i,leg) in legs.enumerated(){if let route=leg.route,!route.isEmpty {var path=Path();for (j,p) in route.enumerated(){let a=point(p[0],p[1]);if j==0{path.move(to:a)}else{path.addLine(to:a)}};c.stroke(path,with:.color(i==policy.stops.count ? .orange:.blue),lineWidth:2)}}
        for (i,s) in policy.stops.enumerated(){let p=point(s.position[0],-s.position[2]);c.fill(Path(ellipseIn:CGRect(x:p.x-10,y:p.y-10,width:20,height:20)),with:.color(.teal));c.draw(Text(String(i+1)).font(.caption.bold()).foregroundColor(.white),at:p)}
    }}
}

struct PatrolTourProgress:View {
    let tour:PatrolTourStatus
    var body:some View {VStack(alignment:.leading,spacing:10){
        Text(tour.outcome=="completed" ? "Inspection complete":tour.outcome=="incomplete" ? "Inspection incomplete":tour.outcome=="returning" ? "Observations retained · returning":"Inspection in progress").font(.headline).foregroundStyle(tour.outcome=="incomplete" ? Color.orange:.teal)
        Text("\(tour.visits.count) / \(tour.policy.stops.count) original observation bundles").font(.caption.monospaced())
        ForEach(Array(tour.policy.stops.enumerated()),id:\.element.room_id){i,s in VStack(alignment:.leading,spacing:3){Label("\(i+1). "+s.room_id,systemImage:i<tour.visits.count ? "checkmark.circle.fill":"circle.dashed").font(.callout);if i<tour.visits.count {Text(String(format:"Original capture %.3f s · %@",Double(tour.visits[i].sample_simulation_ns)/1e9,String(tour.visits[i].manifest_sha256.prefix(12)))).font(.caption2.monospaced()).foregroundStyle(.secondary)}else{Text("Unobserved; no coverage claim").font(.caption2).foregroundStyle(.secondary)}}}
        Text("A return without every required observation is incomplete. Captures describe retained evidence, never room clearance.").font(.caption).foregroundStyle(.secondary)
    }.padding(12).background(Color.teal.opacity(0.06),in:RoundedRectangle(cornerRadius:12))}
}
