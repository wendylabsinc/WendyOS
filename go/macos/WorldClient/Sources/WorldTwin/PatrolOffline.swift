import SwiftUI
import AppKit

struct PatrolOfflineBinding:Decodable {
    let kit_id:String;let run_id:String;let package_sha256:String;let runtime_sha256:String;let model_sha256:String;let preparation_boot_id:String
    var valid:Bool {kit_id.range(of:"^kit-[a-f0-9]{24}$",options:.regularExpression) != nil && run_id=="patrol-"+kit_id && !preparation_boot_id.isEmpty && [package_sha256,runtime_sha256,model_sha256].allSatisfy {$0.range(of:"^[a-f0-9]{64}$",options:.regularExpression) != nil}}
}
struct PatrolOfflineEntry:Decodable,Identifiable {
    struct Profile:Decodable {
        struct Frame:Decodable {let id:String;let revision:String}
        let id:String;let name:String;let run_id:String;let package_sha256:String;let model_sha256:String
        let prepared_unix_ms:Int64;let expires_unix_ms:Int64;let frame:Frame
    }
    struct Policy:Decodable {
        let tour:PatrolTourPolicy?
        let duration_seconds:Int;let observation_policy:String;let dwell_seconds:Int;let no_progress_seconds:Int;let defer_seconds:Int
        let world_watch:PatrolWorldPolicy?;let environment:PatrolEnvironmentPolicy?;let rooms:[String];let revisit_seconds:[String:Int]?;let work:PatrolWorkPolicy?;let `return`:PatrolScheduleReviewInput.ReturnPolicy?;let exclusions:PatrolRestrictionPolicy?
    }
    let profile:Profile;let kit_sha256:String;let state:String;let reason:String?;let bytes:Int64;let file_count:Int;let policy:Policy?
    var id:String {profile.id}
    func label(at:Date)->String {
        if state=="ready" && profile.expires_unix_ms<=Int64(at.timeIntervalSince1970*1000){return "Expired"}
        return activatable(at:at) ? "Verified · available once":state.capitalized
    }
    func activatable(at:Date)->Bool {state=="ready" && profile.expires_unix_ms>Int64(at.timeIntervalSince1970*1000) && profile.prepared_unix_ms<=Int64(at.timeIntervalSince1970*1000) && profile.run_id=="patrol-"+id && id.range(of:"^kit-[a-f0-9]{24}$",options:.regularExpression) != nil && kit_sha256.count==64 && policy != nil}
    static func load(_ url:URL)throws->[Self] {
        let raw=try Data(contentsOf:url);guard raw.count<=512*1024 else {throw DemoError.invalidFile}
        let values=try JSONDecoder().decode([Self].self,from:raw)
        guard values.count<=32,Set(values.map(\.id)).count==values.count, values.allSatisfy({entry in entry.profile.name.count<=120 && entry.id.count<=128 && entry.profile.frame.id.count<=256 && entry.profile.frame.revision.count<=256 && (entry.policy?.tour?.valid ?? true) && (entry.policy?.return.map {$0.position.count==3 && $0.position.allSatisfy(\.isFinite) && $0.yaw.isFinite} ?? true) && (entry.policy?.world_watch?.valid ?? true) && (entry.policy?.environment?.valid ?? true) && (entry.policy?.work?.valid ?? true) && (entry.policy?.exclusions?.valid ?? true) && (entry.policy.map {p in !p.rooms.isEmpty && p.rooms.count<=32 && Set(p.rooms).count==p.rooms.count && p.rooms.allSatisfy {$0.count<=128} && p.duration_seconds>0 && p.duration_seconds<=86400 && p.dwell_seconds==2 && p.no_progress_seconds==15 && p.defer_seconds==30 && p.observation_policy=="observe-and-report" && (p.revisit_seconds.map {Set($0.keys)==Set(p.rooms) && $0.values.allSatisfy {(10...86400).contains($0)}} ?? true)} ?? true)}) else {throw DemoError.invalidFile};return values
    }
    func selection(at:Date,reviewed:Bool)throws->Data {
        guard reviewed,activatable(at:at) else {throw DemoError.invalidFile}
        return try JSONSerialization.data(withJSONObject:["action":"activate","id":id],options:[.sortedKeys])
    }
}

