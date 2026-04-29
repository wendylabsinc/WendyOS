import Foundation

public struct CommandResult: Sendable {
    public let exitStatus: Int32
    public let stdout: String
    public let stderr: String

    public init(exitStatus: Int32, stdout: String, stderr: String) {
        self.exitStatus = exitStatus
        self.stdout = stdout
        self.stderr = stderr
    }
}

public enum MachineError: Error, CustomStringConvertible {
    case invalidMachineSpec(String)
    case invalidPath(String)
    case commandFailed(machine: String, command: String, result: CommandResult)

    public var description: String {
        switch self {
        case .invalidMachineSpec(let spec):
            return "Invalid machine spec: \(spec)"
        case .invalidPath(let path):
            return "Invalid path: \(path)"
        case .commandFailed(let machine, let command, let result):
            var message = "Command failed on \(machine) (exit \(result.exitStatus)): \(command)"
            if !result.stdout.isEmpty {
                message += "\nstdout:\n\(result.stdout)"
            }
            if !result.stderr.isEmpty {
                message += "\nstderr:\n\(result.stderr)"
            }
            return message
        }
    }
}

public struct Machine: Sendable {
    enum Location: Sendable {
        case local
        case ssh(target: String)
    }

    let location: Location
    let baseDirectory: String

    public static func local(_ path: String) -> Self {
        Self(location: .local, baseDirectory: (path as NSString).expandingTildeInPath)
    }

    public static func ssh(_ spec: String) -> Self {
        do {
            return try parseSSH(spec)
        } catch {
            preconditionFailure("\(error)")
        }
    }

    public static func parse(_ spec: String) throws -> Self {
        if spec.hasPrefix("local:") {
            let path = String(spec.dropFirst("local:".count))
            guard !path.isEmpty else {
                throw MachineError.invalidMachineSpec(spec)
            }
            return .local(path)
        }
        return try parseSSH(spec)
    }

    public var isLocal: Bool {
        if case .local = self.location {
            return true
        }
        return false
    }

    public var sshTarget: String? {
        if case .ssh(let target) = self.location {
            return target
        }
        return nil
    }

    public var description: String {
        switch self.location {
        case .local:
            return "local:\(self.baseDirectory)"
        case .ssh(let target):
            return "\(target):\(self.baseDirectory)"
        }
    }

    @discardableResult
    public func run(_ command: String) async throws -> CommandResult {
        let result: CommandResult
        switch self.location {
        case .local:
            result = try await ProcessRunner.run(
                executable: "/bin/bash",
                arguments: ["-lc", self.wrapped(command)],
                commandDescription: "[\(self.description)] \(command)"
            )
        case .ssh(let target):
            result = try await ProcessRunner.run(
                executable: "/usr/bin/ssh",
                arguments: [target, self.wrapped(command)],
                commandDescription: "[\(self.description)] \(command)"
            )
        }

        guard result.exitStatus == 0 else {
            throw MachineError.commandFailed(
                machine: self.description,
                command: command,
                result: result
            )
        }
        return result
    }

    public func push(_ sourcePath: String, to destination: Machine) async throws {
        let sourceName = try Self.lastPathComponent(of: sourcePath)
        try await destination.prepareIncomingPath(sourceName)

        switch (self.location, destination.location) {
        case (.local, .local):
            let source = try self.localAbsolutePath(for: sourcePath)
            let target = try destination.localAbsolutePath(for: sourceName)
            let result = try await ProcessRunner.run(
                executable: "/bin/cp",
                arguments: ["-R", source, target],
                commandDescription:
                    "[\(self.description) -> \(destination.description)] cp -R \(sourcePath) \(sourceName)"
            )
            guard result.exitStatus == 0 else {
                throw MachineError.commandFailed(
                    machine: "\(self.description) -> \(destination.description)",
                    command: "push \(sourcePath)",
                    result: result
                )
            }
        case (.local, .ssh), (.ssh, .local):
            let result = try await ProcessRunner.run(
                executable: "/usr/bin/scp",
                arguments: [
                    "-r", try self.copyArgument(forSourcePath: sourcePath),
                    try destination.copyArgument(forDestinationPath: sourceName),
                ],
                commandDescription:
                    "[\(self.description) -> \(destination.description)] scp -r \(sourcePath) \(sourceName)"
            )
            guard result.exitStatus == 0 else {
                throw MachineError.commandFailed(
                    machine: "\(self.description) -> \(destination.description)",
                    command: "push \(sourcePath)",
                    result: result
                )
            }
        case (.ssh, .ssh):
            let result = try await ProcessRunner.run(
                executable: "/usr/bin/scp",
                arguments: [
                    "-3", "-r", try self.copyArgument(forSourcePath: sourcePath),
                    try destination.copyArgument(forDestinationPath: sourceName),
                ],
                commandDescription:
                    "[\(self.description) -> \(destination.description)] scp -3 -r \(sourcePath) \(sourceName)"
            )
            guard result.exitStatus == 0 else {
                throw MachineError.commandFailed(
                    machine: "\(self.description) -> \(destination.description)",
                    command: "push \(sourcePath)",
                    result: result
                )
            }
        }
    }

