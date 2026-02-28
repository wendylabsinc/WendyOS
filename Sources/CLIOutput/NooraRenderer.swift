import Foundation
import NIOCore
internal import Noora

// Helper to flush stdout in Swift 6
@inline(__always)
private func flushStdout() {
    #if os(Linux)
        fflush(nil)
    #else
        fflush(stdout)
    #endif
}

// Helper to wrap non-Sendable closures for use in Sendable contexts
private struct UnsafeSendableBox<T>: @unchecked Sendable {
    let value: T
}

let noora = Noora(theme: .emerald(), terminal: Terminal(signalBehavior: .none))

/// Interactive CLI output renderer using Noora TUI library.
public struct NooraRenderer: CLIOutput, Sendable {
    public init() {}

    public func success(_ message: String) {
        noora.success(SuccessAlert(stringLiteral: message))
    }

    public func error(_ message: String, suggestion: String?) {
        // ErrorAlert doesn't support suggestions in its string literal init,
        // so we show them separately
        noora.error(ErrorAlert(stringLiteral: message))
        if let suggestion {
            noora.info(InfoAlert(stringLiteral: "Suggestion: \(suggestion)"))
        }
    }

    public func info(_ message: String) {
        noora.info(InfoAlert(stringLiteral: message))
    }

    public func warning(_ message: String) {
        noora.warning(WarningAlert(stringLiteral: message))
    }

    public func table(headers: [String], rows: [[String]]) {
        noora.table(headers: headers, rows: rows)
    }

    public func streamingTable<T: Encodable & Sendable, E: Error>(
        initial: T,
        updates: some AsyncSequence<T, E> & Sendable,
        renderTable: @escaping @Sendable (T) -> (headers: [String], rows: [[String]])
    ) async throws {
        let initialRendered = renderTable(initial)
        let tableData = TableData(
            columns: initialRendered.headers.map { TableColumn(title: $0) },
            rows: initialRendered.rows.map { row in row.map { TerminalText(stringLiteral: $0) } }
        )

        // Convert AsyncStream<T> to AsyncStream<TableData> for Noora
        let (stream, continuation) = AsyncStream<TableData>.makeStream()

        // Use async let to run producer and consumer concurrently with structured concurrency
        async let producer: Void = {
            for try await value in updates {
                let rendered = renderTable(value)
                let tableData = TableData(
                    columns: rendered.headers.map { TableColumn(title: $0) },
                    rows: rendered.rows.map { row in row.map { TerminalText(stringLiteral: $0) } }
                )
                continuation.yield(tableData)
            }
            continuation.finish()
        }()

        async let consumer: Void = noora.table(tableData, updates: stream)

        _ = try await (producer, consumer)
    }

    private actor Results<S: BidirectionalCollection> where S.Index == Int, S.Element: Sendable {
        var results: S

        init(initial: S) {
            self.results = initial
        }

        subscript(index: Int) -> S.Element? {
            if index > results.endIndex {
                return nil
            }
            return results[index]
        }

        func set(to value: S) {
            results = value
        }
    }

    public func selectFromStreamingTable<S: BidirectionalCollection & Sendable, E: Error>(
        initial: S,
        updates: some AsyncSequence<S, E> & Sendable,
        pageSize: Int,
        renderTable: @escaping @Sendable ([S.Element]) -> (headers: [String], rows: [[String]])
    ) async throws -> S.Element where S.Index == Int, S.Element: Sendable & Comparable {
        let initialRendered = renderTable(initial.sorted())

        let tableData = TableData(
            columns: initialRendered.headers.map { TableColumn(title: $0) },
            rows: initialRendered.rows.map { row in row.map { TerminalText(stringLiteral: $0) } }
        )

        // Convert AsyncStream<T> to AsyncStream<TableData> for Noora
        let (stream, continuation) = AsyncStream<TableData>.makeStream()

        // Use async let to run producer and consumer concurrently with structured concurrency
        return try await withThrowingTaskGroup(of: Void.self) { group in
            let results = Results<[S.Element]>(initial: initial.sorted())

            group.addTask { [updates] in
                for try await value in updates {
                    let value = value.sorted()
                    let rendered = renderTable(value)
                    let tableData = TableData(
                        columns: rendered.headers.map { TableColumn(title: $0) },
                        rows: rendered.rows.map { row in row.map { TerminalText(stringLiteral: $0) }
                        }
                    )
                    await results.set(to: value)
                    continuation.yield(tableData)
                }
                continuation.finish()
            }

            defer { group.cancelAll() }
            repeat {
                let index = try await noora.selectableTable(
                    tableData,
                    updates: stream,
                    pageSize: pageSize
                )
                if let result = await results[index] {
                    return result
                }
            } while !Task.isCancelled

            throw CancellationError()
        }
    }

    public func selectFromTable(
        title: String?,
        headers: [String],
        rows: [[String]],
        pageSize: Int
    ) async throws -> Int {
        let tableRows: [TableRow] = rows.map { row in
            TableRow(row.map { TerminalText(stringLiteral: $0) })
        }

        let tableData = TableData(
            columns: headers.map { TableColumn(title: $0) },
            rows: tableRows
        )

        return try await noora.selectableTable(tableData, pageSize: pageSize)
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
        noora.info(InfoAlert(stringLiteral: text))
    }

