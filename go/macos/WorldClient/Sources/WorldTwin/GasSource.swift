import SwiftUI
import AppKit
import CryptoKit
import Darwin

struct GasAssumptions: Codable {
    let revision: String
    let wind_radians: Double
    let downwind_m: Double
    let upwind_m: Double
    let crosswind_m: Double
}
struct GasObservation: Codable {
    struct Version: Codable { let epoch: UInt64; let sequence: UInt64 }
    let id: String
    let version: Version
    let captured_s: Double
    let valid_until_s: Double
    let value_ppm: Double
    let uncertainty_ppm: Double
    let calibration: String
}
struct GasSensor: Codable, Equatable { let id: String; let position_xz: [Double] }
struct GasReading: Codable, Identifiable {
    var id: String { sensor.id }
    let sensor: GasSensor
    let state: String
    let observation: GasObservation?
    let predicted_ppm: Double?
    let residual_ppm: Double?
}
struct GasCell: Codable { let position_xz: [Double]; let normalized_rms: Double }
struct GasEstimate: Codable {
    let position_xz: [Double]
    let strength_ppm: Double
    let normalized_rms: Double
    let region_radius_m: Double
    let cell_size_m: Double
    let cells: [GasCell]
    var valid_until_s: Double
}
struct GasResult: Codable {
    let schema: String
    var state: String
    let reason: String
    let now_s: Double
    let assumptions: GasAssumptions
    let coverage: Int
    let total: Int
    let readings: [GasReading]
    var estimate: GasEstimate?
    let fit_normalized_rms: Double?
}
struct GasNetwork: Codable {
    struct Candidate: Codable, Identifiable {
        var id: String { gateway }
        let gateway: String
        let state: String
        let reason: String
        let sha256: String?
        let version: GasObservation.Version
        let remaining_s: Double
        let coverage: Int
        let radius_m: Double
    }
    let reader: String
    let node: String
    let state: String
    let reason: String
    let gateway: String?
    let sha256: String?
    let version: GasObservation.Version
    let contract_sha256: String
    let candidates: [Candidate]
    let partition: Bool
    let paused: [String: Bool]
    let controls: Bool
}
struct GasSnapshot: Codable {
    struct Truth: Codable { let position_xz: [Double]; let wind_radians: Double }
    struct Event: Codable { let time_s: Double; let action: String; let target: String }
    let schema: String
    let run_id: String
    var sequence: Int
    let last_command_sequence: Int
    var result: GasResult
    let image: String
    let image_sha256: String
    let traces: [String: [[Double]]]
    let enabled: [String: Bool]
    let partition: Bool
    let duplicates: Bool
    let bias: Bool
    let reveal: Bool
    let releasing: Bool?
    let events: [Event]
    let evaluation_truth: Truth?
    var network: GasNetwork? = nil
}

