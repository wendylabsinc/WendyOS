import Foundation

/// Reject duplicate members, unknown fields and deep nesting before Codable
/// decoding, so Go and Swift cannot interpret a signed artifact differently.
enum DeliveryStrictJSON {
    static func decode<T: Decodable>(_ type: T.Type, data: Data, keys: Set<String>) throws -> T {
        var scanner = Scanner(bytes: Array(data), rootKeys: keys)
        try scanner.value(depth: 0)
        scanner.whitespace()
        guard scanner.index == scanner.bytes.count else {
            throw BuildDeliveryContract.Failure("trailing JSON")
        }
        return try JSONDecoder().decode(type, from: data)
    }
    private struct Scanner {
        let bytes: [UInt8]
        let rootKeys: Set<String>
        var index = 0
        mutating func whitespace() {
            while index < bytes.count && [9, 10, 13, 32].contains(bytes[index]) { index += 1 }
        }
        mutating func take(_ byte: UInt8) -> Bool {
            whitespace()
            guard index < bytes.count, bytes[index] == byte else { return false }
            index += 1
            return true
        }
        mutating func string() throws -> String {
            whitespace()
            let start = index
            guard take(34) else { throw BuildDeliveryContract.Failure("expected JSON string") }
            while index < bytes.count {
                let byte = bytes[index]
                index += 1
                if byte == 34 {
                    return try JSONDecoder().decode(String.self, from: Data(bytes[start..<index]))
                }
                if byte == 92 {
                    guard index < bytes.count else { break }
                    index += 1
                }
            }
            throw BuildDeliveryContract.Failure("unterminated JSON string")
        }
        mutating func value(depth: Int) throws {
            guard depth <= 32 else {
                throw BuildDeliveryContract.Failure("JSON nesting exceeds limit")
            }
            whitespace()
            guard index < bytes.count else {
                throw BuildDeliveryContract.Failure("incomplete JSON")
            }
            if take(123) {
                var keys: Set<String> = []
                if !take(125) {
                    repeat {
                        let key = try string()
                        guard keys.insert(key).inserted, take(58) else {
                            throw BuildDeliveryContract.Failure(
                                "duplicate JSON member or missing colon"
                            )
                        }
                        try value(depth: depth + 1)
                        if take(125) { break }
                        guard take(44) else {
                            throw BuildDeliveryContract.Failure("invalid JSON object")
                        }
                    } while true
                }
                if depth == 0, keys != rootKeys {
                    throw BuildDeliveryContract.Failure("missing or unknown delivery JSON fields")
                }
            } else if take(91) {
                guard depth > 0 else {
                    throw BuildDeliveryContract.Failure("expected delivery JSON object")
                }
                if take(93) { return }
                repeat {
                    try value(depth: depth + 1)
                    if take(93) { break }
                    guard take(44) else {
                        throw BuildDeliveryContract.Failure("invalid JSON array")
                    }
                } while true
            } else if bytes[index] == 34 {
                guard depth > 0 else {
                    throw BuildDeliveryContract.Failure("expected delivery JSON object")
                }
                _ = try string()
            } else {
                guard depth > 0 else {
                    throw BuildDeliveryContract.Failure("expected delivery JSON object")
                }
                let start = index
                while index < bytes.count && ![9, 10, 13, 32, 44, 93, 125].contains(bytes[index]) {
                    index += 1
                }
                guard index > start else {
                    throw BuildDeliveryContract.Failure("invalid JSON value")
                }
                if bytes[start] == 45 || (48...57).contains(bytes[start]) {
                    let digits =
                        bytes[start] == 45 ? bytes[(start + 1)..<index] : bytes[start..<index]
                    guard !digits.isEmpty, digits.allSatisfy({ (48...57).contains($0) }),
                        !(digits.count > 1 && digits.first == 48)
                    else {
                        throw BuildDeliveryContract.Failure("delivery numbers must be integers")
                    }
                }
            }
        }
    }
}
