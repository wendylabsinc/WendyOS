import Foundation
import XCTest
@testable import WorldClient

final class IncidentTriggerTests:XCTestCase {
    func component() throws -> JSONValue {
        let source:JSONValue = .object(["device":.string("reader"),"app":.string("incident"),"service":.string("association"),"stream":.string("state")])
        let simulation:JSONValue = .object(["run_id":.string("run-native"),"scenario_version":.string("fixture"),"seed":.integer(73),"clock_domain":.string("simulation:run-native")])
        let inputs=(0..<4).map {JSONValue.object(["id":.string("original-\($0)"),"origin":.string("simulated"),"simulation":simulation])}
        let supports=["gas","visual"].enumerated().map { i,kind -> JSONValue in
            var s=source.object!;s["device"] = .string(kind)
            return .object(["kind":.string(kind),"sha256":.string(String(repeating:i==0 ? "a":"b",count:64)),"source":.object(s),"version":.object(["epoch":.integer(1),"sequence":.integer(2)]),"captured_at":.string("2026-01-01T00:00:00Z"),"valid_until":.string("2026-01-01T00:00:04Z")])
        }
        return .object(["key":.object(["tenant":.string("tenant"),"site":.string("office"),"entity":.string("run-native/incident-inspection"),"type":.string(IncidentTriggerWire.schema),"source":source]),"version":.object(["epoch":.integer(1),"sequence":.integer(2)]),"frame":.object(["id":.string("office-floor"),"revision":.string("office-frame-v1")]),"timing":.object(["sample_time":.string("2026-01-01T00:00:00Z"),"clock_domain":.string("simulation:run-native"),"error_known":.bool(true),"error_bound_ns":.integer(0),"age_hint_ns":.integer(0)]),"ttl_ns":.integer(4_000_000_000),"provenance":.object(["origin":.string("inferred"),"app_version":.string("world-incident-inspection-v1"),"model_id":.string("incident-inspection-selection"),"model_version":.string("1"),"inputs":.array(inputs)]),"payload":.object(["schema":.string(IncidentTriggerWire.schema),"incident_sha256":.string(String(repeating:"c",count:64)),"contract_sha256":.string(String(repeating:"d",count:64)),"input_digest":.string(String(repeating:"e",count:64)),"state":.string("consistent"),"position_m":.array([.number(1.25),.number(0.2),.integer(0)]),"uncertainty_m":.number(0.1),"supports":.array(supports),"valid_until":.string("2026-01-01T00:00:04Z")]),"tombstone":.bool(false)])
    }
    func testSubMillisecondSpatialDeadlinesAreExact()throws{
        var c=try component().object!,t=c["timing"]!.object!,p=c["payload"]!.object!
        let sample:JSONValue = .string("2026-01-01T00:00:00.0001Z"),end:JSONValue = .string("2026-01-01T01:00:01.0002+01:00")
        t["sample_time"]=sample;c["timing"] = .object(t);c["ttl_ns"] = .integer(1_000_100_000);p["valid_until"]=end
        p["supports"] = .array(p["supports"]!.array!.map{item in var support=item.object!;support["captured_at"]=sample;support["valid_until"]=end;return .object(support)})
        c["payload"] = .object(p)
        XCTAssertNoThrow(try IncidentTriggerWire.review(.object(c),fixedTarget:[1.25,0.2,0],destination:nil))
        var supports=p["supports"]!.array!,one=supports[0].object!;one["valid_until"] = .string("2026-01-01T00:00:01.000199999Z");supports[0] = .object(one);p["supports"] = .array(supports);c["payload"] = .object(p)
        XCTAssertThrowsError(try IncidentTriggerWire.review(.object(c),fixedTarget:[1.25,0.2,0],destination:nil))
    }
    func trigger(_ c:JSONValue) throws -> Data {
        let bytes=try NativeTriggerWire.component(c)
        return GoAuthorityJSON.object([("sha256",GoAuthorityJSON.string(WorldCredentials.digest(bytes))),("component",bytes)])
    }
    func spec(_ raw:Data,target:[Double]=[1.25,0.2,0]) throws -> Data {
        let url=Bundle.module.url(forResource:"native-mission-v1",withExtension:"json",subdirectory:"Fixtures")!
        var s=try JSONValue.decode(Data(contentsOf:url))["spec"]!.object!
        s["target"] = .array(target.map(JSONValue.number));s["trigger"] = try JSONValue.decode(raw)
        return try MissionProof.specBytes(.object(s),rawTrigger:raw)
    }
    func testCanonicalIncidentAndStatusKeepHistoricalBytes() throws {
        let raw=try trigger(component());let selected=try MissionNativeTrigger(canonical:raw,fixedTarget:[1.25,0.2,0],originalEnvelopeSHA256:String(repeating:"f",count:64))
        XCTAssertEqual(selected.resultSchema,IncidentTriggerWire.schema)
        let instruction=try spec(raw);XCTAssertEqual(try MissionRawJSON.field(instruction,path:["trigger"]),raw)
        XCTAssertEqual(try MissionProof.specBytes(instruction),instruction)
        XCTAssertThrowsError(try spec(raw,target:[2,0.2,0]))
    }
    func testPayloadAndStatusRefuseAdversarialIncident() throws {
        for name in ["null-position","unknown","unknown-support","null-support","duplicate-support","duplicate-ancestry","wrong-run","changed-deadline"] {
            var c=try component().object!;var p=c["payload"]!.object!;var ancestry=c["provenance"]!.object!
            switch name {
            case "null-position":p["position_m"] = .array([.null,.number(0.2),.integer(0)])
            case "unknown":p["approved"] = .bool(true)
            case "unknown-support":var s=p["supports"]!.array!;var one=s[0].object!;one["trusted"] = .bool(true);s[0] = .object(one);p["supports"] = .array(s)
            case "null-support":var s=p["supports"]!.array!;var one=s[0].object!;one["source"] = .null;s[0] = .object(one);p["supports"] = .array(s)
            case "duplicate-support":let s=p["supports"]!.array!;p["supports"] = .array([s[0],s[0]])
            case "duplicate-ancestry":var leaves=ancestry["inputs"]!.array!;leaves[1]=leaves[0];ancestry["inputs"] = .array(leaves)
            case "wrong-run":var leaves=ancestry["inputs"]!.array!;var leaf=leaves[0].object!;var sim=leaf["simulation"]!.object!;sim["run_id"] = .string("other");leaf["simulation"] = .object(sim);leaves[0] = .object(leaf);ancestry["inputs"] = .array(leaves)
            default:p["valid_until"] = .string("2026-01-01T00:00:05Z")
            };c["payload"] = .object(p);c["provenance"] = .object(ancestry)
            // Preserve adversarial payload bytes while allowing outer canonical
            // hash recomputation: rejection must be semantic, not an old digest.
            do {let raw=try trigger(.object(c));XCTAssertThrowsError(try MissionNativeTrigger(canonical:raw,fixedTarget:[1.25,0.2,0],originalEnvelopeSHA256:String(repeating:"f",count:64)),name);XCTAssertThrowsError(try spec(raw),name)} catch { /* strict payload canonicalization refused first */ }
        }
    }
}