    private static func parseSSH(_ spec: String) throws -> Self {
        guard let colonIndex = spec.firstIndex(of: ":") else {
            throw MachineError.invalidMachineSpec(spec)
        }

        let target = String(spec[..<colonIndex])
        let path = String(spec[spec.index(after: colonIndex)...])
        guard !target.isEmpty, !path.isEmpty else {
            throw MachineError.invalidMachineSpec(spec)
        }

        return Self(location: .ssh(target: target), baseDirectory: path)
    }

    private static func lastPathComponent(of path: String) throws -> String {
        let component = (path as NSString).lastPathComponent
        guard !component.isEmpty, component != "/", component != "." else {
            throw MachineError.invalidPath(path)
        }
        return component
    }

    private func wrapped(_ command: String) -> String {
        "cd \(Self.shellQuote(self.baseDirectory)) && \(command)"
    }

    private func prepareIncomingPath(_ relativePath: String) async throws {
        try await self.run("rm -rf \(Self.shellQuote(relativePath))")
    }

    private func localAbsolutePath(for relativePath: String) throws -> String {
        guard self.isLocal else {
            throw MachineError.invalidPath(relativePath)
        }
        return (self.baseDirectory as NSString).appendingPathComponent(relativePath)
    }

    private func copyArgument(forSourcePath sourcePath: String) throws -> String {
        switch self.location {
        case .local:
            return try self.localAbsolutePath(for: sourcePath)
        case .ssh(let target):
            let absolutePath = (self.baseDirectory as NSString).appendingPathComponent(sourcePath)
            return "\(target):\(Self.shellQuote(absolutePath))"
        }
    }

    private func copyArgument(forDestinationPath destinationPath: String) throws -> String {
        switch self.location {
        case .local:
            return try self.localAbsolutePath(for: destinationPath)
        case .ssh(let target):
            let absolutePath = (self.baseDirectory as NSString).appendingPathComponent(
                destinationPath
            )
            return "\(target):\(Self.shellQuote(absolutePath))"
        }
    }

    private static func shellQuote(_ value: String) -> String {
        "'" + value.replacingOccurrences(of: "'", with: "'\\''") + "'"
    }
}

private enum ProcessRunner {
    static func run(
        executable: String,
        arguments: [String],
        commandDescription: String
    ) async throws -> CommandResult {
        fputs("$ \(commandDescription)\n", stderr)

        let process = Process()
        process.executableURL = URL(fileURLWithPath: executable)
        process.arguments = arguments

        let stdoutPipe = Pipe()
        let stderrPipe = Pipe()
        let stdoutBuffer = OutputBuffer(forwardingTo: FileHandle.standardOutput)
        let stderrBuffer = OutputBuffer(forwardingTo: FileHandle.standardError)

        process.standardOutput = stdoutPipe
        process.standardError = stderrPipe

        stdoutPipe.fileHandleForReading.readabilityHandler = { handle in
            stdoutBuffer.consume(handle.availableData)
        }
        stderrPipe.fileHandleForReading.readabilityHandler = { handle in
            stderrBuffer.consume(handle.availableData)
        }

        try process.run()
        let exitStatus = await withCheckedContinuation { continuation in
            process.terminationHandler = { process in
                continuation.resume(returning: process.terminationStatus)
            }
        }

        stdoutPipe.fileHandleForReading.readabilityHandler = nil
        stderrPipe.fileHandleForReading.readabilityHandler = nil
        stdoutBuffer.consume(stdoutPipe.fileHandleForReading.readDataToEndOfFile())
        stderrBuffer.consume(stderrPipe.fileHandleForReading.readDataToEndOfFile())

        return CommandResult(
            exitStatus: exitStatus,
            stdout: stdoutBuffer.string,
            stderr: stderrBuffer.string
        )
    }
}

private final class OutputBuffer: @unchecked Sendable {
    private let lock = NSLock()
    private var data = Data()
    private let forward: FileHandle

    init(forwardingTo forward: FileHandle) {
        self.forward = forward
    }

    func consume(_ chunk: Data) {
        guard !chunk.isEmpty else {
            return
        }
        self.lock.withLock {
            self.data.append(chunk)
        }
        try? self.forward.write(contentsOf: chunk)
    }

    var string: String {
        self.lock.withLock {
            String(decoding: self.data, as: UTF8.self)
        }
    }
}

extension NSLock {
    fileprivate func withLock<T>(_ body: () -> T) -> T {
        self.lock()
        defer { self.unlock() }
        return body()
    }
}
