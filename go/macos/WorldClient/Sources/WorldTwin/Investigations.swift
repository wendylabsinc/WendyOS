import AppKit
import AVFoundation
import CryptoKit
import Darwin
import SwiftUI
import WorldClient

struct InvestigationProfile:Decodable {
  let version:Int
  let tenant,site,journal_directory:String
  let origins:[Origin]
  struct Origin:Decodable {
    let id,label,backend,pin_credentials_file,read_credentials_file:String
    let sensor_ids:[String]
  }
}

@MainActor final class DemoInvestigations:ObservableObject {
  static let shared=DemoInvestigations()
  @Published var entries:[InvestigationEntry]=[]
  @Published var selectedID:String?
  @Published var draftName="Inspection investigation"
  @Published var draftResult:JSONValue?
  @Published var draftEvidence:[InvestigationEvidence]=[]
  @Published var selectedEvidence=Set<String>()
  @Published var deadline=Date().addingTimeInterval(300)
  @Published var message="Import an explicit standalone capture-store profile."
  @Published var busy=false
  @Published var configured=false
  @Published var previews:[String:VerifiedEvidence]=[:]
  private var journal:InvestigationJournal?
  private var clients:[String:InvestigationClient]=[:]
  private var sensors:[String:String]=[:]
  private var caches:[String:InspectionMediaCache]=[:]
  init() {
    let a=CommandLine.arguments
    if let i=a.firstIndex(of:"--investigation-profile"),i+1<a.count {
      do {try configure(URL(fileURLWithPath:a[i+1]))}catch{message=error.localizedDescription}
    }
  }
  static func privateRead(_ path:String,limit:Int=128<<10)throws->Data {
    guard path.hasPrefix("/") else {throw InvestigationError.invalid}
    let fd=Darwin.open(path,O_RDONLY|O_NOFOLLOW|O_CLOEXEC)
    guard fd>=0 else {throw InvestigationError.invalid};defer{Darwin.close(fd)}
    var st=stat()
    guard fstat(fd,&st)==0,st.st_mode&S_IFMT==S_IFREG,st.st_uid==geteuid(),st.st_mode&0o077==0,
      st.st_nlink==1,st.st_size>0,st.st_size<=limit else {throw InvestigationError.invalid}
    let file=FileHandle(fileDescriptor:fd,closeOnDealloc:false)
    guard let bytes=try file.read(upToCount:limit+1),bytes.count<=limit else {throw InvestigationError.invalid};return bytes
  }
  func configure(_ path:URL)throws {
    guard !configured,!busy else {throw InvestigationError.busy}
    let p=try JSONDecoder().decode(InvestigationProfile.self,from:Self.privateRead(path.path))
    guard p.version==1,p.journal_directory.hasPrefix("/"),(1...8).contains(p.origins.count),
      Set(p.origins.map(\.id)).count==p.origins.count else {throw InvestigationError.invalid}
    var clients:[String:InvestigationClient]=[:],sensors:[String:String]=[:],caches:[String:InspectionMediaCache]=[:]
    for o in p.origins {
      guard o.backend=="standalone-worldd",!o.id.isEmpty,(1...16).contains(o.sensor_ids.count) else {throw InvestigationError.invalid}
      let read=try WorldCredentials(data:Self.privateRead(o.read_credentials_file))
      let client=try InvestigationClient(label:o.label,credentials:InvestigationCredentials(data:Self.privateRead(o.pin_credentials_file)),readCredentials:read)
      guard client.origin.tenant==p.tenant,client.origin.site==p.site,clients[client.origin.id]==nil else {throw InvestigationError.originChanged}
      clients[client.origin.id]=client
      caches[client.origin.id]=try InspectionMediaCache(directory:URL(fileURLWithPath:p.journal_directory).appendingPathComponent("media"),scopeID:read.scopeID)
      for sensor in o.sensor_ids {
        guard !sensor.isEmpty,sensor.utf8.count<=128,sensors[sensor]==nil else {throw InvestigationError.invalid}
        sensors[sensor]=client.origin.id
      }
    }
    journal=try InvestigationJournal(directory:URL(fileURLWithPath:p.journal_directory),tenant:p.tenant,site:p.site)
    self.clients=clients;self.sensors=sensors;self.caches=caches;configured=true
    message="Configured stores only. Native-agent capture pins are not supported."
  }
  func refresh() async {
    guard !busy,let journal else {return}
    do {entries=try await journal.entries()}catch{message=error.localizedDescription}
    previews=previews.filter{$0.value.retainUntil>Date()}
  }
  func prepare(_ result:MissionInspectionResult) async {await prepareHistorical(result.raw)}
  /// Also used by bounded acceptance with an explicitly labeled imported inspection result.
  func prepareHistorical(_ raw:JSONValue) async {
    guard configured,!busy else {return}
    busy=true;defer{busy=false}
    draftResult=nil;draftEvidence=[];selectedEvidence=[];previews=[:];selectedID=nil
    do {
      guard let task=raw["task_id"]?.string,!task.isEmpty,let run=raw["run_id"]?.string,!run.isEmpty,
        let samples=raw["samples"]?.array,(1...16).contains(samples.count) else {throw InvestigationError.invalid}
      var items:[InvestigationEvidence]=[]
      for sample in samples {
        guard let r=sample["evidence"] else {continue}
        guard let sensor=sample["sensor_id"]?.string,let key=sensors[sensor],let client=clients[key],
          let digest=r["sha256"]?.string,let size=r["size"]?.int.flatMap(Int.init(exactly:)),let mime=r["content_type"]?.string else {throw InvestigationError.originChanged}
        let ref=try MissionInspectionEvidence(sha256:digest,contentType:mime,size:size)
        // Fresh authenticated metadata is required when creating a new remote hold.
        let media=try await client.evidence(ref)
        let item=try InvestigationEvidence(origin:client.origin,sensorID:sensor,sha256:digest,size:size,contentType:mime,
          originalRetainUntilUnixMS:Int64(media.retainUntil.timeIntervalSince1970*1000))
        if let cache=caches[key] {try await cache.store(media,reference:ref)}
        previews[item.id]=media;items.append(item)
      }
      guard !items.isEmpty else {throw InvestigationError.invalid}
      draftResult=raw;draftEvidence=items;selectedEvidence=Set(items.map(\.id))
      deadline=min(Date().addingTimeInterval(300),Date(timeIntervalSince1970:Double(items.map(\.originalRetainUntilUnixMS).min()!)/1000))
      message="Review selected bytes, configured stores and the absolute deadline. No pins sent yet."
    }catch{message=error.localizedDescription}
  }
  func saveAndRetain() async {
    guard !busy,let journal,let result=draftResult else{return}
    busy=true;defer{busy=false}
    do {
      let selected=draftEvidence.filter{selectedEvidence.contains($0.id)}
      let entry=try await journal.create(name:draftName,historicalResult:result,evidence:selected,retainUntilUnixMS:Int64(deadline.timeIntervalSince1970*1000))
      selectedID=entry.id;draftResult=nil;draftEvidence=[];selectedEvidence=[]
      // Each store commits independently; a later failure never erases earlier success.
      var failed=0
      for hold in entry.holds {
        guard let client=clients[hold.id] else {failed+=1;continue}
        do {try await journal.reconcile(id:entry.id,originID:hold.id,using:client)}catch{failed+=1}
      }
      entries=try await journal.entries()
      message=failed==0 ? "Saved with acknowledged per-store holds. These are historical observations.":"Saved. Some stores need reconciliation; inspect each outcome."
    }catch{message=error.localizedDescription}
  }
  func act(_ entry:InvestigationEntry,hold:InvestigationHold,release:Bool) async {
    guard !busy,let journal else{return}
    guard let client=clients[hold.id] else {message="The original configured store is unavailable. No request sent.";return}
    busy=true;defer{busy=false}
    do {
      if release {try await journal.release(id:entry.id,originID:hold.id,using:client)}
      else {try await journal.reconcile(id:entry.id,originID:hold.id,using:client)}
      entries=try await journal.entries();message="Saved the exact capture-store outcome. Original deadlines are unchanged."
    }catch{entries=(try? await journal.entries()) ?? entries;message=error.localizedDescription}
  }
  func open(_ entry:InvestigationEntry) async {
    guard !busy else{return};selectedID=entry.id;draftResult=nil;previews=[:]
    busy=true;defer{busy=false}
    message="Saved media on this Mac; remote retention is known only at the last acknowledgement."
    for item in entry.evidence {
      guard let client=clients[item.origin.id],let cache=caches[item.origin.id] else {continue}
      do {
        try await client.authorizeCachedRead()
        let reference=try MissionInspectionEvidence(sha256:item.sha256,contentType:item.contentType,size:item.size)
        if let retained=try await cache.load(reference:reference) {previews[item.id]=retained}
      }catch{message="Some saved media is unavailable or expired. Local metadata remains."}
    }
  }
  func loadRetainedMedia(_ entry:InvestigationEntry) async {
    guard !busy else{return};busy=true;defer{busy=false}
    var loaded=0
    for item in entry.evidence {
      do {
        guard Date().timeIntervalSince1970*1000<Double(item.originalRetainUntilUnixMS),
          let client=clients[item.origin.id],let cache=caches[item.origin.id] else {throw InvestigationError.expired}
        let ref=try MissionInspectionEvidence(sha256:item.sha256,contentType:item.contentType,size:item.size)
        let value=try await client.evidence(ref)
        guard Int64(value.retainUntil.timeIntervalSince1970*1000)==item.originalRetainUntilUnixMS else {throw InvestigationError.invalid}
        try await cache.store(value,reference:ref)
        if let saved=try await cache.load(reference:ref) {previews[item.id]=saved;loaded+=1}
      }catch{previews.removeValue(forKey:item.id)}
    }
    message="Verified \(loaded) of \(entry.evidence.count) historical media items using current evidence-read access. Hold deadlines are unchanged."
  }
  var selected:InvestigationEntry?{entries.first{$0.id==selectedID}}
  static func status(_ hold:InvestigationHold)->String {
    if hold.state != .released && hold.state != .canceled && Date().timeIntervalSince1970*1000>=Double(hold.request.retainUntilUnixMS) {return "Expired"}
    switch hold.state {
    case .pending:return "Not yet sent"
    case .active:return "Protected at last check"
    case .released:return "Release acknowledged"
    case .canceled:return "Canceled before sending"
    case .expired:return "Expired"
    case .denied:return "Authorization denied; current hold unknown"
    case .unavailable:return "Unavailable at this store; outcome unconfirmed"
    case .sendingCreate,.uncertainCreate:return "Create outcome unknown"
    case .sendingRelease,.uncertainRelease:return "Release outcome unknown"
    }
  }
  func capture(_ path:URL)->Bool {
    let host=NSHostingView(rootView:InvestigationPanel(model:self).environment(\.colorScheme,.light));host.appearance=NSAppearance(named:.aqua)
    host.frame=CGRect(x:0,y:0,width:960,height:760);host.layoutSubtreeIfNeeded()
    guard let bitmap=host.bitmapImageRepForCachingDisplay(in:host.bounds) else{return false}
    host.cacheDisplay(in:host.bounds,to:bitmap)
    guard let png=bitmap.representation(using:.png,properties:[:]) else{return false}
    do {
      try png.write(to:path,options:.atomic)
      let receipt:[String:Any]=["scope":"Recorded native SwiftUI panel and scripted application callbacks, not OS input","entries":entries.count,
        "selected_id":selectedID ?? "","states":selected?.holds.map{Self.status($0)} ?? [],"media_previews":previews.count,
        "message":message,"png_sha256":SHA256.hash(data:png).map{String(format:"%02x",$0)}.joined()]
      try JSONSerialization.data(withJSONObject:receipt,options:[.sortedKeys,.prettyPrinted]).write(to:path.appendingPathExtension("json"));return true
    }catch{return false}
  }
}

