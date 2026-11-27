import Foundation
import SwiftUI
import AppKit
#if canImport(WorldClient)
import WorldClient
struct NativeZoneStatus:Decodable {let sha256:String;let source:JSONValue;let state:String;let checked_at:String;let current_remote_state:String;let selection_sequence:Int?}
struct NativeZonePresence:Decodable {let zones:[ZoneState];let events:[JSONValue];struct ZoneState:Decodable,Identifiable{let zone:ZoneRectangle;let state:String;let observed:Int;var id:String{zone.id}}}
#endif

private let trackingAnchor=Date(timeIntervalSince1970:1_767_225_600)
private func trackingSeconds(_ raw:String)throws->Double {
    let formatter=ISO8601DateFormatter()
    formatter.formatOptions=[.withInternetDateTime,.withFractionalSeconds]
    let fractional=formatter.date(from:raw)
    formatter.formatOptions=[.withInternetDateTime]
    guard raw.count<=64,let date=fractional ?? formatter.date(from:raw) else { throw DemoError.invalidFile }
    return date.timeIntervalSince(trackingAnchor)
}
struct TrackingState:Decodable {
    let participant_id:String;let now_ns:Int64;var tracks:[PersonTrack];var cameras:[TrackingCamera];let camera_sources:[TrackingSource]
    #if canImport(WorldClient)
    let zone_catalog:NativeZoneStatus?
    let zone_presence:NativeZonePresence?
    #endif
    enum CodingKeys:String,CodingKey { case participant_id,now_ns,tracks,cameras,camera_sources,zone_catalog,presence }
    init(from decoder:Decoder)throws {
        let c=try decoder.container(keyedBy:CodingKeys.self)
        participant_id=try c.decode(String.self,forKey:.participant_id);now_ns=try c.decode(Int64.self,forKey:.now_ns)
        tracks=try c.decode([PersonTrack].self,forKey:.tracks);cameras=try c.decode([TrackingCamera].self,forKey:.cameras)
        #if canImport(WorldClient)
        zone_catalog=try c.decodeIfPresent(NativeZoneStatus.self,forKey:.zone_catalog)
        zone_presence=try c.decodeIfPresent(NativeZonePresence.self,forKey:.presence)
        if let p=zone_presence {guard p.zones.count<=16,p.events.count<=128,p.zones.allSatisfy({["observed","uncertain","unknown"].contains($0.state)&&$0.observed>=0&&$0.observed<=128}) else{throw DemoError.invalidFile};for z in p.zones{try z.zone.validate()}}
        #endif
        camera_sources=try c.decodeIfPresent([TrackingSource].self,forKey:.camera_sources) ?? []
        guard validReplicaNode(participant_id),now_ns>=0,now_ns<=86_400_000_000_000,tracks.count<=128,cameras.count<=16,
              Set(tracks.map(\.id)).count==tracks.count,Set(cameras.map(\.sensor_id)).count==cameras.count,
              camera_sources.count<=16,Set(camera_sources.map(\.id)).count==camera_sources.count,
              camera_sources.allSatisfy({ validReplicaNode($0.id) && $0.label.count<=100 }) else { throw DemoError.invalidFile }
        for camera in cameras {
            guard validReplicaNode(camera.sensor_id),!camera.sample_id.isEmpty,camera.sample_id.count<=512,
                  camera.image_sha256.utf8.count==64,camera.image_sha256.utf8.allSatisfy({ (48...57).contains($0)||(97...102).contains($0) }),
                  [camera.detected,camera.projected,camera.rejected.count].allSatisfy({ (0...512).contains($0) }),
                  camera.rejected.allSatisfy({ $0.member_index>=0 && $0.member_index<512 && $0.reason.count<=512 }),
                  camera.captured_at_s.map({ $0.isFinite && $0>=0 }) ?? true else { throw DemoError.invalidFile }
            if let path=camera.evidence_path { guard path.hasPrefix("tracking/"+participant_id+"/") else { throw DemoError.invalidFile } }
        }
    }
    func live(at seconds:Double)->[PersonTrack] { tracks.filter { $0.validUntil>max(seconds,Double(now_ns)/1e9) && $0.lastObserved<=max(seconds,Double(now_ns)/1e9) } }
    mutating func expireNative(at now:Int64,clockKnown:Bool) {
        tracks.removeAll { !clockKnown || !$0.nativeFresh(at:now) }
        let sources=Set(tracks.flatMap { $0.members.map(\.source) })
        cameras.removeAll { !sources.contains($0.sensor_id) }
    }
}
struct PersonTrack:Decodable,Identifiable {
    let id:String;let state:String;let position_xz:[Double];let velocity_xz:[Double];let covariance:[Double]
    let last_observed:String;let valid_until:String;let ambiguous:Bool;let members:[TrackMember]
    let motionKnown:Bool
    let displayDeadline:Int64?;let originalEnvelopeSHA256:String?
    let nativeEvidence:[DemoNativeEvidence]
    let lastObserved:Double;let validUntil:Double
    enum CodingKeys:String,CodingKey { case id,state,position_xz,velocity_xz,covariance,last_observed,valid_until,ambiguous,members,motion_known,display_deadline_monotonic_ns,original_envelope_sha256,native_evidence }
    init(from decoder:Decoder)throws {
        let c=try decoder.container(keyedBy:CodingKeys.self)
        id=try c.decode(String.self,forKey:.id);state=try c.decode(String.self,forKey:.state)
        position_xz=try c.decode([Double].self,forKey:.position_xz)
        motionKnown=try c.decodeIfPresent(Bool.self,forKey:.motion_known) ?? true
        displayDeadline=try c.decodeIfPresent(Int64.self,forKey:.display_deadline_monotonic_ns)
        originalEnvelopeSHA256=try c.decodeIfPresent(String.self,forKey:.original_envelope_sha256)
        nativeEvidence=try c.decodeIfPresent([DemoNativeEvidence].self,forKey:.native_evidence) ?? []
        guard validNativeEvidence(nativeEvidence,original:originalEnvelopeSHA256) else { throw DemoError.invalidFile }
        velocity_xz=motionKnown ? try c.decode([Double].self,forKey:.velocity_xz) : [0,0]
        covariance=try c.decode([Double].self,forKey:.covariance)
        last_observed=try c.decode(String.self,forKey:.last_observed);valid_until=try c.decode(String.self,forKey:.valid_until)
        ambiguous=try c.decode(Bool.self,forKey:.ambiguous);members=try c.decode([TrackMember].self,forKey:.members)
        lastObserved=try trackingSeconds(last_observed);validUntil=try trackingSeconds(valid_until)
        guard !id.isEmpty,id.count<=160,["observed","coasting"].contains(state),position_xz.count==2,velocity_xz.count==2,
              position_xz.allSatisfy({ $0.isFinite && abs($0)<=10_000 }),velocity_xz.allSatisfy({ $0.isFinite && abs($0)<=50 }),
              covariance.count==4,covariance.allSatisfy(\.isFinite),covariance[0]>=0,covariance[3]>=0,
              abs(covariance[1]-covariance[2])<1e-6,covariance[0]*covariance[3]-covariance[1]*covariance[2]>=0,
              covariance[0]<=100,covariance[3]<=100,lastObserved>=0,validUntil>lastObserved,members.count<=64,
              members.allSatisfy({ !$0.source.isEmpty && $0.source.count<=256 && !$0.sample_id.isEmpty && $0.sample_id.count<=512 && ($0.evidence?.count ?? 0)<=8 }),
              displayDeadline.map({$0>0}) ?? true,
              originalEnvelopeSHA256.map({$0.utf8.count==64 && $0.utf8.allSatisfy({(48...57).contains($0)||(97...102).contains($0)})}) ?? true else { throw DemoError.invalidFile }
    }
    func nativeFresh(at now:Int64)->Bool { now>=0 && originalEnvelopeSHA256 != nil && displayDeadline.map({now<$0}) == true }
    var shortID:String { "Track "+String(id.suffix(6)) }
    var scenePosition:[Double] { [position_xz[0],-position_xz[1],0.045] }
    var speed:Double { hypot(velocity_xz[0],velocity_xz[1]) }
    var majorVariance:Double { let a=covariance[0],b=covariance[1],d=covariance[3];return (a+d+sqrt((a-d)*(a-d)+4*b*b))/2 }
    var minorVariance:Double { max(0,covariance[0]+covariance[3]-majorVariance) }
    var ellipseAngle:Double { -0.5*atan2(2*covariance[1],covariance[0]-covariance[3]) }
    var tint:NSColor { ambiguous ? .systemOrange:(state=="coasting" ? .systemGray:.systemTeal) }
}
struct TrackMember:Decodable {
    let source:String;let sample_id:String;let evidence:[TrackEvidence]?
}
struct TrackEvidence:Decodable { let sha256:String }
struct TrackingCamera:Decodable,Identifiable {
    var id:String { sensor_id }
    let sensor_id:String;let sample_id:String;let image_sha256:String;let detected:Int;let projected:Int;let rejected:[RejectedDetection]
    let evidence_path:String?
    let captured_at_s:Double?
}
struct RejectedDetection:Decodable { let member_index:Int;let reason:String }
struct TrackingSource:Decodable,Identifiable { let id:String;let label:String;let enabled:Bool }

