import AppKit
import CryptoKit
import SwiftUI
import WorldClient

private struct HistoryProfile:Decodable {
  let version:Int
  let credentials_file,cache_directory,origin,principal,assignee,run_id:String
}
@MainActor final class DemoMissionHistory:ObservableObject {
  static let shared=DemoMissionHistory()
  @Published var taskID=""
  @Published var document:MissionHistoryDocument?
  @Published var configured=false
  @Published var busy=false
  @Published var message="Import a private reader profile with an explicit audit origin and run."
  @Published var source="No history selected"
  private(set) var scope:MissionHistoryScope?
  private var client:MissionHistoryClient?
  private var cache:MissionHistoryCache?
  init(){
    let a=CommandLine.arguments
    if let i=a.firstIndex(of:"--mission-history-profile"),i+1<a.count {
      do{try configure(URL(fileURLWithPath:a[i+1]))}catch{message=error.localizedDescription}
    }
  }
  func configure(_ url:URL)throws{
    guard !configured,!busy else{throw WorldClientError.invalidResponse}
    let p=try JSONDecoder().decode(HistoryProfile.self,from:DemoInvestigations.privateRead(url.path))
    guard p.version==1,p.cache_directory.hasPrefix("/") else{throw WorldClientError.invalidResponse}
    let credentials=try WorldCredentials(data:DemoInvestigations.privateRead(p.credentials_file))
    let scope=try MissionHistoryScope(origin:p.origin,principal:p.principal,assignee:p.assignee,runID:p.run_id)
    let cache=try MissionHistoryCache(directory:URL(fileURLWithPath:p.cache_directory),credentials:credentials,scope:scope)
    self.scope=scope;self.cache=cache;client=MissionHistoryClient(credentials:credentials,scope:scope);configured=true
    source="Configured reader · \(credentials.audience)";message="Choose an exact task ID, or leave it empty to review the origin's local audit prefix."
  }
  func select(task:String,run:String?=nil)async{
    guard !busy else{return}
    guard run==nil || run==scope?.runID else{document=nil;message="This result belongs to another run. Import its explicit origin profile in a new viewer.";return}
    taskID=task;document=nil
    await openSaved()
  }
  func openSaved()async{
    guard !busy,let cache else{return};busy=true;defer{busy=false}
    do{document=try await cache.load(taskID:taskID);message=document==nil ? "No saved timeline for this exact task and origin.":"Saved historical snapshot. Authorization and replica completeness are known only as of retrieval."}
    catch{document=nil;message="Saved timeline is unavailable: \(error.localizedDescription)"}
  }
  func refresh()async{
    guard !busy,let client,let cache else{return};busy=true;defer{busy=false}
    let selected=taskID
    do{
      var pages:[MissionHistoryPage]=[],after=""
      for _ in 0..<4 {
        let p=try await client.page(taskID:selected,after:after)
        if pages.reduce(0,{$0+$1.events.count})+p.events.count>256 || pages.reduce(0,{$0+$1.data.count})+p.data.count>3<<20 {break}
        pages.append(p)
        if p.next.isEmpty{break};after=p.next
      }
      document=try await cache.store(taskID:selected,pages:pages,fetchedUnixMS:Int64(Date().timeIntervalSince1970*1000))
      message="Saved an authenticated reader response. Origin authorization was checked at retrieval; this remains historical, non-executable data."
    }catch{message="Refresh unavailable. The saved timeline is unchanged: \(error.localizedDescription)"}
  }
  func capture(_ path:URL)->Bool{
    let host=NSHostingView(rootView:MissionHistoryPanel(model:self).environment(\.colorScheme,.light));host.appearance=NSAppearance(named:.aqua)
    host.frame=CGRect(x:0,y:0,width:1000,height:760);host.layoutSubtreeIfNeeded()
    guard let bitmap=host.bitmapImageRepForCachingDisplay(in:host.bounds)else{return false}
    host.cacheDisplay(in:host.bounds,to:bitmap);guard let png=bitmap.representation(using:.png,properties:[:])else{return false}
    do{
      try png.write(to:path,options:.atomic)
      let receipt:[String:Any] = ["scope":"Native SwiftUI timeline and scripted application callbacks; no OS input or physical execution", "task_id":taskID,"origin":scope?.origin ?? "","run_id":scope?.runID ?? "","events":document?.events.map{["sequence":$0.sequence,"event":$0.event,"phase":$0.phase]} ?? [],"fetched_unix_ms":document?.fetchedUnixMS ?? 0,"message":message,"png_sha256":SHA256.hash(data:png).map{String(format:"%02x",$0)}.joined()]
      try JSONSerialization.data(withJSONObject:receipt,options:[.prettyPrinted,.sortedKeys]).write(to:path.appendingPathExtension("json"));return true
    }catch{return false}
  }
}
struct MissionHistoryLauncher:View {
    @State private var actions = TwinActions()

