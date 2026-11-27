import SwiftUI
import AppKit
import WorldClient

struct PatrolScheduleReviewInput: Decodable {
    struct Room: Decodable, Identifiable { let id: String; let name: String }
    struct RendezvousOption: Decodable, Identifiable {
        let id:String;let name:String;let x:Double;let y:Double;let yaw:Double
        var valid:Bool {!id.isEmpty && name.count<=120 && x.isFinite && y.isFinite && yaw.isFinite && abs(x)<=9 && abs(y)<=7 && abs(yaw)<=Double.pi}
    }
    struct ReturnPolicy: Codable {let position:[Double];let yaw:Double;let before_expiry_seconds:Int}
    let offline_prepare:Bool?
    let world_watch:PatrolWorldPolicy?
    let environment:PatrolEnvironmentPolicy?
    let work:PatrolWorkPolicy?
    let navigation_url:String?
    let policy_context:PatrolPolicyContext?
    let exclusions:PatrolRestrictionPolicy?
    let rendezvous_options:[RendezvousOption]?
    let duration_seconds:Int?
    let `return`:ReturnPolicy?
    let rooms: [Room]
    let revisit_seconds: [String:Int]?
    var valid: Bool {
        (world_watch?.valid ?? true) && (environment?.valid ?? true) && (work?.valid ?? true) && (work == nil || work!.return_reserve_j == 0 || self.return != nil) && (policy_context?.valid ?? true) && (exclusions?.valid ?? true) &&
        (rendezvous_options.map {!$0.isEmpty && $0.count<=33 && Set($0.map(\.id)).count==$0.count && $0.allSatisfy(\.valid)} ?? true) &&
        (duration_seconds.map {(1...86400).contains($0)} ?? true) &&
        !rooms.isEmpty && rooms.count <= 32 && Set(rooms.map(\.id)).count == rooms.count &&
        rooms.allSatisfy { !$0.id.isEmpty && $0.id.count <= 128 && !$0.name.isEmpty && $0.name.count <= 256 } &&
        (revisit_seconds == nil || (Set(revisit_seconds!.keys) == Set(rooms.map(\.id)) && revisit_seconds!.values.allSatisfy { (10...86400).contains($0) }))
    }
    func returnPolicy(id:String,before:String) throws -> ReturnPolicy {
        guard let point=rendezvous_options?.first(where:{$0.id==id}),point.valid,
              let seconds=Int(before),seconds>=0,seconds<(duration_seconds ?? 3600) else {throw DemoError.invalidFile}
        return ReturnPolicy(position:[point.x,0.3,-point.y],yaw:point.yaw,before_expiry_seconds:seconds)
    }
    static func load(_ url: URL) throws -> Self {
        let raw = try Data(contentsOf:url)
        guard raw.count <= 512*1024 else { throw DemoError.invalidFile }
        let value = try JSONDecoder().decode(Self.self, from:raw)
        guard value.valid else { throw DemoError.invalidFile }
        return value
    }
    func intervals(_ edits: [String:String]) throws -> [String:Int] {
        guard Set(edits.keys) == Set(rooms.map(\.id)) else { throw DemoError.invalidFile }
        var result: [String:Int] = [:]
        for room in rooms {
            guard let value = edits[room.id], let seconds = Int(value), (10...86400).contains(seconds) else { throw DemoError.invalidFile }
            result[room.id] = seconds
        }
        return result
    }
}

/// The launcher invokes this before creating the signed simulation mission.
/// Saving only writes an operator configuration; this view has no motion transport.
struct PatrolScheduleReview: View {
    @State private var actions = TwinActions()

