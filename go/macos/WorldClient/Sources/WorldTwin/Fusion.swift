import Foundation
import SwiftUI
import AppKit
import ImageIO
import Darwin
import CryptoKit

struct FusionState: Decodable {
    var state: String; var label: String; var reason: String; let sample_time_s: Double
    var position: [Double]?; var uncertainty_m: Double?; var confidence: Double?; var basis: String?
    var observations: [FusionObservation]
    let original_envelope_sha256: String?
    let nativeEvidence:[DemoNativeEvidence]
    let replica_node: String?
    let missing_observations: [MissingObservation]
    var localizedHazard: Bool { state == "hazard" && position != nil }
    enum CodingKeys: String, CodingKey { case state,label,reason,sample_time_s,position,uncertainty_m,confidence,basis,observations,replica_node,missing_observations,original_envelope_sha256,native_evidence }
    init(from decoder: Decoder) throws {
        let c=try decoder.container(keyedBy:CodingKeys.self)
        state=try c.decode(String.self,forKey:.state);label=try c.decode(String.self,forKey:.label)
        reason=try c.decode(String.self,forKey:.reason);sample_time_s=try c.decode(Double.self,forKey:.sample_time_s)
        position=try c.decodeIfPresent([Double].self,forKey:.position)
        uncertainty_m=try c.decodeIfPresent(Double.self,forKey:.uncertainty_m)
        confidence=try c.decodeIfPresent(Double.self,forKey:.confidence);basis=try c.decodeIfPresent(String.self,forKey:.basis)
        observations=try c.decode([FusionObservation].self,forKey:.observations)
        replica_node=try c.decodeIfPresent(String.self,forKey:.replica_node)
        missing_observations=try c.decodeIfPresent([MissingObservation].self,forKey:.missing_observations) ?? []
        original_envelope_sha256=try c.decodeIfPresent(String.self,forKey:.original_envelope_sha256)
        nativeEvidence=try c.decodeIfPresent([DemoNativeEvidence].self,forKey:.native_evidence) ?? []
        guard validNativeEvidence(nativeEvidence,original:original_envelope_sha256) else { throw DemoError.invalidFile }
        guard !state.isEmpty,state.count<=64,label.count<=160,reason.count<=1024,(basis?.count ?? 0)<=512,
              sample_time_s.isFinite,sample_time_s>=0,position.map(validFusionPoint) ?? true,
              uncertainty_m.map({ $0.isFinite && $0>=0 && $0<=100 }) ?? true,
              confidence.map({ $0.isFinite && (0...1).contains($0) }) ?? true,
              observations.count+missing_observations.count<=64,
              Set(observations.map(\.id)+missing_observations.map(\.id)).count==observations.count+missing_observations.count,
              replica_node.map(validReplicaNode) ?? true,
              original_envelope_sha256.map({ $0.utf8.count==64 && $0.utf8.allSatisfy { (48...57).contains($0) || (97...102).contains($0) } }) ?? true else { throw DemoError.invalidFile }
        if let replica_node {
            for observation in observations {
                if let source=observation.source_node,let delivery=observation.delivery {
                    guard (delivery == "local") == (source == replica_node) else { throw DemoError.invalidFile }
                }
                if let path=observation.evidence_path {
                    guard path.hasPrefix("fusion/"+replica_node+"/") else { throw DemoError.invalidFile }
                }
            }
        }
    }
    func expiredDisplay() -> FusionState {
        var copy = self
        copy.state = "expired"; copy.label = "No live hazard result"
        copy.reason = "The latest estimate has expired. Waiting for fresh sensor readings."
        copy.position = nil; copy.uncertainty_m = nil; copy.confidence = nil; copy.basis = nil
        copy.observations = []
        return copy
    }
}

