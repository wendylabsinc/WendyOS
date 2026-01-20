import Foundation
import Noora

// Helper to flush stdout in Swift 6
@inline(__always)
private func flushStdout() {
    #if os(Linux)
        fflush(nil)
    #else
        fflush(stdout)
    #endif
}

/// Interactive CLI output renderer using Noora TUI library.
public struct NooraRenderer: CLIOutput, Sendable {
    public init() {}

    public func success(_ message: String) {
        Noora().success(SuccessAlert(stringLiteral: message))
    }

    public func error(_ message: String, suggestion: String?) {
        // ErrorAlert doesn't support suggestions in its string literal init,
        // so we show them separately
        Noora().error(ErrorAlert(stringLiteral: message))
        if let suggestion {
            Noora().info(InfoAlert(stringLiteral: "Suggestion: \(suggestion)"))
        }
    }

    public func info(_ message: String) {
        Noora().info(InfoAlert(stringLiteral: message))
    }

    public func warning(_ message: String) {
        Noora().warning(WarningAlert(stringLiteral: message))
    }

    public func table(headers: [String], rows: [[String]]) {
        Noora().table(headers: headers, rows: rows)
    }

    public func streamingTable<T: Encodable & Sendable>(
        initial: T,
        updates: AsyncStream<T>,
        renderTable: @escaping @Sendable (T) -> (headers: [String], rows: [[String]])
    ) async {
        let initialRendered = renderTable(initial)
        let tableData = TableData(
            columns: initialRendered.headers.map { TableColumn(title: $0) },
            rows: initialRendered.rows.map { row in row.map { TerminalText(stringLiteral: $0) } }
        )

        // Convert AsyncStream<T> to AsyncStream<TableData> for Noora
        let (stream, continuation) = AsyncStream<TableData>.makeStream()

        // Use async let to run producer and consumer concurrently with structured concurrency
        async let producer: Void = {
            for await value in updates {
                let rendered = renderTable(value)
                let tableData = TableData(
                    columns: rendered.headers.map { TableColumn(title: $0) },
                    rows: rendered.rows.map { row in row.map { TerminalText(stringLiteral: $0) } }
                )
                continuation.yield(tableData)
            }
            continuation.finish()
        }()

        async let consumer: Void = Noora().table(tableData, updates: stream)

        _ = await (producer, consumer)
    }

    public func selectFromTable(
        title: String?,
        headers: [String],
        rows: [[String]],
        pageSize: Int
    ) async throws -> Int {
        let tableRows: [TableRow] = rows.map { row in
            row.map { TerminalText(stringLiteral: $0) }
        }

        let tableData = TableData(
            columns: headers.map { TableColumn(title: $0) },
            rows: tableRows
        )

        return try await Noora().selectableTable(tableData, pageSize: pageSize)
    }

    public func result<T: Encodable & Sendable>(_ value: T) {
        // In interactive mode, structured results are typically
        // displayed through other methods (table, info, etc.)
        // If a command only calls result(), fall back to JSON display
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        if let data = try? encoder.encode(value),
            let string = String(data: data, encoding: .utf8)
        {
            print(string)
        }
    }

    public func progress(message: String, percent: Double?) {
        // For simple progress, just show info
        let text: String
        if let percent {
            text = "[\(Int(percent * 100))%] \(message)"
        } else {
            text = message
        }
        Noora().info(InfoAlert(stringLiteral: text))
    }

    public func withProgress<T: Sendable>(
        message: String,
        successMessage: String,
        errorMessage: String,
        operation: @escaping @Sendable () async throws -> T
    ) async throws -> T {
        try await Noora().progressStep(
            message: message,
            successMessage: successMessage,
            errorMessage: errorMessage,
            showSpinner: true
        ) { _ in
            try await operation()
        }
    }

    public func withProgressBar<T: Sendable>(
        message: String,
        operation: @escaping @Sendable (@escaping (Double) -> Void) async throws -> T
    ) async throws -> T {
        try await Noora().progressBarStep(message: message) { updateProgress in
            try await operation(updateProgress)
        }
    }