    var onClose: () -> Void = {}
    let inputURL: URL
    let outputURL: URL
    @State private var input: PatrolScheduleReviewInput?
    @State private var edits: [String:String] = [:]
    @State private var error: String?
    @State private var reviewed = false
    @State private var returnEnabled=false
    @State private var returnID="start"
    @State private var returnBefore="0"
    @State private var worldEnabled=false
    @State private var worldPolicy:PatrolWorldPolicy?
    @State private var environmentEnabled=false
    @State private var environmentHold="80"
    @State private var environmentClear="65"
    @State private var environmentSeconds="2"
    @State private var workEnabled=false
    @State private var workLimit="6"
    @State private var workReserve="4"
    @State private var rectangles:[PatrolRestrictionPolicy.Rectangle]=[]
    @State private var preview:PatrolRestrictionPreview?
    @State private var previewTarget=""
    @State private var previewBusy=false
    @State private var previewError:String?
    var body: some View {
        Group {

        HStack(alignment:.top,spacing:24) {
        VStack(alignment:.leading, spacing:18) {
            Text("Plan a patrol and return").font(.system(size:30,weight:.semibold))
            Text("SIMULATION · PRE-FLIGHT REVIEW").font(.caption).foregroundStyle(.secondary)
            Text("Choose how often each room needs fresh evidence. The first survey is due at activation; departure requires the signed local conditions. Later visits become due after their interval; travel and blocked routes can make coverage overdue.")
                .fixedSize(horizontal:false,vertical:true)
            if let input {
                ScrollView {
                    VStack(alignment:.leading,spacing:12) {
                        ForEach(input.rooms) { room in
                            HStack {
                                VStack(alignment:.leading) { Text(room.name).font(.headline);Text(room.id).font(.caption).foregroundStyle(.secondary) }
                                Spacer()
                                TextField("Seconds",text:Binding(get:{edits[room.id] ?? "300"},set:{edits[room.id]=$0;reviewed=false}))
                                    .textFieldStyle(.roundedBorder).frame(width:100)
                                    .accessibilityIdentifier("patrol-interval-"+room.id)
                                Text("seconds").foregroundStyle(.secondary).frame(width:70,alignment:.leading)
                            }.padding(12).background(Color.secondary.opacity(0.06),in:RoundedRectangle(cornerRadius:10))
                        }
                if let options=input.rendezvous_options {
                    VStack(alignment:.leading,spacing:10) {
                        Toggle("Enable return to an approved rendezvous",isOn:$returnEnabled)
                        if returnEnabled {
                            Picker("Rendezvous",selection:$returnID) {ForEach(options) {option in Text(option.name).tag(option.id)}}
                            if let point=options.first(where:{$0.id==returnID}) {
                                Text(String(format:"Navigation map X %.2f · Y %.2f m",point.x,point.y)).font(.caption).foregroundStyle(.secondary)
                            }
                            HStack {Text("Begin return before expiry");TextField("Seconds",text:$returnBefore).textFieldStyle(.roundedBorder).frame(width:90);Text("seconds")}
                            Text("Use 0 to disable timed return; manual recall and the signed work reserve can still trigger return. Otherwise choose less than the mission's \(input.duration_seconds ?? 3600) seconds. A paused mission waits for Resume. Return cannot extend the deadline; a blocked route holds for operator retry.").font(.caption).foregroundStyle(.secondary).fixedSize(horizontal:false,vertical:true)
                        }
                    }.padding(12).background(Color.green.opacity(0.06),in:RoundedRectangle(cornerRadius:10))
                    .onChange(of:returnEnabled) {_,_ in reviewed=false;preview=nil}
                    .onChange(of:returnID) {_,_ in reviewed=false;preview=nil}
                    .onChange(of:returnBefore) {_,_ in reviewed=false}
                }
                VStack(alignment:.leading,spacing:8) {
                    Toggle("Limit simulated actuator work",isOn:$workEnabled)
                    if workEnabled {
                        HStack {Text("Allowance");TextField("kJ",text:$workLimit).textFieldStyle(.roundedBorder).frame(width:80);Text("kJ");Spacer();Text("Return reserve");TextField("kJ",text:$workReserve).textFieldStyle(.roundedBorder).frame(width:80);Text("kJ")}
                        Text("Positive joint torque × velocity, integrated at each original physics step. A reserve above zero requires an approved rendezvous. Return starts at the reserve; exhaustion or an unknown meter holds navigation.").font(.caption).foregroundStyle(.secondary).fixedSize(horizontal:false,vertical:true)
                        Text("Simulation mechanical work only. This is not battery charge or a guarantee of reaching the rendezvous. Standing still continues to use work.").font(.caption).foregroundStyle(.orange).fixedSize(horizontal:false,vertical:true)
                    }
                }.padding(12).background(Color.orange.opacity(0.06),in:RoundedRectangle(cornerRadius:10))
                .onChange(of:workEnabled) {_,_ in reviewed=false}
                .onChange(of:workLimit) {_,_ in reviewed=false}
                .onChange(of:workReserve) {_,_ in reviewed=false}
                Text("Intervals must be 10–86,400 seconds. The mission deadline, permitted rooms, speed, sensors and stop behavior remain unchanged. Pausing does not postpone coverage deadlines.")
                    .font(.callout).foregroundStyle(.secondary)
                if let policy=worldPolicy {
                    GroupBox("Shared World evidence") {VStack(alignment:.leading,spacing:10){
                        Toggle("Require this signed World evidence condition",isOn:$worldEnabled).onChange(of:worldEnabled){_,_ in reviewed=false}
                        if worldEnabled {
                            PatrolWorldDisclosure(policy:policy)
                            PatrolWorldMap(policy:policy,draw:{low,high in worldPolicy?.min_xz=low;worldPolicy?.max_xz=high;reviewed=false})
                            Text("Drag to replace the watch rectangle. X increases right; World Z increases down. Teal dots are commissioned sensors.").font(.caption).foregroundStyle(.secondary)
                        }
                    }}
                }
                GroupBox("Local environmental watch") {
                    VStack(alignment:.leading,spacing:10) {
                        Toggle("Hold on the signed local gas condition",isOn:$environmentEnabled)
                        if environmentEnabled {
                            HStack {Text("Hold at");TextField("ppm",text:$environmentHold).textFieldStyle(.roundedBorder).frame(width:75);Text("ppm");Spacer();Text("Clear at/below");TextField("ppm",text:$environmentClear).textFieldStyle(.roundedBorder).frame(width:75);Text("ppm")}
                            HStack {Text("Continuous original clear samples");TextField("seconds",text:$environmentSeconds).textFieldStyle(.roundedBorder).frame(width:75);Text("seconds, then explicit Resume")}
                            Text("Gas-1 · synthetic-test-gas · ppm · go2-virtual-mounts-v1").font(.caption.monospaced())
                            Text("Screening value = local concentration + declared uncertainty. High, unknown or stale readings hold both patrol and return. A complete clear interval only enables Resume; it never resumes automatically. Initial departure also waits for this interval.").font(.caption).foregroundStyle(.secondary).fixedSize(horizontal:false,vertical:true)
                            Text("Original 100 ms simulation samples; maximum 500 ms gap and 1 s age. This is a synthetic test policy, not toxicology or a physical safety guarantee.").font(.caption).foregroundStyle(.orange).fixedSize(horizontal:false,vertical:true)
                        }
                    }.padding(5)
                }
                .onChange(of:environmentEnabled) {_,_ in reviewed=false}
                .onChange(of:environmentHold) {_,_ in reviewed=false}
                .onChange(of:environmentClear) {_,_ in reviewed=false}
                .onChange(of:environmentSeconds) {_,_ in reviewed=false}
                    }
                }
                Toggle("I reviewed the schedule, return, work allowance, local environment and World evidence policy",isOn:$reviewed)
                Text(input.offline_prepare == true ? "Prepare retains one signed simulation approval and complete local runtime inputs. Its expiry starts now. Nothing moves during preparation; activation later requires a separate explicit choice. No physical robot is connected.":"Continue creates a new signed simulation mission using these settings. These signed settings cannot be edited during that mission. No physical robot is connected.")
                    .font(.caption).foregroundStyle(.secondary)
            }
            if let error { Text(error).foregroundStyle(.red).accessibilityIdentifier("patrol-schedule-error") }
            HStack {
                Button("Cancel") { onClose() }.keyboardShortcut(.cancelAction)
                Spacer()
                Button(input?.offline_prepare == true ? "Prepare approved offline kit":"Use schedule and start simulation") { save() }
                    .buttonStyle(.borderedProminent).disabled(!reviewed || input == nil || previewBusy || (!rectangles.isEmpty && preview == nil))
                    .accessibilityIdentifier("patrol-schedule-accept")
            }
        }.frame(width:580,height:820)
        if let input, let context=input.policy_context, let url=input.navigation_url {
            VStack(alignment:.leading,spacing:14) {
                Text("Restricted areas").font(.title2.bold())
                Text("Drag a rectangle on the map. These are operator restrictions, not sensed obstacles. Preview compares routes before the mission is signed.").font(.callout).foregroundStyle(.secondary)
                PatrolRestrictionMap(context:context,rectangles:$rectangles,preview:preview,start:input.rendezvous_options?.first)
                    .frame(width:520,height:410)
                    .onChange(of:rectangles) {_,_ in preview=nil;reviewed=false}
                HStack {
                    Text("\(rectangles.count) / 16 areas")
                    Spacer()
                    Button("Undo area") {if !rectangles.isEmpty {rectangles.removeLast()}}.disabled(rectangles.isEmpty || previewBusy)
                    Button("Clear") {rectangles=[]}.disabled(rectangles.isEmpty || previewBusy)
                }
                if let options=input.rendezvous_options {
                    Picker("Preview destination",selection:$previewTarget) {ForEach(options) {Text($0.name).tag($0.id)}}
                        .onChange(of:previewTarget) {_,_ in preview=nil;reviewed=false}
                    Button(previewBusy ? "Planning both routes…":"Compare routes") {actions.submit { [self] in await compare(url:url,context:context,input:input)}}
                        .disabled(rectangles.isEmpty || previewBusy)
                }
                if let preview {
                    Text(preview.restricted.reachable ? String(format:"Restricted route %.1f m · Baseline %.1f m",preview.restricted.length ?? 0,preview.baseline.length ?? 0):"No route with this policy: \(preview.restricted.reason ?? "blocked")")
                        .font(.headline).foregroundStyle(preview.restricted.reachable ? Color.green:Color.orange)
                    Text("Blue: baseline · Green: outbound · Purple: return · Red: no-go area").font(.caption)
                    Text(preview.return_route?.reachable == true ? "Return route is available in this preview.":"Return route is unavailable in this preview; return would hold.").font(.caption).foregroundStyle(.secondary)
                    Text("Policy preview is current. Unreachable destinations remain held; drawing an area never authorizes movement.").font(.caption).foregroundStyle(.secondary)
                }
                if let previewError {Text(previewError).foregroundStyle(.red).font(.callout)}
                Text(String(format:"Go2 heading-independent clearance %.2f m, derived from its padded footprint plus 0.20 m simulation tracking allowance. Frame %@ @ %@.",context.clearance_m,context.frame.id,context.frame.revision)).font(.caption).foregroundStyle(.secondary)
                Spacer()
            }.frame(width:520,height:820)
        }
        }.padding(28)
        .onAppear {
            do {
                let loaded = try PatrolScheduleReviewInput.load(inputURL)
                input=loaded
                rectangles=loaded.exclusions?.rectangles ?? []
                previewTarget=loaded.rendezvous_options?.dropFirst().first?.id ?? "start"
                returnID=loaded.rendezvous_options?.first?.id ?? "start"
                if let policy=loaded.return {
                    guard policy.position.count==3 else {throw DemoError.invalidFile}
                    returnEnabled=true;returnBefore=String(policy.before_expiry_seconds)
                    if let match=loaded.rendezvous_options?.first(where:{abs($0.x-policy.position[0])<0.001 && abs($0.y+policy.position[2])<0.001 && abs($0.yaw-policy.yaw)<0.001}) {
                        returnID=match.id
                    } else {
                        returnID="";self.error="The saved rendezvous differs from this run's available points. Choose a rendezvous again before approving."
                    }
                }
                worldPolicy=loaded.world_watch;worldEnabled=loaded.world_watch != nil
                if let policy=loaded.environment {environmentEnabled=true;environmentHold=String(policy.hold_ppm);environmentClear=String(policy.clear_ppm);environmentSeconds=String(Double(policy.clear_for_ms)/1000)}
                if let work=loaded.work {workEnabled=true;workLimit=String(work.limit_j/1000);workReserve=String(work.return_reserve_j/1000)}
                edits=Dictionary(uniqueKeysWithValues:loaded.rooms.map { ($0.id,String(loaded.revisit_seconds?[$0.id] ?? 300)) })
            } catch { self.error="Cannot load room configuration. Close this review and check the launcher input." }
        }

        }.task { await actions.run() }
}
    @MainActor private func compare(url:String,context:PatrolPolicyContext,input:PatrolScheduleReviewInput) async {
        previewBusy=true;preview=nil;previewError=nil;reviewed=false
        let requestedTarget=previewTarget,requestedReturn=returnID,requestedReturnEnabled=returnEnabled
        defer {previewBusy=false}
        do {
            guard let origin=URL(string:url),origin.scheme=="http",origin.host=="127.0.0.1",let target=input.rendezvous_options?.first(where:{$0.id==previewTarget}) else {throw DemoError.invalidFile}
            let policy=PatrolRestrictionPolicy(frame:context.frame,revision:1,rectangles:rectangles,sha256:"")
            var request=URLRequest(url:origin.appendingPathComponent("policy/preview"));request.httpMethod="POST";request.timeoutInterval=45
            request.setValue("application/json",forHTTPHeaderField:"Content-Type")
            let returnPoint=input.rendezvous_options?.first(where:{$0.id==(returnEnabled ? returnID:"start")})
            request.httpBody=try JSONSerialization.data(withJSONObject:["policy":JSONSerialization.jsonObject(with:JSONEncoder().encode(policy)),"target":["x":target.x,"y":target.y,"yaw":target.yaw],"return_target":["x":returnPoint?.x ?? target.x,"y":returnPoint?.y ?? target.y,"yaw":returnPoint?.yaw ?? target.yaw]])
            let (data,response)=try await URLSession.shared.data(for:request)
            guard let http=response as? HTTPURLResponse,http.statusCode==200 else {
                let error=(try? JSONSerialization.jsonObject(with:data)) as? [String:Any]
                previewError=error?["error"] as? String ?? "Preview unavailable; check localization and route readiness.";return
            }
            let result=try JSONDecoder().decode(PatrolRestrictionPreview.self,from:data)
            guard result.policy.rectangles==rectangles,result.policy.frame==context.frame,result.policy.sha256.count==64,requestedTarget==previewTarget,requestedReturn==returnID,requestedReturnEnabled==returnEnabled else {throw DemoError.invalidFile}
            preview=result
        } catch {previewError="Preview failed: \(error.localizedDescription)"}
    }
    private func save() {
        do {
            guard reviewed, let input else { return }
            let intervals = try input.intervals(edits)
            guard rectangles.isEmpty || preview?.policy.rectangles == rectangles else {throw DemoError.invalidFile}
            var document:[String:Any] = ["revisit_seconds":intervals]
            if let preview, !rectangles.isEmpty {document["exclusions"]=try JSONSerialization.jsonObject(with:JSONEncoder().encode(preview.policy))}
            if returnEnabled {
                let policy=try input.returnPolicy(id:returnID,before:returnBefore)
                document["return"]=try JSONSerialization.jsonObject(with:JSONEncoder().encode(policy))
            }
            if worldEnabled {guard let policy=worldPolicy else {throw DemoError.invalidFile};document["world_watch"]=try policy.document()}
            if environmentEnabled {
                let policy=try PatrolEnvironmentPolicy.parse(hold:environmentHold,clear:environmentClear,seconds:environmentSeconds)
                document["environment"]=try JSONSerialization.jsonObject(with:JSONEncoder().encode(policy))
            }
            if workEnabled {
                let policy=try PatrolWorkPolicy.parse(limit:workLimit,reserve:workReserve,hasReturn:returnEnabled)
                document["work"]=try JSONSerialization.jsonObject(with:JSONEncoder().encode(policy))
            }
            let raw = try JSONSerialization.data(withJSONObject:document,options:[.prettyPrinted,.sortedKeys])
            guard !FileManager.default.fileExists(atPath:outputURL.path) else { throw DemoError.invalidFile }
            try raw.write(to:outputURL,options:.atomic)
            try FileManager.default.setAttributes([.posixPermissions:0o600],ofItemAtPath:outputURL.path)
            onClose()
        } catch { self.error="Check room intervals, return, work and environmental thresholds. Clear must be below Hold, with 1–30 seconds of original samples. Reserve must be smaller than the allowance and needs an approved return. Use a fresh output file." }
    }
}

