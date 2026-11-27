import Foundation
import SwiftUI

/// Consumer diagnostics. The chosen FusionState retains its publisher's score,
/// source observations and uncertainty; this model does not combine claims.
struct FusionSelection: Decodable {
    var state: String
    var reason: String
    var selected: String?
    var challenger: String?
    var hold_progress: Double
    var unique_evidence: Int
    var candidates: [Candidate]
    let events: [Event]
    let fixture_moved: Bool
    var displayExpired = false
    struct Candidate: Decodable, Identifiable {
        var id: String { gateway }
        let gateway: String; let state: String
        let uncertainty_m: Double?; let remaining_s: Double?
        let input_digest: String?
        let input_enabled: Bool; let result_enabled: Bool
        let position_error_m: Double
    }
    struct Event: Decodable {
        let time_s: Double; let kind: String; let from: String?; let to: String?; let reason: String
    }
    enum CodingKeys: String,CodingKey { case state,reason,selected,challenger,hold_progress,unique_evidence,candidates,events,fixture_moved }
    init(from decoder: Decoder) throws {
        let c=try decoder.container(keyedBy:CodingKeys.self)
        state=try c.decode(String.self,forKey:.state);reason=try c.decode(String.self,forKey:.reason)
        selected=try c.decodeIfPresent(String.self,forKey:.selected);challenger=try c.decodeIfPresent(String.self,forKey:.challenger)
        hold_progress=try c.decode(Double.self,forKey:.hold_progress);unique_evidence=try c.decode(Int.self,forKey:.unique_evidence)
        candidates=try c.decode([Candidate].self,forKey:.candidates);events=try c.decode([Event].self,forKey:.events)
        fixture_moved=try c.decode(Bool.self,forKey:.fixture_moved)
        let ids=Set(candidates.map(\.gateway))
        guard ["selected","unavailable","disputed"].contains(state),reason.count<=1024,
              !candidates.isEmpty,candidates.count<=16,ids.count==candidates.count,
              candidates.allSatisfy({validReplicaNode($0.gateway) && $0.state.count<=64 &&
                  $0.position_error_m.isFinite && (0...100).contains($0.position_error_m) &&
                  $0.uncertainty_m.map({$0.isFinite && $0>0 && $0<=100}) ?? true &&
                  $0.remaining_s.map({$0.isFinite && (0...30).contains($0)}) ?? true}),
              selected.map({ids.contains($0)}) ?? true,challenger.map({ids.contains($0)}) ?? true,
              (state == "selected") == (selected != nil),
              hold_progress.isFinite,(0...1).contains(hold_progress),
              (0...candidates.count).contains(unique_evidence),events.count<=32,
              events.allSatisfy({$0.time_s.isFinite && $0.time_s>=0 && $0.kind.count<=64 && $0.reason.count<=1024 &&
                  $0.from.map({ids.contains($0)}) ?? true && $0.to.map({ids.contains($0)}) ?? true}) else {throw DemoError.invalidFile}
        if let selected {
            guard let candidate=candidates.first(where:{$0.gateway==selected}),candidate.state=="hazard",
                  candidate.uncertainty_m != nil,(candidate.remaining_s ?? 0)>0 else {throw DemoError.invalidFile}
        }
    }
    func expiredDisplay() -> FusionSelection {
        var copy=self
        copy.state="unavailable";copy.reason="The local display deadline expired. Waiting for a fresh delivery."
        copy.selected=nil;copy.challenger=nil;copy.hold_progress=0;copy.unique_evidence=0;copy.displayExpired=true
        copy.candidates=candidates.map { Candidate(gateway:$0.gateway,state:"expired",uncertainty_m:nil,remaining_s:0,
            input_digest:$0.input_digest,input_enabled:$0.input_enabled,result_enabled:$0.result_enabled,position_error_m:$0.position_error_m) }
        return copy
    }
    func agrees(with fusion: FusionState?, mapNode:String) -> Bool {
        if let selected { return fusion?.localizedHazard == true && fusion?.replica_node==selected && mapNode==selected }
        return fusion?.localizedHazard != true
    }
}

