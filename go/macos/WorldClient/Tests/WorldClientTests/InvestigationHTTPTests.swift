import Foundation
import XCTest
@testable import WorldClient

/// Opt-in actual Go capture-store interoperability; the harness owns loopback nodes.
final class InvestigationHTTPTests:XCTestCase {
  func testActualCaptureRetention() async throws {
    guard let path=ProcessInfo.processInfo.environment["INVESTIGATION_HTTP_FIXTURE"] else {throw XCTSkip("run tests/check_investigation_http.py with disposable real capture nodes")}
    let root=URL(fileURLWithPath:path)
    func client(_ name:String)throws->InvestigationClient {
      try InvestigationClient(label:name,credentials:InvestigationCredentials(data:Data(contentsOf:root.appendingPathComponent(name+"-pin.json"))),readCredentials:WorldCredentials(data:Data(contentsOf:root.appendingPathComponent(name+"-read.json"))))
    }
    func mode(_ name:String,_ v:[String:Bool])throws {try JSONSerialization.data(withJSONObject:v).write(to:root.appendingPathComponent(name+"-proxy/mode.json"),options:.atomic)}
    func hostAction(_ name:String) async throws {
      try Data().write(to:root.appendingPathComponent("request-"+name),options:.atomic)
      let timeout=Date().addingTimeInterval(8)
      while !FileManager.default.fileExists(atPath:root.appendingPathComponent("done-"+name).path) {
        guard Date()<timeout else {throw InvestigationError.invalid}
        try await Task.sleep(for:.milliseconds(30))
      }
    }
    let a=try client("capture-a"),b=try client("capture-b"),digest=WorldCredentials.digest(Data("abc".utf8))
    let reference=try MissionInspectionEvidence(sha256:digest,contentType:"image/png",size:3)
    let ea=try await a.evidence(reference),eb=try await b.evidence(reference)
    let directory=root.appendingPathComponent("http-journal")
    try FileManager.default.createDirectory(at:directory,withIntermediateDirectories:false,attributes:[.posixPermissions:0o700])
    let journal=try InvestigationJournal(directory:directory,tenant:"lab",site:"synthetic-office")
    let raw=InvestigationTests().result(digest)
    let until=Int64(Date().timeIntervalSince1970*1000)+60000
    let refs=try [InvestigationEvidence(origin:a.origin,sensorID:"camera-a",sha256:digest,size:3,contentType:"image/png",originalRetainUntilUnixMS:Int64(ea.retainUntil.timeIntervalSince1970*1000)),InvestigationEvidence(origin:b.origin,sensorID:"camera-a",sha256:digest,size:3,contentType:"image/png",originalRetainUntilUnixMS:Int64(eb.retainUntil.timeIntervalSince1970*1000))]
    let entry=try await journal.create(name:"Actual two-store HTTP",historicalResult:raw,evidence:refs,retainUntilUnixMS:until)
    try mode("capture-a",["drop_pin":true])
    do {try await journal.reconcile(id:entry.id,originID:a.origin.id,using:a);XCTFail("lost acknowledgement accepted")}catch{}
    try mode("capture-a",[:]);try mode("capture-b",["partition":true])
    do {try await journal.reconcile(id:entry.id,originID:b.origin.id,using:b);XCTFail("partition accepted")}catch{}
    let reopen=try InvestigationJournal(directory:directory,tenant:"lab",site:"synthetic-office")
    var rows=try await reopen.entries()
    XCTAssertEqual(rows[0].holds.first{$0.id==a.origin.id}?.state,.uncertainCreate)
    try await reopen.reconcile(id:entry.id,originID:a.origin.id,using:a)
    try mode("capture-b",[:]);try await reopen.reconcile(id:entry.id,originID:b.origin.id,using:b)
    rows=try await reopen.entries();XCTAssertTrue(rows[0].holds.allSatisfy{$0.state == .active})
    try await hostAction("restart-a")
    try await reopen.reconcile(id:entry.id,originID:a.origin.id,using:a)
    rows=try await reopen.entries();XCTAssertEqual(rows[0].holds.first{$0.id==a.origin.id}?.state,.active)
    try mode("capture-a",["drop_unpin":true])
    do {try await reopen.release(id:entry.id,originID:a.origin.id,using:a);XCTFail("lost release acknowledged")}catch{}
    try mode("capture-a",[:])
    let again=try InvestigationJournal(directory:directory,tenant:"lab",site:"synthetic-office")
    try await again.release(id:entry.id,originID:a.origin.id,using:a)
    try await again.release(id:entry.id,originID:b.origin.id,using:b)
    rows=try await again.entries();XCTAssertTrue(rows[0].holds.allSatisfy{$0.state == .released})
    XCTAssertEqual(rows[0].holds.map(\.request),entry.holds.map(\.request))
    // A released immutable identity cannot be reactivated by exact retry.
    let receipt=try await a.pin(entry.holds.first{$0.id==a.origin.id}!.request)
    XCTAssertEqual(receipt.state,"released")
    let second=InvestigationPinRequest(id:"bounded-second",sha256:[digest],retainUntilUnixMS:until)
    _=try await a.pin(second)
    do {_=try await a.pin(InvestigationPinRequest(id:"quota-third",sha256:[digest],retainUntilUnixMS:until));XCTFail("pin quota exceeded")}catch{}
    let stillActive=try await a.pin(second);XCTAssertEqual(stillActive.state,"active")
    for fault in ["changed_digest","changed_deadline"] {
      try mode("capture-a",[fault:true])
      do {_=try await a.pin(second);XCTFail("changed receipt accepted")}catch{}
      try mode("capture-a",[:])
    }
    try await hostAction("revoke-a")
    do {_=try await a.pin(second);XCTFail("revoked pin authority accepted")}catch{}
    // Existing hold is not a read capability: the separate reader still verifies media.
    let afterRevocation=try await a.evidence(reference);XCTAssertEqual(afterRevocation.sha256,digest)
    let short=InvestigationPinRequest(id:"short-deadline",sha256:[digest],retainUntilUnixMS:Int64(Date().timeIntervalSince1970*1000)+300)
    try mode("capture-b",["delay_response":true])
    do {_=try await b.pin(short);XCTFail("response crossing original expiry accepted")}catch{}
    try mode("capture-b",[:])
    do {_=try await b.pin(short);XCTFail("expired original deadline reused")}catch{}
    try await hostAction("wrong-authority")
    try await hostAction("quota-pressure")
    let preserved=try await a.evidence(reference);XCTAssertEqual(preserved.sha256,digest)
  }
}