@MainActor final class GasSourceModel: ObservableObject {
    let session: URL
    @Published var snapshot: GasSnapshot?
    @Published var image: NSImage?
    @Published var error: String?
    @Published var tick: Int = 0
    @Published var pending: Int?
    var clock: () -> ContinuousClock.Instant = { .now }
    private var timer: Timer?
    private var lastUpdate: ContinuousClock.Instant?
    private var deadlines: [String: (String, ContinuousClock.Instant)] = [:]
    private var estimateDeadline: (String, ContinuousClock.Instant)?
    private var networkDeadlines: [String: (String, ContinuousClock.Instant)] = [:]
    private var lastCommand = 0
    private var qaSequence = 0
    private var pendingAt: ContinuousClock.Instant?
    init(session: URL) { self.session = session }
    var fresh: Bool { lastUpdate.map { $0.duration(to: clock()) < .seconds(snapshot?.network == nil ? 1 : 3) } ?? false }
    var currentState: String {
        guard fresh, let r = snapshot?.result else { return "unavailable" }
        guard r.estimate == nil || (liveReadings.count == r.coverage && estimateFresh) else { return "unavailable" }
        return r.state
    }
    private var estimateFresh: Bool { estimateDeadline.map { clock() < $0.1 } ?? false }
    var reason: String {
        guard fresh else { return "Producer stopped updating. Current source region and coverage have expired." }
        if snapshot?.result.estimate != nil && liveReadings.count != snapshot?.result.coverage { return "Original supporting measurements expired. Waiting for fresh observations." }
        if snapshot?.result.estimate != nil && !estimateFresh { return "The source estimate expired. Original sensor readings may still be fresh." }
        return snapshot?.result.reason ?? "Waiting for sensor observations"
    }
    func live(_ reading: GasReading) -> Bool {
        guard fresh, reading.state == "live", let deadline = deadlines[reading.id]?.1 else { return false }
        return clock() < deadline
    }
    var liveReadings: [GasReading] { snapshot?.result.readings.filter { live($0) } ?? [] }
    var visibleEstimate: GasEstimate? { currentState == "unavailable" ? nil : snapshot?.result.estimate }
    var canCommand: Bool { fresh && error == nil && pending == nil && lastCommand < 4096 && snapshot?.network?.controls != false }
    func networkRemaining(_ candidate: GasNetwork.Candidate) -> Double {
        guard fresh, let deadline=networkDeadlines[candidate.gateway]?.1 else { return 0 }
        let delta=clock().duration(to:deadline).components
        return max(0,Double(delta.seconds)+Double(delta.attoseconds)/1e18)
    }
    var networkState: String {
        guard fresh,let network=snapshot?.network else {return "unavailable"}
        if network.state == "disputed" {return network.candidates.filter {networkRemaining($0)>0}.count == 2 ? "disputed" : "unavailable"}
        guard let selected=network.candidates.first(where:{$0.gateway==network.gateway}),networkRemaining(selected)>0,currentState != "unavailable" else {return "unavailable"}
        return network.state
    }
    func start() {
        guard timer == nil else { return }
        poll()
        timer = Timer.scheduledTimer(withTimeInterval: 0.1, repeats: true) { [weak self] _ in
            MainActor.assumeIsolated { self?.poll() }
        }
    }
    func stop() { timer?.invalidate(); timer = nil }
    private func read(_ name: String, limit: Int) throws -> Data {
        let url = session.appendingPathComponent(name)
        let values = try url.resourceValues(forKeys: [.isRegularFileKey, .isSymbolicLinkKey, .fileSizeKey])
        guard values.isRegularFile == true, values.isSymbolicLink != true, let size = values.fileSize, size <= limit else { throw CocoaError(.fileReadCorruptFile) }
        return try Data(contentsOf: url)
    }
    private func valid(_ value: GasSnapshot) -> Bool {
        let r=value.result
        func number(_ x:Double,_ min:Double,_ max:Double)->Bool { x.isFinite && x>=min && x<=max }
        func point(_ p:[Double])->Bool { p.count==2 && p.allSatisfy {number($0,-100,100)} }
        guard value.schema=="wendy.gas-demo.v1",r.schema=="wendy.gas-source.v1",
              value.sequence>0,!value.run_id.isEmpty,value.run_id.count<=128,
              number(r.now_s,0,1e9),(4...16).contains(r.total),r.readings.count==r.total,
              Set(r.readings.map(\.id)).count==r.total,
              ["localized","ambiguous","unavailable"].contains(r.state),
              ["scene-0.png","scene-1.png"].contains(value.image),
              value.traces.count<=16,value.traces.values.allSatisfy({$0.count<=60 && $0.allSatisfy {$0.count==2 && $0.allSatisfy(\.isFinite)}}),
              value.evaluation_truth.map({point($0.position_xz) && number($0.wind_radians,-.pi,.pi)}) ?? true else {return false}
        for reading in r.readings {
            guard !reading.id.isEmpty,reading.id.count<=128,point(reading.sensor.position_xz),
                  ["missing","expired","unaligned","live"].contains(reading.state) else {return false}
            if let o=reading.observation {
                guard !o.id.isEmpty,o.id.count<=128,o.version.epoch>0,o.version.sequence>0,
                      number(o.captured_s,0,r.now_s),number(o.valid_until_s,o.captured_s,1e9+5),
                      o.valid_until_s>o.captured_s,o.valid_until_s-o.captured_s<=5,
                      number(o.value_ppm,0,10000),number(o.uncertainty_ppm,0.1,100) else {return false}
                if reading.state=="live" && o.valid_until_s<=r.now_s {return false}
            } else if reading.state != "missing" {return false}
            if let residual=reading.residual_ppm,!number(residual,-20000,20000) {return false}
        }
        let support=r.readings.filter {$0.state=="live"}
        guard r.coverage==support.count else {return false}
        if let e=r.estimate {
            guard r.state != "unavailable",support.count>=4,point(e.position_xz),
                  number(e.cell_size_m,0.05,1),number(e.region_radius_m,e.cell_size_m/sqrt(2)-1e-8,300),
                  number(e.strength_ppm,0,10000),number(e.normalized_rms,0,3),
                  r.fit_normalized_rms==e.normalized_rms,!e.cells.isEmpty,e.cells.count<=4096,
                  e.valid_until_s.isFinite,e.valid_until_s>r.now_s,
                  e.valid_until_s <= support.compactMap({$0.observation?.valid_until_s}).min()!,
                  e.cells.contains(where:{$0.position_xz==e.position_xz}),
                  e.cells.allSatisfy({point($0.position_xz) && number($0.normalized_rms,0,3) && hypot($0.position_xz[0]-e.position_xz[0],$0.position_xz[1]-e.position_xz[1])+e.cell_size_m/sqrt(2)<=e.region_radius_m+1e-8}) else {return false}
        } else if r.state != "unavailable" {return false}
        if let network=value.network {
            func hash(_ h:String)->Bool { h.count==64 && h.allSatisfy {"0123456789abcdef".contains($0)} }
            guard ["east","west"].contains(network.reader), network.node == (network.reader == "east" ? "node-2" : "node-3"),
                  network.controls == (network.reader == "east"), hash(network.contract_sha256),
                  ["localized","ambiguous","unavailable","disputed"].contains(network.state),
                  network.candidates.count==2,Set(network.candidates.map(\.gateway))==Set(["east","west"]),
                  Set(network.paused.keys)==Set(["east","west"]) else {return false}
            for item in network.candidates {
                guard ["localized","ambiguous","unavailable"].contains(item.state),number(item.remaining_s,0,5),number(item.radius_m,0,300),(0...16).contains(item.coverage),item.reason.count<1024 else {return false}
                if item.state != "unavailable" {
                    guard item.sha256.map(hash) == true,item.version.epoch>0,item.version.sequence>0,item.remaining_s>0,item.coverage>=4 else {return false}
                }
            }
            if network.state=="localized" || network.state=="ambiguous" {
                guard network.state==r.state,network.sha256.map(hash)==true,
                      let selected=network.candidates.first(where:{$0.gateway==network.gateway}),selected.sha256==network.sha256,
                      selected.state==network.state,selected.coverage==r.coverage,
                      selected.version.epoch==network.version.epoch,selected.version.sequence==network.version.sequence,
                      let estimate=r.estimate,estimate.valid_until_s-r.now_s<=selected.remaining_s+1e-6 else {return false}
            } else if r.estimate != nil || network.gateway != nil || network.sha256 != nil {return false}
        }
        return true
    }
    func poll() {
        tick &+= 1
        do {
            let candidate = try JSONDecoder().decode(GasSnapshot.self, from: read("snapshot.json", limit: 1_048_576))
            guard valid(candidate) else { throw CocoaError(.fileReadCorruptFile) }
            if let old = snapshot, old.run_id != candidate.run_id || candidate.sequence < old.sequence || candidate.result.now_s < old.result.now_s || old.result.readings.map(\.sensor).sorted(by:{$0.id<$1.id}) != candidate.result.readings.map(\.sensor).sorted(by:{$0.id<$1.id}) { throw CocoaError(.fileReadCorruptFile) }
            if candidate.sequence != snapshot?.sequence {
                let raw = try read(candidate.image, limit: 2_097_152)
                let digest = SHA256.hash(data: raw).map { String(format: "%02x", $0) }.joined()
                // A bridge may advance while files are read. Retry next poll;
                // never show an image that does not match its observation frame.
                guard digest == candidate.image_sha256, let decoded = NSImage(data: raw) else { return }
                let now = clock()
                for reading in candidate.result.readings {
                    guard let obs = reading.observation else { continue }
                    let identity = "\(obs.version.epoch)/\(obs.version.sequence)/\(obs.id)"
                    let remaining = max(0, min(5, obs.valid_until_s-candidate.result.now_s))
                    let deadline = now.advanced(by: .seconds(remaining))
                    if let prior = deadlines[reading.id], prior.0 == identity { deadlines[reading.id] = (identity, min(prior.1, deadline)) }
                    else { deadlines[reading.id] = (identity, deadline) }
                }
                if let estimate=candidate.result.estimate {
                    let support=candidate.result.readings.filter {$0.state == "live"}.sorted {$0.id < $1.id}
                    let identity=support.map { reading in
                        let obs=reading.observation!
                        return "\(reading.id)/\(obs.version.epoch)/\(obs.version.sequence)/\(obs.id)"
                    }.joined(separator:"|")
                    var deadline=now.advanced(by:.seconds(estimate.valid_until_s-candidate.result.now_s))
                    for reading in support { if let original=deadlines[reading.id] { deadline=min(deadline,original.1) } }
                    if let prior=estimateDeadline,prior.0==identity { deadline=min(deadline,prior.1) }
                    estimateDeadline=(identity,deadline)
                }
                if let network=candidate.network {
                    if let old=snapshot?.network,old.reader != network.reader || old.node != network.node || old.contract_sha256 != network.contract_sha256 {throw CocoaError(.fileReadCorruptFile)}
                    for item in network.candidates {
                        let identity="\(item.version.epoch)/\(item.version.sequence)/\(item.sha256 ?? "")"
                        var deadline=now.advanced(by:.seconds(item.remaining_s))
                        if let old=networkDeadlines[item.gateway],old.0==identity {deadline=min(deadline,old.1)}
                        networkDeadlines[item.gateway]=(identity,deadline)
                    }
                } else if snapshot?.network != nil {throw CocoaError(.fileReadCorruptFile)}
                snapshot = candidate; image = decoded; lastUpdate = now
                lastCommand = max(lastCommand, candidate.last_command_sequence)
                error = nil
            }
            if let number = pending {
                let path = String(format: "receipts/%06d.json", number)
                if FileManager.default.fileExists(atPath: session.appendingPathComponent(path).path) {
                    struct Receipt: Codable { let sequence: Int; let accepted: Bool; let error: String }
                    let receipt = try JSONDecoder().decode(Receipt.self, from: read(path, limit: 4096))
                    guard receipt.sequence == number else { throw CocoaError(.fileReadCorruptFile) }
                    if !receipt.accepted { error = receipt.error }
                    pending = nil; pendingAt = nil
                } else if let pendingAt, pendingAt.duration(to: clock()) > .seconds(5) { error = "Control receipt timed out"; pending = nil }
            }
        } catch { self.error = "Cannot read the current simulation snapshot" }
        pollQA()
    }
    @discardableResult func command(_ action: String, target: String = "") -> Bool {
        guard canCommand, let snapshot else { return false }
        let sequence = lastCommand+1
        struct Command: Encodable { let run_id: String; let sequence: Int; let action: String; let target: String }
        let value = Command(run_id:snapshot.run_id, sequence:sequence, action:action, target:target)
        do {
            let raw = try JSONEncoder().encode(value)
            let target = session.appendingPathComponent(String(format: "commands/%06d.json", sequence))
            // Atomic rename of a unique temporary file prevents partial reads.
            // One native control owner writes each session command stream.
            guard !FileManager.default.fileExists(atPath: target.path) else { return false }
            let temp = target.deletingLastPathComponent().appendingPathComponent(".command-"+UUID().uuidString)
            try raw.write(to:temp, options:.withoutOverwriting)
            defer { try? FileManager.default.removeItem(at:temp) }
            guard link(temp.path,target.path) == 0 else { return false }
            lastCommand = sequence; pending = sequence; pendingAt = clock()
            return true
        } catch { self.error = "Could not write simulation control"; return false }
    }
    private func pollQA() {
        // Bounded private acceptance hook invokes the same controls as buttons.
        // Its screenshots are native rendered windows, never fabricated state.
        struct QARequest: Codable { let sequence: Int; let action: String; let target: String? }
        let path = session.appendingPathComponent("native-qa.json")
        guard FileManager.default.fileExists(atPath: path.path),
              let raw = try? read("native-qa.json", limit: 4096),
              let request = try? JSONDecoder().decode(QARequest.self, from:raw),
              request.sequence > qaSequence, request.sequence <= 4096 else { return }
        let number=request.sequence, action=request.action
        qaSequence = number
        var accepted = false
        var capture = ""
        if action == "capture" {
            let host=NSHostingView(rootView:GasSourceContent(model:self).environment(\.colorScheme,.light))
            host.appearance=NSAppearance(named:.aqua)
            host.frame=CGRect(x:0,y:0,width:1500,height:960);host.layoutSubtreeIfNeeded()
            if let bitmap=host.bitmapImageRepForCachingDisplay(in:host.bounds) {
                host.cacheDisplay(in:host.bounds,to:bitmap)
                capture=String(format:"native-%04d.png",number)
                if let png=bitmap.representation(using:.png,properties:[:]) { do { try png.write(to:session.appendingPathComponent(capture),options:.atomic);accepted=true } catch {} }
            }
        } else if ["move_source", "wind_change", "adopt_wind", "toggle_sensor", "poor_geometry", "partition", "duplicates", "bias", "release", "reveal", "reset", "pause_estimator", "restart_east"].contains(action) {
            accepted = command(action, target: request.target ?? "")
        }
        struct QAReceipt: Encodable { let sequence:Int; let accepted:Bool; let action:String; let capture:String; let state:String; let coverage:Int; let snapshot_sequence:Int }
        let receipt = QAReceipt(sequence:number,accepted:accepted,action:action,capture:capture,state:currentState,coverage:liveReadings.count,snapshot_sequence:snapshot?.sequence ?? 0)
        if let raw = try? JSONEncoder().encode(receipt) { try? raw.write(to: session.appendingPathComponent(String(format: "native-receipt-%04d.json", number)), options: .atomic) }
    }
}

