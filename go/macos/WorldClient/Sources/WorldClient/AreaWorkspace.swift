import Foundation

public enum AreaError: Error, LocalizedError, Sendable {
    case invalid, incomplete, changed, unsupported, clock
    public var errorDescription: String? {
        switch self {
        case .invalid: "The area or spatial response is invalid."
        case .incomplete: "The refresh was incomplete. The previous complete snapshot is retained."
        case .changed: "The selected view changed. Refresh again; pages were not combined."
        case .unsupported: "Area workspaces require an explicit supported layer and a standalone World endpoint."
        case .clock: "The saved snapshot clock cannot be verified. Records remain historical."
        }
    }
}

public struct AreaRegion: Codable, Sendable, Equatable {
    public let frameID, frameRevision: String
    public let minXZ, maxXZ: [Double]
    public init(frameID: String, frameRevision: String, minXZ: [Double], maxXZ: [Double]) throws {
        self.frameID=frameID; self.frameRevision=frameRevision; self.minXZ=minXZ; self.maxXZ=maxXZ
        try validate()
    }
    public func validate() throws {
        guard [frameID,frameRevision].allSatisfy({!$0.isEmpty && $0.utf8.count<=256 && $0.trimmingCharacters(in:.whitespacesAndNewlines)==$0}),
              minXZ.count==2,maxXZ.count==2,(minXZ+maxXZ).allSatisfy({$0.isFinite && abs($0)<=1_000_000}),
              minXZ[0]<maxXZ[0],minXZ[1]<maxXZ[1] else {throw AreaError.invalid}
    }
    var json: JSONValue { .object(["frame":.object(["id":.string(frameID),"revision":.string(frameRevision)]),"min_xz":.array(minXZ.map(JSONValue.number)),"max_xz":.array(maxXZ.map(JSONValue.number))]) }
}

public struct AreaDefinition: Codable, Sendable, Equatable, Identifiable {
    public static let supportedTypes = ["wendy.sensor.v1","wendy.hazard.v1","wendy.annotation.note.v1","wendy.annotation.note.v2"]
    public let id, name: String
    public let region: AreaRegion
    public let types: [String]
    public init(id:String=UUID().uuidString.lowercased(),name:String,region:AreaRegion,types:[String]) throws {
        self.id=id;self.name=name;self.region=region;self.types=types.sorted();try validate()
    }
    public func validate() throws {
        try region.validate()
        guard UUID(uuidString:id)?.uuidString.lowercased()==id,!name.trimmingCharacters(in:.whitespacesAndNewlines).isEmpty,name.utf8.count<=120,
              !types.isEmpty,types==types.sorted(),Set(types).count==types.count,types.allSatisfy(Self.supportedTypes.contains) else {throw AreaError.invalid}
    }
    var selection:JSONValue {.object(["types":.array(types.map(JSONValue.string)),"region":region.json])}
    public var selectionDigest:String {WorldCredentials.digest((try? selection.encoded()) ?? Data())}
}

public struct AreaRecord: Sendable, Identifiable {
    public let component:WorldComponent
    public let descriptor:JSONValue
    public let observedAt:Date
    public var id:String {descriptor["key"]!.string!}
    public var located:Bool {descriptor["class"]?.string=="located"}
    public var position:[Double]? {descriptor["position_m"]?.array?.compactMap(\.double)}
    public var uncertainty:Double? {descriptor["uncertainty_m"]?.double}
    public var expiresAt:Date {AreaTime.parse(descriptor["expires_at"]!.string!)!}
    public var reason:String {descriptor["reason"]!.string!}
}

