import Foundation
import Subprocess

#if canImport(System)
    import System
#else
    import SystemPackage
#endif

public struct WendyE2ESession: Sendable {
    public let machine: WendyE2EMachine
    public let workingDirectory: String?
    public let env: [String: String]

    public var wendyCacheDirectory: String {
        let homeDirectory =
            self.env["HOME"].flatMap(Self.nonEmpty)
            ?? Self.defaultHomeDirectory(
                for: self.machine
            )

        switch self.machine.os {
        case .macOS:
            return Self.path(homeDirectory, "Library", "Caches", "wendy")
        case .linux, .wendyOS:
            let cacheHome =
                self.env["XDG_CACHE_HOME"].flatMap(Self.nonEmpty)
                ?? Self.path(homeDirectory, ".cache")
            return Self.path(cacheHome, "wendy")
        case .windows:
            let localAppData =
                self.env["LOCALAPPDATA"].flatMap(Self.nonEmpty)
                ?? Self.path(homeDirectory, "AppData", "Local")
            return Self.path(localAppData, "wendy")
        }
    }

    // MARK: - Beginning and Ending Sessions

    public static func begin(
        for machine: WendyE2EMachine,
        workingDirectory: String? = nil,
        env: [String: String] = [:],
        resetDirectoriesOnFirstCommand: Bool = false,
        verbose: Bool = false,
        recorder: WendyE2ERecorder? = nil
    ) async throws -> WendyE2ESession {
        let session = WendyE2ESession(
            machine: machine,
            workingDirectory: workingDirectory,
            env: env,
            resetDirectoriesOnFirstCommand: resetDirectoriesOnFirstCommand,
            recorder: recorder,
            verbose: verbose || WendyE2EEnvironment.verbose
        )

        return session
    }

    public func end() async throws {
        // Intentionally a no-op for now. This is the future hook for closing
        // persistent transports, PTYs, temp state, or other session resources.
    }

    public static func with<Result>(
        _ machine: WendyE2EMachine,
        body: @Sendable (WendyE2ESession) async throws -> Result
    ) async throws -> Result {
        var sessions: [WendyE2ESession] = []
        do {
            let session = try await Self.begin(for: machine)
            sessions.append(session)
            let result = try await body(session)
            try await Self.end(sessions)
            return result
        } catch {
            try? await Self.end(sessions)
            throw error
        }
    }

    public static func with<Result>(
        _ first: WendyE2EMachine,
        _ second: WendyE2EMachine,
        body: @Sendable (WendyE2ESession, WendyE2ESession) async throws -> Result
    ) async throws -> Result {
        var sessions: [WendyE2ESession] = []
        do {
            let firstSession = try await Self.begin(for: first)
            sessions.append(firstSession)
            let secondSession = try await Self.begin(for: second)
            sessions.append(secondSession)
            let result = try await body(firstSession, secondSession)
            try await Self.end(sessions)
            return result
        } catch {
            try? await Self.end(sessions)
            throw error
        }
    }

    public static func with<Result>(
        _ first: WendyE2EMachine,
        _ second: WendyE2EMachine,
        _ third: WendyE2EMachine,
        body: @Sendable (WendyE2ESession, WendyE2ESession, WendyE2ESession) async throws -> Result
    ) async throws -> Result {
        var sessions: [WendyE2ESession] = []
        do {
            let firstSession = try await Self.begin(for: first)
            sessions.append(firstSession)
            let secondSession = try await Self.begin(for: second)
            sessions.append(secondSession)
            let thirdSession = try await Self.begin(for: third)
            sessions.append(thirdSession)
            let result = try await body(firstSession, secondSession, thirdSession)
            try await Self.end(sessions)
            return result
        } catch {
            try? await Self.end(sessions)
            throw error
        }
    }

    // MARK: - Running Shell Commands

    public func posixShell(_ command: String) async throws -> WendyE2EShellResult {
        if self.verbose {
            Self.printCommand(machine: self.machine.name, command: command)
        }

        let resetDirectories = await self.commandSetupState.resetDirectoriesForNextCommand()
        let harnessPrefix = self.harnessPrefix(resetDirectories: resetDirectories)
        let invocation = self.invocation(for: command, harnessPrefix: harnessPrefix)

        return try await self.runShell(
            command: command,
            invocation: invocation,
            harnessPrefix: harnessPrefix,
            scriptShellName: Self.localShellName
        )
    }