struct PatrolRestrictionPolicy:Codable,Equatable {
    struct Frame:Codable,Equatable {let id:String;let revision:String}
    struct Rectangle:Codable,Equatable,Identifiable {let id:String;let min_mm:[Int];let max_mm:[Int]}
    let frame:Frame;let revision:Int;let rectangles:[Rectangle];let sha256:String
    var valid:Bool { !frame.id.isEmpty && !frame.revision.isEmpty && revision==1 && !rectangles.isEmpty && rectangles.count<=16 && Set(rectangles.map(\.id)).count==rectangles.count && sha256.count==64 && sha256.allSatisfy {"0123456789abcdef".contains($0)} && rectangles.allSatisfy {!$0.id.isEmpty && $0.min_mm.count==2 && $0.max_mm.count==2 && $0.min_mm[0] >= -9000 && $0.max_mm[0]<=9000 && $0.min_mm[1] >= -7000 && $0.max_mm[1]<=7000 && $0.min_mm[0]<$0.max_mm[0] && $0.min_mm[1]<$0.max_mm[1]} }
}
struct PatrolPolicyContext:Decodable {
    struct Grid:Decodable {let resolution:Double;let width:Int;let height:Int;let origin:[Double];let data:[Int]}
    let frame:PatrolRestrictionPolicy.Frame;let grid:Grid;let clearance_m:Double
    var valid:Bool { !frame.id.isEmpty && !frame.revision.isEmpty && grid.width>0 && grid.width<=1000 && grid.height>0 && grid.height<=1000 && grid.data.count==grid.width*grid.height && grid.origin.count==3 && grid.origin.allSatisfy(\.isFinite) && grid.resolution.isFinite && grid.resolution>0 && grid.resolution<=1 && clearance_m.isFinite && clearance_m>0 && clearance_m<2 }
}
struct PatrolRestrictionPreview:Decodable {
    struct Route:Decodable {let reachable:Bool;let route:[[Double]];let length:Double?;let reason:String?}
    let policy:PatrolRestrictionPolicy;let baseline:Route;let restricted:Route;let return_route:Route?
}
struct PatrolRestrictionMap:View {
    let context:PatrolPolicyContext
    @Binding var rectangles:[PatrolRestrictionPolicy.Rectangle]
    let preview:PatrolRestrictionPreview?
    let start:PatrolScheduleReviewInput.RendezvousOption?
    @State private var dragStart:CGPoint?
    @State private var dragEnd:CGPoint?
    var body:some View {
        GeometryReader {geometry in
            Canvas {canvas,size in
                let g=context.grid
                let scale=min(size.width/(Double(g.width)*g.resolution),size.height/(Double(g.height)*g.resolution))
                func point(_ x:Double,_ y:Double)->CGPoint {CGPoint(x:(x-g.origin[0])*scale,y:size.height-(y-g.origin[1])*scale)}
                canvas.fill(Path(CGRect(origin:.zero,size:size)),with:.color(Color(red:0.08,green:0.11,blue:0.14)))
                for y in 0..<g.height {for x in 0..<g.width where y*g.width+x<g.data.count && g.data[y*g.width+x]>=65 {
                    let p=point(g.origin[0]+Double(x)*g.resolution,g.origin[1]+Double(y+1)*g.resolution)
                    canvas.fill(Path(CGRect(x:p.x,y:p.y,width:g.resolution*scale+1,height:g.resolution*scale+1)),with:.color(.gray.opacity(0.7)))
                }}
                for r in rectangles where r.min_mm.count==2 && r.max_mm.count==2 {
                    let a=point(Double(r.min_mm[0])/1000,Double(r.max_mm[1])/1000),b=point(Double(r.max_mm[0])/1000,Double(r.min_mm[1])/1000)
                    let rect=Path(CGRect(x:a.x,y:a.y,width:b.x-a.x,height:b.y-a.y))
                    canvas.fill(rect,with:.color(.red.opacity(0.35)));canvas.stroke(rect,with:.color(.red),lineWidth:2)
                }
                for (route,color) in [(preview?.baseline.route ?? [],Color.blue),(preview?.restricted.route ?? [],Color.green),(preview?.return_route?.route ?? [],Color.purple)] {
                    var path=Path()
                    for (i,p) in route.enumerated() where p.count>=2 {if i==0 {path.move(to:point(p[0],p[1]))} else {path.addLine(to:point(p[0],p[1]))}}
                    canvas.stroke(path,with:.color(color),lineWidth:3)
                }
                if let start {let p=point(start.x,start.y);canvas.fill(Path(ellipseIn:CGRect(x:p.x-5,y:p.y-5,width:10,height:10)),with:.color(.white))}
                if let a=dragStart,let b=dragEnd {canvas.stroke(Path(CGRect(x:min(a.x,b.x),y:min(a.y,b.y),width:abs(a.x-b.x),height:abs(a.y-b.y))),with:.color(.orange),style:StrokeStyle(lineWidth:2,dash:[4,3]))}
            }.gesture(DragGesture(minimumDistance:5).onChanged {value in dragStart=value.startLocation;dragEnd=value.location}.onEnded {value in
                defer {dragStart=nil;dragEnd=nil}
                guard rectangles.count<16 else{return}
                let g=context.grid,scale=min(geometry.size.width/(Double(g.width)*g.resolution),geometry.size.height/(Double(g.height)*g.resolution))
                func world(_ p:CGPoint)->[Int] { [Int(((max(0,min(geometry.size.width,p.x))/scale+g.origin[0])*1000).rounded()),Int((((geometry.size.height-max(0,min(geometry.size.height,p.y)))/scale+g.origin[1])*1000).rounded())] }
                let a=world(value.startLocation),b=world(value.location)
                let low=[min(a[0],b[0]),min(a[1],b[1])],high=[max(a[0],b[0]),max(a[1],b[1])]
                guard high[0]-low[0]>=50,high[1]-low[1]>=50 else{return}
                rectangles.append(.init(id:"area-"+UUID().uuidString.lowercased(),min_mm:low,max_mm:high))
            })
        }.clipShape(RoundedRectangle(cornerRadius:12))
    }
}

