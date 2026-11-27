import Foundation

public struct MissionHistoryScope:Codable,Sendable,Equatable {
  public let origin,principal,assignee,runID:String
  public init(origin:String,principal:String,assignee:String,runID:String)throws{
    guard [origin,principal,assignee,runID].allSatisfy({!$0.isEmpty && $0.utf8.count<=256}) else{throw WorldClientError.invalidResponse}
    self.origin=origin;self.principal=principal;self.assignee=assignee;self.runID=runID
  }
}
public struct MissionHistoryEvent:Sendable,Identifiable {
  public let raw:JSONValue
  public let sequence:UInt64
  public let taskID,actor,event,phase,reason,currentVerification,originalVerification:String
  public let eventUnixMS,exportedUnixMS:Int64
  public var id:UInt64{sequence}
}
public struct MissionHistoryPage:Sendable {
  public let data:Data
  public let raw:JSONValue
  public let events:[MissionHistoryEvent]
  public let next:String
  public let through:UInt64
  public let incarnation:String
  public let lastSyncUnixMS:Int64
  public let via:String
  public let caughtUpWithPeer:Bool
  public init(data:Data,tenant:String,site:String,scope:MissionHistoryScope,taskID:String)throws{
    _=try MissionRawJSON.field(data,path:[],limit:1<<20)
    let v=try JSONValue.decode(data,limit:1<<20)
    guard v["historical"]?.bool==true,v["origin"]?.string==scope.origin,v["run_id"]?.string==scope.runID,
      v["task_id"]?.string==taskID,let next=v["next"]?.string,next.utf8.count<=2048,
      let rows=v["events"]?.array,rows.count<=100,let progress=v["progress"],let through=progress["through"]?.uint,
      let incarnation=progress["incarnation"]?.string,(incarnation.isEmpty && through==0)||WorldClient.isDigest(incarnation),
      let synced=progress["last_sync_unix_ms"]?.int,synced>=0,let via=progress["via"]?.string,
      let caught=progress["caught_up_with_peer"]?.bool else{throw WorldClientError.invalidResponse}
    var events:[MissionHistoryEvent]=[],last:UInt64=0
    for row in rows {
      guard row["executable"]?.bool==false,row["historical_provenance_verified"]?.bool==true,
        let a=row["event"],let r=row["receipt"],r["event"]==a,r["version"]?.int==1,
        r["audience"]?.string==scope.origin,r["assignee"]?.string==scope.assignee,r["run_id"]?.string==scope.runID,r["incarnation"]?.string==incarnation,
        let g=r["grant"]?["grant"],g["tenant"]?.string==tenant,g["site"]?.string==site,g["domain"]?.string=="simulation",g["kind"]?.string=="device",g["principal"]?.string==scope.principal,g["task_status_audience"]?.string==scope.origin,g["task_status_assignee"]?.string==scope.assignee,
        let seq=a["sequence"]?.uint,seq>last,seq<=through,r["sequence"]?.uint==seq,
        let task=a["task_id"]?.string,taskID.isEmpty || task==taskID,
        let actor=a["principal"]?.string,let event=a["event"]?.string,let status=a["status"],let phase=status["Phase"]?.string,let reason=status["Reason"]?.string,
        let wall=a["unix_ms"]?.int,wall>0,let exported=r["exported_unix_ms"]?.int,exported>=wall,
        let current=row["current_verification"]?.string,["current_origin_grant_verified","not_currently_verified","revocation_state_unavailable","origin_grant_expired"].contains(current),
        let original=row["original_instruction_verification"]?.string,["not_present","signature_valid_at_issuance_no_historical_revocation_claim"].contains(original),
        r["signature"]?.string.flatMap({Data(base64Encoded:$0)})?.count==64 else{throw WorldClientError.invalidResponse}
      last=seq;events.append(MissionHistoryEvent(raw:row,sequence:seq,taskID:task,actor:actor,event:event,phase:phase,reason:reason,currentVerification:current,originalVerification:original,eventUnixMS:wall,exportedUnixMS:exported))
    }
    self.data=data;raw=v;self.events=events;self.next=next;self.through=through;self.incarnation=incarnation;lastSyncUnixMS=synced;self.via=via;caughtUpWithPeer=caught
  }
}

