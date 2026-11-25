import Foundation
import Compression
import XCTest
@testable import WorldClient
final class RisingIncidentTests:XCTestCase {
 func testOriginalNanosecondDeadlineDoesNotLoseFractionalDigits()throws{
  let sample:JSONValue = .string("2026-11-25T08:56:53.758511Z")
  let end:JSONValue = .string("2026-11-25T09:56:57.351745+01:00")
  let c:JSONValue = .object(["timing":.object(["sample_time":sample]),"ttl_ns":.integer(3593234000)])
  XCTAssertEqual(try RisingIncidentTriggerWire.nanos(end),try RisingIncidentTriggerWire.deadlineNanos(c))
  XCTAssertEqual(try RisingIncidentTriggerWire.nanos(.string("2026-11-25T08:56:57.351745001Z")),try RisingIncidentTriggerWire.deadlineNanos(c)+1)
  XCTAssertThrowsError(try RisingIncidentTriggerWire.nanos(.string("2026-11-25T08:56:57.3517450001Z")))
  XCTAssertThrowsError(try RisingIncidentTriggerWire.nanos(.string("2026-11-25T08:56:57+24:00")))
  XCTAssertThrowsError(try RisingIncidentTriggerWire.nanos(.string("2026-11-25T08:56:57+01:60")))
  XCTAssertThrowsError(try RisingIncidentTriggerWire.nanos(.string("9999-11-25T08:56:57Z")))
  let overflow:JSONValue = .object(["timing":.object(["sample_time":.string("2262-04-11T23:47:16Z")]),"ttl_ns":.integer(10_000_000_000)])
  XCTAssertThrowsError(try RisingIncidentTriggerWire.deadlineNanos(overflow))
 }
 func testActualCarrierDeadlineRejectsOneNanosecondExcess()throws{
  guard let path=ProcessInfo.processInfo.environment["WENDY_RISING_LIVE_TRIGGER_FIXTURE"]else{throw XCTSkip("Supply archived actual paired trigger")}
  let value=try JSONValue.decode(Data(contentsOf:URL(fileURLWithPath:path)),limit:64<<10),c=value["component"]!,p=c["payload"]!
  let raw=Data(base64Encoded:p["joined_json"]!.string!)!,joined=try JSONValue.decode(raw,limit:40<<10)
  let spatial=try JSONValue.decode(Data(base64Encoded:p["spatial_json"]!.string!)!,limit:40<<10)
  let target=try MissionWire.vector(spatial["payload"]?["position_m"])
  let end=try RisingIncidentTriggerWire.nanos(p["valid_until"])
  let formatter=ISO8601DateFormatter();formatter.timeZone=TimeZone(secondsFromGMT:0);formatter.formatOptions=[.withInternetDateTime]
  func stamp(_ n:Int64)->String{let whole=formatter.string(from:Date(timeIntervalSince1970:Double(n/1_000_000_000)));return String(whole.dropLast())+String(format:".%09lldZ",n%1_000_000_000)}
  for excess in [Int64(0),1]{
   var carrier=joined["payload"]!["carriers"]!.array![0].object!;carrier["valid_until"] = .string(stamp(end-excess))
   var payload=joined["payload"]!.object!;payload["carriers"] = .array([.object(carrier)])
   var member=joined.object!;member["payload"] = .object(payload);let bytes=try JSONValue.object(member).encoded()
   var outerPayload=p.object!;outerPayload["joined_json"] = .string(bytes.base64EncodedString());outerPayload["joined_sha256"] = .string(WorldCredentials.digest(bytes))
   var outer=c.object!;outer["payload"] = .object(outerPayload)
   if excess==0{XCTAssertNoThrow(try RisingIncidentTriggerWire.review(.object(outer),fixedTarget:target,destination:nil))}
   else{XCTAssertThrowsError(try RisingIncidentTriggerWire.review(.object(outer),fixedTarget:target,destination:nil))}
  }
 }
 func testWindowCannotOutliveOnePointByHalfMicrosecond()throws{
  let(_,value,target)=try fixture();let c=value["component"]!,p=c["payload"]!
  let temporal=try decodeTemporal(p),claim=try JSONValue.decode(temporal,limit:40<<10)
  let pointBytes=try MissionRawJSON.field(temporal,path:["payload","window","traces","0","points","0"],limit:40<<10)
  var point=try JSONValue.decode(pointBytes).object!;point["valid_until_s"] = .number(claim["payload"]!["window"]!["valid_until_s"]!.double!-0.0000005)
  var changed=temporal;guard let range=changed.range(of:pointBytes)else{return XCTFail("missing original point")};changed.replaceSubrange(range,with:try JSONValue.object(point).encoded())
  let oldHash=p["temporal_sha256"]!.string!,newHash=WorldCredentials.digest(changed)
  let joined=Data(base64Encoded:p["joined_json"]!.string!)!;let joinedText=String(decoding:joined,as:UTF8.self).replacingOccurrences(of:oldHash,with:newHash);let changedJoined=Data(joinedText.utf8)
  var outerPayload=p.object!;outerPayload["temporal_json"] = .string(try compressTemporal(changed).base64EncodedString());outerPayload["temporal_sha256"] = .string(newHash);outerPayload["joined_json"] = .string(changedJoined.base64EncodedString());outerPayload["joined_sha256"] = .string(WorldCredentials.digest(changedJoined))
  var outer=c.object!;outer["payload"] = .object(outerPayload)
  XCTAssertThrowsError(try RisingIncidentTriggerWire.review(.object(outer),fixedTarget:target,destination:nil))
 }
 func decodeTemporal(_ p:JSONValue)throws->Data {try RisingTemporalCodec.decode(Data(base64Encoded:p["temporal_json"]!.string!)!,codec:p["temporal_codec"]?.string)}
 func compressTemporal(_ input:Data)throws->Data {
  var output=[UInt8](repeating:0,count:12<<10)
  let count=input.withUnsafeBytes{source in output.withUnsafeMutableBytes{destination in compression_encode_buffer(destination.bindMemory(to:UInt8.self).baseAddress!,destination.count,source.bindMemory(to:UInt8.self).baseAddress!,input.count,nil,COMPRESSION_ZLIB)}}
  guard count>0 else{throw MissionError.invalidDraft};return Data(output.prefix(count))
 }
 func testTemporalCodecRejectsUnknownTrailingTruncatedAndExpansionBomb()throws{
  let input=Data("exact original bytes".utf8),encoded=try compressTemporal(input)
  XCTAssertEqual(try RisingTemporalCodec.decode(encoded,codec:"deflate-json-v1"),input)
  XCTAssertThrowsError(try RisingTemporalCodec.decode(encoded,codec:"unknown"))
  XCTAssertThrowsError(try RisingTemporalCodec.decode(encoded+Data([0]),codec:"deflate-json-v1"))
  XCTAssertThrowsError(try RisingTemporalCodec.decode(encoded.dropLast(),codec:"deflate-json-v1"))
  XCTAssertThrowsError(try RisingTemporalCodec.decode(Data(repeating:0,count:(12<<10)+1),codec:"deflate-json-v1"))
  XCTAssertThrowsError(try RisingTemporalCodec.decode(try compressTemporal(Data(repeating:120,count:(40<<10)+1)),codec:"deflate-json-v1"))
 }
 func fixture()throws->(Data,JSONValue,[Double]){
  guard let path=ProcessInfo.processInfo.environment["WENDY_RISING_TRIGGER_FIXTURE"]else{throw XCTSkip("Supply exact Go-produced rising trigger fixture")}
  let raw=try Data(contentsOf:URL(fileURLWithPath:path)),value=try JSONValue.decode(raw,limit:64<<10)
  let spatial=try JSONValue.decode(Data(base64Encoded:value["component"]!["payload"]!["spatial_json"]!.string!)!)
  return(raw,value,try MissionWire.vector(spatial["payload"]?["position_m"]))
 }
 func testExactGoRisingContextAndOriginalArrayBytes()throws{
  let(raw,value,target)=try fixture();let trigger=try MissionNativeTrigger(canonical:raw,fixedTarget:target,originalEnvelopeSHA256:String(repeating:"a",count:64))
  XCTAssertEqual(trigger.resultSchema,RisingIncidentTriggerWire.schema);XCTAssertEqual(trigger.canonical,raw)
  let rawClaim=try decodeTemporal(value["component"]!["payload"]!),claim=try JSONValue.decode(rawClaim)
  let first=try MissionRawJSON.field(rawClaim,path:["payload","inputs","0"])
  XCTAssertEqual(try JSONValue.decode(first),claim["payload"]!["inputs"]!.array![0])
  XCTAssertThrowsError(try MissionNativeTrigger(canonical:raw,fixedTarget:[target[0]+2,target[1],target[2]],originalEnvelopeSHA256:String(repeating:"a",count:64)))
 }
 func testGoHistoricalInconsistenciesAreAlsoRejectedBySDK()throws{
  guard let dir=ProcessInfo.processInfo.environment["WENDY_RISING_NEGATIVES"]else{throw XCTSkip("Supply exact Go adversarial fixtures")}
  let(_,_,target)=try fixture()
  for name in ["future-window","changed-calibration","changed-source"]{
   let raw=try Data(contentsOf:URL(fileURLWithPath:dir).appendingPathComponent("invalid-"+name+".json"))
   XCTAssertThrowsError(try MissionNativeTrigger(canonical:raw,fixedTarget:target,originalEnvelopeSHA256:String(repeating:"a",count:64)),name)
  }
 }
 func testRetainedTransitAgeUsesOriginalIdentityWithoutSanitizingEvidence()throws{
  guard let dir=ProcessInfo.processInfo.environment["WENDY_RISING_NEGATIVES"]else{throw XCTSkip("Supply Go transit-age fixture")}
  let(_,_,target)=try fixture();let root=URL(fileURLWithPath:dir)
  let raw=try Data(contentsOf:root.appendingPathComponent("trigger-transit-age-fixture.json"))
  let trigger=try MissionNativeTrigger(canonical:raw,fixedTarget:target,originalEnvelopeSHA256:String(repeating:"a",count:64));XCTAssertEqual(trigger.canonical,raw)
  let value=try JSONValue.decode(raw,limit:64<<10);let claim=try decodeTemporal(value["component"]!["payload"]!)
  let original=try MissionRawJSON.field(claim,path:["payload","inputs","0"],limit:40<<10)
  let retained=try JSONValue.decode(original);XCTAssertEqual(retained["timing"]?["age_hint_ns"]?.int,4396916)
  let identity=try MissionRawJSON.originalIdentity(original);XCTAssertNotEqual(identity,original)
  let normalized=try JSONValue.decode(identity);XCTAssertEqual(normalized["timing"]?["age_hint_ns"]?.int,0)
  XCTAssertEqual(normalized["payload"],retained["payload"]);XCTAssertEqual(normalized["provenance"],retained["provenance"])
  for name in ["negative-age","exhausted-age","other-timing-field"]{
   let bad=try Data(contentsOf:root.appendingPathComponent("invalid-"+name+".json"))
   XCTAssertThrowsError(try MissionNativeTrigger(canonical:bad,fixedTarget:target,originalEnvelopeSHA256:String(repeating:"a",count:64)),name)
  }
 }
 func testTypedRisingRejectsNullUnknownAndChangedOriginal()throws{
  let(_,value,target)=try fixture();let c=value["component"]!
  for field in ["joined_json","temporal_json","spatial_json","valid_until"]{
   var p=c["payload"]!.object!;p[field] = .null;var object=c.object!;object["payload"] = .object(p)
   XCTAssertThrowsError(try RisingIncidentTriggerWire.review(.object(object),fixedTarget:target,destination:nil))
  }
  var p=c["payload"]!.object!;p["future_unknown"] = .bool(true);var object=c.object!;object["payload"] = .object(p)
  XCTAssertThrowsError(try RisingIncidentTriggerWire.review(.object(object),fixedTarget:target,destination:nil))
  p=c["payload"]!.object!;var bytes=Data(base64Encoded:p["temporal_json"]!.string!)!;bytes[bytes.count/2] ^= 1;p["temporal_json"] = .string(bytes.base64EncodedString());object=c.object!;object["payload"] = .object(p)
  XCTAssertThrowsError(try RisingIncidentTriggerWire.review(.object(object),fixedTarget:target,destination:nil))
 }
}