struct PatrolWorkPolicy:Codable {
    let limit_j:Double;let return_reserve_j:Double
    var valid:Bool {limit_j.isFinite && limit_j>0 && limit_j<=1e9 && return_reserve_j.isFinite && return_reserve_j>=0 && return_reserve_j<limit_j}
    static func parse(limit:String,reserve:String,hasReturn:Bool) throws -> Self {
        guard let l=Double(limit),let r=Double(reserve) else {throw DemoError.invalidFile}
        let p=Self(limit_j:l*1000,return_reserve_j:r*1000)
        guard p.valid,(r==0 || hasReturn) else {throw DemoError.invalidFile}
        return p
    }
}

struct PatrolEnvironmentPolicy:Codable {
    let sensor_id:String;let calibration_revision:String;let species:String;let unit:String
    let hold_ppm:Double;let clear_ppm:Double;let clear_for_ms:Int64
    var valid:Bool {sensor_id=="gas-1" && calibration_revision=="go2-virtual-mounts-v1" && species=="synthetic-test-gas" && unit=="ppm" && hold_ppm.isFinite && clear_ppm.isFinite && clear_ppm>=0 && hold_ppm>clear_ppm && hold_ppm<=1e6 && (1000...30000).contains(clear_for_ms)}
    static func parse(hold:String,clear:String,seconds:String)throws->Self {
        guard let h=Double(hold),let c=Double(clear),let s=Double(seconds),s.isFinite,s>=1,s<=30,(s*1000).rounded()==s*1000 else {throw DemoError.invalidFile}
        let p=Self(sensor_id:"gas-1",calibration_revision:"go2-virtual-mounts-v1",species:"synthetic-test-gas",unit:"ppm",hold_ppm:h,clear_ppm:c,clear_for_ms:Int64(s*1000))
        guard p.valid else {throw DemoError.invalidFile};return p
    }
}

