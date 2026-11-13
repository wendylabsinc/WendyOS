internal import Foundation
import Subprocess

public struct WendyE2EShellStatus: Sendable, CustomStringConvertible {
    public var isSuccess: Bool {
        self.terminationStatus.isSuccess
    }

    public var isFailure: Bool {
        !self.isSuccess
    }

    public var description: String {
        String(describing: self.terminationStatus)
    }

    // MARK: - Internal

    let terminationStatus: TerminationStatus

    init(_ terminationStatus: TerminationStatus) {
        self.terminationStatus = terminationStatus
    }
}

public struct WendyE2EShellResult: Sendable {
    public let machine: WendyE2EMachine
    public let command: String
    public let processID: String?
    public let status: WendyE2EShellStatus
    public let duration: Duration
    public let stdout: String
    public let stderr: String

    public var normalizedStdout: String {
        Self.normalizeLineEndings(self.stdout)
    }

    public var normalizedStderr: String {
        Self.normalizeLineEndings(self.stderr)
    }

    /// Stderr as a person would read it. In JSON mode, which the CLI turns on
    /// by itself whenever it runs without a terminal, a failure is reported as
    /// one stderr line, `{"error":{"message":...,"next_steps":[...]}}`, whose
    /// text is JSON-escaped. This replaces each such envelope line with its
    /// message followed by its next steps, each on its own line indented two
    /// spaces, and leaves every other line unchanged. Use it to match error
    /// text that contains quotes or spans lines.
    public var readableStderr: String {
        Self.readableErrorText(self.stderr)
    }

    /// The transformation behind `readableStderr`, for any captured stderr.
    /// Like `unwrap_error_envelopes` in evals/agent-experience/retry_guard.py,
    /// which keeps only the message, except that the next steps are kept too,
    /// laid out as the CLI's text mode shows them.
    public static func readableErrorText(_ stderr: String) -> String {
        Self.normalizeLineEndings(stderr)
            .split(separator: "\n", omittingEmptySubsequences: false)
            .map { line in
                let candidate = line.trimmingCharacters(in: .whitespaces)
                guard candidate.hasPrefix("{"), candidate.contains("\"error\""),
                    let object = try? JSONSerialization.jsonObject(with: Data(candidate.utf8))
                        as? [String: Any],
                    let error = object["error"] as? [String: Any],
                    let message = error["message"] as? String
                else {
                    return String(line)
                }
                let steps = (error["next_steps"] as? [String]) ?? []
                return ([message] + steps.map { "  " + $0 }).joined(separator: "\n")
            }
            .joined(separator: "\n")
    }

    public func requireSuccess() throws {
        guard self.status.isSuccess else {
            throw WendyE2EMachineError.commandFailed(
                machine: self.machine.description,
                command: self.command,
                status: self.status
            )
        }
    }

    private static func normalizeLineEndings(_ value: String) -> String {
        value.replacingOccurrences(of: "\r\n", with: "\n")
    }
}
