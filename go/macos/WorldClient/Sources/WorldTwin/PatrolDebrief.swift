import SwiftUI
import AppKit
import CryptoKit

struct PatrolDebriefState:Decodable {
    struct Frame:Decodable {let id:String;let revision:String}
    struct Source:Decodable {let device:String;let app:String;let service:String;let stream:String}
    struct Scope:Decodable {let tenant:String;let site:String;let run_id:String;let mission_id:String;let spec_sha256:String;let frame:Frame;let source:Source;let retain_until_unix_ms:Int64}
    struct Room:Decodable,Identifiable {let id:String;let name:String;let polygon:[[Double]];let visits:Int;let last_observed_s:Double?;let next_due_unix_ms:Int64?;let revisit_seconds:Int?;let blocked_reason:String?;let evidence:[String]?
        var valid:Bool {!id.isEmpty && id.count<=128 && name.count<=256 && visits>=0 && polygon.count>=3 && polygon.count<=128 && polygon.allSatisfy {$0.count==2 && $0.allSatisfy(\.isFinite)} && (last_observed_s?.isFinite ?? true)}
    }
    struct Coverage:Decodable {let tour:PatrolTourStatus?;let mission_id:String;let state:String;let rooms:[Room];let reason:String?;let expires_at_unix_ms:Int64
        var valid:Bool {(tour?.valid ?? true) && !mission_id.isEmpty && state.count<=80 && rooms.count<=32 && !rooms.isEmpty && Set(rooms.map(\.id)).count==rooms.count && rooms.allSatisfy(\.valid)}
    }
    struct Live:Decodable {let run_id:String;let spec_sha256:String;let state:Coverage;let observed_unix_ms:Int64;let expires_unix_ms:Int64}
    struct Evidence:Decodable,Identifiable {let sha256:String;let size:Int64;let content_type:String;let sensor_id:String;var id:String{sha256}}
    struct Report:Decodable,Identifiable {
        let schema:String;let id:String;let content_sha256:String;let kind:String;let run_id:String;let mission_id:String;let spec_sha256:String;let original_unix_ms:Int64;let original_clock:String;let simulation_ns:Int64?;let sequence:UInt64?;let frame:Frame;let retain_until_unix_ms:Int64;let state:Coverage?;let room_ids:[String]?;let evidence:[Evidence]
        var cameras:[Evidence]{evidence.filter {$0.content_type=="image/png" || $0.content_type=="image/jpeg"}}
    }
    let schema:String;let config:Scope;let reader:String;let updated_unix_ms:Int64;let connected:Bool;let live:Live?;let reports:[Report];let rejected:Int;let error:String?
    static func digest(_ value:String)->Bool {value.range(of:"^[a-f0-9]{64}$",options:.regularExpression) != nil}
    var valid:Bool {
        schema=="wendy.nav.patrol.debrief-view.v1" && config.tenant=="simulation" && config.site=="studio-01" && !config.run_id.isEmpty && Self.digest(config.spec_sha256) && reports.count<=65 && Set(reports.map(\.id)).count==reports.count &&
        (live.map {$0.run_id==config.run_id && $0.spec_sha256==config.spec_sha256 && $0.state.mission_id==config.mission_id && $0.state.valid && $0.expires_unix_ms-$0.observed_unix_ms==5000} ?? true) &&
        reports.allSatisfy {$0.schema=="wendy.nav.patrol.debrief.v1" && Self.digest($0.id) && Self.digest($0.content_sha256) && $0.run_id==config.run_id && $0.mission_id==config.mission_id && $0.spec_sha256==config.spec_sha256 && $0.frame.id==config.frame.id && $0.frame.revision==config.frame.revision && $0.original_unix_ms>0 && $0.retain_until_unix_ms==config.retain_until_unix_ms && $0.evidence.count<=8 && $0.evidence.allSatisfy {Self.digest($0.sha256) && $0.size>0 && $0.size<=16*1024*1024} && ($0.state?.valid ?? true) && ["capture","result"].contains($0.kind)}
    }
    func current(at:Date)->Bool {let now=Int64(at.timeIntervalSince1970*1000);return connected && updated_unix_ms<=now && now-updated_unix_ms<3500 && now<config.retain_until_unix_ms}
    func liveCurrent(at:Date)->Bool {current(at:at) && live.map {Int64(at.timeIntervalSince1970*1000)<$0.expires_unix_ms} == true}
}

