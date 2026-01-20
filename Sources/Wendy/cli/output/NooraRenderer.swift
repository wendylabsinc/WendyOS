import Foundation
import Noora

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
}
