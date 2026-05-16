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
    public let standardOutput: String
    public let standardError: String

    public var stdout: String {
        self.standardOutput
    }

    public var stderr: String {
        self.standardError
    }

    public var normalizedStdout: String {
        Self.normalizeLineEndings(self.standardOutput)
    }

    public var normalizedStderr: String {
        Self.normalizeLineEndings(self.standardError)
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
