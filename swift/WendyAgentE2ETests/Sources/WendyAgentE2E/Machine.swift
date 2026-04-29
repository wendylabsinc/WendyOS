import Foundation
import Subprocess

#if canImport(System)
    import System
#else
    import SystemPackage
#endif

public struct Machine: Sendable {
    public let sshTarget: String
    public let baseDirectory: String

    // MARK: - Creating Machines

    public init(ssh: String, path: String) throws {
        guard !ssh.isEmpty, !path.isEmpty else {
            throw MachineError.invalidMachineSpec("ssh: \(ssh), path: \(path)")
        }

        self.init(
            sshTarget: ssh,
            baseDirectory: path,
            sshExecutable: "/usr/bin/ssh"
        )
    }

    // MARK: - Running Commands

    public func run(_ command: String) async throws {
        let outcome = try await self.run(command) { _, _, stdout, stderr in
            async let forwardStdout = Self.forward(stdout, to: .standardOutput)
            async let forwardStderr = Self.forward(stderr, to: .standardError)
            _ = try await (forwardStdout, forwardStderr)
        }

        guard outcome.terminationStatus.isSuccess else {
            throw MachineError.commandFailed(
                machine: self.description,
                command: command,
                terminationStatus: outcome.terminationStatus
            )
        }
    }

    public func run<Output: OutputProtocol, Error: ErrorOutputProtocol>(
        _ command: String,
        output: Output,
        error: Error = .discarded
    ) async throws -> ExecutionRecord<Output, Error> {
        Self.printCommand(machine: self.description, command: command)

        return try await Self.invokeSSH(
            executable: self.sshExecutable,
            arguments: self.commandArguments(for: command),
            output: output,
            error: error
        )
    }

    public func run<Result>(
        _ command: String,
        preferredBufferSize: Int? = nil,
        isolation: isolated (any Actor)? = #isolation,
        body:
            @Sendable (
                _ execution: Execution,
                _ inputWriter: StandardInputWriter,
                _ standardOutput: AsyncBufferSequence,
                _ standardError: AsyncBufferSequence
            ) async throws -> Result
    ) async throws -> ExecutionOutcome<Result> {
        Self.printCommand(machine: self.description, command: command)

        return try await Subprocess.run(
            .path(FilePath(self.sshExecutable)),
            arguments: Arguments(self.commandArguments(for: command)),
            preferredBufferSize: preferredBufferSize,
            isolation: isolation,
            body: body
        )
    }

    // MARK: - Internal

    init(
        sshTarget: String,
        baseDirectory: String,
        sshExecutable: String = "/usr/bin/ssh"
    ) {
        self.sshTarget = sshTarget
        self.baseDirectory = baseDirectory
        self.sshExecutable = sshExecutable
    }

    // MARK: - Private

    private let sshExecutable: String

    private func commandArguments(for command: String) -> [String] {
        [
            "-T",
            self.sshTarget,
            self.wrapped(command),
        ]
    }

    private func wrapped(_ command: String) -> String {
        "cd \(Self.shellQuote(self.baseDirectory)) && \(command)"
    }

    private static func shellQuote(_ value: String) -> String {
        "'" + value.replacingOccurrences(of: "'", with: "'\\''") + "'"
    }

    private static func printCommand(machine: String, command: String) {
        fputs("$ [\(machine)] \(command)\n", stderr)
    }

    private static func forward(
        _ sequence: AsyncBufferSequence,
        to handle: FileHandle
    ) async throws {
        for try await buffer in sequence {
            let data = buffer.withUnsafeBytes { Data($0) }
            try handle.write(contentsOf: data)
        }
    }

    private static func invokeSSH<Output: OutputProtocol, Error: ErrorOutputProtocol>(
        executable: String,
        arguments: [String],
        output: Output,
        error: Error
    ) async throws -> ExecutionRecord<Output, Error> {
        try await Subprocess.run(
            .path(FilePath(executable)),
            arguments: Arguments(arguments),
            output: output,
            error: error
        )
    }
}

// MARK: - CustomStringConvertible

extension Machine: CustomStringConvertible {
    public var description: String {
        "\(self.sshTarget):\(self.baseDirectory)"
    }
}