    public func powerShell(_ command: String) async throws -> WendyE2EShellResult {
        if self.verbose {
            Self.printCommand(machine: self.machine.name, command: command)
        }

        let resetDirectories = await self.commandSetupState.resetDirectoriesForNextCommand()
        let harnessPrefix = self.powerShellHarnessPrefix(resetDirectories: resetDirectories)
        let invocation = try self.powerShellInvocation(for: command, harnessPrefix: harnessPrefix)

        return try await self.runShell(
            command: command,
            invocation: invocation,
            harnessPrefix: harnessPrefix,
            scriptShellName: Self.localPowerShellName
        )
    }

    public func sh(_ command: String) async throws {
        let result = try await self.posixShell(command)
        try result.requireSuccess()
    }

    public func sh<Result>(
        _ command: String,
        body: @Sendable (_ result: WendyE2EShellResult) async throws -> Result
    ) async throws -> Result {
        try await body(try await self.posixShell(command))
    }

    // MARK: - Internal

    private init(
        machine: WendyE2EMachine,
        workingDirectory: String? = nil,
        env: [String: String] = [:],
        resetDirectoriesOnFirstCommand: Bool = false,
        recorder: WendyE2ERecorder? = nil,
        verbose: Bool = false
    ) {
        precondition(workingDirectory?.isEmpty != true, "workingDirectory must not be empty")
        for key in env.keys {
            precondition(
                Self.isValidEnvironmentKey(key),
                "env keys must be valid shell variable names"
            )
        }

        self.machine = machine
        self.workingDirectory =
            workingDirectory ?? (machine.isLocal ? FileManager.default.currentDirectoryPath : nil)
        self.env = env
        self.commandSetupState = CommandSetupState(
            resetDirectoriesOnFirstCommand: resetDirectoriesOnFirstCommand
        )
        self.recorder = recorder
        self.verbose = verbose
    }

    // MARK: - Private

    private let commandSetupState: CommandSetupState
    private let recorder: WendyE2ERecorder?
    private let verbose: Bool

    private static func end(_ sessions: [WendyE2ESession]) async throws {
        for session in sessions.reversed() {
            try await session.end()
        }
    }

    private func runShell(
        command: String,
        invocation: Invocation,
        harnessPrefix: [String],
        scriptShellName: String
    ) async throws -> WendyE2EShellResult {
        let start = ContinuousClock.now
        let record = try await Self.invoke(
            invocation,
            output: StringOutput<UTF8>.string(limit: .max),
            error: StringOutput<UTF8>.string(limit: .max)
        )
        let duration = start.duration(to: .now)

        self.recorder?.record(
            session: self,
            command: command,
            processID: String(describing: record.processIdentifier),
            status: String(describing: record.terminationStatus),
            duration: duration,
            standardOutput: record.standardOutput ?? "",
            standardError: record.standardError ?? "",
            harnessPrefix: harnessPrefix,
            scriptShellName: scriptShellName
        )

        return WendyE2EShellResult(
            machine: self.machine,
            command: command,
            processID: String(describing: record.processIdentifier),
            status: WendyE2EShellStatus(record.terminationStatus),
            duration: duration,
            standardOutput: record.standardOutput ?? "",
            standardError: record.standardError ?? ""
        )
    }

    private func invocation(for command: String, harnessPrefix: [String]) -> Invocation {
        if self.machine.isLocal {
            return self.localInvocation(for: command, harnessPrefix: harnessPrefix)
        }

        return self.sshInvocation(for: command, harnessPrefix: harnessPrefix)
    }

    private func localInvocation(for command: String, harnessPrefix: [String]) -> Invocation {
        Invocation(
            executable: Self.localShellPath,
            arguments: ["-lc", self.wrapped(command, harnessPrefix: harnessPrefix)],
            environment: .inherit,
            workingDirectory: nil
        )
    }