// Complete response pages are retained verbatim. The client checks hashes,
// located geometry and scope. Full sensor-schema classification and origin
// authority are delegated to the authenticated gateway. Unlocated rows remain
// visible separately; this metadata does not replace origin proofs.
public struct AreaSnapshot: Codable, Sendable, Equatable {
    public let definition:AreaDefinition
    public let scopeID,tenant,site:String
    public let pages:[Data]
    public let receivedAt:Date
    public let bootID:String
    public let receivedNS:Int64
    public let requestStartedNS:Int64
    public let requestStartedAt:Date
    public init(definition:AreaDefinition,scopeID:String,tenant:String,site:String,pages:[Data],requestStartedAt:Date,requestStartedClock:AnnotationClockReading) throws {
        let reading=try AnnotationMonotonicClock.read()
        self.definition=definition;self.scopeID=scopeID;self.tenant=tenant;self.site=site;self.pages=pages
        self.receivedAt=Date();self.requestStartedAt=requestStartedAt;self.bootID=reading.bootID;self.receivedNS=reading.nanoseconds
        guard requestStartedClock.bootID==reading.bootID,requestStartedClock.nanoseconds<=reading.nanoseconds else{throw AreaError.clock};self.requestStartedNS=requestStartedClock.nanoseconds
        _=try records()
    }
    public var partialReplica:Bool {
        guard let page=pages.first,let value=try? JSONValue.decode(page,limit:256<<10) else{return true}
        return value["replica_scope"]?["partial"]?.bool==true
    }
    public func records() throws -> [AreaRecord] {
        try definition.validate()
        guard WorldClient.isDigest(scopeID),!tenant.isEmpty,!site.isEmpty,!pages.isEmpty,pages.count<=32,
              pages.reduce(0,{$0+$1.count})<=2<<20,receivedAt>=requestStartedAt,receivedNS>=0,requestStartedNS>=0,requestStartedNS<=receivedNS,!bootID.isEmpty else {throw AreaError.invalid}
        var result:[AreaRecord]=[],revision:String?,previous=""
        for (index,page) in pages.enumerated() {
            let parsed=try AreaPage(page,definition:definition,tenant:tenant,site:site,maxBytes:256<<10)
            guard revision==nil || revision==parsed.revision,parsed.complete==(index==pages.count-1) else {throw AreaError.changed}
            revision=parsed.revision
            for row in parsed.records {guard row.id>previous else {throw AreaError.invalid};previous=row.id;result.append(row)}
        }
        guard result.count<=512 else {throw AreaError.incomplete}
        return result
    }
    /// Offline means historical even before its cap. A different boot or backward
    /// clock never makes saved records current. Elapsed suspension consumes TTL.
    public func expired(_ record:AreaRecord,at now:Date=Date()) -> Bool {
        guard let current=try? AnnotationMonotonicClock.read() else{return true}
        return expired(record,at:now,reading:current)
    }
    func expired(_ record:AreaRecord,at now:Date,reading current:AnnotationClockReading) -> Bool {
        guard current.bootID==bootID,current.nanoseconds>=receivedNS,now>=receivedAt,
              let ttl=record.component.raw["ttl_ns"]?.int,let age=record.component.raw["timing"]?["age_hint_ns"]?.int,
              ttl>0,age>=0,age<ttl else{return true}
        let remaining=min(record.expiresAt.timeIntervalSince(record.observedAt),Double(ttl-age)/1e9)
        // Server and client wall clocks need not agree. Consume the server's
        // remaining lifetime from before the whole request using continuous time.
        let elapsed=Double(current.nanoseconds-requestStartedNS)/1e9
        return remaining<=0 || elapsed>=remaining

    }
}