struct FusionObservation: Decodable, Identifiable {
    let id: String; let kind: String; let label: String; let position: [Double]; let target: [Double]?
    let value: Double?; let unit: String?; let active: Bool; let evidence_path: String?
    let source_node: String?; let delivery: String?; let age_s: Double?
    enum CodingKeys: String, CodingKey { case id,kind,label,position,target,value,unit,active,evidence_path,source_node,delivery,age_s }
    init(from decoder: Decoder) throws {
        let c=try decoder.container(keyedBy:CodingKeys.self)
        id=try c.decode(String.self,forKey:.id);kind=try c.decode(String.self,forKey:.kind)
        label=try c.decode(String.self,forKey:.label);position=try c.decode([Double].self,forKey:.position)
        target=try c.decodeIfPresent([Double].self,forKey:.target);value=try c.decodeIfPresent(Double.self,forKey:.value)
        unit=try c.decodeIfPresent(String.self,forKey:.unit);active=try c.decode(Bool.self,forKey:.active)
        evidence_path=try c.decodeIfPresent(String.self,forKey:.evidence_path)
        source_node=try c.decodeIfPresent(String.self,forKey:.source_node)
        delivery=try c.decodeIfPresent(String.self,forKey:.delivery)
        age_s=try c.decodeIfPresent(Double.self,forKey:.age_s)
        guard !id.isEmpty,id.count<=80,!kind.isEmpty,kind.count<=32,label.count<=160,
              validFusionPoint(position),target.map(validFusionPoint) ?? true,value.map(\.isFinite) ?? true,
              (unit?.count ?? 0)<=32,(evidence_path?.count ?? 0)<=512,
              source_node.map(validReplicaNode) ?? true,
              delivery.map({ ["local","received"].contains($0) }) ?? true,
              age_s.map({ $0.isFinite && $0>=0 && $0<=86_400 }) ?? true else { throw DemoError.invalidFile }
        let metadataCount=[source_node != nil,delivery != nil,age_s != nil].filter { $0 }.count
        guard metadataCount==0 || metadataCount==3 else { throw DemoError.invalidFile }
    }
    var symbol: String {
        switch kind { case "camera":return "camera.fill";case "lidar":return "dot.radiowaves.left.and.right"
        case "gas":return "aqi.medium";case "audio":return "waveform";default:return "sensor.fill" }
    }
    var tint: NSColor {
        switch kind { case "camera":return .init(red:0.25,green:0.47,blue:0.75,alpha:1)
        case "lidar":return .init(red:0.12,green:0.56,blue:0.48,alpha:1)
        case "gas":return .init(red:0.74,green:0.42,blue:0.16,alpha:1)
        case "audio":return .init(red:0.53,green:0.38,blue:0.67,alpha:1);default:return .secondaryLabelColor }
    }
    var reading: String? {
        guard let value else{return nil}
        return String(format:abs(value)>=100 ? "%.0f" : "%.1f",value)+(unit.map { " "+$0 } ?? "")
    }
}
struct MissingObservation: Decodable, Identifiable {
    let id:String;let kind:String;let label:String;let source_node:String;let reason:String
    enum CodingKeys:String,CodingKey { case id,kind,label,source_node,reason }
    init(from decoder:Decoder)throws {
        let c=try decoder.container(keyedBy:CodingKeys.self)
        id=try c.decode(String.self,forKey:.id);kind=try c.decode(String.self,forKey:.kind)
        label=try c.decode(String.self,forKey:.label);source_node=try c.decode(String.self,forKey:.source_node)
        reason=try c.decode(String.self,forKey:.reason)
        guard !id.isEmpty,id.count<=80,!kind.isEmpty,kind.count<=32,label.count<=160,
              validReplicaNode(source_node),["not_received","expired"].contains(reason) else { throw DemoError.invalidFile }
    }
    var status:String { reason == "expired" ? "Expired":"Not received" }
}
func validReplicaNode(_ name:String)->Bool {
    !name.isEmpty && name.utf8.count<=64 && name.utf8.allSatisfy { (48...57).contains($0) || (65...90).contains($0) || (97...122).contains($0) || $0==45 || $0==95 }
}
func validFusionPoint(_ p:[Double])->Bool { p.count==3 && p.allSatisfy { $0.isFinite && abs($0)<=10_000 } }

