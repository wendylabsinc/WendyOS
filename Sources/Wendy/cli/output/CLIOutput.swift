import Foundation

// Helper to flush stdout in Swift 6
@inline(__always)
private func flushStdout() {
    #if os(Linux)
        fflush(nil)
    #else
        fflush(stdout)
    #endif
}

/// Protocol for CLI output rendering. Commands emit structured events
/// through this interface, and different implementations handle formatting
/// based on output mode (interactive, JSON, JSON stream).
public protocol CLIOutput: Sendable {
    /// Emit a success message
    func success(_ message: String)

    /// Emit an error message with optional suggestion
    func error(_ message: String, suggestion: String?)

    /// Emit an informational message
    func info(_ message: String)

    /// Emit a warning message
    func warning(_ message: String)

    /// Emit a table with headers and rows
    func table(headers: [String], rows: [[String]])

    /// Display a streaming table that updates in real-time.
    /// In interactive mode, shows a live-updating table.
    /// In JSON mode, emits each update as a JSON line.
    func streamingTable<T: Encodable & Sendable>(
        initial: T,
        updates: AsyncStream<T>,
        renderTable: @escaping @Sendable (T) -> (headers: [String], rows: [[String]])
    ) async

    /// Select an item from a table interactively. Returns the index of the selected row.
    /// In JSON mode, this throws an error requiring explicit selection via CLI args.
    func selectFromTable(
        title: String?,
        headers: [String],
        rows: [[String]],
        pageSize: Int
    ) async throws -> Int

    /// Emit a structured result that can be encoded as JSON
    func result<T: Encodable & Sendable>(_ value: T)

    /// Emit a progress update
    func progress(message: String, percent: Double?)

    /// Flush any buffered output
    func flush()

    /// Execute an async operation with progress indication.
    /// In interactive mode, shows a spinner. In JSON mode, runs silently.
    func withProgress<T: Sendable>(
        message: String,
        successMessage: String,
        errorMessage: String,
        operation: @escaping @Sendable () async throws -> T
    ) async throws -> T

    /// Execute an async operation with progress bar indication.
    /// In interactive mode, shows a progress bar. In JSON mode, runs silently.
    func withProgressBar<T: Sendable>(
        message: String,
        operation: @escaping @Sendable (@escaping (Double) -> Void) async throws -> T
    ) async throws -> T

    /// Execute an operation with streaming output displayed in a collapsible box.
    /// In interactive mode, shows a scrollable bordered box with the output.
    /// In JSON mode, just runs the operation silently.
    /// - Parameters:
    ///   - title: Title for the output section
    ///   - maxLines: Maximum lines to show in the scrolling view
    ///   - operation: The operation to run, receives a callback to emit each line
    func withStreamingOutput<T: Sendable>(
        title: String,
        maxLines: Int,
        operation:
            @escaping @Sendable (@escaping @Sendable (String) async throws -> Void) async throws ->
            T
    ) async throws -> T
}

// MARK: - Default implementations

/// Error thrown when interactive selection is required but not available
public struct InteractiveSelectionRequiredError: Error, CustomStringConvertible {
    public let message: String

    public var description: String { message }

    public init(argument: String, description: String) {
        self.message = "Interactive selection not available in JSON mode. \(description)"
    }
}

extension CLIOutput {
    public func error(_ message: String) {
        error(message, suggestion: nil)
    }

    public func progress(message: String) {
        progress(message: message, percent: nil)
    }

    public func flush() {
        // Default: no-op
    }

    public func selectFromTable(
        title: String?,
        headers: [String],
        rows: [[String]],
        pageSize: Int
    ) async throws -> Int {
        // Default: not supported - requires interactive mode
        throw InteractiveSelectionRequiredError(
            argument: "selection",
            description: "Provide the selection via CLI arguments"
        )
    }

    public func withProgress<T: Sendable>(
        message: String,
        successMessage: String,
        errorMessage: String,
        operation: @Sendable () async throws -> T
    ) async throws -> T {
        // Default: just run the operation
        try await operation()
    }