    public func withProgress<T: Sendable>(
        message: String,
        successMessage: String,
        errorMessage: String,
        operation: @escaping @Sendable () async throws -> T
    ) async throws -> T {
        try await noora.progressStep(
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
        successMessage: String,
        errorMessage: String,
        operation: @escaping @Sendable (@escaping @Sendable (Double) -> Void) async throws -> T
    ) async throws -> T {
        try await noora.progressBarStep(message: message) { updateProgress in
            // Wrap the non-Sendable updateProgress in an @unchecked Sendable box
            let box = UnsafeSendableBox(value: updateProgress)
            return try await operation { progress in
                box.value(progress)
            }
        }
    }

    public func withLabeledProgressBar<T: Sendable>(
        message: String,
        operation: @escaping @Sendable (@escaping (ProgressBarUpdate) -> Void) async throws -> T
    ) async throws -> T {
        try await _withLabeledProgressBarImpl(message: message, operation: operation)
    }

    public func withStreamingOutput<T: Sendable>(
        title: String,
        operation:
            @escaping @Sendable (@escaping @Sendable (ByteBuffer) async throws -> Void) async throws
            ->
            T
    ) async throws -> T {
        return try await operation { chunk in
            try FileHandle.standardOutput.write(contentsOf: chunk.readableBytesView)
        }
    }

    public func withStreamingOutputBox<T: Sendable>(
        title: String,
        maxLines: Int,
        operation:
            @escaping @Sendable (@escaping @Sendable (ByteBuffer) async throws -> Void) async throws
            ->
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

        let collector = StdoutCollector(title: title, width: 80, height: maxLines)
        await collector.box.printTop()

        do {
            let value = try await operation { chunk in
                // Write to temp file
                try fileHandle.write(contentsOf: chunk.readableBytesView)
                await collector.append(chunk)
            }
            await collector.finish()
            noora.success("Full output: \(logFile.path)")
            return value
        } catch {
            await collector.finish()
            var output = ByteBuffer()
            for line in await collector.lastLines {
                output.writeImmutableBuffer(line)
                output.writeString("\n")
            }
            try FileHandle.standardError.write(contentsOf: output.readableBytesView)
            noora.error("Full output: \(logFile.path)")
            throw error
        }
    }

    // MARK: - Interactive prompts

    public func yesOrNoPrompt(question: String, defaultAnswer: Bool) async throws -> Bool {
        noora.yesOrNoChoicePrompt(
            question: TerminalText(stringLiteral: question),
            defaultAnswer: defaultAnswer
        )
    }

    public func singleChoicePrompt<Option: CustomStringConvertible & Equatable>(
        title: String?,
        question: String,
        options: [Option]
    ) async throws -> Option {
        noora.singleChoicePrompt(
            title: title.map { TerminalText(stringLiteral: $0) },
            question: TerminalText(stringLiteral: question),
            options: options
        )
    }

    public func textPrompt(title: String?, prompt: String) async throws -> String {
        noora.textPrompt(
            title: title.map { TerminalText(stringLiteral: $0) },
            prompt: TerminalText(stringLiteral: prompt)
        )
    }

    public func multipleChoicePrompt<Option: CustomStringConvertible & Equatable>(
        question: String,
        options: [Option]
    ) async throws -> [Option] {
        noora.multipleChoicePrompt(
            question: TerminalText(stringLiteral: question),
            options: options
        )
    }

    public func secureTextPrompt(title: String, prompt: String) throws -> String {
        try CLIOutput_secureTextPrompt(title: title, prompt: prompt)
    }
}

fileprivate actor StdoutCollector {
    var output = ByteBuffer()
    var lastLines = [ByteBuffer]()
    nonisolated let box: BorderedBox

    init(title: String, width: Int, height: Int) {
        self.box = BorderedBox(title: title, width: width, height: height)
    }

    func finish() async {
        await box.finish()
    }

    func append(_ chunk: ByteBuffer) async {
        output.writeImmutableBuffer(chunk)
        while let newlineIndex = output.readableBytesView.firstIndex(
            of: UInt8(ascii: "\n")
        ) {
            let lineLength = output.readableBytesView.distance(
                from: output.readableBytesView.startIndex,
                to: newlineIndex
            )
            let lineBuffer = output.readSlice(length: lineLength)!
            output.moveReaderIndex(forwardBy: 1)  // Skip the newline
            lastLines.append(lineBuffer)
            var line = String(buffer: lineBuffer)
            // Strip ANSI escape sequences (and orphaned sequences split across chunks)
            line.replace(
                /\u{1B}\[[0-9;]*[A-Za-z~]|\[[0-9;]*[A-Za-z~]|\u{1B}/,
                with: ""
            )
            line = line.trimmingCharacters(in: .whitespacesAndNewlines)
            if line.isEmpty { continue }
            await box.addLine(line)
        }
        output.discardReadBytes()
        if lastLines.count > 200 {
            lastLines.removeFirst(lastLines.count - 200)
        }
    }
}

/// A fixed-size bordered box that redraws in place for streaming terminal output.
/// Uses an actor to ensure thread-safe access from concurrent stdout/stderr streams.
fileprivate actor BorderedBox {
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