struct TrackingPanel:View {
    @ObservedObject var model:DemoModel
    let tracking:TrackingState
    private var live:[PersonTrack] { tracking.live(at:model.snapshot?.time_s ?? 0) }
    var body:some View {
        VStack(alignment:.leading,spacing:15) {
            Text("ANONYMOUS TRACKS").font(.system(size:9,weight:.semibold)).tracking(1.2).foregroundStyle(.secondary)
            HStack(alignment:.firstTextBaseline) {
                Text("\(live.count)").font(.system(size:42,weight:.light,design:.rounded))
                Text(live.count==1 ? "active estimate":"active estimates").font(.system(size:12)).foregroundStyle(.secondary)
            }
            Text("\(tracking.participant_id == "hub" ? "Site hub":tracking.participant_id.capitalized) replica · \(Set(live.flatMap { $0.members.map(\.source) }).count) camera sources")
                .font(.system(size:10,weight:.medium)).foregroundStyle(.secondary)
            Text("Rendered camera pixels → pose detections → calibrated floor estimates.").font(.system(size:11)).foregroundStyle(.secondary).fixedSize(horizontal:false,vertical:true)
            ForEach(tracking.camera_sources) { source in
                Toggle(source.label,isOn:Binding(get:{source.enabled},set:{_ in model.command(model.liveNativeReplica ? "toggle_sensor":"toggle_camera",target:source.id)}))
                    .toggleStyle(.switch).controlSize(.mini).tint(.teal).font(.system(size:11)).disabled(!model.canCommand)
            }
            ForEach(tracking.cameras) { camera in
                VStack(alignment:.leading,spacing:5) {
                    ZStack {
                        RoundedRectangle(cornerRadius:9).fill(Color(red:0.14,green:0.20,blue:0.22))
                        if let image=model.trackingImages[camera.sensor_id] { Image(nsImage:image).resizable().scaledToFit() }
                        else { Label("Frame unavailable",systemImage:"camera").font(.system(size:10)).foregroundStyle(.white.opacity(0.7)) }
                    }.aspectRatio(4/3,contentMode:.fit).clipShape(RoundedRectangle(cornerRadius:9))
                    Text(camera.sensor_id.replacingOccurrences(of:"-",with:" ").capitalized).font(.system(size:11,weight:.medium))
                    Text("\(camera.detected) detected · \(camera.projected) projected · \(camera.rejected.count) rejected")
                        .font(.system(size:9)).foregroundStyle(.secondary)
                    if let time=camera.captured_at_s { Text(String(format:"Captured %.2f s · %.1f s old",time,max(0,(model.snapshot?.time_s ?? 0)-time))).font(.system(size:9)).foregroundStyle(.secondary) }
                }
            }
            Divider()
            ForEach(live) { track in
                Button { model.selectTrack(track.id) } label: {
                    VStack(alignment:.leading,spacing:5) {
                        HStack { Circle().fill(Color(nsColor:track.tint)).frame(width:6,height:6);Text(track.shortID).font(.system(size:12,weight:.semibold));Spacer();Text(track.motionKnown ? String(format:"%.1f m/s",track.speed):"Motion unknown").font(.system(size:10,design:.monospaced)) }
                        Text(track.ambiguous ? "Association uncertain":(track.state=="coasting" ? "Coasting · no new observation":"Observed"))
                            .font(.system(size:10)).foregroundStyle(.secondary)
                        Text("\(Set(track.members.map(\.source)).count) camera sources · "+String(format:"%.1f s since observation",max(0,(model.snapshot?.time_s ?? 0)-track.lastObserved)))
                            .font(.system(size:9)).foregroundStyle(.secondary)
                    }.padding(10).background(model.selectedTrack==track.id ? Color.teal.opacity(0.07):Color.clear,in:RoundedRectangle(cornerRadius:10))
                }.buttonStyle(.plain)
            }
            Text("IDs identify short-lived estimates, not people. Rings show 2σ positional uncertainty; arrows show inferred velocity.")
                .font(.system(size:10)).foregroundStyle(.secondary).fixedSize(horizontal:false,vertical:true)
        }
    }
}
