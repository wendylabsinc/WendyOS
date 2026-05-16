internal import Foundation
public import Subprocess

public enum WendyE2EShellDialect: String, Sendable {
    case posix
    case power
}

public struct WendyE2EShellResult: Sendable {
    public let machine: WendyE2EMachine
    public let dialect: WendyE2EShellDialect
    public let command: String
    public let processID: String?
    public let status: TerminationStatus
    public let duration: Duration
    public let standardOutput: String
    public let standardError: String

    public var stdout: String {
        self.standardOutput
    }

    public var stderr: String {
        self.standardError
    }

    public var isSuccess: Bool {
        self.status.isSuccess
    }

    public var isFailure: Bool {
        !self.isSuccess
    }

    public var normalizedStandardOutput: String {
        Self.normalizeLineEndings(self.standardOutput)
    }

    public var normalizedStandardError: String {
        Self.normalizeLineEndings(self.standardError)
    }

    public var normalizedStdout: String {
        self.normalizedStandardOutput
    }

    public var normalizedStderr: String {
        self.normalizedStandardError
    }

    public func requireSuccess() throws {
        guard self.status.isSuccess else {
            throw WendyE2EMachineError.commandFailed(
                machine: self.machine.description,
                command: self.command,
                terminationStatus: self.status
            )
        }
    }

    private static func normalizeLineEndings(_ value: String) -> String {
        value.replacingOccurrences(of: "\r\n", with: "\n")
    }
}