    public func withProgressBar<T: Sendable>(
        message: String,
        operation: @Sendable (@escaping (Double) -> Void) async throws -> T
    ) async throws -> T {
        // Default: just run the operation with no-op progress callback
        try await operation({ _ in })
    }

    public func withStreamingOutput<T: Sendable>(
        title: String,
        maxLines: Int,
        operation:
            @escaping @Sendable (@escaping @Sendable (String) async throws -> Void) async throws ->
            T
    ) async throws -> T {
        // Default: just print each line as it comes
        try await operation { @Sendable line in
            print(line)
        }
    }

    public func streamingTable<T: Encodable & Sendable>(
        initial: T,
        updates: AsyncStream<T>,
        renderTable: @Sendable (T) -> (headers: [String], rows: [[String]])
    ) async {
        // Default: print each update as JSON
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys]
        if let data = try? encoder.encode(initial),
            let string = String(data: data, encoding: .utf8)
        {
            print(string)
            flushStdout()
        }
        for await update in updates {
            if let data = try? encoder.encode(update),
                let string = String(data: data, encoding: .utf8)
            {
                print(string)
                flushStdout()
            }
        }
    }
}

// MARK: - Output Mode

/// The current output mode for CLI commands.
public enum OutputMode: Sendable {
    /// Human-readable interactive output via Noora
    case interactive

    /// Single JSON response for LLMs and third-party tools
    case json

    /// Line-delimited JSON for streaming progress events
    case jsonStream
}

// MARK: - TaskLocal for current output

/// TaskLocal storage for the current CLI output instance.
/// Commands can access this to emit output without passing the instance around.
public enum CLIOutputContext {
    @TaskLocal
    public static var current: (any CLIOutput)? = nil

    @TaskLocal
    public static var mode: OutputMode = .interactive
}

/// Execute a block with a specific CLI output context.
public func withCLIOutput<T: Sendable>(
    _ output: any CLIOutput,
    mode: OutputMode,
    _ body: @Sendable () async throws -> T
) async rethrows -> T {
    try await CLIOutputContext.$current.withValue(output) {
        try await CLIOutputContext.$mode.withValue(mode) {
            try await body()
        }
    }
}

// MARK: - Convenience accessor

/// Get the current CLI output, falling back to a default if not set.
public var cliOutput: any CLIOutput {
    CLIOutputContext.current ?? DefaultCLIOutput.shared
}

/// Check if we're in JSON output mode (either single or streaming).
public var isJSONOutputMode: Bool {
    switch CLIOutputContext.mode {
    case .interactive:
        return false
    case .json, .jsonStream:
        return true
    }
}

// MARK: - Default output (fallback)

/// Default CLI output that writes to stdout. Used as fallback when no context is set.
internal struct DefaultCLIOutput: CLIOutput, @unchecked Sendable {
    static let shared = DefaultCLIOutput()

    func success(_ message: String) {
        print("✓ \(message)")
    }

    func error(_ message: String, suggestion: String?) {
        print("✗ \(message)")
        if let suggestion {
            print("  Suggestion: \(suggestion)")
        }
    }

    func info(_ message: String) {
        print(message)
    }

    func warning(_ message: String) {
        print("⚠ \(message)")
    }

    func table(headers: [String], rows: [[String]]) {
        // Simple table output
        print(headers.joined(separator: "\t"))
        for row in rows {
            print(row.joined(separator: "\t"))
        }
    }

    func result<T: Encodable & Sendable>(_ value: T) {
        // In default mode, try to print as JSON
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        if let data = try? encoder.encode(value),
            let string = String(data: data, encoding: .utf8)
        {
            print(string)
        }
    }

    func progress(message: String, percent: Double?) {
        if let percent {
            print("[\(Int(percent * 100))%] \(message)")
        } else {
            print("... \(message)")
        }
    }
}
