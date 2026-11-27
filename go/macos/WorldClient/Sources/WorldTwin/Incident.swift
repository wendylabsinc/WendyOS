import SwiftUI
import AppKit
import CryptoKit
import Darwin

struct IncidentSource: Codable, Equatable { let device, app, service, stream:String }
struct IncidentSupport: Codable { let kind,sha256:String; let source:IncidentSource; let version:GasObservation.Version; let captured_at,valid_until:String }
struct IncidentSpatial: Codable { let associated:Bool; let reason:String; let position_m:[Double]; let uncertainty_m:Double; let reprojection_error_px:Double; let lidar_support_count:Int }
struct IncidentImageRef:Codable {let sha256:String}
struct IncidentSample:Codable {let sensor_id:String;let evidence:IncidentImageRef?}
struct IncidentInput:Codable {let payload:IncidentSample}
struct IncidentVisualResult: Codable { let inputs:[IncidentInput]; let spatial:IncidentSpatial; let oldest,latest,valid_until:String }
struct IncidentVisualClaim: Codable { let schema,contract_sha256,evaluated_at:String; let result:IncidentVisualResult }
struct IncidentVisual: Codable { let state,reason:String; let sha256:String?; let version:GasObservation.Version; let remaining_s:Double; let claim:IncidentVisualClaim? }
struct IncidentGas: Codable { let state,reason:String; let gateway,sha256:String?; let contract_sha256:String; let result:GasResult }
struct IncidentResult: Codable { let schema,contract_sha256,state,reason,at,valid_until:String; let gas:IncidentGas; let visual:IncidentVisual; let supports:[IncidentSupport]; let distance_to_region_m:Double?; let input_digest:String }
struct IncidentSnapshot: Codable {
 struct Camera:Codable {let name,file,sha256:String}
 struct Event:Codable {let action:String;let time_s:Double}
 let schema,run_id,reader,node:String
 let sequence,last_command_sequence:Int
 let result:IncidentResult
 let image,image_sha256:String
 let cameras:[Camera]
 let controls,partition,visual_enabled,gas_enabled,duplicates,displaced,wind_mismatch:Bool
 let events:[Event]
}
func incidentDate(_ value:String)->Date? { let f=ISO8601DateFormatter();f.formatOptions=[.withInternetDateTime,.withFractionalSeconds];if let d=f.date(from:value){return d};f.formatOptions=[.withInternetDateTime];return f.date(from:value) }
func incidentDigest(_ value:String)->Bool {value.count==64 && value.allSatisfy {"0123456789abcdef".contains($0)}}

@MainActor final class IncidentModel:ObservableObject {
 let actions = TwinActions()