    private func sshInvocation(for command: String, harnessPrefix: [String]) -> Invocation {
        let wrappedCommand = self.wrapped(command, harnessPrefix: harnessPrefix)
        let loginShellCommand = "exec \"${SHELL:-/bin/sh}\" -lc \(Self.shellQuote(wrappedCommand))"

        return Invocation(
            executable: "/usr/bin/ssh",
            arguments: [
                "-o",
                "BatchMode=yes",
                "-o",
                "StrictHostKeyChecking=no",
                "-o",
                "UserKnownHostsFile=/dev/null",
                "-o",
                "LogLevel=ERROR",
                "-T",
                self.sshTarget(address: self.machine.address),
                loginShellCommand,
            ],
            environment: .inherit,
            workingDirectory: nil
        )
    }

    private func powerShellInvocation(
        for command: String,
        harnessPrefix: [String]
    ) throws
        -> Invocation
    {
        guard self.machine.isLocal else {
            throw WendyE2EMachineError.powerShellUnavailable(machine: self.description)
        }

        return Invocation(
            executable: try Self.localPowerShellPath(machine: self.description),
            arguments: [
                "-NoProfile",
                "-NonInteractive",
                "-Command",
                self.powerShellWrapped(command, harnessPrefix: harnessPrefix),
            ],
            environment: .inherit,
            workingDirectory: nil
        )
    }

    private static var localShellPath: String {
        let shell = ProcessInfo.processInfo.environment["SHELL"] ?? "/bin/sh"
        let normalizedShell = shell.isEmpty ? "/bin/sh" : shell
        Self.preconditionPOSIXCompatibleShell(normalizedShell)
        return normalizedShell
    }

    private static var localShellName: String {
        let name = URL(fileURLWithPath: Self.localShellPath, isDirectory: false).lastPathComponent
        return name.isEmpty ? "sh" : name
    }

    private static var localPowerShellName: String {
        let name = (try? Self.localPowerShellPath()).map {
            URL(fileURLWithPath: $0, isDirectory: false).lastPathComponent
        }
        return name.flatMap(Self.nonEmpty) ?? "pwsh"
    }

    private static func localPowerShellPath(
        machine: String = WendyE2EMachine.current.description
    ) throws -> String {
        let candidates = ["pwsh", "pwsh.exe", "powershell", "powershell.exe"]
        guard let path = Self.findExecutable(named: candidates) else {
            throw WendyE2EMachineError.powerShellUnavailable(machine: machine)
        }
        return path
    }

    private static func preconditionPOSIXCompatibleShell(_ shell: String) {
        let shellName = URL(fileURLWithPath: shell, isDirectory: false).lastPathComponent
            .lowercased()
        let unsupportedShells: Set<String> = ["csh", "fish", "pwsh", "powershell", "tcsh"]
        precondition(
            !unsupportedShells.contains(shellName),
            """
            Wendy E2E tests require SHELL to be a POSIX-compatible shell.
            Unsupported SHELL: \(shell)
            Use sh, bash, zsh, dash, or ksh. For example:
              export SHELL=/bin/zsh
            Then rerun the E2E command.
            """
        )
    }

    private func harnessPrefix(resetDirectories: Bool) -> [String] {
        var parts = self.env.keys.sorted().map { key in
            "export \(key)=\(Self.shellEnvironmentValue(self.env[key] ?? ""))"
        }

        let setupDirectories = self.setupDirectories()
        if resetDirectories, !setupDirectories.isEmpty {
            parts.append(
                "rm -rf "
                    + setupDirectories
                    .map(Self.shellEnvironmentValue)
                    .joined(separator: " ")
            )
        }

        if !setupDirectories.isEmpty {
            parts.append(
                "mkdir -p "
                    + setupDirectories
                    .map(Self.shellEnvironmentValue)
                    .joined(separator: " ")
            )
        }

        if let workingDirectory = self.workingDirectory {
            parts.append("cd \(Self.shellEnvironmentValue(workingDirectory))")
        }

        return parts
    }

    private func powerShellHarnessPrefix(resetDirectories: Bool) -> [String] {
        var parts = self.env.keys.sorted().map { key in
            "$env:\(key) = \(Self.powerShellEnvironmentValue(self.env[key] ?? ""))"
        }

        let setupDirectories = self.setupDirectories()
        if resetDirectories {
            parts.append(
                contentsOf: setupDirectories.map { directory in
                    "Remove-Item -LiteralPath \(Self.powerShellEnvironmentValue(directory)) -Recurse -Force -ErrorAction SilentlyContinue"
                }
            )
        }

        parts.append(
            contentsOf: setupDirectories.map { directory in
                "New-Item -ItemType Directory -Force -Path \(Self.powerShellEnvironmentValue(directory)) | Out-Null"
            }
        )

        if let workingDirectory = self.workingDirectory {
            parts.append(
                "Set-Location -LiteralPath \(Self.powerShellEnvironmentValue(workingDirectory))"
            )
        }

        return parts
    }