private let gasTeal = Color(red: 0.04, green: 0.47, blue: 0.40)
private let gasInk = Color(red: 0.12, green: 0.20, blue: 0.22)

struct GasSourceContent: View {
    @StateObject private var model: GasSourceModel
    private var captureMode=false
    init(session: URL) { _model = StateObject(wrappedValue: GasSourceModel(session: session)) }
    init(model:GasSourceModel) { _model=StateObject(wrappedValue:model);captureMode=true }
    private var statusColor: Color { model.currentState == "localized" ? gasTeal : model.currentState == "ambiguous" ? .orange : .secondary }
    var body: some View {
        VStack(alignment: .leading, spacing: 18) {
            HStack(alignment: .center) {
                VStack(alignment: .leading, spacing: 5) {
                    Text("WENDY WORLD  /  SENSOR FUSION LAB").font(.system(size: 11, weight: .semibold)).tracking(2).foregroundStyle(gasTeal)
                    Text(model.snapshot?.network == nil ? "Where is the source?" : "One site. Independent views.").font(.system(size: 31, weight: .semibold, design: .rounded)).foregroundStyle(gasInk)
                }
                Spacer()
                Label("SIMULATED", systemImage: "cube.transparent").font(.system(size: 12, weight: .semibold)).padding(10).background(gasTeal.opacity(0.09), in: Capsule())
                Text(model.fresh ? "LIVE" : "STALE").font(.system(size: 11, weight: .bold)).foregroundStyle(model.fresh ? gasTeal : .secondary)
            }
            if let network=model.snapshot?.network {
                HStack(alignment:.center,spacing:22) {
                    VStack(alignment:.leading,spacing:5) {
                        Text("OPERATOR \(network.reader.uppercased()) · \(network.node.uppercased())").font(.system(size:11,weight:.semibold)).foregroundStyle(gasTeal)
                        Text(model.networkState == "unavailable" ? "No current source region" : model.networkState == "disputed" ? "Gateway regions disagree" : "Selected \(network.gateway?.uppercased() ?? "") · \(network.sha256?.prefix(12) ?? "")").font(.system(size:14,weight:.medium))
                        Text("Local selection history · independent claims are never fused").font(.system(size:10)).foregroundStyle(.secondary)
                        Text(model.networkState == "unavailable" ? "No current claim. Previous selections are historical." : network.reason).font(.system(size:9)).foregroundStyle(.secondary).lineLimit(1)
                    }
                    Spacer()
                    ForEach(network.candidates) { item in
                        VStack(alignment:.leading,spacing:5) {
                            Text("\(item.gateway.uppercased()) · \(model.networkRemaining(item)>0 ? item.state : "unavailable")").font(.system(size:11,weight:.semibold))
                            Text(model.networkRemaining(item)>0 ? String(format:"%d sensors · %.1fs original TTL",item.coverage,model.networkRemaining(item)) : "No fresh compatible claim").font(.system(size:10)).foregroundStyle(.secondary)
                        }.frame(width:190,alignment:.leading)
                    }
                }.padding(12).background(gasTeal.opacity(0.06),in:RoundedRectangle(cornerRadius:10))
            }
            HStack(alignment: .top, spacing: 22) {
                VStack(alignment: .leading, spacing: 14) {
                    if let image = model.image { Image(nsImage: image).resizable().aspectRatio(1.6, contentMode: .fit).clipShape(RoundedRectangle(cornerRadius: 14)) }
                    HStack { Text("MuJoCo laboratory · 8 fixed concentration sensors").font(.system(size: 12, weight: .medium)); Spacer(); Text("Source truth hidden from estimator").font(.system(size: 11)).foregroundStyle(.secondary) }
                    HStack(alignment: .top, spacing: 16) {
                        VStack(alignment: .leading, spacing: 8) {
                            Text("SOURCE REGION / SITE X–Z METRES").font(.system(size: 10, weight: .semibold)).tracking(1)
                            GasSourcePlan(model: model).frame(height: 235)
                        }.frame(maxWidth: .infinity)
                        VStack(alignment: .leading, spacing: 8) {
                            Text("ORIGINAL READINGS / PPM").font(.system(size: 10, weight: .semibold)).tracking(1)
                            GasTraces(snapshot: model.snapshot).frame(height: 165)
                            Text("Recent concentration history. A missing reading never means zero or safe.").font(.system(size: 11)).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                        }.frame(maxWidth: .infinity)
                    }
                }.frame(maxWidth: .infinity)
                VStack(alignment: .leading, spacing: model.snapshot?.network == nil ? 13 : 9) {
                    HStack { Circle().fill(statusColor).frame(width: 9, height: 9); Text(model.currentState.uppercased()).font(.system(size: 13, weight: .bold)).tracking(1); Spacer(); Text("\(model.liveReadings.count)/\(model.snapshot?.result.total ?? 8) live").font(.system(size: 12, weight: .medium)) }
                    Text(model.reason).font(.system(size: 12)).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true).frame(minHeight: 42, alignment: .top)
                    if let estimate=model.visibleEstimate { Text(String(format:"Best fit · X %.2f m   Z %.2f m",estimate.position_xz[0],estimate.position_xz[1])).font(.system(size:12,weight:.medium)).foregroundStyle(gasTeal) }
                    HStack {
                        metric("FIT RESIDUAL", (model.fresh && (model.snapshot?.result.estimate == nil || model.visibleEstimate != nil)) ? model.snapshot?.result.fit_normalized_rms.map { String(format: "%.2f", $0) } ?? "—" : "—")
                        Spacer()
                        metric("REGION RADIUS", model.visibleEstimate.map { String(format: "%.2f m", $0.region_radius_m) } ?? "—")
                    }
                    Text("Residuals use declared sensor uncertainty. The model-fit region is not a confidence interval.").font(.system(size: 10)).foregroundStyle(.secondary).fixedSize(horizontal:false,vertical:true)
                    Divider()
                    HStack { Text("Sensor").frame(width: 45, alignment: .leading); Spacer(); Text("ppm"); Text("fit error").frame(width: 60, alignment: .trailing) }.font(.system(size: 10, weight: .semibold)).foregroundStyle(.secondary)
                    ForEach(model.snapshot?.result.readings ?? []) { reading in
                        HStack(spacing: 7) {
                            Button { model.command("toggle_sensor", target: reading.id) } label: { Image(systemName: model.snapshot?.enabled[reading.id] == true ? "checkmark.circle.fill" : "circle").foregroundStyle(model.live(reading) ? gasTeal : .secondary) }.buttonStyle(.plain).disabled(!model.canCommand)
                            Text(reading.id.uppercased()).font(.system(size: 12, weight: .semibold)).frame(width: 20)
                            Text(model.live(reading) ? "fresh" : reading.state == "live" ? "expired" : reading.state).font(.system(size: 10)).foregroundStyle(.secondary)
                            Spacer()
                            Text(model.live(reading) ? String(format: "%.1f", reading.observation?.value_ppm ?? 0) : "—").monospacedDigit().font(.system(size: 12))
                            Text(model.fresh && model.live(reading) && (model.snapshot?.result.estimate == nil || model.visibleEstimate != nil) ? reading.residual_ppm.map { String(format: "%+.1f", $0) } ?? "—" : "—").monospacedDigit().font(.system(size: 12)).foregroundStyle(.secondary).frame(width: 60, alignment: .trailing)
                        }
                    }
                    Divider()
                    Text("DECLARED AIRFLOW").font(.system(size: 10, weight: .semibold)).tracking(1)
                    Text(model.snapshot.map { String(format: "%.0f° · downwind %.1f m · crosswind %.1f m", $0.result.assumptions.wind_radians*180 / .pi, $0.result.assumptions.downwind_m, $0.result.assumptions.crosswind_m) } ?? "Waiting for model inputs").font(.system(size: 11)).foregroundStyle(.secondary)
                    LazyVGrid(columns: [GridItem(.flexible()), GridItem(.flexible())], spacing: 7) {
                        control("Move source", "move_source")
                        control("Change true wind", "wind_change")
                        if model.snapshot?.network == nil { control("Use current wind", "adopt_wind") }
                        else { Button("Restart east node") {model.command("restart_east")}.font(.system(size:11)).buttonStyle(.bordered).disabled(!model.canCommand) }
                        control(model.snapshot?.bias == true ? "Remove bias" : "Bias sensor C", "bias")
                        if let network=model.snapshot?.network {
                            Button(network.paused["west"] == true ? "Resume west estimator" : "Pause west estimator") {model.command("pause_estimator",target:"west")}.font(.system(size:11)).buttonStyle(.bordered).disabled(!model.canCommand)
                        } else {control("Collinear coverage", "poor_geometry")}
                        control(model.snapshot?.partition == true ? "Restore gateway links" : "Partition east links", "partition")
                        control(model.snapshot?.duplicates == true ? "Resume captures" : "Repeat old packets", "duplicates")
                        control("Restore scenario", "reset")
                        control(model.snapshot?.releasing == false ? "Resume release" : "Pause release", "release")
                    }
                    Button { model.command("reveal") } label: { Label(model.snapshot?.reveal == true ? "Hide evaluation truth" : "Reveal evaluation truth", systemImage: "eye") }.font(.system(size: 11)).buttonStyle(.plain).foregroundStyle(.orange).disabled(!model.canCommand)
                    if let error = model.error { Text(error).font(.system(size: 11)).foregroundStyle(.red) }
                    if model.pending != nil { Text("Waiting for control receipt…").font(.system(size: 10)).foregroundStyle(.secondary) }
                }.padding(18).frame(width: 345).background(.white, in: RoundedRectangle(cornerRadius: 16))
            }
            HStack { Text("Synthetic analytic gas · MuJoCo geometry and rendering · No fluid dynamics or physical accuracy claim").font(.system(size: 11)).foregroundStyle(.secondary).lineLimit(1).minimumScaleFactor(0.8); Spacer(); if let last = model.snapshot?.events.last { Text(String(format: "%.1fs  %@", last.time_s, last.action.replacingOccurrences(of: "_", with: " "))).font(.system(size: 10, design: .monospaced)).foregroundStyle(gasTeal).lineLimit(1).frame(maxWidth:220,alignment:.trailing) } }
        }.padding(26).foregroundStyle(gasInk).background(Color(red: 0.95, green: 0.96, blue: 0.94)).onAppear { if !captureMode { model.start() } }.onDisappear { if !captureMode { model.stop() } }
    }
    private func metric(_ label: String, _ value: String) -> some View { VStack(alignment: .leading, spacing: 5) { Text(label).font(.system(size: 9, weight: .semibold)).foregroundStyle(.secondary); Text(value).font(.system(size: 25, weight: .medium, design: .rounded)).monospacedDigit() } }
    private func control(_ title: String, _ action: String) -> some View { Button(title) { model.command(action) }.font(.system(size: 11)).buttonStyle(.bordered).frame(maxWidth: .infinity).disabled(!model.canCommand) }
}

