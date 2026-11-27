import XCTest
@testable import WorldClient

final class AreaWorkspaceTests:XCTestCase {
    func testAreaBoundsAndExplicitLayers() throws {
        let r=try AreaRegion(frameID:"floor",frameRevision:"1",minXZ:[0,0],maxXZ:[2,3])
        XCTAssertThrowsError(try AreaRegion(frameID:"floor",frameRevision:"",minXZ:[0,0],maxXZ:[2,3]))
        XCTAssertThrowsError(try AreaRegion(frameID:"floor",frameRevision:"1",minXZ:[0,0],maxXZ:[0,3]))
        XCTAssertThrowsError(try AreaDefinition(name:"area",region:r,types:["arbitrary"]))
        XCTAssertThrowsError(try AreaDefinition(name:"area",region:r,types:[]))
        XCTAssertThrowsError(try AreaDefinition(name:"area",region:r,types:["wendy.sensor.v1","wendy.sensor.v1"]))
    }
    func testDurableNamesAndStaleWriterNeverOverwrite() async throws {
        let dir=FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer{try? FileManager.default.removeItem(at:dir)}
        let scope=String(repeating:"a",count:64)
        let a=try AreaJournal(directory:dir,scopeID:scope),b=try AreaJournal(directory:dir,scopeID:scope)
        let region=try AreaRegion(frameID:"floor",frameRevision:"1",minXZ:[0,0],maxXZ:[2,3])
        let first=try AreaDefinition(name:"first",region:region,types:["wendy.sensor.v1"])
        try await a.create(first)
        do {try await b.create(AreaDefinition(name:"stale",region:region,types:["wendy.sensor.v1"]));XCTFail("stale writer") } catch {}
        let reopened=try AreaJournal(directory:dir,scopeID:scope)
        let list=await reopened.list();XCTAssertEqual(list.count,1);XCTAssertEqual(list.first?.definition,first)
        XCTAssertNil(list.first?.snapshot)
    }
    func testRawArrayPreservesContentBytesAndRejectsDuplicateKeys() throws {
        let raw=Data("[{\"x\":1,\"s\":\"a,}\\\"b\"},{\"x\":2}]".utf8)
        let pieces=try AreaRawJSON.elements(raw)
        XCTAssertEqual(pieces.count,2)
        XCTAssertEqual(String(data:pieces[1],encoding:.utf8),"{\"x\":2}")
        XCTAssertThrowsError(try AreaRawJSON.elements(Data("[{\"x\":1,\"x\":2}]".utf8)))
    }
}