    private func setupDirectories() -> [String] {
        var directories: [String] = []
        var seen: Set<String> = []

        func append(_ directory: String?) {
            guard let directory, !directory.isEmpty, seen.insert(directory).inserted else {
                return
            }
            directories.append(directory)
        }

        append(self.env["HOME"])
        append(self.env["TMPDIR"])
        append(self.workingDirectory)

        return directories
    }

    private func wrapped(_ command: String, harnessPrefix: [String]) -> String {
        (harnessPrefix + [command]).joined(separator: " && ")
    }

    private func powerShellWrapped(_ command: String, harnessPrefix: [String]) -> String {
        (["$ErrorActionPreference = 'Stop'"] + harnessPrefix + [command]).joined(separator: "\n")
    }

    private func sshTarget(address: String) -> String {
        let host = address.contains(":") ? "[\(address)]" : address
        return self.machine.user.map { "\($0)@\(host)" } ?? host
    }

    private static func defaultHomeDirectory(for machine: WendyE2EMachine) -> String {
        if machine.isLocal {
            FileManager.default.homeDirectoryForCurrentUser.path
        } else {
            "$HOME"
        }
    }

    private static func nonEmpty(_ value: String) -> String? {
        value.isEmpty ? nil : value
    }

    private static func path(_ first: String, _ rest: String...) -> String {
        rest.reduce(first) { path, component in
            let suffix = component.trimmingCharacters(in: CharacterSet(charactersIn: "/"))
            return path.hasSuffix("/") ? "\(path)\(suffix)" : "\(path)/\(suffix)"
        }
    }

    private static func shellEnvironmentValue(_ value: String) -> String {
        var parts: [String] = []
        var literal = ""
        var index = value.startIndex

        func flushLiteral() {
            guard !literal.isEmpty else {
                return
            }
            parts.append(Self.shellQuote(literal))
            literal = ""
        }

        while index < value.endIndex {
            guard value[index] == "$" else {
                literal.append(value[index])
                index = value.index(after: index)
                continue
            }

            let next = value.index(after: index)
            guard next < value.endIndex else {
                literal.append(value[index])
                index = next
                continue
            }

            if value[next] == "{" {
                guard let close = value[next...].firstIndex(of: "}") else {
                    literal.append(value[index])
                    index = next
                    continue
                }

                let nameStart = value.index(after: next)
                let name = String(value[nameStart..<close])
                guard Self.isValidEnvironmentName(name) else {
                    literal.append(value[index])
                    index = next
                    continue
                }

                flushLiteral()
                parts.append("${\(name)}")
                index = value.index(after: close)
                continue
            }

            guard Self.isEnvironmentNameStart(value[next]) else {
                literal.append(value[index])
                index = next
                continue
            }

            var end = value.index(after: next)
            while end < value.endIndex, Self.isEnvironmentNameBody(value[end]) {
                end = value.index(after: end)
            }

            flushLiteral()
            parts.append("$\(String(value[next..<end]))")
            index = end
        }

        flushLiteral()
        return parts.isEmpty ? "''" : parts.joined()
    }

    private static func shellQuote(_ value: String) -> String {
        "'" + value.replacingOccurrences(of: "'", with: "'\\''") + "'"
    }

