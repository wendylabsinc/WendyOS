import AppKit
import CryptoKit
import Darwin
import SwiftUI

struct IncidentChangeWitness:Codable {let sensor,sha256,captured_at,valid_until:String}
struct IncidentChangeTemporal:Codable {let state,reason,valid_until:String;let gateway,sha256:String?;let window:ChangeWindow?}
struct IncidentChangeCarrier:Codable {let sha256,gas_sha256,change_sha256,valid_until:String;let source:IncidentSource;let version:GasObservation.Version}
struct IncidentChangeResult:Codable {let schema,contract_sha256,state,reason,at,valid_until,input_digest:String;let spatial:IncidentResult;let temporal:IncidentChangeTemporal;let witnesses:[IncidentChangeWitness];let supports:[IncidentSupport];let carriers:[IncidentChangeCarrier]?}
struct IncidentChangeSnapshot:Codable {let schema,run_id,reader,node,mode:String;let sequence:Int;let written_unix_s:Double;let result:IncidentChangeResult;let spatial_view:IncidentSnapshot;let temporal_paused,dropout:Bool}
@MainActor final class IncidentChangeModel:ObservableObject {
 let actions = TwinActions()

 let session:URL
 let spatial:IncidentModel
 @Published var snapshot:IncidentChangeSnapshot?
 @Published var tick=0
 @Published var error:String?
 @Published var pending:Int?
 var clock:()->ContinuousClock.Instant={.now}
 var wallClock:()->Date={Date()}
 private var timer:Timer?
 private var updated:ContinuousClock.Instant?
 private var joined:(String,ContinuousClock.Instant)?
 private var temporal:(String,ContinuousClock.Instant)?
 private var lastCommand=0
 private var pendingAt:ContinuousClock.Instant?
 private var qaSequence=0
 private var observationSequence=0
 private var observationStart:ContinuousClock.Instant?
 private var observationSeconds=0.0
 private var observationSamples:[[String:Any]]=[]
 init(session:URL){self.session=session;spatial=IncidentModel(session:session)}
 var fresh:Bool{error==nil && (updated.map{$0.duration(to:clock()) < .seconds(3.5)} ?? false)}
 var temporalVisible:Bool{fresh && temporal.map{clock()<$0.1}==true}
 var state:String{guard fresh,let s=snapshot else{return "unavailable"};if ["supported_rise","non_rising"].contains(s.result.state){return joined.map{clock()<$0.1}==true && spatial.state=="consistent" && temporalVisible ? s.result.state:"unavailable"};return s.result.state}
 var reason:String{guard fresh else{return "Producer view expired. Retained images are historical."};if state != snapshot?.result.state{return "Original joined support expired. Independent spatial layers retain their own lifetimes."};return snapshot?.result.reason ?? "Waiting for current evidence"}
 var canCommand:Bool{fresh && snapshot?.reader=="east" && pending==nil && lastCommand<4096}
 func start(){guard timer==nil else{return};poll();timer=Timer.scheduledTimer(withTimeInterval:0.1,repeats:true){[weak self]_ in MainActor.assumeIsolated{self?.poll()}}}
 func stop(){timer?.invalidate();timer=nil}
 private func read(_ name:String,_ limit:Int)throws->Data{let url=session.appendingPathComponent(name);let v=try url.resourceValues(forKeys:[.isRegularFileKey,.isSymbolicLinkKey,.fileSizeKey]);guard v.isRegularFile==true,v.isSymbolicLink != true,let size=v.fileSize,size<=limit else{throw CocoaError(.fileReadCorruptFile)};return try Data(contentsOf:url)}
 func accept(_ s:IncidentChangeSnapshot,now:ContinuousClock.Instant)throws{
  let r=s.result;let states=["supported_rise","non_rising","unsupported","unavailable"]
  guard s.schema=="wendy.incident-change-demo.v1",r.schema=="wendy.incident.change.v1",states.contains(r.state),incidentDigest(r.contract_sha256),incidentDigest(r.input_digest),s.sequence>0,s.sequence<=1000000,s.written_unix_s.isFinite,abs(wallClock().timeIntervalSince1970-s.written_unix_s)<4,s.reader==s.spatial_view.reader,s.node==s.spatial_view.node,s.run_id==s.spatial_view.run_id,s.sequence==s.spatial_view.sequence,r.supports.count<=3,r.witnesses.count<=4,Set(r.supports.map{$0.kind}).count==r.supports.count,let at=incidentDate(r.at),abs(at.timeIntervalSince(wallClock()))<4 else{throw CocoaError(.fileReadCorruptFile)}
  // The nested spatial snapshot is the same exact evaluation, not a separately
  // relabeled location that can be attached to a temporal claim.
  let encoder=JSONEncoder();encoder.outputFormatting=[.sortedKeys]
  guard try encoder.encode(r.spatial)==encoder.encode(s.spatial_view.result) else{throw CocoaError(.fileReadCorruptFile)}
  if let old=snapshot{guard s.run_id==old.run_id,s.reader==old.reader,s.node==old.node,r.contract_sha256==old.result.contract_sha256,s.sequence>old.sequence else{throw CocoaError(.fileReadCorruptFile)}}
  var nextTemporal:(String,ContinuousClock.Instant)?
  if let w=r.temporal.window {
   guard ["rising","falling","steady","mixed"].contains(w.state),w.state==r.temporal.state,incidentDigest(w.input_digest),let hash=r.temporal.sha256,incidentDigest(hash),r.temporal.gateway==r.spatial.gas.gateway,w.traces.count==4,Set(w.traces.map{$0.sensor}).count==4,w.evaluated_s.isFinite,w.valid_until_s.isFinite,w.valid_until_s>w.evaluated_s,let until=incidentDate(r.temporal.valid_until),until>at else{throw CocoaError(.fileReadCorruptFile)}
   let anchor=at.timeIntervalSince1970-r.spatial.gas.result.now_s
   guard until.timeIntervalSince1970<=anchor+w.valid_until_s+0.002 else{throw CocoaError(.fileReadCorruptFile)}
   for t in w.traces {guard t.points.count==3,t.min_rate_ppm_s.isFinite,t.max_rate_ppm_s.isFinite,t.min_rate_ppm_s<=t.max_rate_ppm_s else{throw CocoaError(.fileReadCorruptFile)};var previous = -Double.infinity
    for p in t.points{guard p.captured_s.isFinite,p.captured_s>previous,p.value_ppm.isFinite,(0...10000).contains(p.value_ppm),p.uncertainty_ppm.isFinite,(0.1...100).contains(p.uncertainty_ppm),p.valid_until_s.isFinite,p.valid_until_s>p.captured_s,p.valid_until_s-p.captured_s<=5.001,incidentDigest(p.sha256),w.valid_until_s<=p.valid_until_s else{throw CocoaError(.fileReadCorruptFile)};previous=p.captured_s}
   }
   var deadline=now.advanced(by:.seconds(max(0,until.timeIntervalSince(wallClock()))));if let old=temporal,old.0==hash{deadline=min(deadline,old.1)};nextTemporal=(hash,deadline)
  }else{guard r.temporal.state=="unavailable",r.temporal.sha256==nil else{throw CocoaError(.fileReadCorruptFile)}}
  var nextJoined:(String,ContinuousClock.Instant)?
  if ["supported_rise","non_rising"].contains(r.state){
   guard r.spatial.state=="consistent",r.witnesses.count==4,Set(r.witnesses.map{$0.sensor}).count==4,let w=r.temporal.window,r.supports.count==3,Set(r.supports.map{$0.kind})==Set(["gas","visual","change"]),r.supports.first(where:{$0.kind=="change"})?.sha256==r.temporal.sha256,let until=incidentDate(r.valid_until),until>at,(r.state=="supported_rise")==(w.state=="rising") else{throw CocoaError(.fileReadCorruptFile)}
   guard try encoder.encode(r.supports.filter{$0.kind != "change"})==encoder.encode(r.spatial.supports),let changeSupport=r.supports.first(where:{$0.kind=="change"}),changeSupport.valid_until==r.temporal.valid_until,changeSupport.source==r.spatial.supports.first(where:{$0.kind=="gas"})?.source else{throw CocoaError(.fileReadCorruptFile)}
   for support in r.supports{guard incidentDigest(support.sha256),let bound=incidentDate(support.valid_until),until<=bound else{throw CocoaError(.fileReadCorruptFile)}}
   for witness in r.witnesses{guard let trace=w.traces.first(where:{$0.sensor==witness.sensor}),let last=trace.points.last,witness.sha256==last.sha256,let captured=incidentDate(witness.captured_at),let bound=incidentDate(witness.valid_until),bound>captured,until<=bound,abs(captured.timeIntervalSince1970-(at.timeIntervalSince1970-r.spatial.gas.result.now_s+last.captured_s))<0.002,last.valid_until_s<=bound.timeIntervalSince1970-(at.timeIntervalSince1970-r.spatial.gas.result.now_s)+0.002 else{throw CocoaError(.fileReadCorruptFile)}}
   var deadline=now.advanced(by:.seconds(max(0,until.timeIntervalSince(wallClock()))));if let old=joined,old.0==r.input_digest{deadline=min(deadline,old.1)};nextJoined=(r.input_digest,deadline)
  }else{guard r.supports.isEmpty else{throw CocoaError(.fileReadCorruptFile)}}
  if let carriers=r.carriers {
   guard carriers.count<=1 else{throw CocoaError(.fileReadCorruptFile)}
   for carrier in carriers {guard incidentDigest(carrier.sha256),carrier.gas_sha256==r.spatial.gas.sha256,carrier.change_sha256==r.temporal.sha256,let bound=incidentDate(carrier.valid_until),bound>at else{throw CocoaError(.fileReadCorruptFile)}
    if !r.supports.isEmpty {guard let gas=r.supports.first(where:{$0.kind=="gas"}),let change=r.supports.first(where:{$0.kind=="change"}),gas.source==carrier.source,change.source==carrier.source,gas.version.epoch==carrier.version.epoch,change.version.epoch==carrier.version.epoch,gas.version.sequence==carrier.version.sequence,change.version.sequence==carrier.version.sequence,let until=incidentDate(r.valid_until),until<=bound else{throw CocoaError(.fileReadCorruptFile)}}
   }
  }
  spatial.clock=clock;spatial.wallClock=wallClock;try spatial.accept(s.spatial_view,now:now)
  temporal=nextTemporal;joined=nextJoined;snapshot=s;updated=now;lastCommand=max(lastCommand,s.spatial_view.last_command_sequence);error=nil
 }
 func poll(){tick+=1
  do{let s=try JSONDecoder().decode(IncidentChangeSnapshot.self,from:read("snapshot.json",1024<<10))
   if snapshot==nil || s.sequence>snapshot!.sequence{
    let view=s.spatial_view;guard ["scene-0.png","scene-1.png"].contains(view.image),incidentDigest(view.image_sha256),view.cameras.count<=2 else{throw CocoaError(.fileReadCorruptFile)}
    var images:[String:NSImage]=[:]
    func image(_ name:String,_ sha:String)throws->NSImage{let raw=try read(name,4<<20);guard SHA256.hash(data:raw).map({String(format:"%02x",$0)}).joined()==sha,let image=NSImage(data:raw)else{throw CocoaError(.fileReadCorruptFile)};return image}
    images["scene"]=try image(view.image,view.image_sha256)
    for c in view.cameras{guard ["camera-a","camera-b"].contains(c.name),[c.name+"-0.png",c.name+"-1.png"].contains(c.file)else{throw CocoaError(.fileReadCorruptFile)};images[c.name]=try image(c.file,c.sha256)}
    try accept(s,now:clock());spatial.images=images
   }
   if let seq=pending {let path=String(format:"receipts/%06d.json",seq);if FileManager.default.fileExists(atPath:session.appendingPathComponent(path).path){struct Receipt:Decodable{let sequence:Int;let accepted:Bool;let error:String};let r=try JSONDecoder().decode(Receipt.self,from:read(path,4096));guard r.sequence==seq,r.accepted else{throw CocoaError(.fileReadCorruptFile)};pending=nil;pendingAt=nil}else if let at=pendingAt,at.duration(to:clock()) > .seconds(6){error="Simulation control receipt timed out";pending=nil}}
  }catch{self.error="Current snapshot unavailable or inconsistent"}
  spatial.pollInspection();pollQA();pollObservation()
 }
 @discardableResult func command(_ action:String)->Bool{
  guard canCommand,let s=snapshot,["ramp","steady","decline","spike","temporal","displace","partition","duplicates","restart","dropout"].contains(action)else{return false};let seq=lastCommand+1
  do{let data=try JSONSerialization.data(withJSONObject:["run_id":s.run_id,"sequence":seq,"action":action,"target":""]);let dest=session.appendingPathComponent(String(format:"commands/%06d.json",seq));let tmp=dest.deletingLastPathComponent().appendingPathComponent(".command-"+UUID().uuidString);try data.write(to:tmp,options:.withoutOverwriting);defer{try? FileManager.default.removeItem(at:tmp)};guard link(tmp.path,dest.path)==0 else{return false};lastCommand=seq;pending=seq;pendingAt=clock();return true}catch{self.error="Could not write simulation control";return false}
 }
 // Opt-in bounded diagnostics measure the actual live model; no clock freezing,
 // replay import or freshness extension is used for these visibility receipts.
 private func pollObservation(){
  struct Request:Decodable{let sequence:Int;let seconds:Double}
  if observationStart==nil,let raw=try? read("native-observe.json",4096),let request=try? JSONDecoder().decode(Request.self,from:raw),request.sequence>observationSequence,request.sequence<=4096,request.seconds.isFinite,(1...20).contains(request.seconds){observationSequence=request.sequence;observationSeconds=request.seconds;observationSamples=[];observationStart=clock()}
  guard let start=observationStart else{return}
  let duration=start.duration(to:clock()).components
  let elapsed=Double(duration.seconds)+Double(duration.attoseconds)/1e18
  if observationSamples.count<500{observationSamples.append(["elapsed_s":elapsed,"state":state,"spatial_state":spatial.state,"temporal_visible":temporalVisible,"snapshot_sequence":snapshot?.sequence ?? 0,"error":error ?? ""])}
  if elapsed>=observationSeconds || observationSamples.count>=500 {
   let receipt:[String:Any]=["sequence":observationSequence,"reader":snapshot?.reader ?? "unknown","live_wall_clock":true,"seconds":elapsed,"samples":observationSamples]
   if let raw=try? JSONSerialization.data(withJSONObject:receipt){try? raw.write(to:session.appendingPathComponent(String(format:"native-observation-%04d.json",observationSequence)),options:.atomic)}
   observationStart=nil;observationSamples=[]
  }
 }
 private func inspectionReceipt(_ sequence:Int,_ action:String,_ accepted:Bool,_ previous:String){let value:[String:Any] = ["sequence":sequence,"action":action,"accepted":accepted,"capture":"","state":state,"state_at_start":previous,"snapshot_sequence":snapshot?.sequence ?? 0,"error":spatial.inspectionMessage,"task_id":spatial.missions.draft?.id ?? spatial.missions.entries.last?.id ?? "","reviewed_payload_bytes":spatial.missions.reviewedPayloadBytes ?? 0];if let data=try? JSONSerialization.data(withJSONObject:value){try? data.write(to:session.appendingPathComponent(String(format:"native-receipt-%04d.json",sequence)),options:.atomic)}}
 private func pollQA(){struct Q:Decodable{let sequence:Int;let action:String};guard let raw=try? read("native-qa.json",4096),let q=try? JSONDecoder().decode(Q.self,from:raw),q.sequence>qaSequence,q.sequence<=4096 else{return};qaSequence=q.sequence;let stateAtStart=state;var accepted=false;var capture=""
  if q.action=="capture" || q.action=="capture-inspection"{let content=q.action=="capture-inspection" ? AnyView(DemoMissionPanel(missions:spatial.missions)):AnyView(IncidentChangeContent(model:self,capture:true));let host=NSHostingView(rootView:content.environment(\.colorScheme,.light));host.appearance=NSAppearance(named:.aqua);host.frame=CGRect(x:0,y:0,width:1500,height:960);host.layoutSubtreeIfNeeded();if let bitmap=host.bitmapImageRepForCachingDisplay(in:host.bounds){host.cacheDisplay(in:host.bounds,to:bitmap);capture=String(format:"native-%04d.png",q.sequence);if let data=bitmap.representation(using:.png,properties:[:]){do{try data.write(to:session.appendingPathComponent(capture),options:.atomic);accepted=true}catch{}}}}else if q.action.hasPrefix("inspection-"){actions.submit { [self] in let ok=await self.risingInspectionAction(q.action);self.inspectionReceipt(q.sequence,q.action,ok,stateAtStart)};return}else{accepted=command(q.action)}
  let value:[String:Any]=["sequence":q.sequence,"action":q.action,"accepted":accepted,"capture":capture,"state":state,"state_at_start":stateAtStart,"snapshot_sequence":snapshot?.sequence ?? 0,"error":error ?? ""];if let data=try? JSONSerialization.data(withJSONObject:value){try? data.write(to:session.appendingPathComponent(String(format:"native-receipt-%04d.json",q.sequence)),options:.atomic)}
 }
}
private let joinTeal=Color(red:0.03,green:0.43,blue:0.40)
struct IncidentChangeContent:View {
 @StateObject private var model:IncidentChangeModel
 private var capture=false
 init(session:URL){_model=StateObject(wrappedValue:IncidentChangeModel(session:session))}
 init(model:IncidentChangeModel,capture:Bool=false){_model=StateObject(wrappedValue:model);self.capture=capture}
 var title:String{["supported_rise":"A rise with a place.","non_rising":"Spatial evidence. No sustained rise.","unsupported":"The evidence does not join.","unavailable":"Waiting for original support."][model.state] ?? "Unavailable"}
 var body:some View{
  VStack(alignment:.leading,spacing:18){
   HStack(alignment:.top){VStack(alignment:.leading,spacing:8){Text("WENDY WORLD / EXACT-WITNESS INCIDENTS").font(.system(size:12,weight:.bold,design:.monospaced)).tracking(2).foregroundStyle(joinTeal);Text(title).font(.system(size:32,weight:.semibold));Text(model.reason).font(.system(size:14)).foregroundStyle(.secondary).lineLimit(2)};Spacer();VStack(alignment:.trailing,spacing:7){Text("OPERATOR "+(model.snapshot?.reader.uppercased() ?? "—")).font(.system(size:13,weight:.bold));Text(model.snapshot?.node ?? "Waiting").foregroundStyle(.secondary);Text("SIMULATED / ANALYTIC GAS").font(.system(size:10,weight:.bold)).padding(8).background(joinTeal.opacity(0.1)).clipShape(Capsule())}}
   HStack(alignment:.top,spacing:22){
    VStack(alignment:.leading,spacing:12){HStack{Text("SHARED SPATIAL FRAME").font(.system(size:12,weight:.bold,design:.monospaced));Spacer();Text("Gas cells + actual image/LiDAR fixture").font(.system(size:11)).foregroundStyle(.secondary)};IncidentPlan(model:model.spatial).frame(height:285).background(.white).clipShape(RoundedRectangle(cornerRadius:14));HStack(spacing:12){ForEach(["camera-a","camera-b"],id:\.self){name in VStack(alignment:.leading,spacing:6){if let image=model.spatial.images[name]{Image(nsImage:image).resizable().aspectRatio(contentMode:.fit).frame(height:160).frame(maxWidth:.infinity).background(Color.black.opacity(0.04))}else{Rectangle().fill(Color.gray.opacity(0.1)).frame(height:160)};Text(name.uppercased()+" · "+(model.spatial.visualVisible ? "ORIGINAL PIXELS":"HISTORICAL / UNAVAILABLE")).font(.system(size:10,weight:.medium,design:.monospaced))}}};Text("Cameras localize the colored fixture. They do not detect gas. Independent spatial layers remain visible until their own original support expires.").font(.system(size:12)).foregroundStyle(.secondary)}.frame(maxWidth:.infinity)
    VStack(alignment:.leading,spacing:8){
     HStack{Text("TEMPORAL SUPPORT").font(.system(size:12,weight:.bold,design:.monospaced));Spacer();Text(model.temporalVisible ? (model.snapshot?.result.temporal.state.uppercased() ?? "UNKNOWN") : "UNAVAILABLE").font(.system(size:12,weight:.bold)).foregroundStyle(joinTeal)}
     if model.temporalVisible,let w=model.snapshot?.result.temporal.window{VStack(spacing:8){JoinTraces(traces:w.traces).frame(height:70);VStack(spacing:5){ForEach(w.traces){trace in HStack{Text(trace.sensor.uppercased()).font(.system(size:13,weight:.bold,design:.monospaced));Text(trace.state).font(.system(size:12)).foregroundStyle(.secondary);Spacer();Text(String(format:"%+.1f … %+.1f ppm/s",trace.min_rate_ppm_s,trace.max_rate_ppm_s)).font(.system(size:11,design:.monospaced))}}}}}else{Text("No complete current history. Missing evidence does not resolve an incident or establish clearance.").font(.system(size:15)).foregroundStyle(.secondary).padding(.vertical,18)}
     Divider();Text("FOUR EXACT ORIGINAL WITNESSES").font(.system(size:11,weight:.bold,design:.monospaced))
     if let carrier=model.snapshot?.result.carriers?.first {Text("SIGNED PAIR  "+String(carrier.sha256.prefix(16))+"…").font(.system(size:10,design:.monospaced)).foregroundStyle(.secondary)}
     if ["supported_rise","non_rising"].contains(model.state),let s=model.snapshot{VStack(spacing:5){ForEach(s.result.witnesses,id:\.sensor){w in HStack{Text(w.sensor.uppercased()).fontWeight(.semibold);Spacer();Text(String(w.sha256.prefix(20))+"…").font(.system(size:11,design:.monospaced)).foregroundStyle(.secondary)}}}}else{Text("Current join unavailable. Matching capture time alone cannot attach a rise to a location.").font(.system(size:13)).foregroundStyle(.secondary)}
     if model.spatial.inspectionAvailable || model.spatial.inspectionStatus != nil {RisingInspectionCard(model:model,spatial:model.spatial,missions:model.spatial.missions)}
     if !model.spatial.inspectionAvailable && model.spatial.inspectionStatus == nil,let image=model.spatial.images["scene"]{Image(nsImage:image).resizable().aspectRatio(contentMode:.fit).frame(height:110).frame(maxWidth:.infinity).clipShape(RoundedRectangle(cornerRadius:10))}
     Text("Actual MuJoCo rendering; analytic gas. Truth coordinates stay outside the estimators. Gas originals remain gateway-attested. The reader verifies both signed claim members.").font(.system(size:11)).foregroundStyle(.secondary)
    }.padding(18).frame(width:440).background(.white).clipShape(RoundedRectangle(cornerRadius:16))
   }.frame(height:610)
   Spacer(minLength:0)
   VStack(alignment:.leading,spacing:10){HStack{control("Sustained rise","ramp");control("Hold steady","steady");control("Decline","decline");control("One spike","spike");control("Pause temporal","temporal");Spacer();Text(model.pending != nil ? "Waiting for receipt…":"Fixture controls only").font(.system(size:11)).foregroundStyle(.secondary)};HStack{control("Displace fixture","displace");control("Partition east","partition");control("Repeat originals","duplicates");control("Restart history","restart");control("Drop sensor H","dropout");Spacer()}}.padding(14).background(.white).clipShape(RoundedRectangle(cornerRadius:12))
   HStack{Text("PRIVATE SIMULATION · Original hashes and minimum lifetimes · Independent reader histories");Spacer();Text("Inference grants no motion authority; inspection requires explicit approval").foregroundStyle(joinTeal)}.font(.system(size:10))
  }.padding(24).background(Color(red:0.965,green:0.972,blue:0.967)).onAppear{if !capture{model.start()}}.onDisappear{if !capture{model.stop()}}.task { if !capture { await model.actions.run() } }
 }
 private func control(_ title:String,_ action:String)->some View{Button(title){model.command(action)}.buttonStyle(.bordered).disabled(!model.canCommand)}
}
struct JoinTraces:View {let traces:[ChangeTrace];var body:some View{Canvas{ctx,size in let pts=traces.flatMap{$0.points};guard let lo=pts.map(\.value_ppm).min(),let hi=pts.map(\.value_ppm).max(),let start=pts.map(\.captured_s).min(),let end=pts.map(\.captured_s).max(),end>start else{return};let colors:[Color]=[.teal,.blue,.purple,.orange];for(i,t)in traces.enumerated(){var path=Path();for(j,p)in t.points.enumerated(){let point=CGPoint(x:8+(p.captured_s-start)/(end-start)*(size.width-16),y:size.height-8-(p.value_ppm-lo)/max(1,hi-lo)*(size.height-16));if j==0{path.move(to:point)}else{path.addLine(to:point)};ctx.fill(Path(ellipseIn:CGRect(x:point.x-3,y:point.y-3,width:6,height:6)),with:.color(colors[i%4]))};ctx.stroke(path,with:.color(colors[i%4]),lineWidth:2)}}.background(Color.gray.opacity(0.04))}}