/// Uses the existing authenticated reader. The gateway verifies origin proofs;
/// this SDK checks response scope and retains exact bytes, not independent PKI.
public actor MissionHistoryClient {
  public nonisolated let credentials:WorldCredentials
  public nonisolated let scope:MissionHistoryScope
  private var last:Int64=0
  public init(credentials:WorldCredentials,scope:MissionHistoryScope){self.credentials=credentials;self.scope=scope}
  public func page(taskID:String,after:String="")async throws->MissionHistoryPage {
    guard taskID.utf8.count<=256,after.utf8.count<=2048 else{throw WorldClientError.invalidResponse}
    var parts=URLComponents(url:credentials.endpoint.appendingPathComponent("v1/task-history"),resolvingAgainstBaseURL:false)!
    parts.queryItems=[URLQueryItem(name:"origin",value:scope.origin),URLQueryItem(name:"task_id",value:taskID),URLQueryItem(name:"limit",value:"100"),URLQueryItem(name:"after",value:after)]
    let now=Int64(Date().timeIntervalSince1970*1000);guard now>=last else{throw WorldClientError.invalidResponse};last=now
    var q=URLRequest(url:parts.url!,cachePolicy:.reloadIgnoringLocalCacheData,timeoutInterval:3)
    q.setValue(try credentials.signedReadAuthorization(operation:"read",nonce:UUID().uuidString,issuedAtUnixMS:now).base64EncodedString(),forHTTPHeaderField:"X-World-Authorization")
    let data=try await BoundedRequest(limit:1<<20,resourceTimeout:3).run(q)
    let done=Int64(Date().timeIntervalSince1970*1000);guard done>=last else{throw WorldClientError.invalidResponse};last=done
    guard done<credentials.notAfter else{throw WorldClientError.expiredCredentials}
    return try MissionHistoryPage(data:data,tenant:credentials.tenant,site:credentials.site,scope:scope,taskID:taskID)
  }
}
public struct MissionHistoryDocument:Sendable {
  public let taskID:String
  public let pages:[MissionHistoryPage]
  public let fetchedUnixMS:Int64
  public var events:[MissionHistoryEvent]{pages.flatMap(\.events)}
  public var completeLocalView:Bool{pages.last?.next.isEmpty==true}
}
public actor MissionHistoryCache {
  private let file:MissionFile
  private let tenant,site:String
  private let scope:MissionHistoryScope
  private let scopeID:String
  private var stored:String?
  private var saved:Saved
  private struct Entry:Codable{let taskID:String;let pages:[Data];let fetchedUnixMS:Int64}
  private struct Saved:Codable{let version:Int;let scopeID:String;var entries:[Entry]}
  public init(directory:URL,credentials:WorldCredentials,scope:MissionHistoryScope)throws {
    let tenant=credentials.tenant,site=credentials.site
    let identifier=WorldCredentials.digest(try JSONValue.array([.string("mission-history-v1"),.string(credentials.scopeID),.string(scope.origin),.string(scope.principal),.string(scope.assignee),.string(scope.runID)]).encoded())
    let f=MissionFile(directory:directory,scopeID:identifier),raw=try f.load()
    let decoded=try raw.map{try JSONDecoder().decode(Saved.self,from:$0)} ?? Saved(version:1,scopeID:identifier,entries:[])
    guard decoded.version==1,decoded.scopeID==identifier,decoded.entries.count<=32,Set(decoded.entries.map(\.taskID)).count==decoded.entries.count else{throw WorldClientError.invalidCache}
    for e in decoded.entries {_=try Self.document(e,tenant:tenant,site:site,scope:scope)}
    self.tenant=tenant;self.site=site;self.scope=scope;scopeID=identifier;file=f;stored=raw.map(WorldCredentials.digest);saved=decoded
  }
  private static func document(_ e:Entry,tenant:String,site:String,scope:MissionHistoryScope)throws->MissionHistoryDocument{
    guard e.taskID.utf8.count<=256,e.fetchedUnixMS>0,e.pages.count>0,e.pages.count<=4,e.pages.reduce(0,{$0+$1.count})<=3<<20 else{throw WorldClientError.invalidCache}
    let pages=try e.pages.map{try MissionHistoryPage(data:$0,tenant:tenant,site:site,scope:scope,taskID:e.taskID)}
    let rows=pages.flatMap(\.events);guard rows.count<=256,zip(rows,rows.dropFirst()).allSatisfy({$0.0.sequence<$0.1.sequence}),Set(pages.map(\.incarnation)).count==1 else{throw WorldClientError.invalidCache}
    guard pages.dropLast().allSatisfy({!$0.next.isEmpty}),zip(pages,pages.dropFirst()).allSatisfy({$0.0.through <= $0.1.through}) else{throw WorldClientError.invalidCache}
    return MissionHistoryDocument(taskID:e.taskID,pages:pages,fetchedUnixMS:e.fetchedUnixMS)
  }
  public func load(taskID:String)throws->MissionHistoryDocument?{try saved.entries.first{$0.taskID==taskID}.map{try Self.document($0,tenant:tenant,site:site,scope:scope)}}
  public func store(taskID:String,pages:[MissionHistoryPage],fetchedUnixMS:Int64)throws->MissionHistoryDocument{
    let entry=Entry(taskID:taskID,pages:pages.map(\.data),fetchedUnixMS:fetchedUnixMS),doc=try Self.document(entry,tenant:tenant,site:site,scope:scope)
    if let old=saved.entries.first(where:{$0.taskID==taskID}) {
      let before=try Self.document(old,tenant:tenant,site:site,scope:scope)
      guard fetchedUnixMS>=old.fetchedUnixMS,(doc.pages[0].incarnation==before.pages[0].incarnation || (before.pages[0].incarnation.isEmpty && before.pages.last!.through==0)),doc.pages.last!.through>=before.pages.last!.through else{throw WorldClientError.invalidCache}
      for item in before.events {guard let match=doc.events.first(where:{$0.sequence==item.sequence}),match.raw["event"]==item.raw["event"] else{throw WorldClientError.invalidCache}}
    }
    var next=saved;next.entries.removeAll{$0.taskID==taskID};guard next.entries.count<32 else{throw WorldClientError.invalidCache};next.entries.append(entry)
    let data=try JSONEncoder().encode(next);try file.save(data,replacing:stored);stored=WorldCredentials.digest(data);saved=next;return doc
  }
}