    private static func powerShellEnvironmentValue(_ value: String) -> String {
        var parts: [String] = []
        var literal = ""
        var index = value.startIndex

        func flushLiteral() {
            guard !literal.isEmpty else {
                return
            }
            parts.append(Self.powerShellQuote(literal))
            literal = ""
        }

        while index < value.endIndex {
            guard value[index] == "$" else {
                literal.append(value[index])
                index = value.index(after: index)
                continue
            }

            let next = value.index(after: index)
            guard next < value.endIndex else {
                literal.append(value[index])
                index = next
                continue
            }

            if value[next] == "{" {
                guard let close = value[next...].firstIndex(of: "}") else {
                    literal.append(value[index])
                    index = next
                    continue
                }

                let nameStart = value.index(after: next)
                let name = String(value[nameStart..<close])
                guard Self.isValidEnvironmentName(name) else {
                    literal.append(value[index])
                    index = next
                    continue
                }

                flushLiteral()
                parts.append("$env:\(name)")
                index = value.index(after: close)
                continue
            }

            guard Self.isEnvironmentNameStart(value[next]) else {
                literal.append(value[index])
                index = next
                continue
            }

            var end = value.index(after: next)
            while end < value.endIndex, Self.isEnvironmentNameBody(value[end]) {
                end = value.index(after: end)
            }

            flushLiteral()
            parts.append("$env:\(String(value[next..<end]))")
            index = end
        }

        flushLiteral()
        return parts.isEmpty ? "''" : parts.joined(separator: " + ")
    }

    private static func powerShellQuote(_ value: String) -> String {
        "'" + value.replacingOccurrences(of: "'", with: "''") + "'"
    }

    private static func findExecutable(named candidates: [String]) -> String? {
        let environment = ProcessInfo.processInfo.environment
        let pathValue = environment["PATH"] ?? environment["Path"] ?? environment["path"] ?? ""
        let pathSeparator: Character
        #if os(Windows)
            pathSeparator = ";"
        #else
            pathSeparator = ":"
        #endif

        for directory in pathValue.split(separator: pathSeparator, omittingEmptySubsequences: false)
        {
            let directoryPath = directory.isEmpty ? "." : String(directory)
            for candidate in candidates {
                let path = Self.executablePath(directory: directoryPath, candidate: candidate)
                if FileManager.default.isExecutableFile(atPath: path) {
                    return path
                }
            }
        }

        return nil
    }

    private static func executablePath(directory: String, candidate: String) -> String {
        if directory.hasSuffix("/") || directory.hasSuffix("\\") {
            return "\(directory)\(candidate)"
        }

        #if os(Windows)
            return "\(directory)\\\(candidate)"
        #else
            return "\(directory)/\(candidate)"
        #endif
    }

    private static func isValidEnvironmentKey(_ key: String) -> Bool {
        guard let first = key.first else {
            return false
        }
        guard first == "_" || first.isASCII && first.isLetter else {
            return false
        }

        return key.dropFirst().allSatisfy { character in
            character == "_" || character.isASCII && (character.isLetter || character.isNumber)
        }
    }

    private static func isValidEnvironmentName(_ name: String) -> Bool {
        guard let first = name.first else {
            return false
        }
        return Self.isEnvironmentNameStart(first)
            && name.dropFirst().allSatisfy(Self.isEnvironmentNameBody)
    }

    private static func isEnvironmentNameStart(_ character: Character) -> Bool {
        character == "_" || character.isASCII && character.isLetter
    }

    private static func isEnvironmentNameBody(_ character: Character) -> Bool {
        character == "_" || character.isASCII && (character.isLetter || character.isNumber)
    }

    private static func printCommand(machine: String, command: String) {
        Self.printToStandardError("[\(machine)] $ \(command)\n")
    }

    private static func printToStandardError(_ message: String) {
        _ = try? FileDescriptor.standardError.writeAll(message.utf8)
    }

    private static func outputDescription(_ output: some Sendable) -> String {
        let value = output as Any
        if let string = value as? String {
            return string
        }

        if let string = value as? String? {
            return string ?? ""
        }

        return String(describing: value)
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

private actor CommandSetupState {
    private let resetDirectoriesOnFirstCommand: Bool
    private var didRunCommand = false

    init(resetDirectoriesOnFirstCommand: Bool) {
        self.resetDirectoriesOnFirstCommand = resetDirectoriesOnFirstCommand
    }

    func resetDirectoriesForNextCommand() -> Bool {
        defer { self.didRunCommand = true }
        return self.resetDirectoriesOnFirstCommand && !self.didRunCommand
    }
}

private struct Invocation: Sendable {
    let executable: String
    let arguments: [String]
    let environment: Subprocess.Environment
    let workingDirectory: FilePath?
}

// MARK: - CustomStringConvertible

extension WendyE2ESession: CustomStringConvertible {
    public var description: String {
        self.machine.description
    }
}