 let session:URL
 let missions=DemoMissions()
 @Published var inspectionStatus:IncidentInspectionStatus?
 var inspectionPoseDeadline:ContinuousClock.Instant?
 var inspectionPoseIdentity:Int64?
 @Published var inspectionMessage="Select current consistent evidence, then review the fixed destination."
 @Published var inspectionSheet=false
 @Published var preparingInspection=false
 @Published var snapshot:IncidentSnapshot?
 @Published var images:[String:NSImage]=[:]
 @Published var error:String?
 @Published var tick=0
 @Published var pending:Int?
 var clock:()->ContinuousClock.Instant={.now}
 var wallClock:()->Date={Date()}
 private var timer:Timer?
 private var updated:ContinuousClock.Instant?
 private var deadlines:[String:(String,ContinuousClock.Instant)]=[:]
 private var associationDeadline:(String,ContinuousClock.Instant)?
 private var lastCommand=0
 private var pendingAt:ContinuousClock.Instant?
 private var qaSequence=0
 init(session:URL){self.session=session}
 var fresh:Bool {error==nil && (updated.map {$0.duration(to:clock()) < .seconds(3.5)} ?? false)}
 func remaining(_ kind:String)->Double {guard fresh,let d=deadlines[kind]?.1 else{return 0};let c=clock().duration(to:d).components;return max(0,Double(c.seconds)+Double(c.attoseconds)/1e18)}
 var gasVisible:Bool {remaining("gas")>0 && snapshot?.result.gas.result.estimate != nil}
 var visualVisible:Bool {remaining("visual")>0 && snapshot?.result.visual.claim != nil}
 var state:String {
  guard fresh,let s=snapshot else{return "unavailable"}
  if gasVisible && visualVisible {return associationDeadline.map {clock() < $0.1} == true ? s.result.state : "unavailable"}
  if visualVisible{return "visual_only"};if gasVisible{return "gas_only"};return "unavailable"
 }
 var reason:String {guard fresh else{return "Current producer view expired. Recorded images and events remain historical."};guard let s=snapshot else{return "Waiting for independent replica"};if state != s.result.state{return "Original source evidence expired locally. No renewed association."};return s.result.reason}
 var canCommand:Bool {fresh && snapshot?.controls==true && pending==nil && lastCommand<4096}
 func start(){guard timer==nil else{return};poll();timer=Timer.scheduledTimer(withTimeInterval:0.1,repeats:true){[weak self]_ in MainActor.assumeIsolated{self?.poll()}}}
 func stop(){timer?.invalidate();timer=nil}
 private func read(_ name:String,limit:Int)throws->Data {
  let url=session.appendingPathComponent(name);let v=try url.resourceValues(forKeys:[.isRegularFileKey,.isSymbolicLinkKey,.fileSizeKey]);guard v.isRegularFile==true,v.isSymbolicLink != true,let size=v.fileSize,size<=limit else{throw CocoaError(.fileReadCorruptFile)};return try Data(contentsOf:url)
 }
 func accept(_ value:IncidentSnapshot,now:ContinuousClock.Instant)throws {
  let r=value.result;let states=["consistent","conflicting","gas_only","visual_only","unavailable"]
  guard value.schema=="wendy.incident-demo.v1",r.schema=="wendy.incident.association.v1",states.contains(r.state),incidentDigest(r.contract_sha256),incidentDigest(r.input_digest),value.sequence>0,value.sequence<=1000000,value.last_command_sequence>=0,value.last_command_sequence<=4096,["east","west"].contains(value.reader),["node-2","node-3"].contains(value.node),value.controls == (value.reader=="east"),r.supports.count<=2,Set(r.supports.map{$0.kind}).count==r.supports.count,let at=incidentDate(r.at),at.timeIntervalSince(wallClock())<=0.5,wallClock().timeIntervalSince(at)<4 else{throw CocoaError(.fileReadCorruptFile)}
  if let old=snapshot {guard old.run_id==value.run_id,old.reader==value.reader,old.node==value.node,old.result.contract_sha256==r.contract_sha256,value.sequence>old.sequence else{throw CocoaError(.fileReadCorruptFile)}}
  var incoming:[String:(String,ContinuousClock.Instant)]=[:]
  for support in r.supports {
   guard ["gas","visual"].contains(support.kind),incidentDigest(support.sha256),!support.source.device.isEmpty,!support.source.service.isEmpty,support.version.epoch>0,support.version.sequence>0,let from=incidentDate(support.captured_at),let until=incidentDate(support.valid_until),until>from,until.timeIntervalSince(from)<=10,until>at else{throw CocoaError(.fileReadCorruptFile)}
   var deadline=now.advanced(by:.seconds(max(0,until.timeIntervalSince(wallClock()))))
   if let old=deadlines[support.kind],old.0==support.sha256{deadline=min(deadline,old.1)}
   incoming[support.kind]=(support.sha256,deadline)
  }
  if let visual=r.visual.claim {
   let p=visual.result.spatial
   guard visual.schema=="wendy.visual.fixture.v1",visual.contract_sha256==r.contract_sha256,r.visual.state=="localized",p.associated,p.position_m.count==3,p.position_m.allSatisfy({$0.isFinite && abs($0)<10000}),p.uncertainty_m.isFinite,p.uncertainty_m>0,p.uncertainty_m<=10,p.lidar_support_count>0,let support=r.supports.first(where:{$0.kind=="visual"}),support.sha256==r.visual.sha256,let until=incidentDate(visual.result.valid_until),let supportUntil=incidentDate(support.valid_until),supportUntil<=until else{throw CocoaError(.fileReadCorruptFile)}
  }else if incoming["visual"] != nil {throw CocoaError(.fileReadCorruptFile)}
  if let estimate=r.gas.result.estimate {
   guard ["localized","ambiguous"].contains(r.gas.state),estimate.position_xz.count==2,estimate.position_xz.allSatisfy({$0.isFinite && abs($0)<10000}),estimate.cells.count>0,estimate.cells.count<=4096,estimate.cells.allSatisfy({$0.position_xz.count==2 && $0.position_xz.allSatisfy({$0.isFinite && abs($0)<10000})}),estimate.cell_size_m.isFinite,estimate.cell_size_m>0,estimate.cell_size_m<=1,estimate.valid_until_s>r.gas.result.now_s,let support=r.supports.first(where:{$0.kind=="gas"}),support.sha256==r.gas.sha256,let until=incidentDate(support.valid_until),until.timeIntervalSince(at)<=estimate.valid_until_s-r.gas.result.now_s+0.001 else{throw CocoaError(.fileReadCorruptFile)}
  }else if incoming["gas"] != nil {throw CocoaError(.fileReadCorruptFile)}
  if r.state=="consistent" || r.state=="conflicting" {guard incoming.count==2,r.gas.state=="localized",let distance=r.distance_to_region_m,distance.isFinite,distance>=0 else{throw CocoaError(.fileReadCorruptFile)}}
  var association:ContinuousClock.Instant?
  if !r.supports.isEmpty {guard let until=incidentDate(r.valid_until),let earliest=r.supports.compactMap({incidentDate($0.valid_until)}).min(),until<=earliest,until>at else{throw CocoaError(.fileReadCorruptFile)};association=now.advanced(by:.seconds(max(0,until.timeIntervalSince(wallClock()))));if let old=associationDeadline,old.0==r.input_digest{association=min(association!,old.1)}}
  if let claim=r.visual.claim {let originals=claim.result.inputs.compactMap {item -> String? in guard let evidence=item.payload.evidence else{return nil};return item.payload.sensor_id+"/"+evidence.sha256};guard originals.count==2,originals.sorted()==value.cameras.map({$0.name+"/"+$0.sha256}).sorted()else{throw CocoaError(.fileReadCorruptFile)}}else if !value.cameras.isEmpty{throw CocoaError(.fileReadCorruptFile)}
  if let association{associationDeadline=(r.input_digest,association)}else{associationDeadline=nil}
  deadlines=incoming;snapshot=value;updated=now;lastCommand=max(lastCommand,value.last_command_sequence);error=nil
 }
 func poll(){tick+=1
  do {
   let value=try JSONDecoder().decode(IncidentSnapshot.self,from:read("snapshot.json",limit:512<<10))
   if snapshot==nil || value.sequence>snapshot!.sequence {
    guard ["scene-0.png","scene-1.png"].contains(value.image),incidentDigest(value.image_sha256),value.cameras.count<=2,Set(value.cameras.map{$0.name}).count==value.cameras.count else{throw CocoaError(.fileReadCorruptFile)}
    var loaded:[String:NSImage]=[:]
    func image(_ file:String,_ digest:String)throws->NSImage{let data=try read(file,limit:4<<20);let actual=SHA256.hash(data:data).map{String(format:"%02x",$0)}.joined();guard actual==digest,let image=NSImage(data:data)else{throw CocoaError(.fileReadCorruptFile)};return image}
    loaded["scene"]=try image(value.image,value.image_sha256)
    for camera in value.cameras {guard ["camera-a","camera-b"].contains(camera.name),[camera.name+"-0.png",camera.name+"-1.png"].contains(camera.file),incidentDigest(camera.sha256)else{throw CocoaError(.fileReadCorruptFile)};loaded[camera.name]=try image(camera.file,camera.sha256)}
    try accept(value,now:clock());images=loaded
   }
   if let number=pending {let name=String(format:"receipts/%06d.json",number)
    if FileManager.default.fileExists(atPath:session.appendingPathComponent(name).path){struct Receipt:Decodable{let sequence:Int;let accepted:Bool;let error:String};let r=try JSONDecoder().decode(Receipt.self,from:read(name,limit:4096));guard r.sequence==number else{throw CocoaError(.fileReadCorruptFile)};if !r.accepted{error=r.error};pending=nil;pendingAt=nil}
    else if let at=pendingAt,at.duration(to:clock()) > .seconds(6){error="Simulation control receipt timed out";pending=nil}
   }
  }catch{self.error="Cannot verify the current incident snapshot"}
  pollInspection()
  pollQA()
 }
 @discardableResult func command(_ action:String)->Bool{
  guard canCommand,let snapshot,["displace","partition","visual","gas","duplicates","wind","reset"].contains(action)else{return false};let sequence=lastCommand+1
  struct Command:Encodable{let run_id:String;let sequence:Int;let action,target:String}
  do{let raw=try JSONEncoder().encode(Command(run_id:snapshot.run_id,sequence:sequence,action:action,target:""));let dest=session.appendingPathComponent(String(format:"commands/%06d.json",sequence));let temp=dest.deletingLastPathComponent().appendingPathComponent(".command-"+UUID().uuidString);try raw.write(to:temp,options:.withoutOverwriting);defer{try? FileManager.default.removeItem(at:temp)};guard link(temp.path,dest.path)==0 else{return false};lastCommand=sequence;pending=sequence;pendingAt=clock();return true}catch{self.error="Could not write simulation control";return false}
 }
 private func pollQA(){
  struct Request:Decodable{let sequence:Int;let action:String}
  guard let raw=try? read("native-qa.json",limit:4096),let q=try? JSONDecoder().decode(Request.self,from:raw),q.sequence>qaSequence,q.sequence<=4096 else{return};qaSequence=q.sequence
  if q.action.hasPrefix("inspection-") {
   actions.submit { [self] in  let accepted=await inspectionAction(q.action);let value:[String:Any]=["sequence":q.sequence,"action":q.action,"accepted":accepted,"message":inspectionMessage,"task_id":missions.draft?.id ?? missions.entries.last?.id ?? ""];if let data=try? JSONSerialization.data(withJSONObject:value){try? data.write(to:session.appendingPathComponent(String(format:"native-receipt-%04d.json",q.sequence)),options:.atomic)} };return
  }
  var accepted=false;var capture=""
  if q.action=="capture" || q.action=="capture-inspection" {
   let content = q.action=="capture-inspection" ? AnyView(DemoMissionPanel(missions:missions)) : AnyView(IncidentContent(model:self,capture:true));let host=NSHostingView(rootView:content.environment(\.colorScheme,.light));host.appearance=NSAppearance(named:.aqua);host.frame=CGRect(x:0,y:0,width:1500,height:960);host.layoutSubtreeIfNeeded()
   if let bitmap=host.bitmapImageRepForCachingDisplay(in:host.bounds){host.cacheDisplay(in:host.bounds,to:bitmap);capture=String(format:"native-%04d.png",q.sequence);if let data=bitmap.representation(using:.png,properties:[:]){do{try data.write(to:session.appendingPathComponent(capture),options:.atomic);accepted=true}catch{}}}
  }else{accepted=command(q.action)}
  struct Receipt:Encodable{let sequence:Int;let action:String;let accepted:Bool;let capture,state:String;let snapshot_sequence:Int}
  let r=Receipt(sequence:q.sequence,action:q.action,accepted:accepted,capture:capture,state:state,snapshot_sequence:snapshot?.sequence ?? 0);if let data=try? JSONEncoder().encode(r){try? data.write(to:session.appendingPathComponent(String(format:"native-receipt-%04d.json",q.sequence)),options:.atomic)}
 }
}

