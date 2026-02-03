import Foundation
import NIOCore

/// Streaming JSON output renderer that emits line-delimited JSON (NDJSON).
/// Each event is output immediately as a separate JSON line.
/// This is useful for real-time progress updates and streaming to other tools.
public struct JSONStreamRenderer: CLIOutput, Sendable {
    public init() {}

    public func success(_ message: String) {
        emit(StreamEvent(type: "success", message: message))
    }

    public func error(_ message: String, suggestion: String?) {
        emit(StreamEvent(type: "error", message: message, suggestion: suggestion))
    }

    public func info(_ message: String) {
        emit(StreamEvent(type: "info", message: message))
    }

    public func warning(_ message: String) {
        emit(StreamEvent(type: "warning", message: message))
    }

    public func table(headers: [String], rows: [[String]]) {
        emit(TableEvent(headers: headers, rows: rows))
    }

    public func result<T: Encodable & Sendable>(_ value: T) {
        emit(ResultEvent(result: value))
    }

    public func progress(message: String, percent: Double?) {
        emit(ProgressEvent(message: message, percent: percent))
    }
    
    public func withStreamingOutput<T>(
        title: String,
        operation: @escaping @Sendable (@escaping @Sendable (ByteBuffer) async throws -> Void) async throws -> T
    ) async throws -> T {
        return try await operation { string in
            emit(StreamEvent(type: "output", message: String(buffer: string)))
        }
    }

    public func withStreamingOutputBox<T>(
        title: String,
        maxLines: Int,
        operation: @escaping @Sendable (@escaping @Sendable (ByteBuffer) async throws -> Void) async throws -> T
    ) async throws -> T {
        return try await withStreamingOutput(title: title, operation: operation)
    }

    private func emit<T: Encodable>(_ event: T) {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys]
        guard let data = try? encoder.encode(event),
            let line = String(data: data, encoding: .utf8)
        else { return }
        FileHandle.standardOutput.write(Data((line + "\n").utf8))
    }
}

// MARK: - Stream Event Types

private struct StreamEvent: Encodable, Sendable {
    let type: String
    let message: String
    let suggestion: String?

    init(type: String, message: String, suggestion: String? = nil) {
        self.type = type
        self.message = message
        self.suggestion = suggestion
    }
}

private struct TableEvent: Encodable, Sendable {
    let type = "table"
    let headers: [String]
    let rows: [[String]]
}

private struct ProgressEvent: Encodable, Sendable {
    let type = "progress"
    let message: String
    let percent: Double?
}

private struct ResultEvent<T: Encodable>: Encodable {
    let type = "result"
    let result: T
}