@MainActor final class PatrolOfflineModel:ObservableObject {
    let inputURL:URL;let outputURL:URL
    @Published var entries:[PatrolOfflineEntry]=[]
    @Published var selected:String?
    @Published var reviewed=false
    @Published var error:String?
    @Published var now=Date()
    var selection:PatrolOfflineEntry? {entries.first {$0.id==selected}}
    init(inputURL:URL,outputURL:URL) {
        self.inputURL=inputURL;self.outputURL=outputURL
        do {entries=try PatrolOfflineEntry.load(inputURL)} catch {self.error="Cannot inspect this catalog: \(error)"}
    }
    func select(_ id:String) {selected=id;reviewed=false}
    private func write(_ raw:Data)throws {guard !FileManager.default.fileExists(atPath:outputURL.path) else {throw DemoError.invalidFile};try raw.write(to:outputURL,options:.withoutOverwriting)}
    func activate()->Bool {do {guard let entry=selection else {throw DemoError.invalidFile};try write(entry.selection(at:Date(),reviewed:reviewed));return true} catch {self.error="Activation was not selected: \(error)";return false}}
    func prepare()->Bool {do {try write(Data("{\"action\":\"prepare\"}".utf8));return true} catch {self.error="Preparation was not selected: \(error)";return false}}
}