struct GasSourcePlan: View {
    @ObservedObject var model: GasSourceModel
    var body: some View {
        Canvas { context, size in
            func point(_ p: [Double]) -> CGPoint { CGPoint(x: 22+(p[0]+2.5)/5*(size.width-44), y: 15+(2.5-p[1])/5*(size.height-30)) }
            for i in -2...2 {
                var path = Path(); path.move(to: point([Double(i), -2.3])); path.addLine(to: point([Double(i), 2.3])); path.move(to: point([-2.3, Double(i)])); path.addLine(to: point([2.3, Double(i)])); context.stroke(path, with: .color(.gray.opacity(0.14)), lineWidth: 1)
            }
            if let estimate = model.visibleEstimate {
                for cell in estimate.cells { let p = point(cell.position_xz); let width=estimate.cell_size_m/5*(size.width-44); let height=estimate.cell_size_m/5*(size.height-30); context.fill(Path(CGRect(x: p.x-width/2, y: p.y-height/2, width: width, height: height)), with: .color((model.currentState == "ambiguous" ? Color.orange : gasTeal).opacity(0.3))) }
                let p = point(estimate.position_xz)
                context.stroke(Path(ellipseIn: CGRect(x: p.x-7, y: p.y-7, width: 14, height: 14)), with: .color(gasTeal), lineWidth: 2)
            }
            for reading in model.snapshot?.result.readings ?? [] {
                let p = point(reading.sensor.position_xz)
                context.fill(Path(ellipseIn: CGRect(x: p.x-4, y: p.y-4, width: 8, height: 8)), with: .color(model.live(reading) ? gasTeal : .gray.opacity(0.4)))
                context.draw(Text(reading.id.uppercased()).font(.system(size: 10, weight: .semibold)).foregroundColor(gasInk), at: CGPoint(x: p.x, y: p.y+14))
            }
            if model.snapshot?.reveal == true, let truth = model.snapshot?.evaluation_truth {
                let p = point(truth.position_xz); var cross = Path(); cross.move(to: CGPoint(x: p.x-6, y: p.y)); cross.addLine(to: CGPoint(x: p.x+6, y: p.y)); cross.move(to: CGPoint(x: p.x, y: p.y-6)); cross.addLine(to: CGPoint(x: p.x, y: p.y+6)); context.stroke(cross, with: .color(.orange), lineWidth: 2)
                context.draw(Text("Evaluation truth").font(.system(size: 9)).foregroundColor(.orange), at: CGPoint(x: size.width/2, y: size.height-5))
            }
        }.background(.white, in: RoundedRectangle(cornerRadius: 12))
    }
}
struct GasTraces: View {
    let snapshot: GasSnapshot?
    var body: some View {
        Canvas { context, size in
            let series = snapshot?.traces ?? [:]
            let last = series.values.compactMap { $0.last?.first }.max() ?? 0
            let palette: [Color] = [gasTeal, .orange, .blue, .purple, .cyan, .brown, .pink, .indigo]
            for (index, key) in series.keys.sorted().enumerated() {
                var line = Path(); var started = false
                for pair in series[key] ?? [] where pair.count == 2 && pair[0] >= last-12 {
                    let p = CGPoint(x: 8+(pair[0]-last+12)/12*(size.width-16), y: size.height-15-min(160, max(0, pair[1]))/160*(size.height-30))
                    if started { line.addLine(to: p) } else { line.move(to: p); started = true }
                }
                context.stroke(line, with: .color(palette[index % palette.count].opacity(0.8)), lineWidth: 1.7)
            }
            context.draw(Text("160 ppm").font(.system(size: 9)).foregroundColor(.secondary), at: CGPoint(x: 30, y: 10))
            context.draw(Text("0").font(.system(size: 9)).foregroundColor(.secondary), at: CGPoint(x: 12, y: size.height-6))
        }.background(.white, in: RoundedRectangle(cornerRadius: 12))
    }
}
