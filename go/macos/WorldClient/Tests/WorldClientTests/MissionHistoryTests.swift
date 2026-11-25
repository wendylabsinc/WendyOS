import XCTest
@testable import WorldClient

final class MissionHistoryTests:XCTestCase {
  let scope=try! MissionHistoryScope(origin:"origin-a",principal:"device-a",assignee:"robot/nav",runID:"run-a")
  func value(task:String="task",seq:Int64=1,through:Int64=1,next:String="")->JSONValue{
    let event:JSONValue = .object(["sequence":.integer(seq),"unix_ms":.integer(1000),"task_id":.string(task),"principal":.string("operator"),"event":.string("accepted"),"status":.object(["Phase":.string("accepted"),"Reason":.string("")])])
    let receipt:JSONValue = .object(["version":.integer(1),"grant":.object(["grant":.object(["tenant":.string("tenant"),"site":.string("office"),"domain":.string("simulation"),"kind":.string("device"),"principal":.string("device-a"),"task_status_audience":.string("origin-a"),"task_status_assignee":.string("robot/nav")])]),"event":event,"audience":.string("origin-a"),"assignee":.string("robot/nav"),"run_id":.string("run-a"),"incarnation":.string(String(repeating:"a",count:64)),"sequence":.integer(seq),"exported_unix_ms":.integer(2000),"signature":.string(Data(repeating:0,count:64).base64EncodedString())])
    return .object(["origin":.string("origin-a"),"run_id":.string("run-a"),"task_id":.string(task),"historical":.bool(true),"next":.string(next),"progress":.object(["through":.integer(through),"incarnation":.string(String(repeating:"a",count:64)),"last_sync_unix_ms":.integer(3000),"via":.string("relay"),"caught_up_with_peer":.bool(true)]),"events":.array([.object(["receipt":receipt,"event":event,"executable":.bool(false),"historical_provenance_verified":.bool(true),"current_verification":.string("current_origin_grant_verified"),"original_instruction_verification":.string("not_present")])])])
  }
  func page(_ v:JSONValue)throws->MissionHistoryPage{try MissionHistoryPage(data:v.encoded(),tenant:"tenant",site:"office",scope:scope,taskID:"task")}
  func testScopeAndNonExecution()throws{
    let v=value();XCTAssertEqual(try page(v).events.first?.event,"accepted")
    for key in ["origin","run_id","task_id"] {var o=v.object!;o[key] = .string("other");XCTAssertThrowsError(try page(.object(o)))}
    var o=v.object!,row=o["events"]!.array![0].object!;row["executable"] = .bool(true);o["events"] = .array([.object(row)]);XCTAssertThrowsError(try page(.object(o)))
    row["executable"] = .bool(false);var a=row["event"]!.object!;a["event"] = .string("stopped");row["event"] = .object(a);o["events"] = .array([.object(row)]);XCTAssertThrowsError(try page(.object(o)))
  }
  func testDuplicatesOrderingAndBounds()throws{
    var v=value().object!;v["events"] = .array([v["events"]!.array![0],v["events"]!.array![0]]);XCTAssertThrowsError(try page(.object(v)))
    let raw=try value().encoded(),bad=Data("{\"origin\":\"other\",".utf8)+raw.dropFirst();XCTAssertThrowsError(try MissionHistoryPage(data:bad,tenant:"tenant",site:"office",scope:scope,taskID:"task"))
    XCTAssertThrowsError(try page(value(next:String(repeating:"a",count:2049))))
  }
  func testDurableExactCacheAndStaleWriter()async throws{
    let helper=InvestigationTests(),dir=FileManager.default.temporaryDirectory.appendingPathComponent("history-tests-"+UUID().uuidString),credentials=try WorldCredentials(data:helper.credentials(scopes:["read"]))
    try FileManager.default.createDirectory(at:dir,withIntermediateDirectories:false,attributes:[.posixPermissions:0o700]);addTeardownBlock{try? FileManager.default.removeItem(at:dir)}
    let one=try MissionHistoryCache(directory:dir,credentials:credentials,scope:scope),stale=try MissionHistoryCache(directory:dir,credentials:credentials,scope:scope)
    let p=try page(value()),doc=try await one.store(taskID:"task",pages:[p],fetchedUnixMS:4000)
    XCTAssertEqual(doc.events.count,1)
    do{_=try await stale.store(taskID:"task",pages:[p],fetchedUnixMS:4001);XCTFail("stale writer replaced cache")}catch{}
    let reopened=try MissionHistoryCache(directory:dir,credentials:credentials,scope:scope),loaded=try await reopened.load(taskID:"task")
    XCTAssertEqual(loaded?.pages[0].data,p.data)
    var changed=value().object!,row=changed["events"]!.array![0].object!,a=row["event"]!.object!,r=row["receipt"]!.object!;a["event"] = .string("stopped");row["event"] = .object(a);r["event"] = .object(a);row["receipt"] = .object(r);changed["events"] = .array([.object(row)])
    do{_=try await reopened.store(taskID:"task",pages:[page(.object(changed))],fetchedUnixMS:4002);XCTFail("rewrote immutable event")}catch{}
    let absent=try await reopened.load(taskID:"another-task");XCTAssertNil(absent)
  }
  func testEmptyViewDoesNotPinUnprovenIncarnation()async throws{
    let helper=InvestigationTests(),dir=FileManager.default.temporaryDirectory.appendingPathComponent("history-empty-"+UUID().uuidString),credentials=try WorldCredentials(data:helper.credentials(scopes:["read"]))
    try FileManager.default.createDirectory(at:dir,withIntermediateDirectories:false,attributes:[.posixPermissions:0o700]);addTeardownBlock{try? FileManager.default.removeItem(at:dir)}
    let cache=try MissionHistoryCache(directory:dir,credentials:credentials,scope:scope)
    var empty=value().object!,progress=empty["progress"]!.object!;progress["through"] = .integer(0);progress["incarnation"] = .string("");empty["progress"] = .object(progress);empty["events"] = .array([])
    _=try await cache.store(taskID:"task",pages:[page(.object(empty))],fetchedUnixMS:4000)
    let populated=try await cache.store(taskID:"task",pages:[page(value())],fetchedUnixMS:5000)
    XCTAssertEqual(populated.events.count,1)
  }
  func testActualLoopbackHistoryWhenConfigured()async throws{
    guard let path=ProcessInfo.processInfo.environment["WORLD_HISTORY_CREDENTIALS"] else{throw XCTSkip("explicit actual-node fixture required")}
    let c=try WorldCredentials(data:Data(contentsOf:URL(fileURLWithPath:path)))
    let s=try MissionHistoryScope(origin:"node-1",principal:"lab/node-1",assignee:"sim-robot/navigation",runID:"mission-history-demo")
    let p=try await MissionHistoryClient(credentials:c,scope:s).page(taskID:"review-inspection")
    XCTAssertEqual(p.events.map(\.event),["accepted","stop"]);XCTAssertTrue(p.next.isEmpty)
  }
}