  var task:String?=nil
  var run:String?=nil
  @ObservedObject var model=DemoMissionHistory.shared
  @State private var shown=false
  var body:some View{
        Group {

    Button{actions.submit { [self] in if let task{await model.select(task:task,run:run)};shown=true}}label:{Label("Mission history",systemImage:"clock.arrow.circlepath")}.buttonStyle(.bordered)
      .sheet(isPresented:$shown){MissionHistoryPanel(model:model)}

        }.task { await actions.run() }
}
}
struct MissionHistoryPanel:View {
    @State private var actions = TwinActions()

  @ObservedObject var model:DemoMissionHistory
  @Environment(\.dismiss) private var dismiss
  @State private var importing=false
  private func date(_ ms:Int64)->String{Date(timeIntervalSince1970:Double(ms)/1000).formatted(date:.abbreviated,time:.standard)}
  var body:some View{
        Group {

    VStack(alignment:.leading,spacing:16){
      HStack{VStack(alignment:.leading,spacing:4){Text("Mission history").font(.system(size:28,weight:.semibold));Text("Durable decisions · exact origin · read-only history").font(.callout).foregroundStyle(.secondary)};Spacer();Button("Import profile"){importing=true}.disabled(model.configured || model.busy);Button("Done"){dismiss()}}
      if let scope=model.scope{
        HStack(alignment:.top,spacing:32){VStack(alignment:.leading){Text("ASSIGNEE ORIGIN").font(.caption2.weight(.bold));Text(scope.origin).font(.headline);Text(scope.assignee).font(.caption)};VStack(alignment:.leading){Text("EXACT RUN").font(.caption2.weight(.bold));Text(scope.runID).font(.headline);Text(model.source).font(.caption)}}
        .padding(14).frame(maxWidth:.infinity,alignment:.leading).background(Color.teal.opacity(0.08),in:RoundedRectangle(cornerRadius:10))
      }
      HStack{TextField("Exact task ID (empty = origin timeline)",text:Binding(get:{model.taskID},set:{model.taskID=$0;model.document=nil})).textFieldStyle(.roundedBorder).disabled(model.busy);Button("Open saved"){actions.submit { [self] in await model.openSaved()}}.disabled(!model.configured || model.busy);Button("Refresh from reader"){actions.submit { [self] in await model.refresh()}}.buttonStyle(.borderedProminent).tint(.teal).disabled(!model.configured || model.busy)}
      if let doc=model.document{
        HStack{Label("Historical snapshot",systemImage:"archivebox").font(.headline);Spacer();Text("Retrieved \(date(doc.fetchedUnixMS))").font(.caption)}
        Text("\(doc.events.count) events · source prefix through \(doc.pages.last?.through ?? 0) · \(doc.completeLocalView ? "end of this replica's matching history" : "bounded partial view; more available through CLI")").font(.caption).foregroundStyle(.secondary)
        ScrollView{LazyVStack(alignment:.leading,spacing:12){ForEach(doc.events){e in
          HStack(alignment:.top,spacing:14){Text(String(format:"%02llu",e.sequence)).font(.title2.monospacedDigit().weight(.light)).foregroundStyle(.teal).frame(width:40);VStack(alignment:.leading,spacing:5){HStack{Text(e.event.replacingOccurrences(of:"_",with:" ").capitalized).font(.headline);Text(e.phase).font(.caption.weight(.semibold)).padding(.horizontal,8).padding(.vertical,3).background(Color.teal.opacity(0.09),in:Capsule());Spacer();Text(date(e.eventUnixMS)).font(.caption)};Text(e.taskID+" · "+e.actor).font(.caption.monospaced());if !e.reason.isEmpty{Text(e.reason).font(.callout)};Text(e.currentVerification=="current_origin_grant_verified" ? "Origin grant verified by reader at retrieval":"Origin was not currently verified at retrieval: "+e.currentVerification).font(.caption2).foregroundStyle(.secondary);DisclosureGroup("Provenance and export attestation"){Text("Origin attested this retained event at \(date(e.exportedUnixMS)). This is not a signature made when the event occurred.").font(.caption);Text(e.originalVerification=="not_present" ? "No original operator instruction attached.":"Original instruction signature retained. No claim of historical revocation certainty.").font(.caption)}}}.padding(14).background(.white,in:RoundedRectangle(cornerRadius:10))
        }}}
      }else{Spacer();Text("Inspect an assignee's accepted, stopped and interrupted decisions after reconnecting. Replicated history cannot dispatch or resume a task.").font(.title3).foregroundStyle(.secondary).frame(maxWidth:.infinity);Spacer()}
      Text(model.message).font(.caption).fixedSize(horizontal:false,vertical:true)
      Text("A replica's caught-up prefix is not global completeness. Retained events do not renew a task, grant or evidence deadline.").font(.caption2).foregroundStyle(.secondary)
    }.padding(24).frame(width:1000,height:760).foregroundStyle(Color(red:0.14,green:0.22,blue:0.24)).background(Color(red:0.97,green:0.97,blue:0.95))
      .fileImporter(isPresented:$importing,allowedContentTypes:[.json]){r in if case .success(let url)=r{do{try model.configure(url)}catch{model.message=error.localizedDescription}}}

        }.task { await actions.run() }
}
}
