import Foundation
import NIOCore

// Helper to flush stdout in Swift 6
@inline(__always)
private func flushStdout() {
    #if os(Linux)
        fflush(nil)
    #else
        fflush(stdout)
    #endif
}

/// Our own ProgressBarUpdate type so Noora doesn't leak through the public API.
public struct ProgressBarUpdate: Sendable, Equatable {
    public let progress: Double
    public let detail: String?

    public init(progress: Double, detail: String? = nil) {
        self.progress = progress
        self.detail = detail
    }
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
    func streamingTable<T: Encodable & Sendable, E: Error>(
        initial: T,
        updates: some AsyncSequence<T, E> & Sendable,
        renderTable: @escaping @Sendable (T) -> (headers: [String], rows: [[String]])
    ) async throws

    /// Select an item from a table interactively. Returns the index of the selected row.
    /// In JSON mode, this throws an error requiring explicit selection via CLI args.
    func selectFromTable(
        title: String?,
        headers: [String],
        rows: [[String]],
        pageSize: Int
    ) async throws -> Int

    /// Select an item from a streaming table that updates in real-time.
    /// Returns the selected element.
    func selectFromStreamingTable<S: BidirectionalCollection & Sendable>(
        initial: S,
        updates: some AsyncSequence<S, Never> & Sendable,
        pageSize: Int,
        renderTable: @escaping @Sendable ([S.Element]) -> (headers: [String], rows: [[String]])
    ) async throws -> S.Element where S.Index == Int, S.Element: Sendable & Comparable

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
        successMessage: String,
        errorMessage: String,
        operation: @escaping @Sendable (@escaping @Sendable (Double) -> Void) async throws -> T
    ) async throws -> T

    /// Execute an async operation with progress bar indication and label updates.
    /// In interactive mode, shows a progress bar with label text. In JSON mode, runs silently.
    func withLabeledProgressBar<T: Sendable>(
        message: String,
        operation: @escaping @Sendable (@escaping (ProgressBarUpdate) -> Void) async throws -> T
    ) async throws -> T

    /// Execute an operation with streaming output displayed in a collapsible box.
    /// In interactive mode, shows a scrollable bordered box with the output.
    /// In JSON mode, just runs the operation silently.
    /// - Parameters:
    ///   - title: Title for the output section
    ///   - maxLines: Maximum lines to show in the scrolling view
    ///   - operation: The operation to run, receives a callback to emit each line
    func withStreamingOutputBox<T: Sendable>(
        title: String,
        maxLines: Int,
        operation:
            @escaping @Sendable (@escaping @Sendable (ByteBuffer) async throws -> Void) async throws
            ->
            T
    ) async throws -> T

    func withStreamingOutput<T: Sendable>(
        title: String,
        operation:
            @escaping @Sendable (@escaping @Sendable (ByteBuffer) async throws -> Void) async throws
            ->
            T
    ) async throws -> T

    // MARK: - Interactive prompts

    /// Yes/No confirmation prompt
    func yesOrNoPrompt(question: String, defaultAnswer: Bool) async throws -> Bool

    /// Single choice from a list of options
    func singleChoicePrompt(
        title: String?,
        question: String,
        options: [String]
    ) async throws -> String

    /// Free-text input prompt
    func textPrompt(title: String?, prompt: String) async throws -> String

    /// Multiple choice selection from a list of options
    func multipleChoicePrompt(question: String, options: [String]) async throws -> [String]

    /// Secure password input prompt
    func secureTextPrompt(title: String, prompt: String) throws -> String
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

    public func selectFromStreamingTable<S: BidirectionalCollection & Sendable>(
        initial: S,
        updates: some AsyncSequence<S, Never> & Sendable,
        pageSize: Int,
        renderTable: @escaping @Sendable ([S.Element]) -> (headers: [String], rows: [[String]])
    ) async throws -> S.Element where S.Index == Int, S.Element: Sendable & Comparable {
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
        successMessage: String,
        errorMessage: String,
        operation: @Sendable @escaping (@escaping @Sendable (Double) -> Void) async throws -> T
    ) async throws -> T {
        // Default: just run the operation with no-op progress callback
        try await operation({ _ in })
    }

    public func withLabeledProgressBar<T: Sendable>(
        message: String,
        operation: @Sendable (@escaping (ProgressBarUpdate) -> Void) async throws -> T
    ) async throws -> T {
        // Default: just run the operation with no-op progress callback
        try await operation({ _ in })
    }

    public func streamingTable<T: Encodable & Sendable, E: Error>(
        initial: T,
        updates: some AsyncSequence<T, E> & Sendable,
        renderTable: @Sendable (T) -> (headers: [String], rows: [[String]])
    ) async throws {
        // Default: print each update as JSON
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys]
        if let data = try? encoder.encode(initial),
            let string = String(data: data, encoding: .utf8)
        {
            print(string)
            flushStdout()
        } else {
            assertionFailure("Failed to serialize result to JSON")
        }
        for try await update in updates {
            if let data = try? encoder.encode(update),
                let string = String(data: data, encoding: .utf8)
            {
                print(string)
                flushStdout()
            } else {
                assertionFailure("Failed to serialize result to JSON")
            }
        }
    }

    public func yesOrNoPrompt(question: String, defaultAnswer: Bool) async throws -> Bool {
        throw InteractiveSelectionRequiredError(
            argument: "confirmation",
            description: "Provide confirmation via CLI arguments"
        )
    }

    public func singleChoicePrompt(
        title: String?,
        question: String,
        options: [String]
    ) async throws -> String {
        throw InteractiveSelectionRequiredError(
            argument: "choice",
            description: "Provide the choice via CLI arguments"
        )
    }

    public func textPrompt(title: String?, prompt: String) async throws -> String {
        throw InteractiveSelectionRequiredError(
            argument: "input",
            description: "Provide the input via CLI arguments"
        )
    }

    public func multipleChoicePrompt(question: String, options: [String]) async throws -> [String] {
        throw InteractiveSelectionRequiredError(
            argument: "choices",
            description: "Provide the choices via CLI arguments"
        )
    }

    public func secureTextPrompt(title: String, prompt: String) throws -> String {
        throw InteractiveSelectionRequiredError(
            argument: "password",
            description: "Provide the password via CLI arguments"
        )
    }
}

// MARK: - Output Mode

/// The current output mode for CLI commands.
public enum OutputMode: Sendable {
    /// Human-readable interactive output
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
internal struct DefaultCLIOutput: CLIOutput, Sendable {
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
        } else {
            assertionFailure("Failed to serialize result to JSON")
        }
    }

    func progress(message: String, percent: Double?) {
        if let percent {
            print("[\(Int(percent * 100))%] \(message)")
        } else {
            print("... \(message)")
        }
    }

    func withStreamingOutputBox<T: Sendable>(
        title: String,
        maxLines: Int,
        operation:
            @escaping @Sendable (@escaping @Sendable (ByteBuffer) async throws -> Void) async throws
            ->
            T
    ) async throws -> T {
        // Default: just print each line as it comes
        try await operation { @Sendable chunk in
            print(String(buffer: chunk))
        }
    }

    func withStreamingOutput<T: Sendable>(
        title: String,
        operation:
            @escaping @Sendable (@escaping @Sendable (ByteBuffer) async throws -> Void) async throws
            ->
            T
    ) async throws -> T {
        // Default: just print each line as it comes
        try await operation { @Sendable chunk in
            print(String(buffer: chunk))
        }
    }
}
