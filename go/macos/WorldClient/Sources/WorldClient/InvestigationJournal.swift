import Foundation

/// Durable user intent. Reopening never sends or silently renews a retention request.
public actor InvestigationJournal {
  public nonisolated let scopeID: String
  private let tenant,site: String
  private let file: MissionFile
  private let nowSource: @Sendable () -> Int64
  private var values: [InvestigationEntry]
  private var latest: Int64
  private var storedDigest: String?
  private var busy = Set<String>()
  private struct Saved:Codable { let version:Int;let tenant,site,scopeID:String;let latest:Int64;let entries:[InvestigationEntry] }
  public init(directory:URL,tenant:String,site:String) throws {
    try self.init(directory:directory,tenant:tenant,site:site,nowSource:{Int64(Date().timeIntervalSince1970*1000)})
  }
  init(directory:URL,tenant:String,site:String,nowSource:@escaping @Sendable ()->Int64) throws {
    guard [tenant,site].allSatisfy({!$0.isEmpty && $0.utf8.count<=256}) else {throw InvestigationError.invalid}
    self.tenant=tenant;self.site=site;self.nowSource=nowSource
    scopeID=WorldCredentials.digest(try JSONValue.array([.string("investigations-v1"),.string(tenant),.string(site)]).encoded())
    file=MissionFile(directory:directory,scopeID:scopeID)
    let raw=try file.load();storedDigest=raw.map(WorldCredentials.digest)
    if let raw {
      let saved=try JSONDecoder().decode(Saved.self,from:raw)
      guard saved.version==1,saved.tenant==tenant,saved.site==site,saved.scopeID==scopeID,saved.latest>0,
        saved.entries.count<=32,Set(saved.entries.map(\.id)).count==saved.entries.count else {throw InvestigationError.invalid}
      for entry in saved.entries {try Self.validate(entry,tenant:tenant,site:site)}
      latest=saved.latest
      values=saved.entries.map { entry in
        var e=entry
        for i in e.holds.indices {
          if e.holds[i].state == .sendingCreate {e.holds[i].state = .uncertainCreate}
          if e.holds[i].state == .sendingRelease {e.holds[i].state = .uncertainRelease}
        }
        return e
      }
    } else {values=[];latest=0}
  }
  private func clock() throws -> Int64 {
    let now=nowSource();guard now>0,now>=latest else {throw InvestigationError.clock};latest=now;return now
  }
  private func persist() throws {
    let data=try JSONEncoder().encode(Saved(version:1,tenant:tenant,site:site,scopeID:scopeID,latest:latest,entries:values))
    try file.save(data,replacing:storedDigest);storedDigest=WorldCredentials.digest(data)
  }
  private func replace(_ value:InvestigationEntry) throws {
    guard let i=values.firstIndex(where:{$0.id==value.id}) else {throw InvestigationError.invalid}
    try Self.validate(value,tenant:tenant,site:site)
    let old=values[i];values[i]=value
    do {try persist()} catch {values[i]=old;throw error}
  }
  /// Local expiry is persisted; an observed expiry cannot reopen after viewer restart.
  public func entries() throws -> [InvestigationEntry] {
    let now=try clock()
    let old=values
    for i in values.indices {for h in values[i].holds.indices {
      if values[i].holds[h].request.retainUntilUnixMS<=now && ![.released,.canceled,.expired].contains(values[i].holds[h].state) {
        values[i].holds[h].state = .expired;values[i].holds[h].lastError=nil
      }
    }}
    do {try persist()} catch {values=old;throw error}
    return values
  }
  /// The supplied result is saved historical application context, not new origin authority.
  @discardableResult public func create(name:String,historicalResult:JSONValue,evidence:[InvestigationEvidence],retainUntilUnixMS:Int64) throws -> InvestigationEntry {
    guard values.count<32 else {throw InvestigationError.full}
    let now=try clock(),id=UUID().uuidString.lowercased()
    guard retainUntilUnixMS>now,retainUntilUnixMS-now<=7*86_400_000,
      let task=historicalResult["task_id"]?.string,let run=historicalResult["run_id"]?.string else {throw InvestigationError.invalid}
    let origins=Dictionary(grouping:evidence,by:{$0.origin.id})
    let holds=origins.keys.sorted().map { key -> InvestigationHold in
      let members=origins[key]!,origin=members[0].origin
      return InvestigationHold(origin:origin,request:InvestigationPinRequest(id:id+"-"+String(key.prefix(16)),sha256:Array(Set(members.map(\.sha256))).sorted(),retainUntilUnixMS:retainUntilUnixMS),intent:.retain,state:.pending,createAttempted:false,createAcknowledged:false)
    }
    let entry=InvestigationEntry(id:id,name:name,tenant:tenant,site:site,taskID:task,runID:run,resultSHA256:WorldCredentials.digest(try historicalResult.encoded()),createdUnixMS:now,result:historicalResult,evidence:evidence,holds:holds)
    try Self.validate(entry,tenant:tenant,site:site)
    values.append(entry)
    do {try persist()} catch {values.removeLast();throw error}
    return entry
  }
  public func reconcile(id:String,originID:String,using transport:any InvestigationTransport) async throws {
    guard busy.insert(id).inserted else {throw InvestigationError.busy};defer{busy.remove(id)}
    try await operate(id:id,originID:originID,transport:transport)
  }
  /// Persists release intent before any network action. Unknown creates are resolved first.
  public func release(id:String,originID:String,using transport:any InvestigationTransport) async throws {
    guard busy.insert(id).inserted else {throw InvestigationError.busy};defer{busy.remove(id)}
    var entry=try find(id),index=try holdIndex(entry,originID:originID,transport:transport)
    _=try clock()
    entry.holds[index].intent = .release
    if !entry.holds[index].createAttempted {entry.holds[index].state = .canceled}
    try replace(entry)
    try await operate(id:id,originID:originID,transport:transport)
  }
  private func find(_ id:String) throws -> InvestigationEntry {
    guard let entry=values.first(where:{$0.id==id}) else {throw InvestigationError.invalid};return entry
  }
  private func holdIndex(_ entry:InvestigationEntry,originID:String,transport:any InvestigationTransport) throws -> Int {
    guard let index=entry.holds.firstIndex(where:{$0.id==originID}),entry.holds[index].origin==transport.origin else {throw InvestigationError.originChanged};return index
  }
  private func operate(id:String,originID:String,transport:any InvestigationTransport) async throws {
    var entry=try find(id);let index=try holdIndex(entry,originID:originID,transport:transport)
    var hold=entry.holds[index]
    if [.released,.canceled,.expired].contains(hold.state) {return}
    let now=try clock()
    if hold.request.retainUntilUnixMS<=now {hold.state = .expired;entry.holds[index]=hold;try replace(entry);return}
    do {
      // An unresolved create is replayed with identical body before a release can
      // be confirmed. Unpin404 alone cannot cancel a create that has not arrived.
      if hold.intent == .retain || !hold.createAcknowledged {
        hold.createAttempted=true;hold.state = .sendingCreate;hold.lastError=nil
        entry.holds[index]=hold;try replace(entry)
        let receipt=try await transport.pin(hold.request)
        try receipt.validate(hold.request,releasing:false)
        let finished=try clock()
        hold.checkedAtUnixMS=finished
        if receipt.state=="released" {hold.state = .released;entry.holds[index]=hold;try replace(entry);return}
        hold.createAcknowledged=true
        hold.state=finished<hold.request.retainUntilUnixMS ? .active:.expired
        entry.holds[index]=hold;try replace(entry)
      }
      if hold.intent == .release && hold.state != .expired {
        hold.state = .sendingRelease;hold.lastError=nil;entry.holds[index]=hold;try replace(entry)
        let receipt=try await transport.unpin(hold.request)
        try receipt.validate(hold.request,releasing:true)
        hold.checkedAtUnixMS=try clock();hold.state = .released
        entry.holds[index]=hold;try replace(entry)
      }
    } catch {
      // Never turn a failed local persistence operation into network success.
      // The previously durable sending state is sufficient to recover uncertainty.
      var current=try find(id);var failed=current.holds[index]
      if failed.state == .sendingRelease {failed.state = .uncertainRelease}
      else if failed.state == .sendingCreate {failed.state = .uncertainCreate}
      if case WorldClientError.httpStatus(let status)=error {
        if status==401 || status==403 {failed.state = .denied}
        else if status==404 {failed.state = .unavailable}
      }
      if latest>=failed.request.retainUntilUnixMS {failed.state = .expired}
      failed.lastError=String(error.localizedDescription.prefix(240));current.holds[index]=failed
      try? replace(current)
      throw error
    }
  }
  private static func validate(_ e:InvestigationEntry,tenant:String,site:String) throws {
    guard UUID(uuidString:e.id) != nil,e.id==e.id.lowercased(),!e.name.trimmingCharacters(in:.whitespacesAndNewlines).isEmpty,
      e.name.utf8.count<=120,e.tenant==tenant,e.site==site,e.createdUnixMS>0,
      [e.taskID,e.runID].allSatisfy({!$0.isEmpty && $0.utf8.count<=256}),
      try e.result.encoded().count<=128<<10,WorldCredentials.digest(try e.result.encoded())==e.resultSHA256,
      e.result["task_id"]?.string==e.taskID,e.result["run_id"]?.string==e.runID,
      (1...32).contains(e.evidence.count),Set(e.evidence.map(\.id)).count==e.evidence.count,
      (1...8).contains(e.holds.count),Set(e.holds.map(\.id)).count==e.holds.count else {throw InvestigationError.invalid}
    for item in e.evidence {
      try item.validate()
      guard item.origin.tenant==tenant,item.origin.site==site,
        let sample=e.result["samples"]?.array?.first(where:{$0["sensor_id"]?.string==item.sensorID}),
        sample["evidence"]?["sha256"]?.string==item.sha256,sample["evidence"]?["size"]?.int==Int64(item.size),
        sample["evidence"]?["content_type"]?.string==item.contentType else {throw InvestigationError.invalid}
    }
    guard Set(e.holds.map(\.id))==Set(e.evidence.map{$0.origin.id}) else {throw InvestigationError.invalid}
    for h in e.holds {
      try h.origin.validate();try h.request.validate()
      let members=e.evidence.filter{$0.origin.id==h.id}
      guard members.allSatisfy({$0.origin==h.origin && h.request.retainUntilUnixMS<=$0.originalRetainUntilUnixMS}),
        h.request.id==e.id+"-"+String(h.id.prefix(16)),h.request.sha256==Array(Set(members.map(\.sha256))).sorted(),
        h.request.retainUntilUnixMS>e.createdUnixMS,h.request.retainUntilUnixMS-e.createdUnixMS<=7*86_400_000,
        !h.createAcknowledged || h.createAttempted,
        h.state != .active || h.createAcknowledged,
        h.state != .canceled || (h.intent == .release && !h.createAttempted),
        h.checkedAtUnixMS.map({$0>=e.createdUnixMS}) ?? true else {throw InvestigationError.invalid}
    }
  }
}