/// Reads only bounded, regular images reached through no-follow session dirfds.
/// A missing/replaced/refused image removes its thumbnail instead of retaining an old frame.
@MainActor final class EvidenceThumbnails {
    private struct Stamp: Equatable { let inode:UInt64;let device:Int32;let size:Int64;let seconds:Int;let nanos:Int;let changeSeconds:Int;let changeNanos:Int }
    private var cache:[String:(Stamp,String,NSImage)]=[:]
    func load(_ relative:String,session:URL,expectedSHA256:String?=nil)->NSImage? {
        let parts=relative.split(separator:"/",omittingEmptySubsequences:false).map(String.init)
        guard relative.utf8.count<=512,!relative.hasPrefix("/"),!relative.contains("\\"),!relative.contains(":"),!relative.contains("\0"),
              (1...8).contains(parts.count),parts.allSatisfy({ !$0.isEmpty && $0 != "." && $0 != ".." }),
              ["png","jpg","jpeg"].contains(URL(fileURLWithPath:relative).pathExtension.lowercased()) else { return nil }
        var directory=Darwin.open(session.path,O_RDONLY|O_DIRECTORY|O_NOFOLLOW)
        guard directory>=0 else { return nil }
        defer { Darwin.close(directory) }
        for part in parts.dropLast() {
            let next=openat(directory,part,O_RDONLY|O_DIRECTORY|O_NOFOLLOW)
            guard next>=0 else { return nil }
            Darwin.close(directory);directory=next
        }
        let fd=openat(directory,parts.last!,O_RDONLY|O_NOFOLLOW|O_NONBLOCK)
        guard fd>=0 else { cache.removeValue(forKey:relative);return nil }
        defer { Darwin.close(fd) }
        var before=stat()
        guard fstat(fd,&before)==0,before.st_mode&S_IFMT==S_IFREG,before.st_uid==geteuid(),before.st_nlink==1,
              before.st_size>0,before.st_size<=4<<20 else { cache.removeValue(forKey:relative);return nil }
        let stamp=Stamp(inode:before.st_ino,device:before.st_dev,size:before.st_size,
                        seconds:before.st_mtimespec.tv_sec,nanos:before.st_mtimespec.tv_nsec,
                        changeSeconds:before.st_ctimespec.tv_sec,changeNanos:before.st_ctimespec.tv_nsec)
        if let old=cache[relative],old.0==stamp,expectedSHA256 == nil || old.1==expectedSHA256 { return old.2 }
        cache.removeValue(forKey:relative)
        var data=Data(count:Int(before.st_size));let expected=data.count
        let count=data.withUnsafeMutableBytes { buffer in
            var offset=0
            while offset<expected {
                let got=Darwin.read(fd,buffer.baseAddress!.advanced(by:offset),expected-offset)
                if got<=0 { return -1 };offset+=got
            }
            return offset
        }
        var after=stat()
        let digest=SHA256.hash(data:data).map { String(format:"%02x",$0) }.joined()
        guard count==expected,fstat(fd,&after)==0,after.st_size==before.st_size,after.st_ino==before.st_ino,
              expectedSHA256 == nil || expectedSHA256==digest,
              after.st_mtimespec.tv_sec==before.st_mtimespec.tv_sec,after.st_mtimespec.tv_nsec==before.st_mtimespec.tv_nsec,
              after.st_ctimespec.tv_sec==before.st_ctimespec.tv_sec,after.st_ctimespec.tv_nsec==before.st_ctimespec.tv_nsec,
              let source=CGImageSourceCreateWithData(data as CFData,[kCGImageSourceShouldCache:false] as CFDictionary),
              CGImageSourceGetCount(source)==1,
              let attributes=CGImageSourceCopyPropertiesAtIndex(source,0,nil) as? [CFString:Any],
              let width=attributes[kCGImagePropertyPixelWidth] as? Int,let height=attributes[kCGImagePropertyPixelHeight] as? Int,
              (1...2048).contains(width),(1...2048).contains(height),
              let image=CGImageSourceCreateThumbnailAtIndex(source,0,[kCGImageSourceCreateThumbnailFromImageAlways:true,
                  kCGImageSourceCreateThumbnailWithTransform:true,kCGImageSourceThumbnailMaxPixelSize:512] as CFDictionary) else { return nil }
        let thumbnail=NSImage(cgImage:image,size:NSSize(width:image.width,height:image.height))
        if cache.count>=8 { cache.removeAll() }
        cache[relative]=(stamp,digest,thumbnail);return thumbnail
    }
}

struct SensorInputsPanel: View {
    @ObservedObject var model:DemoModel
    private let teal=Color(red:0.12,green:0.47,blue:0.43)
    var body:some View {
        VStack(alignment:.leading,spacing:12) {
            Divider()
            Text("SENSOR INPUTS").font(.system(size:9,weight:.semibold)).tracking(1.2).foregroundStyle(.secondary)
            Text("Turn simulated sensor inputs on or off.").font(.system(size:11)).foregroundStyle(.secondary).fixedSize(horizontal:false,vertical:true)
            ForEach(model.sensorSources) { sensor in
                Toggle(isOn:Binding(get:{sensor.enabled},set:{_ in model.command("toggle_sensor",target:sensor.id)})) {
                    VStack(alignment:.leading,spacing:3) {
                        Text(sensor.title).font(.system(size:11,weight:.medium))
                        Text(sensor.enabled ? "Enabled":"Paused").font(.system(size:9)).foregroundStyle(.secondary)
                    }
                }.toggleStyle(.switch).controlSize(.mini).tint(teal)
                    .disabled(!model.canCommand).accessibilityLabel("Enable \(sensor.title)")
            }
            if model.sensorSources.isEmpty {Text("Waiting for source state").font(.system(size:10)).foregroundStyle(.secondary)}
            Text(model.pending == nil ? "Switches show the latest sensor state.":"Waiting for the sensor update.")
                .font(.system(size:9)).foregroundStyle(.secondary).fixedSize(horizontal:false,vertical:true)
        }
    }
}

