import Foundation
import zlib

// Lossless transport only: the historical decoder still validates the exact
// expanded original bytes, their digest and every original lifetime.
enum RisingTemporalCodec {
 static func decode(_ input:Data,codec:String?)throws->Data {
  guard codec=="deflate-json-v1",!input.isEmpty,input.count<=12<<10 else{throw MissionError.invalidDraft}
  var stream=z_stream()
  guard inflateInit2_(&stream,-MAX_WBITS,ZLIB_VERSION,Int32(MemoryLayout<z_stream>.size))==Z_OK else{throw MissionError.invalidDraft}
  defer{inflateEnd(&stream)}
  var output=[UInt8](repeating:0,count:(40<<10)+1)
  let size:Int = try input.withUnsafeBytes {source in
   try output.withUnsafeMutableBytes {destination in
    stream.next_in=UnsafeMutablePointer(mutating:source.bindMemory(to:UInt8.self).baseAddress!);stream.avail_in=uInt(input.count)
    stream.next_out=destination.bindMemory(to:UInt8.self).baseAddress!;stream.avail_out=uInt(destination.count)
    let status=inflate(&stream,Z_FINISH)
    let count=Int(stream.total_out)
    guard status==Z_STREAM_END,stream.avail_in==0,count>0,count<=40<<10 else{throw MissionError.invalidDraft};return count
   }
  }
  return Data(output.prefix(size))
 }
}

public extension MissionNativeTrigger {
 /// Exact temporal original JSON retained by this typed historical trigger.
 var risingTemporalOriginalJSON:Data? {
  guard resultSchema==RisingIncidentTriggerWire.schema,let root=try? JSONValue.decode(canonical,limit:64<<10),let p=root["component"]?["payload"],let encoded=p["temporal_json"]?.string.flatMap({Data(base64Encoded:$0)}) else{return nil}
  return try? RisingTemporalCodec.decode(encoded,codec:p["temporal_codec"]?.string)
 }
}