struct AreaPage {
    let records:[AreaRecord],revision:String,next:String
    let complete:Bool
    init(_ bytes:Data,definition:AreaDefinition,tenant:String,site:String,maxBytes:Int,decodeTime:(String)->Date? = AreaTime.parse) throws {
        guard bytes.count<=maxBytes else{throw AreaError.invalid}
        let value=try JSONValue.decode(bytes,limit:maxBytes)
        let rawComponents=try AreaRawJSON.elements(MissionRawJSON.field(bytes,path:["components"],limit:maxBytes))
        guard value["version"]?.int==1,let revision=value["revision"]?.string,!revision.isEmpty,revision.utf8.count<=4096,
              let complete=value["complete"]?.bool,let observed=value["observed_at"]?.string.flatMap(decodeTime),
              let components=value["components"]?.array,components.count<=128,rawComponents.count==components.count else{throw AreaError.invalid}
        let descriptors=value["spatial"]?.array ?? []
        let next=value["next_cursor"]?.string ?? ""
        guard descriptors.count==components.count,next.utf8.count<=4096,complete==next.isEmpty,complete || !components.isEmpty else{throw AreaError.incomplete}
        if let scope=value["replica_scope"],scope != .null {
            guard scope["partial"]?.bool==true,scope["tenant"]?.string==tenant,scope["site"]?.string==site,
                  let digest=scope["policy_sha256"]?.string,WorldClient.isDigest(digest),scope["selection"]?.object != nil,
                  scope["selection"]?["region"]==nil else{throw AreaError.invalid}
        }
        var records:[AreaRecord]=[],previous=""
        for i in components.indices {
            let c=components[i],d=descriptors[i],raw=rawComponents[i]
            let keyBytes=try MissionRawJSON.field(raw,path:["key"],limit:maxBytes)
            let key=WorldCredentials.digest(keyBytes)
            guard c["key"]?["tenant"]?.string==tenant,c["key"]?["site"]?.string==site,
                  let type=c["key"]?["type"]?.string,definition.types.contains(type),c["tombstone"]?.bool != true,
                  c["frame"]?["id"]?.string==definition.region.frameID,c["frame"]?["revision"]?.string==definition.region.frameRevision,
                  key>previous,d["key"]?.string==key,d["content_sha256"]?.string==WorldCredentials.digest(raw),
                  let expiry=d["expires_at"]?.string.flatMap(decodeTime),expiry>observed,
                  let ttl=c["ttl_ns"]?.int,let age=c["timing"]?["age_hint_ns"]?.int,ttl>0,age>=0,age<ttl,expiry<=observed.addingTimeInterval(Double(ttl-age)/1e9+0.000001),
                  let reason=d["reason"]?.string,!reason.isEmpty,reason.utf8.count<=256,
                  ["located","unlocated"].contains(d["class"]?.string ?? "") else{throw AreaError.invalid}
            previous=key
            try Self.validatePosition(c,d,region:definition.region)
            records.append(AreaRecord(component:try WorldComponent(raw:c),descriptor:d,observedAt:observed))
        }
        self.records=records;self.revision=revision;self.next=next;self.complete=complete
    }
    private static func validatePosition(_ c:JSONValue,_ d:JSONValue,region:AreaRegion) throws {
        let payload=c["payload"],type=c["key"]?["type"]?.string ?? ""
        if d["class"]?.string=="unlocated" {
            guard d["position_m"]==nil,d["uncertainty_m"]==nil else{throw AreaError.invalid}
            // Hazard location has no additional sensor-schema binding. Its
            // complete declared bounds must not be silently downgraded.
            if type=="wendy.hazard.v1",let p=payload?["position_m"]?.array,p.count==3,p.compactMap(\.double).count==3,p.compactMap(\.double).allSatisfy({$0.isFinite && abs($0)<=1e6}),let u=payload?["uncertainty_m"]?.double,u.isFinite,u>=0,u<=1e6 {throw AreaError.invalid}
            return
        }
        guard let position=d["position_m"]?.array?.compactMap(\.double),position.count==3,position.allSatisfy({$0.isFinite && abs($0)<=1e6}),
              let radius=d["uncertainty_m"]?.double,radius.isFinite,radius>=0,radius<=1e6 else{throw AreaError.invalid}
        if type.hasPrefix("wendy.annotation.note.") {
            guard let mm=payload?["position_mm"]?.array,mm.count==3,mm.compactMap(\.int).count==3,
                  mm.compactMap(\.double).map({$0/1000})==position,radius==0 else{throw AreaError.invalid}
        } else {
            guard payload?["position_m"]?.array?.compactMap(\.double)==position,payload?["uncertainty_m"]?.double==radius else{throw AreaError.invalid}
            if type=="wendy.sensor.v1" {
                guard payload?["schema_version"]?.string=="wendy.sensor.sample.v1",payload?["frame"]==c["frame"],payload?["site_id"]==c["key"]?["site"],payload?["run_id"]==c["provenance"]?["simulation"]?["run_id"],payload?["sensor_id"]==c["key"]?["source"]?["service"] else{throw AreaError.invalid}
            }
        }
        guard position[0]+radius>=region.minXZ[0],position[0]-radius<=region.maxXZ[0],position[2]+radius>=region.minXZ[1],position[2]-radius<=region.maxXZ[1] else{throw AreaError.invalid}
    }
}