private let incidentInk=Color(red:0.11,green:0.19,blue:0.22)
private let incidentTeal=Color(red:0.04,green:0.49,blue:0.44)
private let incidentAmber=Color(red:0.88,green:0.53,blue:0.12)
struct IncidentContent:View {
 @StateObject private var model:IncidentModel
 private var capture=false
 init(session:URL){_model=StateObject(wrappedValue:IncidentModel(session:session))}
 init(model:IncidentModel,capture:Bool=false){_model=StateObject(wrappedValue:model);self.capture=capture}
 var tone:Color{model.state=="consistent" ? incidentTeal : model.state=="conflicting" ? .red : incidentAmber}
 var title:String{["consistent":"Evidence aligns","conflicting":"Locations disagree","gas_only":"Gas evidence only","visual_only":"Visual evidence only","unavailable":"Association unavailable"][model.state] ?? "Unavailable"}
 var body:some View{
  VStack(alignment:.leading,spacing:0){
   HStack(alignment:.top){VStack(alignment:.leading,spacing:6){Text("WENDY WORLD / INCIDENTS").font(.system(size:12,weight:.bold,design:.monospaced)).tracking(2).foregroundStyle(incidentTeal);Text("One site. Two independent views.").font(.system(size:30,weight:.semibold));Text("MuJoCo rendered cameras + analytic gas • Spatial association, not leak detection").font(.system(size:14)).foregroundStyle(.secondary)};Spacer();VStack(alignment:.trailing,spacing:8){Text("Operator "+(model.snapshot?.reader.capitalized ?? "—")).font(.system(size:18,weight:.semibold));Text(model.snapshot?.node ?? "Waiting for replica").font(.system(size:13,design:.monospaced)).foregroundStyle(.secondary);Text(model.snapshot?.controls==true ? "SIMULATION CONTROLS" : "INDEPENDENT READER").font(.system(size:10,weight:.bold)).padding(7).background(incidentTeal.opacity(0.1)).clipShape(Capsule())}}.padding(28)
   HStack(spacing:12){Circle().fill(tone).frame(width:11,height:11);Text(title).font(.system(size:20,weight:.semibold));Text(model.reason).font(.system(size:13)).foregroundStyle(.secondary).lineLimit(2);Spacer()}.padding(.horizontal,28).padding(.vertical,17).background(tone.opacity(0.09))
   HStack(alignment:.top,spacing:22){
    VStack(alignment:.leading,spacing:16){
     HStack{Text("SHARED SPATIAL TWIN").font(.system(size:12,weight:.bold,design:.monospaced)).tracking(1);Spacer();legend("Gas model-fit cells",incidentAmber);legend("Image + LiDAR fixture",incidentTeal)}
     IncidentPlan(model:model).frame(height:350).background(Color.white).clipShape(RoundedRectangle(cornerRadius:14))
     HStack(alignment:.top,spacing:14){ForEach(["camera-a","camera-b"],id:\.self){name in VStack(alignment:.leading,spacing:6){if let image=model.images[name]{Image(nsImage:image).resizable().aspectRatio(contentMode:.fit).frame(height:170).frame(maxWidth:.infinity).background(Color.black.opacity(0.04)).clipShape(RoundedRectangle(cornerRadius:9))}else{Rectangle().fill(Color.black.opacity(0.04)).overlay(Text("No image support").foregroundStyle(.secondary)).frame(height:180)};HStack{Text(name.uppercased()).font(.system(size:11,weight:.bold,design:.monospaced));Spacer();Text(model.visualVisible ? "Retained original pixels" : "Historical / unavailable").font(.system(size:10)).foregroundStyle(.secondary)}}}}
     Text("The colored fixture is localized from the two actual images and LiDAR returns. Cameras do not measure gas.").font(.system(size:12)).foregroundStyle(.secondary)
    }.frame(maxWidth:.infinity)
    VStack(alignment:.leading,spacing:17){
     supportCard("Gas source region",kind:"gas",color:incidentAmber,detail:model.gasVisible ? String(model.snapshot?.result.gas.result.coverage ?? 0)+" sensors • "+(model.snapshot?.result.gas.state ?? "unavailable") : "No current gas support")
     supportCard("Visual fixture",kind:"visual",color:incidentTeal,detail:model.visualVisible ? "Two calibrated images • "+String(model.snapshot?.result.visual.claim?.result.spatial.lidar_support_count ?? 0)+" LiDAR returns" : "No current visual support")
     if let image=model.images["scene"]{Image(nsImage:image).resizable().aspectRatio(contentMode:.fit).frame(maxHeight:model.inspectionStatus == nil ? 240 : 110).clipShape(RoundedRectangle(cornerRadius:10));if model.inspectionStatus == nil {Text("Actual MuJoCo scene. Gas source truth is withheld from both estimators.").font(.system(size:11)).foregroundStyle(.secondary)}}
     if model.inspectionStatus != nil || model.inspectionAvailable { IncidentInspectionCard(model:model,missions:model.missions) }
     if model.inspectionStatus == nil {Text("ASSOCIATION CONTRACT").font(.system(size:11,weight:.bold,design:.monospaced)).tracking(1)
     Text("Same site, frame and run. Captures within 350 ms. Each source expires independently. No confidence scores are added.").font(.system(size:13)).foregroundStyle(.secondary)}
     if ["consistent","conflicting"].contains(model.state),let separation=model.snapshot?.result.distance_to_region_m{Text(String(format:"Visual footprint to gas cells: %.2f m",separation)).font(.system(size:12,design:.monospaced))}
    }.frame(width:345)
   }.padding(24)
   Spacer(minLength:0)
   HStack(spacing:9){control("Displace fixture","displace");control("Partition east","partition");control("Pause cameras","visual");control("Pause gas","gas");control("Change airflow","wind");control("Repeat originals","duplicates");control("Reset","reset");Spacer();Text(model.pending != nil ? "Waiting for receipt…" : (model.snapshot?.partition==true ? "Partition active" : "Connected replicas")).font(.system(size:11)).foregroundStyle(.secondary)}.padding(.horizontal,24).padding(.vertical,16).background(Color.white)
   HStack{Text("PRIVATE SIMULATION • No hardware motion or physical gas accuracy claim");Spacer();Text(String(model.snapshot?.sequence ?? 0)+" / "+String((model.snapshot?.result.contract_sha256 ?? "").prefix(10)))}.font(.system(size:10,design:.monospaced)).foregroundStyle(.secondary).padding(.horizontal,24).padding(.vertical,11)
  }.foregroundStyle(incidentInk).background(Color(red:0.965,green:0.972,blue:0.967)).onAppear{if !capture{model.start()}}.onDisappear{if !capture{model.stop()}}.task { if !capture { await model.actions.run() } }
 }
 private func legend(_ label:String,_ color:Color)->some View{HStack(spacing:5){Circle().fill(color).frame(width:7,height:7);Text(label).font(.system(size:10)).foregroundStyle(.secondary)}}
 private func supportCard(_ title:String,kind:String,color:Color,detail:String)->some View{VStack(alignment:.leading,spacing:9){HStack{Text(title).font(.system(size:17,weight:.semibold));Spacer();Text(String(format:"%.1f s",model.remaining(kind))).font(.system(size:17,weight:.medium,design:.monospaced)).foregroundStyle(model.remaining(kind)>0 ? color : .secondary)};Text(detail).font(.system(size:12)).foregroundStyle(.secondary);if let s=model.snapshot?.result.supports.first(where:{$0.kind==kind}){Text(s.source.device+" / "+String(s.sha256.prefix(12))).font(.system(size:10,design:.monospaced)).lineLimit(1)};Text(kind=="gas" ? "Model-fit region, not calibrated confidence" : "Geometric footprint, not gas detection").font(.system(size:11)).foregroundStyle(color)}.padding(17).frame(maxWidth:.infinity,alignment:.leading).background(Color.white).clipShape(RoundedRectangle(cornerRadius:12))}
 private func control(_ label:String,_ action:String)->some View{Button(label){model.command(action)}.buttonStyle(.bordered).controlSize(.small).disabled(!model.canCommand)}
}