// The full commissioned contract survives editing. Only the operator's watch
// rectangle is changed here; Go validates the digest before signing/admission.
struct PatrolWorldPolicy:Codable {
    let method:String;let contract:JSONValue;let contract_sha256:String
    let reader_principal:String;let reader_audience:String
    var min_xz:[Double];var max_xz:[Double];let max_age_ms:Int64
    var frame:String {(contract["field"]?["frame"]?["id"]?.string ?? "unknown")+" @ "+(contract["field"]?["frame"]?["revision"]?.string ?? "unknown")}
    var run:String {contract["field"]?["simulation"]?["run_id"]?.string ?? ""}
    var valid:Bool {
        method=="synthetic-source-region-outside-watch-v1" && contract["tenant"]?.string=="simulation" && contract["field"]?["site"]?.string=="studio-01" && !run.isEmpty &&
        contract_sha256.range(of:"^[a-f0-9]{64}$",options:.regularExpression) != nil && !reader_principal.isEmpty && !reader_audience.isEmpty && (500...4000).contains(max_age_ms) &&
        min_xz.count==2 && max_xz.count==2 && (min_xz+max_xz).allSatisfy {$0.isFinite && abs($0)<=1000} && min_xz[0]<max_xz[0] && min_xz[1]<max_xz[1] && contract["gateways"]?.object?.count==2 && (contract["field"]?["sensors"]?.array?.count ?? 0)>=4
    }
    func document()throws->Any {guard valid else {throw DemoError.invalidFile};return try JSONSerialization.jsonObject(with:JSONEncoder().encode(self))}
}
struct PatrolWorldStatus:Decodable {
    let policy:PatrolWorldPolicy;let ready:Bool;let held:Bool;let permanent:Bool
    let reason:String;let transport:String;let remaining_ms:Int64;let original_unix_ms:Int64;let selection:JSONValue
    var valid:Bool {policy.valid && remaining_ms>=0 && remaining_ms<=policy.max_age_ms && (!permanent || !ready) && ["connected","unavailable","refused"].contains(transport)}
}
struct PatrolWorldMap:View {
    let policy:PatrolWorldPolicy
    var selection:JSONValue?=nil
    var draw:(([Double],[Double])->Void)?=nil
    private var low:[Double] {policy.contract["field"]?["min_xz"]?.array?.compactMap(\.double) ?? [-9,-7]}
    private var high:[Double] {policy.contract["field"]?["max_xz"]?.array?.compactMap(\.double) ?? [9,7]}
    var body:some View {
        GeometryReader {g in
            Canvas {ctx,size in
                let lo=low,hi=high
                guard lo.count==2,hi.count==2,hi[0]>lo[0],hi[1]>lo[1] else {return}
                func point(_ p:[Double])->CGPoint {CGPoint(x:(p[0]-lo[0])/(hi[0]-lo[0])*size.width,y:(p[1]-lo[1])/(hi[1]-lo[1])*size.height)}
                ctx.fill(Path(CGRect(origin:.zero,size:size)),with:.color(Color.secondary.opacity(0.08)))
                let a=point(policy.min_xz),b=point(policy.max_xz)
                let rect=CGRect(x:a.x,y:a.y,width:b.x-a.x,height:b.y-a.y)
                ctx.fill(Path(rect),with:.color(.orange.opacity(0.16)));ctx.stroke(Path(rect),with:.color(.orange),lineWidth:2)
                for sensor in policy.contract["field"]?["sensors"]?.array ?? [] {
                    if let p=sensor["position_xz"]?.array?.compactMap(\.double),p.count==2 {let q=point(p);ctx.fill(Path(ellipseIn:CGRect(x:q.x-3,y:q.y-3,width:6,height:6)),with:.color(.teal))}
                }
                let step=policy.contract["field"]?["step_m"]?.double ?? 0.2
                for cell in selection?["result"]?["estimate"]?["cells"]?.array ?? [] {
                    if let p=cell["position_xz"]?.array?.compactMap(\.double),p.count==2 {let a=point([p[0]-step/2,p[1]-step/2]),b=point([p[0]+step/2,p[1]+step/2]);ctx.fill(Path(CGRect(x:a.x,y:a.y,width:b.x-a.x,height:b.y-a.y)),with:.color(.purple.opacity(0.65)))}
                }
            }.gesture(DragGesture(minimumDistance:5).onEnded {v in
                guard let draw else {return};let lo=low,hi=high
                guard lo.count==2,hi.count==2 else{return}
                func world(_ q:CGPoint)->[Double] {[(lo[0]+min(max(q.x/g.size.width,0),1)*(hi[0]-lo[0]))*1000,(lo[1]+min(max(q.y/g.size.height,0),1)*(hi[1]-lo[1]))*1000].map {$0.rounded()/1000}}
                let a=world(v.startLocation),b=world(v.location)
                if abs(a[0]-b[0])>0.05 && abs(a[1]-b[1])>0.05 {draw([min(a[0],b[0]),min(a[1],b[1])],[max(a[0],b[0]),max(a[1],b[1])])}
            })
        }.frame(height:180).clipShape(RoundedRectangle(cornerRadius:8))
    }
}
struct PatrolWorldDisclosure:View {
    let policy:PatrolWorldPolicy
    var body:some View {VStack(alignment:.leading,spacing:5){
        Text("Source-region World watch").font(.headline)
        Text("Frame \(policy.frame) · run \(policy.run)").font(.caption)
        Text(String(format:"Watch X %.2f…%.2f · Z %.2f…%.2f m. Original evidence limit %.1f s.",policy.min_xz[0],policy.max_xz[0],policy.min_xz[1],policy.max_xz[1],Double(policy.max_age_ms)/1000)).font(.caption)
        Text("Reader \(policy.reader_principal) at \(policy.reader_audience)").font(.caption)
        Text("Contract "+policy.contract_sha256).font(.system(size:9,design:.monospaced)).textSelection(.enabled)
        DisclosureGroup("Commissioned sources and model") {Text(String(data:(try? policy.contract.encoded(pretty:true)) ?? Data(),encoding:.utf8) ?? "Unavailable").font(.system(size:9,design:.monospaced)).textSelection(.enabled)}
        Text("Missing, disputed, expired or overlapping source regions HOLD. Compatible evidence enables explicit Resume. Synthetic model condition only; outside this rectangle does not mean gas or route clearance.").font(.caption).foregroundStyle(.orange).fixedSize(horizontal:false,vertical:true)
    }}
}
