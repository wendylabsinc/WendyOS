import Foundation

/// Lossless integers and typed JSON values for versioned payloads the viewer
/// does not interpret. Original response bytes are also retained on snapshots.
public enum JSONValue: Codable, Sendable, Equatable {
    case object([String: JSONValue])
    case array([JSONValue])
    case string(String)
    case integer(Int64)
    case unsigned(UInt64)
    case number(Double)
    case bool(Bool)
    case null

    public init(from decoder: any Decoder) throws {
        let c = try decoder.singleValueContainer()
        if c.decodeNil() { self = .null }
        else if let v = try? c.decode(Bool.self) { self = .bool(v) }
        else if let v = try? c.decode(Int64.self) { self = .integer(v) }
        else if let v = try? c.decode(UInt64.self) { self = .unsigned(v) }
        else if let v = try? c.decode(Double.self), v.isFinite { self = .number(v) }
        else if let v = try? c.decode(String.self) { self = .string(v) }
        else if let v = try? c.decode([JSONValue].self) { self = .array(v) }
        else { self = .object(try c.decode([String: JSONValue].self)) }
    }
    public func encode(to encoder: any Encoder) throws {
        var c = encoder.singleValueContainer()
        switch self {
        case .object(let v): try c.encode(v)
        case .array(let v): try c.encode(v)
        case .string(let v): try c.encode(v)
        case .integer(let v): try c.encode(v)
        case .unsigned(let v): try c.encode(v)
        case .number(let v): try c.encode(v)
        case .bool(let v): try c.encode(v)
        case .null: try c.encodeNil()
        }
    }
    public subscript(_ key: String) -> JSONValue? { object?[key] }
    public var object: [String: JSONValue]? { if case .object(let v) = self { v } else { nil } }
    public var array: [JSONValue]? { if case .array(let v) = self { v } else { nil } }
    public var string: String? { if case .string(let v) = self { v } else { nil } }
    public var bool: Bool? { if case .bool(let v) = self { v } else { nil } }
    public var int: Int64? {
        switch self { case .integer(let v): v; case .unsigned(let v): Int64(exactly: v); default: nil }
    }
    public var double: Double? {
        switch self { case .integer(let v): Double(v); case .unsigned(let v): Double(v); case .number(let v): v; default: nil }
    }
    public var uint: UInt64? {
        switch self { case .integer(let v): UInt64(exactly: v); case .unsigned(let v): v; default: nil }
    }
    public static func decode(_ data: Data, limit: Int = 16 << 20) throws -> JSONValue {
        guard data.count <= limit else { throw WorldClientError.responseTooLarge }
        return try JSONDecoder().decode(JSONValue.self, from: data)
    }
    public func encoded(pretty: Bool = false) throws -> Data {
        let encoder = JSONEncoder()
        encoder.outputFormatting = pretty ? [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes] : [.sortedKeys, .withoutEscapingSlashes]
        return try encoder.encode(self)
    }
}

public enum WorldClientError: Error, LocalizedError, Sendable, Equatable {
    case invalidCredentials
    case invalidEndpoint
    case missingReadScope
    case expiredCredentials
    case invalidResponse
    case responseTooLarge
    case redirectRefused
    case httpStatus(Int)
    case invalidEvidence
    case invalidCache
    case insecureCache
    public var errorDescription: String? {
        switch self {
        case .invalidCredentials: "Invalid simulation viewer credentials."
        case .invalidEndpoint: "A configured gateway URL is invalid."
        case .missingReadScope: "Viewer credentials need read-only World permissions."
        case .expiredCredentials: "Viewer credentials are not currently valid."
        case .invalidResponse: "The gateway returned an invalid response."
        case .responseTooLarge: "The response exceeds the client limit."
        case .redirectRefused: "Gateway redirects are not permitted."
        case .httpStatus(let status): "The gateway returned HTTP \(status)."
        case .invalidEvidence: "Evidence metadata or digest verification failed."
        case .invalidCache: "The cached world is invalid or belongs to another profile."
        case .insecureCache: "The cache requires a private regular directory and file."
        }
    }
}
