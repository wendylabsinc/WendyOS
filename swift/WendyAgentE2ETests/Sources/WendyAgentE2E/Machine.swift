import Foundation
import Subprocess

#if canImport(System)
    import System
#else
    import SystemPackage
#endif

// MARK: - Public

public actor Machine {
    public nonisolated let sshTarget: String
    public nonisolated let baseDirectory: String

    public static func ssh(_ spec: String) -> Machine {
        do {
            return try self.parse(spec)
        } catch {
            preconditionFailure("\(error)")
        }
    }

    public static func parse(_ spec: String) throws -> Machine {
        try self.parse(spec, sshExecutable: "/usr/bin/ssh")
    }

    public func close() async throws {
        guard try await self.isConnected() else {
            try? FileManager.default.removeItem(atPath: self.controlPath)
            return
        }

        _ = try await Self.invokeSSH(
            executable: self.sshExecutable,
            arguments: [
                "-o", "ControlPath=\(self.controlPath)",
                "-O", "exit",
                self.sshTarget,
            ],
            output: .discarded,
            error: .discarded
        )
        try? FileManager.default.removeItem(atPath: self.controlPath)
    }

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
        try await self.ensureConnected()
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
        try await self.ensureConnected()
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

    static func parse(_ spec: String, sshExecutable: String) throws -> Machine {
        guard let colonIndex = spec.firstIndex(of: ":") else {
            throw MachineError.invalidMachineSpec(spec)
        }

        let target = String(spec[..<colonIndex])
        let path = String(spec[spec.index(after: colonIndex)...])
        guard !target.isEmpty, !path.isEmpty else {
            throw MachineError.invalidMachineSpec(spec)
        }

        return Machine(
            sshTarget: target,
            baseDirectory: path,
            sshExecutable: sshExecutable,
            controlPath: Self.makeControlPath()
        )
    }

    init(
        sshTarget: String,
        baseDirectory: String,
        sshExecutable: String = "/usr/bin/ssh",
        controlPath: String,
        controlPersist: String = "10m"
    ) {
        self.sshTarget = sshTarget
        self.baseDirectory = baseDirectory
        self.sshExecutable = sshExecutable
        self.controlPath = controlPath
        self.controlPersist = controlPersist
    }

    deinit {
        let sshExecutable = self.sshExecutable
        let sshTarget = self.sshTarget
        let controlPath = self.controlPath

        Task.detached {
            _ = try? await Self.invokeSSH(
                executable: sshExecutable,
                arguments: [
                    "-o", "ControlPath=\(controlPath)",
                    "-O", "exit",
                    sshTarget,
                ],
                output: .discarded,
                error: .discarded
            )
            try? FileManager.default.removeItem(atPath: controlPath)
        }
    }

    // MARK: - Private

    private let sshExecutable: String
    private let controlPath: String
    private let controlPersist: String

    private func ensureConnected() async throws {
        guard try await self.isConnected() == false else {
            return
        }

        let controlDirectory = (self.controlPath as NSString).deletingLastPathComponent
        try FileManager.default.createDirectory(
            atPath: controlDirectory,
            withIntermediateDirectories: true
        )
        try? FileManager.default.removeItem(atPath: self.controlPath)

        let record = try await Self.invokeSSH(
            executable: self.sshExecutable,
            arguments: [
                "-MNf",
                "-o", "ControlMaster=yes",
                "-o", "ControlPersist=\(self.controlPersist)",
                "-o", "ControlPath=\(self.controlPath)",
                self.sshTarget,
            ],
            output: .string(limit: .max),
            error: .string(limit: .max)
        )

        guard record.terminationStatus.isSuccess else {
            throw MachineError.connectionFailed(
                machine: self.description,
                stderr: record.standardError ?? ""
            )
        }
    }

    private func isConnected() async throws -> Bool {
        let record = try await Self.invokeSSH(
            executable: self.sshExecutable,
            arguments: [
                "-o", "ControlPath=\(self.controlPath)",
                "-O", "check",
                self.sshTarget,
            ],
            output: .discarded,
            error: .discarded
        )
        return record.terminationStatus.isSuccess
    }

    private func commandArguments(for command: String) -> [String] {
        [
            "-T",
            "-o", "ControlMaster=no",
            "-o", "ControlPath=\(self.controlPath)",
            self.sshTarget,
            self.wrapped(command),
        ]
    }

    private func wrapped(_ command: String) -> String {
        "cd \(Self.shellQuote(self.baseDirectory)) && \(command)"
    }

    private nonisolated static func makeControlPath() -> String {
        "/tmp/wendy-e2e-\(UUID().uuidString).sock"
    }

    private nonisolated static func shellQuote(_ value: String) -> String {
        "'" + value.replacingOccurrences(of: "'", with: "'\\''") + "'"
    }

    private nonisolated static func printCommand(machine: String, command: String) {
        fputs("$ [\(machine)] \(command)\n", stderr)
    }

    private nonisolated static func forward(
        _ sequence: AsyncBufferSequence,
        to handle: FileHandle
    ) async throws {
        for try await buffer in sequence {
            let data = buffer.withUnsafeBytes { Data($0) }
            try handle.write(contentsOf: data)
        }
    }

    private nonisolated static func invokeSSH<Output: OutputProtocol, Error: ErrorOutputProtocol>(
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
    public nonisolated var description: String {
        "\(self.sshTarget):\(self.baseDirectory)"
    }
}