struct InvestigationLauncher:View {
  @ObservedObject var investigations=DemoInvestigations.shared
  @State private var shown=false
  var body:some View {
    Button{shown=true}label:{Label("Investigations",systemImage:"archivebox")}.buttonStyle(.bordered)
      .sheet(isPresented:$shown){InvestigationPanel(model:investigations)}
  }
}
struct InvestigationPanel:View {
    @State private var actions = TwinActions()

  @ObservedObject var model:DemoInvestigations
  @Environment(\.dismiss) private var dismiss
  @State private var importing=false
  @State private var reviewing=false
  @State private var releasing:InvestigationHold?
  var body:some View {
        Group {

    VStack(alignment:.leading,spacing:14) {
      HStack {Text("Evidence investigations").font(.system(size:26,weight:.semibold));Spacer();Button("Import profile"){importing=true}.disabled(model.configured);Button("Done"){dismiss()}}
      Text("Historical media · independent capture-store holds · no task authority").font(.caption).foregroundStyle(.secondary)
      HStack(alignment:.top,spacing:20) {
        VStack(alignment:.leading,spacing:12) {
          Text("Saved investigations").font(.headline)
          ScrollView {VStack(alignment:.leading,spacing:10){ForEach(model.entries){entry in
            Button{actions.submit { [self] in await model.open(entry)}}label:{VStack(alignment:.leading,spacing:5){Text(entry.name).font(.headline);Text(entry.taskID).font(.caption);Text("\(entry.evidence.count) evidence items · \(entry.holds.count) stores").font(.caption2)}}
              .buttonStyle(.plain).padding(10).frame(maxWidth:.infinity,alignment:.leading).background(model.selectedID==entry.id ? Color.teal.opacity(0.12):Color.white,in:RoundedRectangle(cornerRadius:10))
          }}}
          Text("Start from an inspection result's Save investigation control.").font(.caption).foregroundStyle(.secondary)
          Text("Native-agent capture pins are not supported. A local media copy is separate from a remote hold.").font(.caption).foregroundStyle(.secondary)
        }.frame(width:235)
        Divider()
        ScrollView {VStack(alignment:.leading,spacing:12) {
          if model.draftResult != nil {
            Text("Review a new investigation").font(.title3.weight(.semibold))
            TextField("Investigation name",text:$model.draftName)
            DatePicker("Retain until",selection:$model.deadline,displayedComponents:[.date,.hourAndMinute])
            ForEach(model.draftEvidence){item in
              Toggle(isOn:Binding(get:{model.selectedEvidence.contains(item.id)},set:{if $0{model.selectedEvidence.insert(item.id)}else{model.selectedEvidence.remove(item.id)}})) { evidence(item) }
            }
            Button("Review retention"){reviewing=true}.buttonStyle(.borderedProminent).tint(.teal).disabled(model.busy || model.selectedEvidence.isEmpty)
            Text("Each store commits independently. Retention cannot exceed the earliest original media expiry.").font(.caption).foregroundStyle(.secondary)
          } else if let entry=model.selected {
            Text(entry.name).font(.title2.weight(.semibold))
            MissionHistoryLauncher(task:entry.taskID,run:entry.runID)
            Text("Task \(entry.taskID) · run \(entry.runID)").font(.caption.monospaced()).textSelection(.enabled)
            if let captured=entry.result["captured_unix_ms"]?.int {Text("Original capture \(Date(timeIntervalSince1970:Double(captured)/1000).formatted())").font(.caption)}
            DisclosureGroup("Saved result identity") {Text(entry.resultSHA256).font(.caption2.monospaced()).textSelection(.enabled);Text("Saved application context, not a new current observation or independent proof.").font(.caption2)}
            LazyVGrid(columns:[GridItem(.flexible()),GridItem(.flexible())],alignment:.leading,spacing:10){ForEach(entry.holds){hold in
              VStack(alignment:.leading,spacing:6) {
                Text(hold.origin.label).font(.headline)
                TimelineView(.periodic(from:.now,by:1)){_ in Text(DemoInvestigations.status(hold)).foregroundStyle(hold.state == .active && Date().timeIntervalSince1970*1000<Double(hold.request.retainUntilUnixMS) ? Color.teal:Color.secondary)}
                Text("\(hold.origin.audience) · \(hold.origin.site)").font(.caption)
                Text("Until \(Date(timeIntervalSince1970:Double(hold.request.retainUntilUnixMS)/1000).formatted())").font(.caption)
                if let checked=hold.checkedAtUnixMS {Text("Last acknowledgement \(Date(timeIntervalSince1970:Double(checked)/1000).formatted())").font(.caption2).foregroundStyle(.secondary)}
                HStack {
                  Button("Check / retry"){actions.submit { [self] in await model.act(entry,hold:hold,release:false)}}.disabled(model.busy || [.released,.canceled,.expired].contains(hold.state))
                  Button("Release",role:.destructive){releasing=hold}.disabled(model.busy || [.released,.canceled,.expired].contains(hold.state))
                }
                if let error=hold.lastError {Text(error).font(.caption).foregroundStyle(.orange)}
              }.padding(12).frame(maxWidth:.infinity,alignment:.leading).background(.white,in:RoundedRectangle(cornerRadius:10))
            }
            }
            HStack{Text("Saved historical media").font(.headline);Spacer();Button("Load from stores"){actions.submit { [self] in await model.loadRetainedMedia(entry)}}.disabled(model.busy)}
            LazyVGrid(columns:[GridItem(.flexible()),GridItem(.flexible())],alignment:.leading,spacing:8){ForEach(entry.evidence){evidence($0)}}
            Text("Remote hold receipts describe the last acknowledged check. They do not promise availability during a partition, after hard expiry or after storage failure.").font(.caption).foregroundStyle(.secondary)
          } else {Text("Select saved inspection evidence to retain a named investigation.").foregroundStyle(.secondary)}
        }.frame(maxWidth:.infinity,alignment:.leading)}
      }
      Spacer(minLength:0)
      Text(model.message).font(.caption).fixedSize(horizontal:false,vertical:true)
    }.padding(24).frame(width:960,height:760).foregroundStyle(Color(red:0.14,green:0.22,blue:0.24)).background(Color(red:0.97,green:0.97,blue:0.95))
      .fileImporter(isPresented:$importing,allowedContentTypes:[.json]){r in if case .success(let url)=r{do{try model.configure(url);actions.submit { [self] in await model.refresh()}}catch{model.message=error.localizedDescription}}}
      .confirmationDialog("Create these independent capture-store holds?",isPresented:$reviewing,titleVisibility:.visible){Button("Save and retain selected evidence"){actions.submit { [self] in await model.saveAndRetain()}}}message:{Text("The exact selected digests and absolute deadline will be saved before sending. No observation becomes current.")}
      .alert("Release this store's hold?",isPresented:Binding(get:{releasing != nil},set:{if !$0{releasing=nil}})){Button("Cancel",role:.cancel){releasing=nil};Button("Release hold",role:.destructive){if let h=releasing,let e=model.selected{actions.submit { [self] in await model.act(e,hold:h,release:true)}};releasing=nil}}message:{Text("An uncertain create must first be resolved using its original request. Other stores and local historical media are unchanged.")}
      .task{await model.refresh();while !Task.isCancelled{try? await Task.sleep(for:.seconds(2));if !Task.isCancelled{await model.refresh()}}}

        }.task { await actions.run() }
}
  @ViewBuilder private func evidence(_ item:InvestigationEvidence)->some View {
    VStack(alignment:.leading,spacing:5) {
      Text(item.sensorID+" · "+item.origin.label).font(.callout.weight(.semibold))
      if let value=model.previews[item.id],value.retainUntil>Date(),let image=DemoInspectionMedia.image(value.data) {
        Image(nsImage:image).resizable().scaledToFit().frame(maxHeight:125).clipShape(RoundedRectangle(cornerRadius:6))
      }
      if item.contentType=="audio/wav",let value=model.previews[item.id],value.retainUntil>Date() {InvestigationAudio(value:value)}
      Text("\(item.contentType) · \(item.size) bytes").font(.caption2).foregroundStyle(.secondary)
      Text(item.sha256).font(.caption2.monospaced()).textSelection(.enabled)
    }.padding(8).frame(maxWidth:.infinity,alignment:.leading)
  }
}

private struct InvestigationAudio:View {
  let value:VerifiedEvidence
  @State private var player:AVAudioPlayer?
  @State private var error:String?
  var body:some View {
    VStack(alignment:.leading) {
      TimelineView(.periodic(from:.now,by:0.2)){_ in
        Button(player?.isPlaying == true ? "Stop saved audio":"Play saved audio") {
          guard value.retainUntil>Date() else {player?.stop();return}
          if player?.isPlaying == true {player?.stop()}
          else {do {let p=try AVAudioPlayer(data:value.data);player=p;if !p.play(){error="Playback unavailable"}}catch{self.error="Verified audio could not be decoded"}}
        }.disabled(value.retainUntil<=Date())
      }
      if let error {Text(error).font(.caption)}
    }.onDisappear{player?.stop()}
      .onReceive(Timer.publish(every:0.1,on:.main,in:.common).autoconnect()){now in if now>=value.retainUntil{player?.stop()}}
  }
}
