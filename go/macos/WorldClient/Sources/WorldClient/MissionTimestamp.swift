import Foundation

// Exact original component timestamps shared by typed mission decoders.
enum MissionTimestamp {
    // ISO8601DateFormatter drops sub-millisecond digits on supported platforms.
    // Retain those original digits before doing checked deadline arithmetic.
    static func nanos(_ value: JSONValue?) throws -> Int64 {
        guard let text=value?.string else { throw MissionError.invalidDraft }
        let pattern = #"^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d{1,9}))?(Z|[+-]\d{2}:\d{2})$"#
        let regex=try NSRegularExpression(pattern:pattern)
        let ns=text as NSString
        guard let match=regex.firstMatch(in:text,range:NSRange(location:0,length:ns.length)),match.range.length==ns.length else { throw MissionError.invalidDraft }
        let zone=ns.substring(with:match.range(at:3))
        if zone != "Z" { let pieces=zone.dropFirst().split(separator:":");guard pieces.count==2,let hours=Int(pieces[0]),let minutes=Int(pieces[1]),hours<24,minutes<60 else {throw MissionError.invalidDraft} }
        let whole=ns.substring(with:match.range(at:1))+zone
        let formatter=ISO8601DateFormatter();formatter.formatOptions=[.withInternetDateTime]
        guard let date=formatter.date(from:whole) else { throw MissionError.invalidDraft }
        let seconds=date.timeIntervalSince1970.rounded()
        guard seconds>=Double(Int64.min/1_000_000_000),seconds<=Double(Int64.max/1_000_000_000) else { throw MissionError.invalidDraft }
        let fraction=match.range(at:2).location==NSNotFound ? "" : ns.substring(with:match.range(at:2))
        guard let part=Int64(fraction+String(repeating:"0",count:9-fraction.count)) else { throw MissionError.invalidDraft }
        let (base,overflow)=Int64(seconds).multipliedReportingOverflow(by:1_000_000_000)
        let (result,addedOverflow)=base.addingReportingOverflow(part)
        guard !overflow,!addedOverflow else { throw MissionError.invalidDraft };return result
    }
    static func time(_ value: JSONValue?) throws -> Date {
        let n=try nanos(value)
        return Date(timeIntervalSince1970:Double(n/1_000_000_000)+Double(n%1_000_000_000)/1e9)
    }
    static func deadlineNanos(_ value:JSONValue) throws -> Int64 {
        guard let ttl=value["ttl_ns"]?.int,ttl>0 else {throw MissionError.invalidDraft}
        let (end,overflow)=(try nanos(value["timing"]?["sample_time"])).addingReportingOverflow(ttl)
        guard !overflow else {throw MissionError.invalidDraft};return end
    }
    static func span(_ end:Int64,_ start:Int64)throws->Int64 {
        let (value,overflow)=end.subtractingReportingOverflow(start)
        guard !overflow else{throw MissionError.invalidDraft};return value
    }
}