@MainActor final class PatrolDebriefModel:ObservableObject {
    let directory:URL
    @Published var state:PatrolDebriefState?
    @Published var selected:String?
    @Published var selectedDigest:String?
    @Published var image:NSImage?
    @Published var focusOriginal=false
    @Published var evidenceMessage="Choose an original camera capture to retrieve it from the capture node."
    @Published var error:String?
    @Published var now=Date()
    private var timer:Timer?
    private var pending:String?
    private var commandNumber=0
    private var qaID:String?
    var report:PatrolDebriefState.Report? {state?.reports.first {$0.id==selected}}
    var coverage:PatrolDebriefState.Coverage? {report?.state ?? state?.reports.first(where:{$0.kind=="result"})?.state ?? state?.live?.state}
    var coverageTimeMS:Int64 {report?.state != nil ? report!.original_unix_ms:(state?.reports.first(where:{$0.kind=="result"})?.original_unix_ms ?? state?.live?.observed_unix_ms ?? 0)}
    func cadence(_ room:PatrolDebriefState.Room)->String {guard let seconds=room.revisit_seconds else{return "No revisit interval in retained policy"};if let due=room.next_due_unix_ms,due<=coverageTimeMS{return "Overdue at this report · interval \(seconds) s"};return "Revisit interval \(seconds) s · status at report time"}

