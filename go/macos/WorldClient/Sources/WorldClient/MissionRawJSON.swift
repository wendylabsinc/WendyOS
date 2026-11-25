import Foundation

/// Reads a member's original bytes from a bounded JSON object. This is used only
/// where Go keeps json.RawMessage; typed outer fields still use MissionWire.
enum MissionRawJSON {
    static func field(_ data: Data, path: [String], limit: Int = 128 << 10, requireCompact: Bool = false) throws -> Data {
        _ = try JSONValue.decode(data, limit: limit)
        var scanner = Scanner(bytes: Array(data), path: path, requireCompact: requireCompact)
        try scanner.value(depth: 0, path: [])
        scanner.whitespace()
        guard scanner.index == scanner.bytes.count, let range = scanner.found else { throw MissionError.invalidDraft }
        return Data(scanner.bytes[range])
    }

    /// Matches World ComponentHash: only unsigned transit age is excluded.
    /// All other original bytes, including number spelling and field order, stay intact.
    static func originalIdentity(_ data:Data,limit:Int=40<<10)throws->Data {
        let value=try JSONValue.decode(data,limit:limit)
        guard let age=value["timing"]?["age_hint_ns"]?.int,age>=0 else{throw MissionError.invalidDraft}
        var scanner=Scanner(bytes:Array(data),path:["timing","age_hint_ns"],requireCompact:true)
        try scanner.value(depth:0,path:[])
        guard scanner.index==scanner.bytes.count,let range=scanner.found else{throw MissionError.invalidDraft}
        var bytes=scanner.bytes;bytes.replaceSubrange(range,with:[0x30]);return Data(bytes)
    }

    private struct Scanner {
        let bytes: [UInt8]
        let path: [String]
        let requireCompact: Bool
        var index = 0
        var found: Range<Int>?

        mutating func value(depth: Int, path current: [String]) throws {
            whitespace()
            guard depth <= 34, index < bytes.count else { throw MissionError.invalidDraft }
            let start = index
            switch bytes[index] {
            case 0x7b:
                index += 1
                var keys = Set<String>()
                if take(0x7d) { break }
                while true {
                    whitespace()
                    let keyStart = index
                    try string()
                    let key = try JSONDecoder().decode(String.self, from: Data(bytes[keyStart..<index]))
                    guard keys.insert(key).inserted, take(0x3a) else { throw MissionError.invalidDraft }
                    try value(depth: depth + 1, path: current + [key])
                    if take(0x7d) { break }
                    guard take(0x2c) else { throw MissionError.invalidDraft }
                }
            case 0x5b:
                index += 1
                if take(0x5d) { break }
                var element = 0
                while true {
                    // Decimal indexes retain an exact nested array element.
                    try value(depth: depth + 1, path: current + [String(element)])
                    element += 1
                    if take(0x5d) { break }
                    guard take(0x2c) else { throw MissionError.invalidDraft }
                }
            case 0x22: try string()
            default:
                while index < bytes.count, ![0x2c, 0x7d, 0x5d].contains(bytes[index]) {
                    if [0x20, 0x09, 0x0a, 0x0d].contains(bytes[index]) { break }
                    index += 1
                }
            }
            if current == path { found = start..<index }
        }

        mutating func take(_ byte: UInt8) -> Bool {
            whitespace()
            guard index < bytes.count, bytes[index] == byte else { return false }
            index += 1; return true
        }

        mutating func whitespace() {
            if !requireCompact {
                while index < bytes.count, [0x20, 0x09, 0x0a, 0x0d].contains(bytes[index]) { index += 1 }
            }
        }

        mutating func string() throws {
            guard take(0x22) else { throw MissionError.invalidDraft }
            while index < bytes.count {
                let byte = bytes[index]; index += 1
                if byte == 0x22 { return }
                if byte == 0x5c {
                    guard index < bytes.count else { throw MissionError.invalidDraft }
                    index += 1 // JSONDecoder has checked the complete escape.
                } else {
                    // json.Marshal compacts and HTML-escapes RawMessage without
                    // changing key order or number/escape spellings. A supplied
                    // canonical trigger must already have that representation.
                    guard !requireCompact || (byte != 0x3c && byte != 0x3e && byte != 0x26) else { throw MissionError.invalidDraft }
                    if requireCompact, byte == 0xe2, index + 1 < bytes.count, bytes[index] == 0x80,
                       bytes[index + 1] == 0xa8 || bytes[index + 1] == 0xa9 { throw MissionError.invalidDraft }
                }
            }
            throw MissionError.invalidDraft
        }
    }
}
