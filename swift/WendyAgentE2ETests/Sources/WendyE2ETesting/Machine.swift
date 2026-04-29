import Darwin
import Foundation
import Subprocess

#if canImport(System)
    import System
#else
    import SystemPackage
#endif

public struct Machine: Sendable {
    public let ssh: String?
    public let path: String?

    // MARK: - Creating Machines

    public init(ssh: String? = nil, path: String? = nil, sshExecutable: String = "/usr/bin/ssh") {
        precondition(ssh?.isEmpty != true, "ssh must not be empty")
        precondition(path?.isEmpty != true, "path must not be empty")
        precondition(!sshExecutable.isEmpty, "sshExecutable must not be empty")

        self.ssh = ssh
        self.path = path ?? (ssh == nil ? FileManager.default.currentDirectoryPath : nil)
        self.sshExecutable = sshExecutable
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

        let invocation = self.invocation(for: command)
        return try await Self.invoke(
            invocation,
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

        let invocation = self.invocation(for: command)
        return try await Subprocess.run(
            .path(FilePath(invocation.executable)),
            arguments: Arguments(invocation.arguments),
            environment: invocation.environment,
            workingDirectory: invocation.workingDirectory,
            preferredBufferSize: preferredBufferSize,
            isolation: isolation,
            body: body
        )
    }

    // MARK: - Private

    private let sshExecutable: String

    private func invocation(for command: String) -> Invocation {
        if let ssh = self.ssh {
            return Invocation(
                executable: self.sshExecutable,
                arguments: [
                    "-T",
                    ssh,
                    self.wrapped(command),
                ],
                environment: .inherit,
                workingDirectory: nil
            )
        }

        let user = Self.currentUser()
        return Invocation(
            executable: user.shell,
            arguments: ["-lc", command],
            environment: Self.loginEnvironment(for: user),
            workingDirectory: self.path.map { FilePath($0) }
        )
    }

    private func wrapped(_ command: String) -> String {
        guard let path = self.path else {
            return command
        }

        return "cd \(Self.shellQuote(path)) && \(command)"
    }

    private static func shellQuote(_ value: String) -> String {
        "'" + value.replacingOccurrences(of: "'", with: "'\\''") + "'"
    }

    private static func currentUser() -> User {
        guard let entry = getpwuid(getuid()) else {
            let environment = ProcessInfo.processInfo.environment
            return User(
                name: environment["USER"] ?? "",
                home: environment["HOME"] ?? FileManager.default.homeDirectoryForCurrentUser.path,
                shell: environment["SHELL"] ?? "/bin/sh"
            )
        }

        return User(
            name: String(cString: entry.pointee.pw_name),
            home: String(cString: entry.pointee.pw_dir),
            shell: String(cString: entry.pointee.pw_shell)
        )
    }

    private static func loginEnvironment(for user: User) -> Environment {
        .custom([
            "HOME": user.home,
            "LOGNAME": user.name,
            "PATH": "/usr/bin:/bin:/usr/sbin:/sbin",
            "SHELL": user.shell,
            "USER": user.name,
        ])
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

    private static func invoke<Output: OutputProtocol, Error: ErrorOutputProtocol>(
        _ invocation: Invocation,
        output: Output,
        error: Error
    ) async throws -> ExecutionRecord<Output, Error> {
        try await Subprocess.run(
            .path(FilePath(invocation.executable)),
            arguments: Arguments(invocation.arguments),
            environment: invocation.environment,
            workingDirectory: invocation.workingDirectory,
            output: output,
            error: error
        )
    }
}

private struct Invocation: Sendable {
    let executable: String
    let arguments: [String]
    let environment: Environment
    let workingDirectory: FilePath?
}

private struct User: Sendable {
    let name: String
    let home: String
    let shell: String
}

// MARK: - CustomStringConvertible

extension Machine: CustomStringConvertible {
    public var description: String {
        let location = self.ssh ?? "local"
        if let path = self.path {
            return "\(location):\(path)"
        }

        return "\(location):~"
    }
}
