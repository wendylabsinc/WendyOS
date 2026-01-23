import Foundation

/// TaskLocal for propagating JSON mode throughout the CLI command execution.
/// When `true`, commands should:
/// - Output only JSON-formatted data
/// - Avoid interactive prompts (use command-line arguments instead)
/// - Provide fallback error responses when interactive input would be required
public enum JSONMode {
    @TaskLocal
    public static var isEnabled: Bool = false
}

/// A JSON-formatted error response for when a command cannot proceed in JSON mode.
public struct JSONErrorResponse: Codable {
    public let error: String
    public let reason: String
    public let suggestion: String?

    public init(error: String, reason: String, suggestion: String? = nil) {
        self.error = error
        self.reason = reason
        self.suggestion = suggestion
    }

    /// Prints the error as JSON to stdout
    public func print() {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        if let data = try? encoder.encode(self),
            let jsonString = String(data: data, encoding: .utf8)
        {
            // Use FileHandle to ensure output is flushed
            FileHandle.standardOutput.write(Data((jsonString + "\n").utf8))
        }
    }
}

/// Executes a body with JSON mode enabled.
/// This propagates the JSON mode setting via TaskLocal to all nested calls,
/// and also sets the appropriate CLI output renderer.
public func withJSONMode<T: Sendable>(
    enabled: Bool,
    _ body: @Sendable () async throws -> T
) async rethrows -> T {
    let output: any CLIOutput = enabled ? JSONRenderer() : NooraRenderer()
    let mode: OutputMode = enabled ? .json : .interactive

    return try await JSONMode.$isEnabled.withValue(enabled) {
        try await withCLIOutput(output, mode: mode) {
            defer { output.flush() }
            return try await body()
        }
    }
}

/// Returns a JSON error for missing required input and exits.
/// Use this when a command requires interactive input but is running in JSON mode.
public func jsonModeRequiresArgument(
    argument: String,
    description: String
) -> Never {
    let response = JSONErrorResponse(
        error: "missing_required_argument",
        reason: "The --\(argument) argument is required when using --json mode",
        suggestion: description
    )
    let encoder = JSONEncoder()
    encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
    if let data = try? encoder.encode(response),
        let jsonString = String(data: data, encoding: .utf8)
    {
        // Use FileHandle to ensure output is flushed before exit
        FileHandle.standardOutput.write(Data((jsonString + "\n").utf8))
    } else {
        assertionFailure("Failed to serialize result to JSON")
    }
    _Exit(1)
}