extension AreaWorkspaceTests {
    private static func samplePage(serverNow:Date,remaining:Double,complete:Bool=true,revision:String="view-one") throws -> Data {
        let key=Data(#"{"tenant":"tenant","site":"site","entity":"hazard","type":"wendy.hazard.v1","source":{"device":"d","app":"a","service":"s","stream":"x"}}"#.utf8)
        let raw=Data(("{\"key\":"+String(decoding:key,as:UTF8.self)+#", "frame":{"id":"floor","revision":"1"},"timing":{"age_hint_ns":0},"ttl_ns":5000000000,"payload":{"position_m":[1,0,1],"uncertainty_m":0.1}}"#).utf8)
        let format=ISO8601DateFormatter();format.formatOptions=[.withInternetDateTime,.withFractionalSeconds]
        let descriptor:JSONValue = .object(["key":.string(WorldCredentials.digest(key)),"content_sha256":.string(WorldCredentials.digest(raw)),"class":.string("located"),"reason":.string("declared bounds"),"position_m":.array([.integer(1),.integer(0),.integer(1)]),"uncertainty_m":.number(0.1),"expires_at":.string(format.string(from:serverNow.addingTimeInterval(remaining)))])
        return Data(("{\"version\":1,\"components\":["+String(decoding:raw,as:UTF8.self)+"],\"spatial\":["+String(decoding:try descriptor.encoded(),as:UTF8.self)+"],\"revision\":\""+revision+"\",\"observed_at\":\""+format.string(from:serverNow)+"\",\"complete\":"+(complete ? "true":"false")+",\"next_cursor\":\""+(complete ? "":"next-page")+"\"}").utf8)
    }
    func testServerClockAheadCannotExtendOriginalRemainingLifetime() throws {
        let started=try AnnotationMonotonicClock.read(),now=Date()
        let definition=try AreaDefinition(name:"clock",region:AreaRegion(frameID:"floor",frameRevision:"1",minXZ:[0,0],maxXZ:[2,2]),types:["wendy.hazard.v1"])
        let page=try Self.samplePage(serverNow:now.addingTimeInterval(60),remaining:2)
        let snapshot=try AreaSnapshot(definition:definition,scopeID:String(repeating:"a",count:64),tenant:"tenant",site:"site",pages:[page],requestStartedAt:now,requestStartedClock:started)
        let row=try XCTUnwrap(snapshot.records().first)
        XCTAssertFalse(snapshot.expired(row,at:snapshot.receivedAt,reading:AnnotationClockReading(bootID:started.bootID,nanoseconds:snapshot.receivedNS)))
        XCTAssertTrue(snapshot.expired(row,at:snapshot.receivedAt.addingTimeInterval(3),reading:AnnotationClockReading(bootID:started.bootID,nanoseconds:snapshot.receivedNS+3_000_000_000)))
        XCTAssertTrue(snapshot.expired(row,at:snapshot.receivedAt,reading:AnnotationClockReading(bootID:"other-boot",nanoseconds:snapshot.receivedNS)))
        XCTAssertTrue(snapshot.expired(row,at:snapshot.receivedAt.addingTimeInterval(-1),reading:AnnotationClockReading(bootID:started.bootID,nanoseconds:snapshot.receivedNS)))
    }
    func testIncompleteAndMixedGenerationCannotReplaceSavedView() async throws {
        let dir=FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString);defer{try? FileManager.default.removeItem(at:dir)}
        let scope=String(repeating:"a",count:64),journal=try AreaJournal(directory:dir,scopeID:scope)
        let d=try AreaDefinition(name:"saved",region:AreaRegion(frameID:"floor",frameRevision:"1",minXZ:[0,0],maxXZ:[2,2]),types:["wendy.hazard.v1"])
        try await journal.create(d)
        let started=try AnnotationMonotonicClock.read(),now=Date()
        let good=try AreaSnapshot(definition:d,scopeID:scope,tenant:"tenant",site:"site",pages:[Self.samplePage(serverNow:now,remaining:4)],requestStartedAt:now,requestStartedClock:started)
        try await journal.install(good)
        XCTAssertThrowsError(try AreaSnapshot(definition:d,scopeID:scope,tenant:"tenant",site:"site",pages:[Self.samplePage(serverNow:now,remaining:4,complete:false)],requestStartedAt:now,requestStartedClock:started))
        XCTAssertThrowsError(try AreaSnapshot(definition:d,scopeID:scope,tenant:"tenant",site:"site",pages:[Self.samplePage(serverNow:now,remaining:4,complete:false),Self.samplePage(serverNow:now,remaining:4,revision:"other-view")],requestStartedAt:now,requestStartedClock:started))
        let reopened=try AreaJournal(directory:dir,scopeID:scope);let rows=await reopened.list();XCTAssertEqual(rows.first?.snapshot,good)
    }
}

extension AreaWorkspaceTests {
    func testWatchWaitDoesNotConsumeNewShortLivedRow() async throws {
        let server=try LoopbackServer{request in
            if request.path=="/v1/watch/selected"{Thread.sleep(forTimeInterval:1)}
            let page=try? Self.samplePage(serverNow:Date(),remaining:0.5)
            return .init(body:page ?? Data())
        }
        defer{server.stop()}
        var cfg=try ProtocolTests.vectors()[0]["credentials"]!.object!,signed=cfg["grant"]!.object!,grant=signed["grant"]!.object!
        grant["tenant"] = .string("tenant");grant["site"] = .string("site");grant["scopes"] = .array([.string("read")])
        grant["not_before_unix_ms"] = .integer(Int64(Date().addingTimeInterval(-60).timeIntervalSince1970*1000));grant["not_after_unix_ms"] = .integer(Int64(Date().addingTimeInterval(60).timeIntervalSince1970*1000))
        signed["grant"] = .object(grant);cfg["grant"] = .object(signed);cfg["url"] = .string(server.url.absoluteString)
        let credentials=try WorldCredentials(data:JSONValue.object(cfg).encoded()),client=WorldClient(credentials:credentials)
        let def=try AreaDefinition(name:"Watch",region:AreaRegion(frameID:"floor",frameRevision:"1",minXZ:[0,0],maxXZ:[2,2]),types:["wendy.hazard.v1"])
        let before=try await client.readArea(def)
        let changed=try await client.watchArea(before,waitMS:1500)
        let after=try XCTUnwrap(changed),record=try XCTUnwrap(after.records().first)
        XCTAssertFalse(after.expired(record))
        XCTAssertEqual(server.requests.map(\.path),["/v1/query","/v1/watch/selected","/v1/query"])
    }
}

extension AreaWorkspaceTests {
    func testUsableHazardCannotBeDowngradedToUnlocated() throws {
        let def=try AreaDefinition(name:"classification",region:AreaRegion(frameID:"floor",frameRevision:"1",minXZ:[0,0],maxXZ:[2,2]),types:["wendy.hazard.v1"])
        let bytes=try Self.samplePage(serverNow:Date(),remaining:3)
        var root=try JSONValue.decode(bytes).object!,row=root["components"]!.array![0],descriptor=root["spatial"]!.array![0].object!
        descriptor["class"] = .string("unlocated");descriptor.removeValue(forKey:"position_m");descriptor.removeValue(forKey:"uncertainty_m")
        // Rebind hashes so this exercises classification, not hash rejection.
        descriptor["key"] = .string(WorldCredentials.digest(try row["key"]!.encoded()))
        descriptor["content_sha256"] = .string(WorldCredentials.digest(try row.encoded()))
        root["spatial"] = .array([.object(descriptor)])
        XCTAssertThrowsError(try AreaPage(JSONValue.object(root).encoded(),definition:def,tenant:"tenant",site:"site",maxBytes:256<<10))
    }
}

extension AreaWorkspaceTests {
    func testNanosecondGatewayExpiryCapDoesNotRoundUp() throws {
        let def=try AreaDefinition(name:"precision",region:AreaRegion(frameID:"floor",frameRevision:"1",minXZ:[0,0],maxXZ:[2,2]),types:["wendy.hazard.v1"])
        var root=try JSONValue.decode(Self.samplePage(serverNow:Date(),remaining:3)).object!
        var row=root["components"]!.array![0].object!,descriptor=root["spatial"]!.array![0].object!
        row["timing"] = .object(["age_hint_ns":.integer(4_876_543_000)])
        root["observed_at"] = .string("2026-11-25T06:10:30.999999Z")
        descriptor["expires_at"] = .string("2026-11-25T06:10:31.123456Z")
        descriptor["key"] = .string(WorldCredentials.digest(try row["key"]!.encoded()))
        descriptor["content_sha256"] = .string(WorldCredentials.digest(try JSONValue.object(row).encoded()))
        root["components"] = .array([.object(row)]);root["spatial"] = .array([.object(descriptor)])
        let page=try AreaPage(JSONValue.object(root).encoded(),definition:def,tenant:"tenant",site:"site",maxBytes:256<<10)
        let record=try XCTUnwrap(page.records.first)
        let legacy=WorldComponent.parseTime("2026-11-25T06:10:31.123456Z")!.timeIntervalSince(WorldComponent.parseTime("2026-11-25T06:10:30.999999Z")!)
        XCTAssertGreaterThan(legacy,0.123457+0.000001,"old parser reproduction must exceed the exact cap")
        XCTAssertEqual(record.expiresAt.timeIntervalSince(record.observedAt),0.123457,accuracy:0.000001)
    }
}

extension AreaWorkspaceTests {
    func testActualGoNanosecondResponseInteroperability() throws {
        guard let path=ProcessInfo.processInfo.environment["AREA_GO_PRECISION_FIXTURE"] else{throw XCTSkip("supply private exact Go TestRegionGoPrecisionFixture response")}
        let bytes=try Data(contentsOf:URL(fileURLWithPath:path))
        let def=try AreaDefinition(name:"Go precision",region:AreaRegion(frameID:"floor",frameRevision:"1",minXZ:[0,0],maxXZ:[2,2]),types:["wendy.hazard.v1"])
        XCTAssertThrowsError(try AreaPage(bytes,definition:def,tenant:"tenant",site:"office",maxBytes:256<<10,decodeTime:WorldComponent.parseTime))
        let accepted=try AreaPage(bytes,definition:def,tenant:"tenant",site:"office",maxBytes:256<<10)
        XCTAssertEqual(accepted.records.count,1)
    }
}

extension AreaWorkspaceTests {
    func testNativeDrawingUsesExactFrameAndExplicitOrderedDraft() throws {
        let scale=550.0/9.0
        let region=try AreaRegion.fromCanvasDrag(frameID:"floor",frameRevision:"2",viewportMin:[-2,-2],viewportMax:[7,7],size:[1000,600],start:[25+7*scale,25+7*scale],end:[25+2*scale,25+2*scale])
        XCTAssertEqual(region.minXZ,[0,0]);XCTAssertEqual(region.maxXZ,[5,5]);XCTAssertEqual(region.frameRevision,"2")
        XCTAssertThrowsError(try AreaRegion.fromCanvasDrag(frameID:"floor",frameRevision:"2",viewportMin:[-2,-2],viewportMax:[7,7],size:[1000,600],start:[20,20],end:[20,20]))
        XCTAssertThrowsError(try AreaRegion.fromCanvasDrag(frameID:"floor",frameRevision:"2",viewportMin:[-2,-2],viewportMax:[7,7],size:[1000,600],start:[-1,20],end:[200,200]))
    }
}