struct FusionSelectionPanel: View {
    @ObservedObject var model: DemoModel
    let value: FusionSelection
    private let accent=Color(red:0.12,green:0.47,blue:0.43)
    var body: some View {
        VStack(alignment:.leading,spacing:12) {
            Text("AUTOMATIC HANDOFF").font(.system(size:10,weight:.bold)).tracking(1.4).foregroundStyle(.secondary)
            HStack {
                Image(systemName:value.state == "disputed" ? "exclamationmark.triangle.fill":"point.3.connected.trianglepath.dotted")
                Text(value.displayExpired ? "Display expired":(value.selected.map { $0.capitalized+" selected" } ?? (value.state == "disputed" ? "Locations disagree":"Waiting for evidence")))
                    .font(.system(size:16,weight:.semibold))
            }.foregroundStyle(value.state == "disputed" ? Color.orange:accent)
            if value.displayExpired { Text(value.reason).font(.system(size:11)).foregroundStyle(.secondary) }
            Text("One original estimate. Its score and evidence stay intact.").font(.system(size:11)).foregroundStyle(.secondary)
            Text("\(value.unique_evidence) distinct input sets; shared observations remain correlated.").font(.system(size:10)).foregroundStyle(.secondary)
            if let challenger=value.challenger {
                VStack(alignment:.leading,spacing:5) {
                    HStack { Text(challenger.capitalized+" improving");Spacer();Text(String(format:"%.0f%%",value.hold_progress*100)).monospacedDigit() }
                    ProgressView(value:value.hold_progress).tint(accent)
                    Text("20% lower uncertainty, sustained for 1 s of new evidence.").font(.system(size:10)).foregroundStyle(.secondary)
                }.font(.system(size:11))
            }
            ForEach(value.candidates) { candidate in
                VStack(alignment:.leading,spacing:7) {
                    HStack {
                        Circle().fill(candidate.gateway==value.selected ? accent:Color.gray.opacity(0.35)).frame(width:6,height:6)
                        Text(candidate.gateway.capitalized).font(.system(size:12,weight:.semibold));Spacer()
                        if let u=candidate.uncertainty_m { Text(String(format:"± %.2f m",u)).font(.system(size:11,weight:.medium,design:.monospaced)) }
                    }
                    HStack {
                        Text(candidate.state.replacingOccurrences(of:"_",with:" "))
                        Spacer()
                        if let remaining=candidate.remaining_s {Text(String(format:"%.2f s left",remaining)).monospacedDigit()}
                    }.font(.system(size:10)).foregroundStyle(.secondary)
                    HStack(spacing:5) {
                        control(candidate.input_enabled ? "Inputs on":"Inputs off","handoff_input",candidate.gateway,candidate.input_enabled)
                        control(candidate.result_enabled ? "Delivery on":"Delivery off","handoff_result",candidate.gateway,candidate.result_enabled)
                    }
                    HStack(spacing:5) {
                        control(candidate.position_error_m<0.1 ? "Restore error":"Improve sensor","handoff_quality",candidate.gateway,candidate.position_error_m<0.1)
                        Button("Redeliver") {model.command("handoff_duplicate",target:candidate.gateway)}.buttonStyle(.bordered).controlSize(.mini).disabled(!model.canCommand)
                    }
                    Text(String(format:"Declared LiDAR position error %.2f m",candidate.position_error_m)).font(.system(size:9)).foregroundStyle(.secondary)
                }.padding(10).background(.white.opacity(0.8)).clipShape(RoundedRectangle(cornerRadius:10))
            }
            Button(value.fixture_moved ? "Return simulated fixture":"Move simulated fixture") {model.command("handoff_fixture",target:"fixture")}
                .buttonStyle(.bordered).controlSize(.small).disabled(!model.canCommand)
            Text("RGB and rays: MuJoCo. Gas and audio: generated. Delivery switches model a partition; they do not change physical devices.")
                .font(.system(size:10)).foregroundStyle(.secondary).fixedSize(horizontal:false,vertical:true)
            Divider()
            Text("DECISIONS").font(.system(size:10,weight:.bold)).tracking(1.3).foregroundStyle(.secondary)
            ForEach(Array(value.events.suffix(4).reversed().enumerated()),id:\.offset) { _,event in
                HStack(alignment:.top,spacing:8) {
                    Text(String(format:"%.1fs",event.time_s)).font(.system(size:10,design:.monospaced)).foregroundStyle(.secondary).frame(width:34,alignment:.leading)
                    VStack(alignment:.leading,spacing:3) {
                        Text(event.kind == "handoff" ? "\(event.from?.capitalized ?? "Start") → \(event.to?.capitalized ?? "Unknown")":event.kind.capitalized).font(.system(size:11,weight:.semibold))
                        Text(event.reason).font(.system(size:10)).foregroundStyle(.secondary).fixedSize(horizontal:false,vertical:true)
                    }
                }
            }
        }
    }
    private func control(_ label:String,_ action:String,_ target:String,_ on:Bool)->some View {
        Button(label){model.command(action,target:target)}.buttonStyle(.bordered).controlSize(.mini).tint(on ? accent:Color.orange).disabled(!model.canCommand)
    }
}
