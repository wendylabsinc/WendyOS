import CryptoKit
import XCTest
@testable import WorldClient

final class InvestigationTests:XCTestCase {
  final class Clock:@unchecked Sendable {
    private let lock=NSLock();private var value:Int64
    init(_ value:Int64){self.value=value}
    func read()->Int64{lock.lock();defer{lock.unlock()};return value}
    func set(_ n:Int64){lock.lock();value=n;lock.unlock()}
  }
  actor Transport:InvestigationTransport {
    nonisolated let origin:InvestigationOrigin
    var pins:[InvestigationPinRequest]=[],releases:[InvestigationPinRequest]=[]
    var state="active",loseCreate=false,loseRelease=false
    let directory:URL?
    init(_ origin:InvestigationOrigin,directory:URL?=nil){self.origin=origin;self.directory=directory}
    func lose(create:Bool=false,release:Bool=false){loseCreate=create;loseRelease=release}
    func pin(_ r:InvestigationPinRequest) async throws->InvestigationPinReceipt {
      if let directory {try persisted(r,intent:nil,state:"sendingCreate",directory:directory)}
      pins.append(r)
      if loseCreate {loseCreate=false;throw URLError(.networkConnectionLost)}
      return InvestigationPinReceipt(id:r.id,sha256:r.sha256,retainUntilUnixMS:r.retainUntilUnixMS,state:state)
    }
    func unpin(_ r:InvestigationPinRequest) async throws->InvestigationPinReceipt {
      if let directory {try persisted(r,intent:"release",state:"sendingRelease",directory:directory)}
      releases.append(r);state="released"
      if loseRelease {loseRelease=false;throw URLError(.networkConnectionLost)}
      return InvestigationPinReceipt(id:r.id,sha256:r.sha256,retainUntilUnixMS:r.retainUntilUnixMS,state:"released")
    }
    func persisted(_ r:InvestigationPinRequest,intent:String?,state:String,directory:URL) throws {
      let files=try FileManager.default.contentsOfDirectory(at:directory,includingPropertiesForKeys:nil).filter{$0.pathExtension=="json"}
      let envelope=try JSONValue.decode(Data(contentsOf:files[0]))
      let raw=Data(base64Encoded:envelope["payload"]!.string!)!
      let v=try JSONValue.decode(raw),h=v["entries"]!.array![0]["holds"]!.array![0]
      guard h["state"]?.string==state,h["request"]?["id"]?.string==r.id,
        intent==nil || h["intent"]?.string==intent else {throw InvestigationError.invalid}
    }
    func counts()->(Int,Int){(pins.count,releases.count)}
    func requests()->[InvestigationPinRequest]{pins+releases}
  }
  func directory() throws->URL {
    let p=FileManager.default.temporaryDirectory.appendingPathComponent("investigation-tests-"+UUID().uuidString)
    try FileManager.default.createDirectory(at:p,withIntermediateDirectories:false,attributes:[.posixPermissions:0o700])
    addTeardownBlock{try? FileManager.default.removeItem(at:p)};return p
  }
  func credentials(scopes:[String],audience:String="capture-a") throws->Data {
    let key=Curve25519.Signing.PrivateKey(),now=Int64(Date().timeIntervalSince1970*1000)
    let g:JSONValue = .object(["version":.integer(1),"id":.string("pin-test"),"tenant":.string("tenant"),"site":.string("office"),"domain":.string("simulation"),"principal":.string("operator"),"kind":.string("operator"),"source":.object(["device":.string("operator"),"app":.string("viewer"),"service":.string("retention"),"stream":.string("main")]),"public_key":.string(key.publicKey.rawRepresentation.base64EncodedString()),"scopes":.array(scopes.map(JSONValue.string)),"publish_audience":.string("capture-a"),"not_before_unix_ms":.integer(now-1000),"not_after_unix_ms":.integer(now+3600000)])
    return try JSONValue.object(["url":.string("http://127.0.0.1:12345"),"audience":.string(audience),"allow_insecure_simulation":.bool(true),"grant":.object(["grant":g,"signature":.string(Data(repeating:0,count:64).base64EncodedString())]),"private_key":.string((key.rawRepresentation+key.publicKey.rawRepresentation).base64EncodedString())]).encoded()
  }
  func origin() throws->InvestigationOrigin {
    try InvestigationClient(label:"Capture A",credentials:InvestigationCredentials(data:credentials(scopes:["evidence-pin"])),readCredentials:WorldCredentials(data:credentials(scopes:["read","evidence-read"]))).origin
  }
  func result(_ digest:String)->JSONValue {
    .object([
      "task_id":.string("inspection-1"), "run_id":.string("run-1"),
      "samples":.array([.object([
        "sensor_id":.string("camera-a"),
        "evidence":.object(["sha256":.string(digest),"size":.integer(3),"content_type":.string("image/png")])
      ])])
    ])
  }
  func create(_ j:InvestigationJournal,_ o:InvestigationOrigin,now:Int64) async throws->InvestigationEntry {
    let digest=String(repeating:"a",count:64)
    return try await j.create(name:"Leaking valve",historicalResult:result(digest),evidence:[InvestigationEvidence(origin:o,sensorID:"camera-a",sha256:digest,size:3,contentType:"image/png",originalRetainUntilUnixMS:now+20000)],retainUntilUnixMS:now+10000)
  }
  func testSeparateCapabilitiesAndOriginBinding() throws {
    XCTAssertThrowsError(try InvestigationCredentials(data:credentials(scopes:["read","evidence-read"])))
    XCTAssertThrowsError(try InvestigationCredentials(data:credentials(scopes:["evidence-pin","read"])))
    XCTAssertThrowsError(try InvestigationCredentials(data:credentials(scopes:["evidence-pin"],audience:"capture-b")))
    XCTAssertThrowsError(try WorldCredentials(data:credentials(scopes:["evidence-pin"])))
    let pin=try InvestigationCredentials(data:credentials(scopes:["evidence-pin"]))
    XCTAssertThrowsError(try InvestigationClient(label:"A",credentials:pin,readCredentials:WorldCredentials(data:credentials(scopes:["read","evidence-read"],audience:"capture-b"))))
    XCTAssertEqual(try origin().backend,"standalone-worldd")
  }
  func testLostCreateReopensAndReleaseResolvesExactRequestBeforeUnpin() async throws {
    let dir=try directory(),o=try origin(),now:Int64=1_800_000_000_000,c=Clock(now)
    let journal=try InvestigationJournal(directory:dir,tenant:"tenant",site:"office",nowSource:{c.read()})
    let entry=try await create(journal,o,now:now),t=Transport(o,directory:dir)
    await t.lose(create:true)
    do{try await journal.reconcile(id:entry.id,originID:o.id,using:t);XCTFail("lost acknowledgement accepted")}catch{}
    let restored=try InvestigationJournal(directory:dir,tenant:"tenant",site:"office",nowSource:{c.read()})
    let restoredValues=try await restored.entries();XCTAssertEqual(restoredValues.first?.holds.first?.state,.uncertainCreate)
    try await restored.release(id:entry.id,originID:o.id,using:t)
    let saved=try await restored.entries();XCTAssertEqual(saved[0].holds[0].state,.released)
    let requests=await t.requests();XCTAssertEqual(requests.count,3);XCTAssertTrue(requests.allSatisfy{$0==entry.holds[0].request})
  }
  func testLostReleaseNeverRecreatesAfterRestart() async throws {
    let dir=try directory(),o=try origin(),now:Int64=1_800_000_000_000,c=Clock(now)
    let j=try InvestigationJournal(directory:dir,tenant:"tenant",site:"office",nowSource:{c.read()}),t=Transport(o,directory:dir)
    let e=try await create(j,o,now:now);try await j.reconcile(id:e.id,originID:o.id,using:t)
    await t.lose(release:true)
    do{try await j.release(id:e.id,originID:o.id,using:t);XCTFail("lost release accepted")}catch{}
    let reopened=try InvestigationJournal(directory:dir,tenant:"tenant",site:"office",nowSource:{c.read()})
    try await reopened.reconcile(id:e.id,originID:o.id,using:t)
    let counts=await t.counts();XCTAssertEqual(counts.0,1);XCTAssertEqual(counts.1,2)
    let reopenedValues=try await reopened.entries();XCTAssertEqual(reopenedValues[0].holds[0].state,.released)
  }
  func testCancelBeforeDeliverySendsNothingAndOriginChangeRefused() async throws {
    let dir=try directory(),o=try origin(),now:Int64=1_800_000_000_000,c=Clock(now)
    let j=try InvestigationJournal(directory:dir,tenant:"tenant",site:"office",nowSource:{c.read()}),t=Transport(o)
    let e=try await create(j,o,now:now),other=Transport(try origin())
    do{try await j.reconcile(id:e.id,originID:o.id,using:other);XCTFail("origin replacement accepted")}catch{}
    try await j.release(id:e.id,originID:o.id,using:t)
    let counts=await t.counts();XCTAssertEqual(counts.0+counts.1,0)
    let canceledValues=try await j.entries();XCTAssertEqual(canceledValues[0].holds[0].state,.canceled)
  }
  func testExpiryPersistsAndBackwardClockCannotReopenIt() async throws {
    let dir=try directory(),o=try origin(),now:Int64=1_800_000_000_000,c=Clock(now)
    let j=try InvestigationJournal(directory:dir,tenant:"tenant",site:"office",nowSource:{c.read()})
    _=try await create(j,o,now:now);c.set(now+11000)
    let expiredValues=try await j.entries();XCTAssertEqual(expiredValues[0].holds[0].state,.expired)
    c.set(now+1)
    let reopened=try InvestigationJournal(directory:dir,tenant:"tenant",site:"office",nowSource:{c.read()})
    do{_=try await reopened.entries();XCTFail("rollback accepted")}catch{}
    c.set(now+12000);let reopenedValues=try await reopened.entries();XCTAssertEqual(reopenedValues[0].holds[0].state,.expired)
  }
  func testConcurrentJournalAndUnsafePermissionsCannotSend() async throws {
    let dir=try directory(),o=try origin(),now:Int64=1_800_000_000_000,c=Clock(now)
    let a=try InvestigationJournal(directory:dir,tenant:"tenant",site:"office",nowSource:{c.read()})
    let b=try InvestigationJournal(directory:dir,tenant:"tenant",site:"office",nowSource:{c.read()})
    let e=try await create(a,o,now:now)
    do{_=try await create(b,o,now:now);XCTFail("stale journal overwrote intent")}catch{}
    try FileManager.default.setAttributes([.posixPermissions:0o755],ofItemAtPath:dir.path)
    let t=Transport(o)
    do{try await a.reconcile(id:e.id,originID:o.id,using:t);XCTFail("unsafe persistence accepted")}catch{}
    let counts=await t.counts();XCTAssertEqual(counts.0+counts.1,0)
  }
  func testChangedMetadataAndExtendedDeadlineRefusedBeforeSave() async throws {
    let dir=try directory(),o=try origin(),now:Int64=1_800_000_000_000,c=Clock(now)
    let j=try InvestigationJournal(directory:dir,tenant:"tenant",site:"office",nowSource:{c.read()})
    let digest=String(repeating:"a",count:64)
    let ref=try InvestigationEvidence(origin:o,sensorID:"camera-a",sha256:digest,size:3,contentType:"image/png",originalRetainUntilUnixMS:now+100)
    do{_=try await j.create(name:"A",historicalResult:result(digest),evidence:[ref],retainUntilUnixMS:now+101);XCTFail("extension accepted")}catch{}
    do{_=try await j.create(name:"A",historicalResult:result(String(repeating:"b",count:64)),evidence:[ref],retainUntilUnixMS:now+99);XCTFail("changed digest accepted")}catch{}
    let empty=try await j.entries();XCTAssertTrue(empty.isEmpty)
  }
  func testCachedMediaReadStillRequiresUnexpiredSeparateReadGrant() async throws {
    var raw=try JSONValue.decode(credentials(scopes:["read","evidence-read"])).object!
    var signed=raw["grant"]!.object!,grant=signed["grant"]!.object!
    let now=Int64(Date().timeIntervalSince1970*1000)
    grant["not_before_unix_ms"] = .integer(now-10000);grant["not_after_unix_ms"] = .integer(now-1000)
    signed["grant"] = .object(grant);raw["grant"] = .object(signed)
    let client=try InvestigationClient(label:"Capture A",credentials:InvestigationCredentials(data:credentials(scopes:["evidence-pin"])),readCredentials:WorldCredentials(data:JSONValue.object(raw).encoded()))
    do {try await client.authorizeCachedRead();XCTFail("expired separate read capability accepted")}catch{}
  }

}
