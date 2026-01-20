import Foundation

/// JSON output renderer that collects events and outputs a single JSON response.
/// This is designed for LLMs and third-party tools that expect structured output.
public final class JSONRenderer: CLIOutput, @unchecked Sendable {
    private let lock = NSLock()
    private var events: [JSONEvent] = []
    private var finalResult: (any Encodable & Sendable)?

    public init() {}

    public func success(_ message: String) {
        lock.withLock {
            events.append(.success(message))
        }
    }

    public func error(_ message: String, suggestion: String?) {
        lock.withLock {
            events.append(.error(message, suggestion: suggestion))
        }
    }

    public func info(_ message: String) {
        lock.withLock {
            events.append(.info(message))
        }
    }

    public func warning(_ message: String) {
        lock.withLock {
            events.append(.warning(message))
        }
    }

    public func table(headers: [String], rows: [[String]]) {
        lock.withLock {
            events.append(.table(headers: headers, rows: rows))
        }
    }

    public func result<T: Encodable & Sendable>(_ value: T) {
        lock.withLock {
            finalResult = value
        }
    }

    public func progress(message: String, percent: Double?) {
        lock.withLock {
            events.append(.progress(message: message, percent: percent))
        }
    }

    public func flush() {
        let output: Data
        lock.lock()
        defer { lock.unlock() }

        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]

        // If there's a final result, output it directly
        if let result = finalResult {
            if let data = try? encoder.encode(AnyEncodable(result)) {
                output = data
            } else {
                output = encodeResponse(encoder: encoder)
            }
        } else {
            output = encodeResponse(encoder: encoder)
        }

        if let string = String(data: output, encoding: .utf8) {
            FileHandle.standardOutput.write(Data((string + "\n").utf8))
        }
    }

    private func encodeResponse(encoder: JSONEncoder) -> Data {
        let response = JSONResponse(events: events)
        return (try? encoder.encode(response)) ?? Data()
    }
}

// MARK: - JSON Event Types

private enum JSONEvent: Sendable {
    case success(String)
    case error(String, suggestion: String?)
    case info(String)
    case warning(String)
    case table(headers: [String], rows: [[String]])
    case progress(message: String, percent: Double?)
}

extension JSONEvent: Encodable {
    private enum CodingKeys: String, CodingKey {
        case type, message, suggestion, headers, rows, percent
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        switch self {
        case .success(let message):
            try container.encode("success", forKey: .type)
            try container.encode(message, forKey: .message)
        case .error(let message, let suggestion):
            try container.encode("error", forKey: .type)
            try container.encode(message, forKey: .message)
            try container.encodeIfPresent(suggestion, forKey: .suggestion)
        case .info(let message):
            try container.encode("info", forKey: .type)
            try container.encode(message, forKey: .message)
        case .warning(let message):
            try container.encode("warning", forKey: .type)
            try container.encode(message, forKey: .message)
        case .table(let headers, let rows):
            try container.encode("table", forKey: .type)
            try container.encode(headers, forKey: .headers)
            try container.encode(rows, forKey: .rows)
        case .progress(let message, let percent):
            try container.encode("progress", forKey: .type)
            try container.encode(message, forKey: .message)
            try container.encodeIfPresent(percent, forKey: .percent)
        }
    }
}

// MARK: - JSON Response

private struct JSONResponse: Encodable {
    let success: Bool
    let events: [JSONEvent]

    init(events: [JSONEvent]) {
        self.events = events
        // Consider the response successful if there are no error events
        self.success = !events.contains { event in
            if case .error = event { return true }
            return false
        }
    }
}

// MARK: - Type-erased Encodable wrapper

private struct AnyEncodable: Encodable {
    private let _encode: (Encoder) throws -> Void

    init<T: Encodable>(_ value: T) {
        _encode = { encoder in
            try value.encode(to: encoder)
        }
    }

    func encode(to encoder: Encoder) throws {
        try _encode(encoder)
    }
}