enum AreaRawJSON {
    static func elements(_ data:Data) throws -> [Data] {
        guard let array=try JSONValue.decode(data,limit:256<<10).array,array.count<=128 else{throw AreaError.invalid}
        _=try MissionRawJSON.field(data,path:[],limit:256<<10)
        let bytes=Array(data);var depth=0,quoted=false,escape=false,start:Int?,out:[Data]=[]
        for (i,b) in bytes.enumerated() {
            if quoted {if escape{escape=false}else if b==92{escape=true}else if b==34{quoted=false};continue}
            if b==34{quoted=true}
            if b==123 || b==91 {if depth==1{start=i};depth+=1}
            if b==125 || b==93 {depth-=1;if depth==1,let s=start {out.append(Data(bytes[s...i]));start=nil}}
        }
        guard out.count==array.count else{throw AreaError.invalid};return out
    }
}

extension WorldClient {
    public func readArea(_ definition:AreaDefinition,pageSize:Int=32) async throws -> AreaSnapshot {
        try definition.validate();guard (1...128).contains(pageSize) else{throw AreaError.invalid}
        return try await collectArea(definition,pageSize:pageSize,first:nil,started:Date(),startedClock:try AnnotationMonotonicClock.read())
    }
    public func watchArea(_ previous:AreaSnapshot,waitMS:Int=3000) async throws -> AreaSnapshot? {
        _=try previous.records()
        guard previous.scopeID==credentials.scopeID,(0...5000).contains(waitMS),let first=previous.pages.first,let revision=try JSONValue.decode(first)["revision"]?.string else{throw AreaError.invalid}
        let body=try JSONValue.object(["version":.integer(1),"selection":previous.definition.selection,"revision":.string(revision),"wait_ms":.integer(Int64(waitMS)),"page_size":.integer(32),"max_bytes":.integer(256<<10)]).encoded()
        let raw=try await request(path:"/v1/watch/selected",body:body,limit:256<<10)
        let page=try AreaPage(raw,definition:previous.definition,tenant:credentials.tenant,site:credentials.site,maxBytes:256<<10)
        if try JSONValue.decode(raw)["timed_out"]?.bool==true {guard page.revision==revision else{throw AreaError.changed};return nil}
        // A watch is a change notification. Requery immediately so its long wait
        // does not consume the lifetime of a newly arrived short-lived record.
        return try await readArea(previous.definition,pageSize:32)
    }
    private func collectArea(_ definition:AreaDefinition,pageSize:Int,first:Data?,started:Date,startedClock:AnnotationClockReading) async throws -> AreaSnapshot {
        var pages:[Data]=[],cursor="",revision:String?,buffered=first
        repeat {
            try Task.checkCancellation()
            let body=try JSONValue.object(["version":.integer(1),"selection":definition.selection,"cursor":.string(cursor),"page_size":.integer(Int64(pageSize)),"max_bytes":.integer(256<<10)]).encoded()
            let raw:Data
            if let first=buffered {raw=first;buffered=nil} else {raw=try await request(path:"/v1/query",body:body,limit:256<<10)}
            let page=try AreaPage(raw,definition:definition,tenant:credentials.tenant,site:credentials.site,maxBytes:256<<10)
            guard revision==nil || revision==page.revision else{throw AreaError.changed};revision=page.revision
            pages.append(raw);cursor=page.next
            guard pages.count<=32,pages.reduce(0,{$0+$1.count})<=2<<20 else{throw AreaError.incomplete}
        } while !cursor.isEmpty
        return try AreaSnapshot(definition:definition,scopeID:credentials.scopeID,tenant:credentials.tenant,site:credentials.site,pages:pages,requestStartedAt:started,requestStartedClock:startedClock)
    }
}

