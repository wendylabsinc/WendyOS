import Foundation
import Synchronization

/// JSON output renderer that collects events and outputs a single JSON response.
/// This is designed for LLMs and third-party tools that expect structured output.
public final class JSONRenderer: CLIOutput, Sendable {
    private struct State: Sendable {
        var events: [JSONEvent] = []
        var finalResultData: Data?
    }

    private let state = Mutex(State())

    public init() {}

    public func success(_ message: String) {
        state.withLock { $0.events.append(.success(message)) }
    }

    public func error(_ message: String, suggestion: String?) {
        state.withLock { $0.events.append(.error(message, suggestion: suggestion)) }
    }

    public func info(_ message: String) {
        state.withLock { $0.events.append(.info(message)) }
    }

    public func warning(_ message: String) {
        state.withLock { $0.events.append(.warning(message)) }
    }

    public func table(headers: [String], rows: [[String]]) {
        state.withLock { $0.events.append(.table(headers: headers, rows: rows)) }
    }

    public func result<T: Encodable & Sendable>(_ value: T) {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        if let data = try? encoder.encode(value) {
            state.withLock { $0.finalResultData = data }
        }
    }

    public func progress(message: String, percent: Double?) {
        state.withLock { $0.events.append(.progress(message: message, percent: percent)) }
    }

    public func flush() {
        // Copy state while holding lock, then release before I/O
        let (events, finalResultData) = state.withLock { ($0.events, $0.finalResultData) }

        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]

        let output = finalResultData ?? encodeResponse(events: events, encoder: encoder)

        if let string = String(data: output, encoding: .utf8) {
            FileHandle.standardOutput.write(Data((string + "\n").utf8))
        }
    }

    private func encodeResponse(events: [JSONEvent], encoder: JSONEncoder) -> Data {
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