@MainActor struct PatrolOfflineChooser:View {
    var onClose: () -> Void = {}
    @StateObject private var model:PatrolOfflineModel
    init(inputURL:URL,outputURL:URL,onClose:@escaping () -> Void = {}) {self.onClose=onClose;_model=StateObject(wrappedValue:PatrolOfflineModel(inputURL:inputURL,outputURL:outputURL))}
    init(model:PatrolOfflineModel) {_model=StateObject(wrappedValue:model)}
    var body:some View {
        VStack(alignment:.leading,spacing:20) {
            HStack {
                Image(systemName:"shippingbox.fill").font(.system(size:30)).foregroundStyle(.teal)
                VStack(alignment:.leading,spacing:3) {Text("Prepared patrols").font(.system(size:30,weight:.semibold));Text("SIMULATION · LOCAL OFFLINE KIT CATALOG").font(.caption).foregroundStyle(.secondary)}
                Spacer();Button("Prepare another kit") {if model.prepare(){onClose()}}.buttonStyle(.bordered)
            }
            Text("Choose a retained approval. Activation uses the installed package and original signed policy; no asset service, signer or renewal is contacted.").font(.callout).foregroundStyle(.secondary)
            HStack(alignment:.top,spacing:24) {
                ScrollView {
                    VStack(spacing:10) {
                        if model.entries.isEmpty {Text("No prepared kits yet. Prepare one while the local inputs and approval are available.").padding(20)}
                        ForEach(model.entries) {entry in
                            Button {model.select(entry.id)} label: {
                                VStack(alignment:.leading,spacing:8) {
                                    HStack {Text(entry.profile.name.isEmpty ? entry.id:entry.profile.name).font(.headline);Spacer();Image(systemName:entry.activatable(at:model.now) ? "checkmark.seal":"exclamationmark.circle")}
                                    Text(entry.label(at:model.now)).font(.caption).foregroundStyle(entry.activatable(at:model.now) ? Color.teal:.orange)
                                    Text(entry.profile.expires_unix_ms>0 ? "Expires "+date(entry.profile.expires_unix_ms):"Unavailable approval").font(.caption).foregroundStyle(.secondary)
                                    Text(entry.reason ?? String(entry.file_count)+" retained files · "+ByteCountFormatter.string(fromByteCount:entry.bytes,countStyle:.file)).font(.caption).foregroundStyle(.secondary)
                                }.padding(15).frame(maxWidth:.infinity,alignment:.leading).background(model.selected==entry.id ? Color.teal.opacity(0.12):Color.secondary.opacity(0.05),in:RoundedRectangle(cornerRadius:12))
                            }.buttonStyle(.plain)
                        }
                    }
                }.frame(width:350)
                Divider()
                ScrollView {
                VStack(alignment:.leading,spacing:16) {
                    if let entry=model.selection {
                        Text(entry.profile.name).font(.title2.bold())
                        Label(entry.activatable(at:model.now) ? "Ready for explicit activation":"Activation unavailable",systemImage:entry.activatable(at:model.now) ? "checkmark.shield":"lock.fill").foregroundStyle(entry.activatable(at:model.now) ? Color.teal:.orange)
                        detail("Original approval",date(entry.profile.prepared_unix_ms)+" → "+date(entry.profile.expires_unix_ms))
                        detail("Coordinate frame",entry.profile.frame.id+" @ "+entry.profile.frame.revision)
                        detail("Complete kit",entry.kit_sha256)
                        detail("World package",entry.profile.package_sha256)
                        detail("Composed physics model",entry.profile.model_sha256)
                        if let policy=entry.policy {
                            Text("Original signed patrol settings").font(.headline)
                            if let tour=policy.tour {Text("Finite inspection tour · once in this order, then return").font(.headline);ForEach(Array(tour.stops.enumerated()),id:\.element.room_id){i,stop in Text(String(format:"%d. %@ · world X %.2f Z %.2f · yaw %.2f",i+1,stop.room_id,stop.position[0],stop.position[2],stop.yaw)).font(.caption)}}
                            Text("Lifetime \(policy.duration_seconds) s from preparation · \(policy.observation_policy)").font(.callout)
                            Text("Fresh observation dwell \(policy.dwell_seconds) s · No progress hold \(policy.no_progress_seconds) s · Blocked-room defer \(policy.defer_seconds) s").font(.caption).foregroundStyle(.secondary)
                            VStack(alignment:.leading,spacing:6) {
                                Text("Room revisit schedule").font(.subheadline.bold())
                                ForEach(policy.rooms,id:\.self) {room in
                                    HStack {Text(room);Spacer();Text(policy.revisit_seconds?[room].map {"Every \($0) s"} ?? "Continuous revisit")}
                                }
                            }.font(.callout)
                            if let point=policy.return {
                                Text(String(format:"Rendezvous: map X %.2f · Y %.2f m · yaw %.3f rad",point.position[0],-point.position[2],point.yaw)).font(.callout)
                                Text(point.before_expiry_seconds==0 ? "Timed return disabled. Manual recall and a signed work reserve can still trigger return.":"Timed return begins \(point.before_expiry_seconds) s before the original expiry.").font(.caption).foregroundStyle(.secondary)
                            } else {Text("No return rendezvous approved").font(.callout)}
                            if let watch=policy.world_watch {PatrolWorldDisclosure(policy:watch);PatrolWorldMap(policy:watch)}
                            if let environment=policy.environment {
                                Text("Local environmental watch · gas-1 / synthetic-test-gas").font(.subheadline.bold())
                                Text(String(format:"Concentration + declared uncertainty: HOLD ≥ %.1f ppm; clear ≤ %.1f ppm for %.1f original seconds, then explicit Resume.",environment.hold_ppm,environment.clear_ppm,Double(environment.clear_for_ms)/1000)).font(.callout)
                                Text("Applies to departure, patrol and return. Unknown/stale data hold; 500 ms maximum original gap, 1 s age. Calibration go2-virtual-mounts-v1. Synthetic screening only; no physical safety claim.").font(.caption).foregroundStyle(.secondary)
                            } else {Text("No local environmental watch selected").font(.callout)}
                            if let work=policy.work {Text(String(format:"Mechanical work %.1f kJ · return reserve %.1f kJ",work.limit_j/1000,work.return_reserve_j/1000)).font(.callout)} else {Text("No mechanical-work allowance selected").font(.callout)}
                            if let exclusions=policy.exclusions {
                                Text("Restricted areas · revision \(exclusions.revision)").font(.subheadline.bold())
                                Text("Navigation X/Y metres; frame "+exclusions.frame.id+" @ "+exclusions.frame.revision).font(.caption).foregroundStyle(.secondary)
                                ForEach(exclusions.rectangles) {r in
                                    Text(String(format:"%@   X %.3f…%.3f · Y %.3f…%.3f m",r.id,Double(r.min_mm[0])/1000,Double(r.max_mm[0])/1000,Double(r.min_mm[1])/1000,Double(r.max_mm[1])/1000)).font(.system(size:11,design:.monospaced))
                                }
                                detail("Restriction digest",exclusions.sha256)
                            } else {Text("No restricted areas selected").font(.callout)}
                        }
                        Text("The original deadline continues while stored. A start consumes this kit even if launch fails. A reset or restart needs new approval. Closing the viewer leaves the local mission running.").font(.callout).foregroundStyle(.secondary).fixedSize(horizontal:false,vertical:true)
                        Text("Cached simulation authority only. Remote revocations issued after preparation are not known offline. This is not physical robot authorization.").font(.caption).foregroundStyle(.orange).fixedSize(horizontal:false,vertical:true)
                        Toggle("I reviewed this original policy and want to activate it now",isOn:$model.reviewed).disabled(!entry.activatable(at:model.now))
                    } else {Text("Select a prepared kit to inspect its scope, deadline and retained content.").foregroundStyle(.secondary)}
                    Spacer()
                }.frame(maxWidth:.infinity,alignment:.leading)
                }
            }
            if let error=model.error {Text(error).foregroundStyle(.red)}
            HStack {Button("Close") {onClose()};Spacer();Button("Activate offline patrol") {if model.activate(){onClose()}}.buttonStyle(.borderedProminent).tint(.teal).disabled(!model.reviewed || model.selection?.activatable(at:model.now) != true)}
        }.padding(30).frame(width:1120,height:790).background(Color.white).preferredColorScheme(.light)
        .onReceive(Timer.publish(every:1,on:.main,in:.common).autoconnect()) {model.now=$0}
    }
    private func date(_ ms:Int64)->String {Date(timeIntervalSince1970:Double(ms)/1000).formatted(date:.abbreviated,time:.standard)}
    private func detail(_ title:String,_ value:String)->some View {VStack(alignment:.leading,spacing:4) {Text(title).font(.caption).foregroundStyle(.secondary);Text(value).font(.system(size:11,design:.monospaced)).textSelection(.enabled).lineLimit(2)}}
}