public struct AreaWorkspace:Codable,Sendable,Identifiable,Equatable {
    public let definition:AreaDefinition
    public var snapshot:AreaSnapshot?
    public var id:String {definition.id}
}
public actor AreaJournal {
    private let file:MissionFile
    private let scopeID:String
    private var entries:[AreaWorkspace]
    private var digest:String?
    public init(directory:URL,scopeID:String) throws {
        guard WorldClient.isDigest(scopeID) else{throw AreaError.invalid}
        self.scopeID=scopeID;file=MissionFile(directory:directory,scopeID:scopeID)
        let data=try file.load();digest=data.map(WorldCredentials.digest)
        entries=try data.map{try JSONDecoder().decode([AreaWorkspace].self,from:$0)} ?? []
        guard entries.count<=12,Set(entries.map(\.id)).count==entries.count else{throw AreaError.invalid}
        for e in entries {try e.definition.validate();if let s=e.snapshot {guard s.definition==e.definition,s.scopeID==scopeID else{throw AreaError.invalid};_=try s.records()}}
    }
    public func list()->[AreaWorkspace]{entries}
    private func persist(_ next:[AreaWorkspace]) throws {
        let raw=try JSONEncoder().encode(next);try file.save(raw,replacing:digest);digest=WorldCredentials.digest(raw);entries=next
    }
    public func create(_ definition:AreaDefinition) throws {
        try definition.validate();guard entries.count<12,!entries.contains(where:{$0.id==definition.id}) else{throw AreaError.invalid}
        try persist(entries+[AreaWorkspace(definition:definition,snapshot:nil)])
    }
    public func install(_ snapshot:AreaSnapshot) throws {
        _=try snapshot.records()
        guard snapshot.scopeID==scopeID,let i=entries.firstIndex(where:{$0.definition==snapshot.definition}) else{throw AreaError.invalid}
        var next=entries;next[i].snapshot=snapshot;try persist(next)
    }
    public func remove(_ id:String) throws {guard entries.contains(where:{$0.id==id}) else{throw AreaError.invalid};try persist(entries.filter{$0.id != id})}
}


enum AreaTime {
    static func parse(_ value:String)->Date? {
        // ISO8601DateFormatter truncates fractional seconds to milliseconds on
        // supported platforms; that can reject a valid nanosecond expiry cap.
        if let precise=try? Date.ISO8601FormatStyle(includingFractionalSeconds:true).parse(value) {return precise}
        return WorldComponent.parseTime(value)
    }
}

extension AreaRegion {
    /// Shared by the native drag callback and its scripted acceptance. Drawing
    /// creates a draft only; the caller must explicitly save the selection.
    public static func fromCanvasDrag(frameID:String,frameRevision:String,viewportMin:[Double],viewportMax:[Double],size:[Double],start:[Double],end:[Double]) throws -> AreaRegion {
        guard [viewportMin,viewportMax,size,start,end].allSatisfy({$0.count==2 && $0.allSatisfy(\.isFinite)}),size.allSatisfy({$0>50 && $0<100000}),viewportMin[0]<viewportMax[0],viewportMin[1]<viewportMax[1],
              (0...size[0]).contains(start[0]),(0...size[1]).contains(start[1]),(0...size[0]).contains(end[0]),(0...size[1]).contains(end[1]) else{throw AreaError.invalid}
        let scale=min((size[0]-50)/(viewportMax[0]-viewportMin[0]),(size[1]-50)/(viewportMax[1]-viewportMin[1]))
        guard scale.isFinite,scale>0 else{throw AreaError.invalid}
        let a=[viewportMin[0]+(start[0]-25)/scale,viewportMin[1]+(start[1]-25)/scale]
        let b=[viewportMin[0]+(end[0]-25)/scale,viewportMin[1]+(end[1]-25)/scale]
        func mm(_ value:Double)->Double {(value*1000).rounded()/1000}
        return try AreaRegion(frameID:frameID,frameRevision:frameRevision,minXZ:[mm(min(a[0],b[0])),mm(min(a[1],b[1]))],maxXZ:[mm(max(a[0],b[0])),mm(max(a[1],b[1]))])
    }
}