    init(directory:URL){self.directory=directory}
    func start(){poll();timer=Timer.scheduledTimer(withTimeInterval:0.5,repeats:true){[weak self] _ in MainActor.assumeIsolated { self?.poll() }}}
    func stop(){timer?.invalidate();timer=nil}
    func poll(){now=Date();do {let url=directory.appendingPathComponent("state.json");let attributes=try FileManager.default.attributesOfItem(atPath:url.path);guard (attributes[.size] as? NSNumber)?.intValue ?? Int.max <= 2*1024*1024 else{throw DemoError.invalidFile};let value=try JSONDecoder().decode(PatrolDebriefState.self,from:Data(contentsOf:url));guard value.valid else {throw DemoError.invalidFile};if let old=state,old.config.run_id != value.config.run_id || old.config.spec_sha256 != value.config.spec_sha256 {throw DemoError.invalidFile};state=value;error=nil;if selected==nil || !value.reports.contains(where:{$0.id==selected}){selected=value.reports.first?.id;image=nil;selectedDigest=nil;focusOriginal=false}}catch {self.error="Peer state unavailable: \(error)"}
        expirePreview();pollReceipt();expirePreview();pollQA()
    }
    private func expirePreview(){if let state,Int64(now.timeIntervalSince1970*1000)>=state.config.retain_until_unix_ms{image=nil;selectedDigest=nil;pending=nil;evidenceMessage="Report retention has elapsed. Originals are unavailable."}}
    func select(_ id:String)->Bool {guard state?.reports.contains(where:{$0.id==id})==true else{return false};selected=id;selectedDigest=nil;image=nil;focusOriginal=false;evidenceMessage="Historical observation. Fetch an original to verify its bytes.";return true}
    @discardableResult func fetch(_ digest:String)->Bool {
        guard let state,state.current(at:now),let report,report.evidence.contains(where:{$0.sha256==digest}),pending==nil else{evidenceMessage="Current peer access and an unexpired referenced report are required.";return false}
        do {let commands=directory.appendingPathComponent("commands");try FileManager.default.createDirectory(at:commands,withIntermediateDirectories:true);repeat {commandNumber+=1}while FileManager.default.fileExists(atPath:commands.appendingPathComponent(String(format:"native-%06d.json",commandNumber)).path)
            let name=String(format:"native-%06d.json",commandNumber);let object:[String:Any]=["schema":"wendy.nav.patrol.debrief-command.v1","run_id":state.config.run_id,"action":"fetch","report_id":report.id,"digest":digest];try JSONSerialization.data(withJSONObject:object,options:.sortedKeys).write(to:commands.appendingPathComponent(name),options:.withoutOverwriting);pending=name;selectedDigest=digest;image=nil;focusOriginal=true;evidenceMessage="Retrieving original from the capture node…";return true
        }catch {evidenceMessage="Original request failed: \(error)";return false}
    }
    static func displayError(_ raw:String?)->String {
        let text=raw ?? "Capture node did not return verified bytes."
        if let range=text.range(of:"node HTTP [0-9]{3}",options:.regularExpression),let code=Int(text[range].suffix(3)) {
            if code==401 || code==403{return "Current evidence access was refused (HTTP \(code))."}
            if code>=500{return "Capture node is unavailable (HTTP \(code)). Retry when its connection returns."}
            if code==404 || code==410{return "Original is unavailable or expired (HTTP \(code))."}
            return "Original request failed (HTTP \(code))."
        }
        return String(text.prefix(240))
    }
    private func pollReceipt(){guard let pending else{return};let url=directory.appendingPathComponent("receipts/"+pending);guard FileManager.default.fileExists(atPath:url.path) else{return};self.pending=nil
        do {struct Receipt:Decodable {let accepted:Bool;let report_id:String;let digest:String;let path:String?;let size:Int64?;let content_type:String?;let retain_until_unix_ms:Int64?;let error:String?}
            let raw=try Data(contentsOf:url);guard raw.count<8192 else{throw DemoError.invalidFile};let r=try JSONDecoder().decode(Receipt.self,from:raw);guard r.report_id==selected,r.digest==selectedDigest else{return};guard r.accepted else{evidenceMessage="Original unavailable: "+Self.displayError(r.error);return};guard let report,let expected=report.evidence.first(where:{$0.sha256==r.digest}),let path=r.path,r.size==expected.size,r.content_type==expected.content_type,r.retain_until_unix_ms==report.retain_until_unix_ms,report.retain_until_unix_ms>Int64(now.timeIntervalSince1970*1000),["media/"+r.digest+".png","media/"+r.digest+".jpg","media/"+r.digest+".wav","media/"+r.digest+".json"].contains(path) else{throw DemoError.invalidFile}
            let file=directory.appendingPathComponent(path);let values=try file.resourceValues(forKeys:[.isSymbolicLinkKey,.fileSizeKey]);guard values.isSymbolicLink==false,values.fileSize==Int(expected.size) else{throw DemoError.invalidFile};let data=try Data(contentsOf:file);guard SHA256.hash(data:data).map({String(format:"%02x",$0)}).joined()==r.digest else{throw DemoError.invalidFile};image=expected.content_type.hasPrefix("image/") ? NSImage(data:data):nil;evidenceMessage="SHA-256 verified · \(expected.size.formatted()) bytes · \(expected.sensor_id). Original simulated capture, not a live camera."
        }catch {image=nil;evidenceMessage="Original verification failed: \(error)"}
    }
    // Private acceptance hook invokes the same selection and fetch callbacks.
    // It renders the actual view; it cannot inject peer records or media.
    private func pollQA(){let url=directory.appendingPathComponent("native-control.json");guard let raw=try? Data(contentsOf:url),raw.count<8192,let c=(try? JSONSerialization.jsonObject(with:raw)) as? [String:Any],let id=c["id"] as? String,id != qaID else{return};qaID=id;var accepted=false
        switch c["action"] as? String {case "select":accepted=select(c["report_id"] as? String ?? "");case "fetch":accepted=fetch(c["digest"] as? String ?? "");case "capture":if let path=c["path"] as? String,path.hasPrefix(directory.path+"/"){accepted=capture(URL(fileURLWithPath:path))};default:break}
        let receipt:[String:Any]=["id":id,"accepted":accepted,"selected":selected ?? "","digest":selectedDigest ?? "","evidence_message":evidenceMessage,"has_image":image != nil,"updated_unix_ms":Int64(now.timeIntervalSince1970*1000)];if let b=try? JSONSerialization.data(withJSONObject:receipt,options:.sortedKeys){try? b.write(to:directory.appendingPathComponent("native-control-result.json"),options:.atomic)}
    }
    func capture(_ url:URL)->Bool {let view=NSHostingView(rootView:PatrolDebriefView(model:self,active:false).environment(\.colorScheme,.light));view.appearance=NSAppearance(named:.aqua);view.frame=CGRect(x:0,y:0,width:1500,height:960);view.layoutSubtreeIfNeeded();guard let bitmap=view.bitmapImageRepForCachingDisplay(in:view.bounds) else{return false};view.cacheDisplay(in:view.bounds,to:bitmap);guard let data=bitmap.representation(using:.png,properties:[:]) else{return false};do {try data.write(to:url,options:.atomic);return true}catch{return false}}
}