    public func withStreamingOutput<T: Sendable>(
        title: String,
        maxLines: Int,
        operation:
            @escaping @Sendable (@escaping @Sendable (String) async throws -> Void) async throws ->
            T
    ) async throws -> T {
        // Create temp file for full output
        let tempDir = FileManager.default.temporaryDirectory
        let logFile = tempDir.appendingPathComponent(
            "wendy-\(title.lowercased().replacingOccurrences(of: " ", with: "-"))-\(String(UUID().uuidString.prefix(8))).log"
        )
        FileManager.default.createFile(atPath: logFile.path, contents: nil)
        let fileHandle = try FileHandle(forWritingTo: logFile)
        defer { try? fileHandle.close() }

        let box = BorderedBox(title: title, width: 80, height: maxLines)
        await box.printTop()

        do {
            let value = try await operation { line in
                // Write to temp file
                if let data = (line + "\n").data(using: .utf8) {
                    try fileHandle.write(contentsOf: data)
                }

                // Split on newlines in case multiple lines are passed at once
                for part in line.split(separator: "\n", omittingEmptySubsequences: true) {
                    let trimmed = part.trimmingCharacters(in: .whitespaces)
                    if !trimmed.isEmpty {
                        await box.addLine(trimmed)
                    }
                }
            }
            await box.finish()
            Noora().info(InfoAlert(stringLiteral: "Full output: \(logFile.path)"))
            return value
        } catch {
            await box.finish()
            Noora().info(InfoAlert(stringLiteral: "Full output: \(logFile.path)"))
            throw error
        }
    }
}

/// A fixed-size bordered box that redraws in place for streaming terminal output.
/// Uses an actor to ensure thread-safe access from concurrent stdout/stderr streams.
private actor BorderedBox {
    let title: String
    let width: Int
    let height: Int
    private var lines: [String] = []
    private var hasDrawn = false

    init(title: String, width: Int, height: Int) {
        self.title = title
        self.width = width
        self.height = height
    }

    func printTop() {
        // First, ensure there's room for the box by printing newlines,
        // then move back up. This prevents scrolling issues.
        let totalHeight = height + 2  // top border + content + bottom border
        var frame = String(repeating: "\n", count: totalHeight)
        frame += "\u{1B}[\(totalHeight)A\r"  // Move up and to column 0

        // ┌─ Title ─────────┐
        let titlePart = "─ \(title) "
        let remaining = max(0, width - titlePart.count - 2)
        frame += "\u{1B}[2K┌\(titlePart)\(String(repeating: "─", count: remaining))┐\n"

        // Empty content lines
        for _ in 0..<height {
            frame += "\u{1B}[2K\(formatLine(""))\n"
        }
        frame += "\u{1B}[2K└\(String(repeating: "─", count: width - 2))┘"

        print(frame)
        flushStdout()
        hasDrawn = true
    }

    func addLine(_ text: String) {
        lines.append(text)
        if lines.count > height {
            lines.removeFirst()
        }
        redraw()
    }

    private func redraw() {
        guard hasDrawn else { return }

        // Build entire frame as one string to avoid escape sequence timing issues
        var frame = "\u{1B}[\(height + 1)A\r"  // Move up and to column 0

        for i in 0..<height {
            let content = i < lines.count ? lines[i] : ""
            frame += "\u{1B}[2K\(formatLine(content))\n"
        }
        frame += "\u{1B}[2K└\(String(repeating: "─", count: width - 2))┘"

        print(frame)
        flushStdout()
    }

    private func formatLine(_ text: String) -> String {
        let contentWidth = width - 4  // Account for "│ " and " │"

        // Strip ANSI escape sequences to get visible text for measurement
        var visibleText = text
        while let range = visibleText.range(
            of: "\u{1B}\\[[0-9;]*[A-Za-z~]",
            options: .regularExpression
        ) {
            visibleText.removeSubrange(range)
        }
        // Also strip OSC sequences (terminated by BEL \x07 or ST \x1B\\)
        while let range = visibleText.range(
            of: "\u{1B}\\][^\u{07}\u{1B}]*(?:\u{07}|\u{1B}\\\\)",
            options: .regularExpression
        ) {
            visibleText.removeSubrange(range)
        }

        let displayText: String
        if visibleText.count > contentWidth {
            displayText = String(visibleText.prefix(contentWidth - 1)) + "…"
        } else {
            displayText =
                visibleText + String(repeating: " ", count: contentWidth - visibleText.count)
        }
        return "│ \(displayText) │"
    }

    func finish() {
        guard hasDrawn else { return }
        // Move cursor up and clear the box area
        print("\u{1B}[\(height + 2)A", terminator: "")  // +2 for top and bottom borders
        for _ in 0..<(height + 2) {
            print("\u{1B}[2K")  // Clear entire line
        }
        // Move cursor back up to where the box started
        print("\u{1B}[\(height + 2)A", terminator: "")
        flushStdout()
    }
}
