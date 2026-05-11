import Foundation
public import Subprocess

#if canImport(Darwin)
    import Darwin
#elseif canImport(Glibc)
    import Glibc
#elseif canImport(Musl)
    import Musl
#endif

#if canImport(System)
    import System
#else
    import SystemPackage
#endif

public struct Session: Sendable {
    public let machine: Machine

    // MARK: - Beginning and Ending Sessions

    public static func begin(for machine: Machine, verbose: Bool = false) async throws -> Session {
        let session = Session(
            machine: machine,
            verbose: verbose || Environment.verbose
        )

        return session
    }

    public func end() async throws {
        // Intentionally a no-op for now. This is the future hook for closing
        // persistent transports, PTYs, temp state, or other session resources.
    }

    public static func with<Result>(
        _ machine: Machine,
        body: @Sendable (Session) async throws -> Result
    ) async throws -> Result {
        var sessions: [Session] = []
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
        _ first: Machine,
        _ second: Machine,
        body: @Sendable (Session, Session) async throws -> Result
    ) async throws -> Result {
        var sessions: [Session] = []
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
        _ first: Machine,
        _ second: Machine,
        _ third: Machine,
        body: @Sendable (Session, Session, Session) async throws -> Result
    ) async throws -> Result {
        var sessions: [Session] = []
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

    public func sh(
        _ command: String,
        filePath: String = #filePath,
        function: String = #function,
        line: Int = #line
    ) async throws {
        let record = try await self.sh(
            command,
            output: .string(limit: .max),
            error: .string(limit: .max),
            filePath: filePath,
            function: function,
            line: line
        )

        guard record.terminationStatus.isSuccess else {
            throw MachineError.commandFailed(
                machine: self.description,
                command: command,
                terminationStatus: record.terminationStatus
            )
        }
    }

    public func sh<Output: OutputProtocol, Error: ErrorOutputProtocol>(
        _ command: String,
        output: Output,
        error: Error = .discarded,
        filePath: String = #filePath,
        function: String = #function,
        line: Int = #line
    ) async throws -> ExecutionRecord<Output, Error> {
        if self.verbose {
            Self.printCommand(machine: self.machine.name, command: command)
        }

        let invocation = self.invocation(for: command)
        let start = ContinuousClock.now
        let record = try await Self.invoke(
            invocation,
            output: output,
            error: error
        )
        let duration = start.duration(to: .now)

        Self.writeExecutionReport(
            session: self,
            command: command,
            filePath: filePath,
            function: function,
            line: line,
            processIdentifier: String(describing: record.processIdentifier),
            terminationStatus: String(describing: record.terminationStatus),
            duration: duration,
            standardOutput: Self.outputDescription(record.standardOutput),
            standardError: Self.outputDescription(record.standardError)
        )

        return record
    }

    public func sh<Result>(
        _ command: String,
        output: StringOutput<UTF8> = .string(limit: .max),
        error: StringOutput<UTF8> = .string(limit: .max),
        filePath: String = #filePath,
        function: String = #function,
        line: Int = #line,
        body: @Sendable (_ standardOutput: String, _ standardError: String) async throws -> Result
    ) async throws -> Result {
        let record = try await self.sh(
            command,
            output: output,
            error: error,
            filePath: filePath,
            function: function,
            line: line
        )

        guard record.terminationStatus.isSuccess else {
            throw MachineError.commandFailed(
                machine: self.description,
                command: command,
                terminationStatus: record.terminationStatus
            )
        }

        return try await body(
            record.standardOutput ?? "",
            record.standardError ?? ""
        )
    }

    public func sh<Result>(
        _ command: String,
        output: StringOutput<UTF8> = .string(limit: .max),
        error: StringOutput<UTF8> = .string(limit: .max),
        filePath: String = #filePath,
        function: String = #function,
        line: Int = #line,
        body:
            @Sendable (
                _ terminationStatus: TerminationStatus,
                _ standardOutput: String,
                _ standardError: String
            ) async throws -> Result
    ) async throws -> Result {
        let record = try await self.sh(
            command,
            output: output,
            error: error,
            filePath: filePath,
            function: function,
            line: line
        )

        return try await body(
            record.terminationStatus,
            record.standardOutput ?? "",
            record.standardError ?? ""
        )
    }

    // MARK: - Internal

    init(machine: Machine, verbose: Bool = false) {
        self.machine = machine
        self.verbose = verbose
    }

    // MARK: - Private

    private let verbose: Bool

    private static let e2eTestRecordsDirectoryName: String = {
        let formatter = DateFormatter()
        formatter.calendar = Calendar(identifier: .gregorian)
        formatter.locale = Locale(identifier: "en_US_POSIX")
        formatter.timeZone = TimeZone(secondsFromGMT: 0)
        formatter.dateFormat = "yyyy-MM-dd.HH-mm-ss"

        return "e2e-test-records.\(formatter.string(from: Date()))"
    }()

    private static func end(_ sessions: [Session]) async throws {
        for session in sessions.reversed() {
            try await session.end()
        }
    }

    private func invocation(for command: String) -> Invocation {
        let wrappedCommand = self.wrapped(command)

        if let ssh = self.machine.ssh {
            return Invocation(
                executable: self.machine.sshExecutable,
                arguments: [
                    "-T",
                    ssh,
                    wrappedCommand,
                ],
                environment: .inherit,
                workingDirectory: nil
            )
        }

        let user = Self.currentUser()
        return Invocation(
            executable: user.shell,
            arguments: ["-lc", wrappedCommand],
            environment: Self.loginEnvironment(for: user),
            workingDirectory: nil
        )
    }

    private func wrapped(_ command: String) -> String {
        var parts = self.machine.env.keys.sorted().map { key in
            "export \(key)=\(Self.shellEnvironmentValue(self.machine.env[key] ?? ""))"
        }

        if let workingDirectory = self.machine.workingDirectory {
            parts.append("cd \(Self.shellQuote(workingDirectory))")
        }
        parts.append(command)

        return parts.joined(separator: " && ")
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

    private static func currentUser() -> User {
        guard let entry = getpwuid(getuid()) else {
            return User(
                name: "",
                home: FileManager.default.homeDirectoryForCurrentUser.path,
                shell: "/bin/sh"
            )
        }

        return User(
            name: String(cString: entry.pointee.pw_name),
            home: String(cString: entry.pointee.pw_dir),
            shell: String(cString: entry.pointee.pw_shell)
        )
    }

    private static func loginEnvironment(for user: User) -> Subprocess.Environment {
        Subprocess.Environment.custom([
            "HOME": user.home,
            "LOGNAME": user.name,
            "PATH": "/usr/bin:/bin:/usr/sbin:/sbin",
            "SHELL": user.shell,
            "USER": user.name,
        ])
    }

    private static func printCommand(machine: String, command: String) {
        Self.printToStandardError("[\(machine)] $ \(command)\n")
    }

    private static func printToStandardError(_ message: String) {
        _ = try? FileDescriptor.standardError.writeAll(message.utf8)
    }

    private static func writeExecutionReport(
        session: Session,
        command: String,
        filePath: String,
        function: String,
        line: Int,
        processIdentifier: String?,
        terminationStatus: String,
        duration: Duration,
        standardOutput: String,
        standardError: String
    ) {
        do {
            let reportURL = try Self.reportURL(filePath: filePath, function: function)
            let fileExists = FileManager.default.fileExists(atPath: reportURL.path)

            if !fileExists {
                try Self.reportHeader(filePath: filePath, function: function)
                    .write(to: reportURL, atomically: true, encoding: .utf8)
            }

            let handle = try FileHandle(forWritingTo: reportURL)
            defer { try? handle.close() }
            try handle.seekToEnd()
            try handle.write(
                contentsOf: Data(
                    Self.commandReport(
                        session: session,
                        command: command,
                        filePath: filePath,
                        line: line,
                        processIdentifier: processIdentifier,
                        terminationStatus: terminationStatus,
                        duration: duration,
                        standardOutput: standardOutput,
                        standardError: standardError
                    ).utf8
                )
            )
        } catch {
            Self.printToStandardError("Failed to write Wendy E2E command report: \(error)\n")
        }
    }

    private static func reportURL(filePath: String, function: String) throws -> URL {
        let directoryURL = try Self.recordsDirectoryURL()

        return directoryURL.appendingPathComponent(
            "\(Self.fileName(from: filePath)).\(Self.slug(function)).md",
            isDirectory: false
        )
    }

    private static func recordsDirectoryURL() throws -> URL {
        try Self.preparedRecordsDirectoryURL.get()
    }

    private static let preparedRecordsDirectoryURL: Result<URL, any Error> = Result {
        let directoryURL = Self.unpreparedRecordsDirectoryURL()
        if FileManager.default.fileExists(atPath: directoryURL.path) {
            let contents = try FileManager.default.contentsOfDirectory(
                at: directoryURL,
                includingPropertiesForKeys: nil
            )
            for url in contents {
                try FileManager.default.removeItem(at: url)
            }
        } else {
            try FileManager.default.createDirectory(
                at: directoryURL,
                withIntermediateDirectories: true
            )
        }

        return directoryURL
    }

    private static func unpreparedRecordsDirectoryURL() -> URL {
        if let path = Environment.testRecordsDirectory {
            return URL(fileURLWithPath: path, isDirectory: true)
        }

        return Self.packageRootDirectoryURL()
            .appendingPathComponent(".build", isDirectory: true)
            .appendingPathComponent(Self.e2eTestRecordsDirectoryName, isDirectory: true)
    }

    private static func packageRootDirectoryURL() -> URL {
        URL(fileURLWithPath: #filePath, isDirectory: false)
            .deletingLastPathComponent()  // Sources/WendyE2ETesting
            .deletingLastPathComponent()  // Sources
            .deletingLastPathComponent()  // swift/WendyE2ETests
    }

    private static func fileName(from filePath: String) -> String {
        URL(fileURLWithPath: filePath, isDirectory: false).deletingPathExtension().lastPathComponent
    }

    static func slug(_ value: String) -> String {
        var slug = ""
        var needsSeparator = false
        var previousKind: SlugCharacterKind?
        let scalars = Array(value.unicodeScalars)

        for index in scalars.indices {
            let scalar = scalars[index]
            guard let kind = SlugCharacterKind(scalar) else {
                needsSeparator = !slug.isEmpty
                previousKind = nil
                continue
            }

            let nextKind =
                scalars.index(after: index) < scalars.endIndex
                ? SlugCharacterKind(scalars[scalars.index(after: index)]) : nil
            if !slug.isEmpty,
                needsSeparator
                    || Self.needsCamelCaseSeparator(
                        previousKind: previousKind,
                        currentKind: kind,
                        nextKind: nextKind
                    )
            {
                slug.append("-")
            }

            slug.append(String(scalar).lowercased())
            needsSeparator = false
            previousKind = kind
        }

        return slug.isEmpty ? "unknown" : slug
    }

    private static func needsCamelCaseSeparator(
        previousKind: SlugCharacterKind?,
        currentKind: SlugCharacterKind,
        nextKind: SlugCharacterKind?
    ) -> Bool {
        switch (previousKind, currentKind, nextKind) {
        case (.lower?, .upper, _), (.digit?, .upper, _), (.upper?, .upper, .lower?):
            return true
        default:
            return false
        }
    }

    private static func reportHeader(filePath: String, function: String) -> String {
        """
        # Wendy E2E test report

        - Source: `\(filePath)`
        - Function: `\(function)`

        """
    }

    private static func commandReport(
        session: Session,
        command: String,
        filePath: String,
        line: Int,
        processIdentifier: String?,
        terminationStatus: String,
        duration: Duration,
        standardOutput: String,
        standardError: String
    ) -> String {
        let machine = session.machine
        let tags = machine.tags.map(\.rawValue).sorted().joined(separator: ", ")

        return """

            ---

            ## Command

            - Source: `\(filePath):\(line)`
            - Machine: `\(machine.name)`
            - Machine ID: `\(machine.id)`
            - OS: `\(machine.os.rawValue)`
            - Tags: `\(tags.isEmpty ? "<none>" : tags)`
            - SSH: `\(machine.ssh ?? "<none>")`
            - Working directory: `\(machine.workingDirectory ?? "<none>")`
            - Command: `\(command)`
            - Process ID: `\(processIdentifier ?? "<unavailable>")`
            - Termination status: `\(terminationStatus)`
            - Duration: `\(duration)`

            ### environment

            ```text
            \(Self.environmentDescription(machine.env))
            ```

            ### stdout

            ```text
            \(standardOutput)
            ```

            ### stderr

            ```text
            \(standardError)
            ```

            """
    }

    private static func environmentDescription(_ environment: [String: String]) -> String {
        guard !environment.isEmpty else {
            return "<none>"
        }

        return environment.keys.sorted().map { key in
            "\(key)=\(environment[key] ?? "")"
        }.joined(separator: "\n")
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

private struct Invocation: Sendable {
    let executable: String
    let arguments: [String]
    let environment: Subprocess.Environment
    let workingDirectory: FilePath?
}

private enum SlugCharacterKind {
    case digit
    case lower
    case upper

    init?(_ scalar: Unicode.Scalar) {
        switch scalar.value {
        case 48...57:
            self = .digit
        case 65...90:
            self = .upper
        case 97...122:
            self = .lower
        default:
            return nil
        }
    }
}

private struct User: Sendable {
    let name: String
    let home: String
    let shell: String
}

// MARK: - CustomStringConvertible

extension Session: CustomStringConvertible {
    public var description: String {
        self.machine.description
    }
}