// Both layers share the same commissioned X/Z metre frame. The native plan is
// orthographic. It does not project evaluation truth or average estimates.
struct IncidentPlan:View {
 @ObservedObject var model:IncidentModel
 var body:some View{Canvas{context,size in
  let scale=min((size.width-90)/5.5,(size.height-45)/4.8);let origin=CGPoint(x:size.width/2,y:size.height/2)
  func point(_ x:Double,_ z:Double)->CGPoint{CGPoint(x:origin.x+x*scale,y:origin.y+z*scale)}
  for i in -2...2 {var path=Path();path.move(to:point(Double(i),-2));path.addLine(to:point(Double(i),2));path.move(to:point(-2,Double(i)));path.addLine(to:point(2,Double(i)));context.stroke(path,with:.color(.gray.opacity(0.16)),lineWidth:1)}
  var boundary=Path();boundary.addRect(CGRect(x:point(-2,-2).x,y:point(-2,-2).y,width:4*scale,height:4*scale));context.stroke(boundary,with:.color(.gray.opacity(0.3)),style:StrokeStyle(lineWidth:1,dash:[4,4]))
  if model.gasVisible,let estimate=model.snapshot?.result.gas.result.estimate {
   for cell in estimate.cells {let p=point(cell.position_xz[0],cell.position_xz[1]);let width=estimate.cell_size_m*scale;context.fill(Path(CGRect(x:p.x-width/2,y:p.y-width/2,width:width,height:width)),with:.color(incidentAmber.opacity(0.65)))}
  }
  if model.visualVisible,let visual=model.snapshot?.result.visual.claim?.result.spatial {let p=point(visual.position_m[0],visual.position_m[2]);let radius=max(5,visual.uncertainty_m*scale);context.fill(Path(ellipseIn:CGRect(x:p.x-radius,y:p.y-radius,width:radius*2,height:radius*2)),with:.color(incidentTeal.opacity(0.14)));context.stroke(Path(ellipseIn:CGRect(x:p.x-radius,y:p.y-radius,width:radius*2,height:radius*2)),with:.color(incidentTeal),lineWidth:2);context.fill(Path(ellipseIn:CGRect(x:p.x-4,y:p.y-4,width:8,height:8)),with:.color(incidentTeal));context.draw(Text("VISUAL FIXTURE").font(.system(size:9,weight:.bold,design:.monospaced)).foregroundStyle(incidentTeal),at:CGPoint(x:p.x,y:p.y-19))}
  for reading in model.snapshot?.result.gas.result.readings ?? [] {guard reading.sensor.position_xz.count==2 else{continue};let p=point(reading.sensor.position_xz[0],reading.sensor.position_xz[1]);let color=reading.state=="live" && model.gasVisible ? incidentInk : Color.gray.opacity(0.35);context.fill(Path(roundedRect:CGRect(x:p.x-5,y:p.y-5,width:10,height:10),cornerRadius:2),with:.color(color));context.draw(Text(reading.sensor.id.uppercased()).font(.system(size:9,weight:.bold,design:.monospaced)).foregroundStyle(.secondary),at:CGPoint(x:p.x,y:p.y+15))}
  if let inspection=model.inspectionStatus {
   if let route=inspection.route,route.count<=4096,route.allSatisfy({$0.count==3 && $0.allSatisfy{$0.isFinite}}),let first=route.first {var path=Path();path.move(to:point(first[0],first[2]));for p in route.dropFirst(){path.addLine(to:point(p[0],p[2]))};context.stroke(path,with:.color(.purple.opacity(0.55)),style:StrokeStyle(lineWidth:2,dash:[5,4]))}
   if let target=inspection.destination,target.count==3 {let p=point(target[0],target[2]);context.stroke(Path(ellipseIn:CGRect(x:p.x-8,y:p.y-8,width:16,height:16)),with:.color(.purple),lineWidth:2);context.draw(Text("REVIEWED DESTINATION").font(.system(size:8,weight:.semibold)).foregroundStyle(.purple),at:CGPoint(x:p.x,y:p.y+21))}
   if model.inspectionPoseDeadline.map { model.clock() < $0 } == true,let pose=inspection.robot_position,pose.count==3 {let p=point(pose[0],pose[2]);context.fill(Path(ellipseIn:CGRect(x:p.x-7,y:p.y-7,width:14,height:14)),with:.color(.blue));context.draw(Text("SIMULATED ROBOT").font(.system(size:8,weight:.semibold)).foregroundStyle(.blue),at:CGPoint(x:p.x,y:p.y-16))}
  }
  context.draw(Text("X/Z METRES  •  SAME COMMISSIONED FRAME").font(.system(size:10,design:.monospaced)).foregroundStyle(.secondary),at:CGPoint(x:size.width/2,y:size.height-14))
  if !model.gasVisible && !model.visualVisible{context.draw(Text("No current spatial evidence").font(.system(size:16)).foregroundStyle(.secondary),at:origin)}
 }}
}