@MainActor struct PatrolDebriefView:View {
    @StateObject private var model:PatrolDebriefModel
    let active:Bool
    init(directory:URL){_model=StateObject(wrappedValue:PatrolDebriefModel(directory:directory));active=true}
    init(model:PatrolDebriefModel,active:Bool=true){_model=StateObject(wrappedValue:model);self.active=active}
    private let ink=Color(red:0.14,green:0.22,blue:0.24)
    private let paper=Color(red:0.97,green:0.97,blue:0.95)
    var body:some View {
        VStack(alignment:.leading,spacing:0){header;Divider();HStack(alignment:.top,spacing:0){timeline.frame(width:340);Divider();ScrollView {VStack(alignment:.leading,spacing:22){progress;if !model.focusOriginal{coverage};if let report=model.report{detail(report)}else{Text("Observations will appear after the publisher verifies and uploads their originals.").foregroundStyle(.secondary)}}.padding(26)}};Divider();footer}.background(paper).foregroundStyle(ink).frame(minWidth:1100,minHeight:720).preferredColorScheme(.light).onAppear{if active{model.start()}}.onDisappear{if active{model.stop()}}
    }
    private var header:some View {HStack(spacing:16){Image(systemName:"tray.full.fill").font(.system(size:29)).foregroundStyle(.teal);VStack(alignment:.leading,spacing:3){Text("Patrol debrief").font(.system(size:29,weight:.semibold,design:.rounded));Text("STUDIO 01  /  SIMULATED ARTICULATED PATROL").font(.system(size:10,weight:.semibold)).tracking(1.4).foregroundStyle(.secondary)};Spacer();VStack(alignment:.trailing,spacing:5){Text("Peer reader · "+(model.state?.reader.capitalized ?? "Connecting")).font(.headline);Label(model.state?.current(at:model.now)==true ? "Peer connected":"Peer unavailable",systemImage:model.state?.current(at:model.now)==true ? "network":"wifi.slash").foregroundStyle(model.state?.current(at:model.now)==true ? Color.teal:.orange)}}.padding(26)}
    private var timeline:some View {ScrollView{VStack(alignment:.leading,spacing:12){Text("RETAINED REPORTS").font(.caption.weight(.semibold)).foregroundStyle(.secondary);Text("Historical observations and mission result. These reports do not authorize navigation.").font(.callout).foregroundStyle(.secondary);ForEach(model.state?.reports ?? []){r in Button{_ = model.select(r.id)}label:{VStack(alignment:.leading,spacing:8){HStack{Image(systemName:r.kind=="result" ? "flag.checkered":"camera.fill");Text(r.kind=="result" ? "Mission "+(r.state?.state ?? "result").capitalized:"Room observation").font(.headline)};Text(r.kind=="result" ? r.state?.reason ?? "Recorded terminal outcome":(r.room_ids ?? []).joined(separator:", ")).font(.callout).lineLimit(3);Text(date(r.original_unix_ms)).font(.caption.monospaced());Text(String(r.evidence.count)+" original references · historical").font(.caption).foregroundStyle(.secondary)}.frame(maxWidth:.infinity,alignment:.leading).padding(15).background(model.selected==r.id ? Color.teal.opacity(0.12):Color.white,in:RoundedRectangle(cornerRadius:12))}.buttonStyle(.plain)};if model.state?.reports.isEmpty != false{Label("No retained report received yet",systemImage:"tray").padding(.vertical,28).foregroundStyle(.secondary)}}.padding(22)}}
    private var progress:some View {VStack(alignment:.leading,spacing:8){HStack{Label(model.state?.liveCurrent(at:model.now)==true ? "Live mission progress":"Live progress unavailable",systemImage:model.state?.liveCurrent(at:model.now)==true ? "dot.radiowaves.left.and.right":"clock.badge.exclamationmark").font(.headline).foregroundStyle(model.state?.liveCurrent(at:model.now)==true ? Color.teal:.orange);Spacer();if let live=model.state?.live{Text(date(live.observed_unix_ms)).font(.caption.monospaced())}};if let live=model.state?.live,model.state?.liveCurrent(at:model.now)==true{Text(live.state.state.replacingOccurrences(of:"_",with:" ").capitalized+" · "+(live.state.reason ?? "Within the original signed mission")).font(.callout)}else{Text("No fresh executor update. Historical coverage below does not establish current clearance, location or mission authority.").font(.callout).foregroundStyle(.secondary)};if let error=model.error ?? model.state?.error,!error.isEmpty{Text(PatrolDebriefModel.displayError(error)).font(.caption).foregroundStyle(.orange)}}.padding(17).background(Color.white,in:RoundedRectangle(cornerRadius:12))}
    private var coverage:some View {VStack(alignment:.leading,spacing:12){Text(model.state?.reports.contains(where:{$0.kind=="result"})==true ? "Coverage at mission end":"Last reported coverage").font(.title2.weight(.semibold));if let coverage=model.coverage{if let tour=coverage.tour{PatrolTourProgress(tour:tour)};HStack(alignment:.top,spacing:20){PatrolDebriefMap(rooms:coverage.rooms).frame(width:390,height:230).background(Color.white,in:RoundedRectangle(cornerRadius:12));VStack(alignment:.leading,spacing:9){ForEach(coverage.rooms){room in HStack{Image(systemName:room.visits>0 ? "checkmark.circle.fill":"circle.dashed").foregroundStyle(room.visits>0 ? Color.teal:.orange);VStack(alignment:.leading,spacing:2){Text(room.name).font(.callout.weight(.semibold));Text(room.visits>0 ? "\(room.visits) observed visit\(room.visits==1 ? "":"s") · sim \(String(format:"%.1f",room.last_observed_s ?? 0)) s":"Unobserved · no coverage claim").font(.caption).foregroundStyle(.secondary);Text(model.cadence(room)).font(.caption2).foregroundStyle(.secondary)}}};Text("Counts describe retained executor observations. They do not mean the rooms are clear.").font(.caption).foregroundStyle(.secondary).padding(.top,7)}.frame(maxWidth:.infinity,alignment:.leading)}}}}
    private func detail(_ report:PatrolDebriefState.Report)->some View {VStack(alignment:.leading,spacing:13){HStack{Text(report.kind=="capture" ? "Original observation":"Terminal record").font(.title2.weight(.semibold));Spacer();if report.kind=="capture"{Button(model.focusOriginal ? "Show room coverage":"Focus original"){model.focusOriginal.toggle()}.buttonStyle(.bordered)};Text("HISTORICAL · SIMULATION").font(.caption.weight(.semibold)).foregroundStyle(.secondary)};Text("Captured "+date(report.original_unix_ms)+" · "+report.frame.id+" @ "+report.frame.revision).font(.callout);if let ns=report.simulation_ns{Text("Original sensor clock \(String(format:"%.3f",Double(ns)/1e9)) s · sequence \(report.sequence ?? 0)").font(.caption.monospaced()).foregroundStyle(.secondary)};if !report.cameras.isEmpty{HStack{ForEach(report.cameras){e in Button{_ = model.fetch(e.sha256)}label:{Label("Fetch "+e.sensor_id,systemImage:"arrow.down.circle")}.buttonStyle(.bordered).disabled(model.state?.current(at:model.now) != true)};Spacer()};if let image=model.image{Image(nsImage:image).resizable().interpolation(.high).scaledToFit().frame(maxWidth:.infinity,maxHeight:310).background(Color.black.opacity(0.04),in:RoundedRectangle(cornerRadius:12))};Text(model.evidenceMessage).font(.callout).foregroundStyle(model.image==nil ? Color.secondary:.teal)};DisclosureGroup("Report identity and provenance"){VStack(alignment:.leading,spacing:7){Text("Publisher "+(model.state?.config.source.app ?? ""));Text("Run "+report.run_id);Text("Mission "+report.mission_id);Text("Original clock "+report.original_clock);Text("Report "+report.id);Text("Content "+report.content_sha256);Text("Signed mission digest "+report.spec_sha256);Text("Retention until "+date(report.retain_until_unix_ms));Text("Publisher-attested executor record. This view does not independently re-authorize the original task.").foregroundStyle(.secondary)}.font(.caption.monospaced()).textSelection(.enabled).padding(.top,8)}}}
    private var footer:some View {HStack{Text("READ ONLY · ORIGINAL AUTHORITY REMAINS WITH THE EXECUTOR").font(.system(size:9,weight:.semibold)).tracking(0.7);Spacer();Text("Bulk originals stay at the capture node · SHA-256 checked on retrieval").font(.caption)}.foregroundStyle(.secondary).padding(.horizontal,26).padding(.vertical,15)}
    private func date(_ ms:Int64)->String{Date(timeIntervalSince1970:Double(ms)/1000).formatted(date:.omitted,time:.standard)}
}
struct PatrolDebriefMap:View {let rooms:[PatrolDebriefState.Room]
    var body:some View {Canvas{context,size in let points=rooms.flatMap(\.polygon);let xs=points.map{$0[0]},ys=points.map{$0[1]};let minX=xs.min() ?? 0,maxX=xs.max() ?? 1,minY=ys.min() ?? 0,maxY=ys.max() ?? 1;let scale=min((size.width-36)/max(1,maxX-minX),(size.height-36)/max(1,maxY-minY));func p(_ v:[Double])->CGPoint{CGPoint(x:18+(v[0]-minX)*scale,y:size.height-18-(v[1]-minY)*scale)};for room in rooms{var path=Path();path.addLines(room.polygon.map(p));path.closeSubpath();context.fill(path,with:.color(room.visits>0 ? Color.teal.opacity(0.14):Color.orange.opacity(0.08)));context.stroke(path,with:.color(room.visits>0 ? .teal:.orange),lineWidth:1.3);let centre=[room.polygon.map{$0[0]}.reduce(0,+)/Double(room.polygon.count),room.polygon.map{$0[1]}.reduce(0,+)/Double(room.polygon.count)];context.draw(Text(room.name).font(.system(size:11,weight:.medium)).foregroundColor(.primary),at:p(centre))}}}
}