struct FusionPanel: View {
    @ObservedObject var model:DemoModel
    let fusion:FusionState
    private let teal=Color(red:0.12,green:0.47,blue:0.43)
    private var tint:Color { fusion.localizedHazard ? Color(red:0.75,green:0.37,blue:0.13) : teal }
    var body:some View {
        VStack(alignment:.leading,spacing:13) {
            HStack { Text("SENSOR FUSION").font(.system(size:9,weight:.semibold)).tracking(1.2);Spacer()
                Circle().fill(model.connected ? teal:.orange).frame(width:5,height:5)
            }.foregroundStyle(.secondary)
            if model.liveNativeReplica {
                Label("Gateway B · shared estimate",systemImage:"square.stack.3d.up").font(.system(size:12,weight:.semibold)).foregroundStyle(teal)
                Text("Combined by Gateway A from both gateways' sensors.").font(.system(size:9)).foregroundStyle(.secondary).fixedSize(horizontal:false,vertical:true)
            }
            if let node=fusion.replica_node {
                VStack(alignment:.leading,spacing:4) {
                    Label("\(name(node)) replica",systemImage:"square.stack.3d.up").font(.system(size:13,weight:.semibold))
                    Text("\(fusion.observations.filter { $0.delivery == "local" }.count) local · \(fusion.observations.filter { $0.delivery == "received" }.count) received · \(fusion.missing_observations.count) missing")
                        .font(.system(size:9)).foregroundStyle(.secondary)
                }
            }
            VStack(alignment:.leading,spacing:7) {
                Label(fusion.state.replacingOccurrences(of:"_",with:" ").capitalized,systemImage:fusion.localizedHazard ? "scope":"viewfinder")
                    .font(.system(size:9,weight:.semibold)).foregroundStyle(tint)
                Text(fusion.label).font(.system(size:19,weight:.semibold,design:.rounded)).fixedSize(horizontal:false,vertical:true)
                Text(model.liveNativeReplica ? (fusion.localizedHazard ? "Combined sensor readings support this location.":"Waiting for fresh sensor readings to support an estimate."):fusion.reason).font(.system(size:10)).foregroundStyle(.secondary).fixedSize(horizontal:false,vertical:true).lineSpacing(2)
                HStack(spacing:14) {
                    if let confidence=fusion.confidence { value("Rule support",String(format:"%.0f%%",confidence*100)) }
                    if let uncertainty=fusion.uncertainty_m { value(model.fireResponseMode ? "Fixture projection bound":"Uncertainty",String(format:"± %.1f m",uncertainty)) }
                }.padding(.top,3)
            }.padding(13).frame(maxWidth:.infinity,alignment:.leading)
                .background(tint.opacity(0.065),in:RoundedRectangle(cornerRadius:13))
                .overlay(RoundedRectangle(cornerRadius:13).stroke(tint.opacity(0.15)))
            if fusion.localizedHazard, fusion.original_envelope_sha256 != nil {
                Button { if model.selectHazard() { model.openSelectedEvidence() } } label: { Label("Open supporting evidence",systemImage:"photo.on.rectangle") }
                    .font(.system(size:11,weight:.medium)).buttonStyle(.bordered).tint(teal)
            }
            let cameras=cameraSlots
            if !cameras.isEmpty {
                Text(fusion.replica_node == nil ? "LATEST CAMERA VIEWS":"CAMERAS IN THIS REPLICA").font(.system(size:8,weight:.semibold)).tracking(0.8).foregroundStyle(.secondary)
                HStack(alignment:.top,spacing:7) { ForEach(cameras) { camera in
                    Button { if model.selectHazard(observationID:camera.id) { model.openSelectedEvidence() } } label: { cameraFrame(camera) }.buttonStyle(.plain)
                } }
            }
            VStack(spacing:10) {
                ForEach(fusion.observations.prefix(8)) { observation in
                    Button { model.selectHazard(observationID:observation.id) } label: { HStack(spacing:8) {
                        Image(systemName:observation.symbol).font(.system(size:13)).foregroundStyle(Color(nsColor:observation.tint)).frame(width:20)
                        VStack(alignment:.leading,spacing:2) {
                            Text(observation.label).font(.system(size:10,weight:.medium)).lineLimit(2)
                            Text(origin(observation)).font(.system(size:9)).foregroundStyle(.secondary).fixedSize(horizontal:false,vertical:true)
                        }
                        Spacer(minLength:2)
                        if let reading=observation.reading { Text(reading).font(.system(size:10,weight:.medium,design:.monospaced)).lineLimit(1) }
                    }.opacity(observation.active ? 1:0.55) }.buttonStyle(.plain)
                }
            }
            if !fusion.missing_observations.isEmpty {
                VStack(alignment:.leading,spacing:9) {
                    Text("MISSING OBSERVATIONS").font(.system(size:8,weight:.semibold)).tracking(0.8).foregroundStyle(.secondary)
                    ForEach(fusion.missing_observations.prefix(8)) { missing in
                        HStack(alignment:.top,spacing:8) {
                            Image(systemName:missing.reason == "expired" ? "clock":"arrow.down.circle.dotted").frame(width:20)
                            VStack(alignment:.leading,spacing:2) {
                                Text(missing.label).font(.system(size:10,weight:.medium))
                                Text("\(missing.status) · \(name(missing.source_node))").font(.system(size:9))
                            }
                        }.foregroundStyle(.secondary)
                    }
                }.padding(.top,3)
            }
            if let basis=fusion.basis,!basis.isEmpty { Text(model.liveNativeReplica ? "Camera images belong to the named source gateways.":basis).font(.system(size:9)).foregroundStyle(.secondary).fixedSize(horizontal:false,vertical:true) }
            Text(model.fireResponseMode ? "Pre-rendered optical input":"Simulated gas · generated audio").font(.system(size:9)).foregroundStyle(.secondary)
            Text("Dashed rays show estimated sightlines. Rule support is not a probability.")
                .font(.system(size:9)).foregroundStyle(.secondary).fixedSize(horizontal:false,vertical:true).lineSpacing(2)
            HStack { Text(model.fireResponseMode ? "Simulation display time":(model.connected ? "Observation time":"Last snapshot"));Spacer();Text(String(format:"%.2f s",fusion.sample_time_s)).monospacedDigit() }
                .font(.system(size:9)).foregroundStyle(.secondary)
        }
    }
    private func value(_ label:String,_ reading:String)->some View {
        VStack(alignment:.leading,spacing:3) { Text(label).font(.system(size:8)).foregroundStyle(.secondary);Text(reading).font(.system(size:12,weight:.semibold,design:.monospaced)) }
    }
    private struct CameraSlot:Identifiable { let id:String;let label:String;let image:NSImage?;let unavailable:String }
    private var cameraSlots:[CameraSlot] {
        let present=fusion.observations.filter { $0.kind=="camera" }.map {
            CameraSlot(id:$0.id,label:$0.label,image:model.evidenceImages[$0.id],unavailable:"Frame unavailable")
        }
        let missing=fusion.missing_observations.filter { $0.kind=="camera" }.map {
            CameraSlot(id:$0.id,label:$0.label,image:nil,unavailable:$0.status)
        }
        return Array((present+missing).sorted { $0.id<$1.id }.prefix(2))
    }
    private func name(_ node:String)->String {
        node == "hub" ? "Site hub" : (model.snapshot?.robots.first { $0.id==node }?.name ?? node.capitalized)
    }
    private func origin(_ observation:FusionObservation)->String {
        guard let source=observation.source_node,let delivery=observation.delivery,let age=observation.age_s else {
            return observation.active ? "Contributing":"No current support"
        }
        let provenance=delivery == "local" ? "Local":"Received from "+name(source)
        return provenance+String(format:" · %.1f s old",age)+(observation.active ? "":" · not used")
    }
    private func cameraFrame(_ observation:CameraSlot)->some View {
        VStack(alignment:.leading,spacing:5) {
            ZStack {
                RoundedRectangle(cornerRadius:8).fill(Color(red:0.14,green:0.20,blue:0.22))
                if let image=observation.image { Image(nsImage:image).resizable().scaledToFit() }
                else { VStack(spacing:4) { Image(systemName:"camera");Text(observation.unavailable).font(.system(size:8)) }.foregroundStyle(.white.opacity(0.65)) }
            }.aspectRatio(4/3,contentMode:.fit).clipShape(RoundedRectangle(cornerRadius:8))
                .accessibilityLabel("\(observation.label) camera evidence")
            Text(observation.label).font(.system(size:9,weight:.medium)).lineLimit(1)
        }.frame(maxWidth:.infinity)
    }
}
