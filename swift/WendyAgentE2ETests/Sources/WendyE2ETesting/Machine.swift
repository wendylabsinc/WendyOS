import Darwin
import Foundation
public import Subprocess

#if canImport(System)
    import System
#else
    import SystemPackage
#endif

public struct Machine: Sendable {
    public let name: String
    public let ssh: String?
    public let workingDirectory: String?
    public let verbose: Bool

    public var id: String {
        let location = self.ssh ?? "local"
        if let workingDirectory = self.workingDirectory {
            return "\(location):\(workingDirectory)"
        }

        return "\(location):~"
    }

    // MARK: - Creating Machines

    public init(
        name: String,
        ssh: String? = nil,
        workingDirectory: String? = nil,
        verbose: Bool = false,
        sshExecutable: String = "/usr/bin/ssh"
    ) {
        precondition(!name.isEmpty, "name must not be empty")
        precondition(ssh?.isEmpty != true, "ssh must not be empty")
        precondition(workingDirectory?.isEmpty != true, "workingDirectory must not be empty")
        precondition(!sshExecutable.isEmpty, "sshExecutable must not be empty")

        let currentDirectoryPathOrNil = ssh == nil ? FileManager.default.currentDirectoryPath : nil

        self.name = name
        self.ssh = ssh
        self.workingDirectory = workingDirectory ?? currentDirectoryPathOrNil
        self.verbose = verbose
        self.sshExecutable = sshExecutable
    }

    // MARK: - Running Commands

    public func run(
        _ command: String,
        filePath: String = #filePath,
        function: String = #function,
        line: Int = #line
    ) async throws {
        let record = try await self.run(
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

    public func run<Output: OutputProtocol, Error: ErrorOutputProtocol>(
        _ command: String,
        output: Output,
        error: Error = .discarded,
        filePath: String = #filePath,
        function: String = #function,
        line: Int = #line
    ) async throws -> ExecutionRecord<Output, Error> {
        if self.verbose {
            Self.printCommand(machine: self.name, command: command)
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
            machine: self,
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

    public func run<Result>(
        _ command: String,
        output: StringOutput<UTF8> = .string(limit: .max),
        error: StringOutput<UTF8> = .string(limit: .max),
        filePath: String = #filePath,
        function: String = #function,
        line: Int = #line,
        body: @Sendable (_ standardOutput: String, _ standardError: String) async throws -> Result
    ) async throws -> Result {
        let record = try await self.run(
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

    // MARK: - Private

    private let sshExecutable: String

    private static let e2eTestRecordsDirectoryName: String = {
        let formatter = DateFormatter()
        formatter.calendar = Calendar(identifier: .gregorian)
        formatter.locale = Locale(identifier: "en_US_POSIX")
        formatter.timeZone = TimeZone(secondsFromGMT: 0)
        formatter.dateFormat = "yyyy-MM-dd.HH-mm-ss"

        return "e2e-test-records.\(formatter.string(from: Date()))"
    }()

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
            workingDirectory: self.workingDirectory.map { FilePath($0) }
        )
    }

    private func wrapped(_ command: String) -> String {
        guard let workingDirectory = self.workingDirectory else {
            return command
        }

        return "cd \(Self.shellQuote(workingDirectory)) && \(command)"
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
        fputs("[\(machine)] $ \(command)\n", stderr)
    }

    private static func writeExecutionReport(
        machine: Machine,
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
                        machine: machine,
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
            fputs("Failed to write Wendy E2E command report: \(error)\n", stderr)
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
        let environment = ProcessInfo.processInfo.environment
        if let path = environment["WENDY_AGENT_E2E_TEST_RECORDS_DIR"], !path.isEmpty {
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
            .deletingLastPathComponent()  // swift/WendyAgentE2ETests
    }

    private static func fileName(from filePath: String) -> String {
        URL(fileURLWithPath: filePath, isDirectory: false).deletingPathExtension().lastPathComponent
    }

    private static func slug(_ value: String) -> String {
        var slug = ""
        var needsSeparator = false

        for scalar in value.unicodeScalars {
            switch scalar.value {
            case 48...57, 65...90, 97...122:
                if needsSeparator, !slug.isEmpty {
                    slug.append("-")
                }
                slug.append(String(scalar).lowercased())
                needsSeparator = false
            default:
                needsSeparator = !slug.isEmpty
            }
        }

        return slug.isEmpty ? "unknown" : slug
    }

    private static func reportHeader(filePath: String, function: String) -> String {
        """
        # Wendy E2E test report

        - Source: `\(filePath)`
        - Function: `\(function)`

        """
    }

    private static func commandReport(
        machine: Machine,
        command: String,
        filePath: String,
        line: Int,
        processIdentifier: String?,
        terminationStatus: String,
        duration: Duration,
        standardOutput: String,
        standardError: String
    ) -> String {
        """

        ---

        ## Command

        - Source: `\(filePath):\(line)`
        - Machine: `\(machine.name)`
        - Machine ID: `\(machine.id)`
        - SSH: `\(machine.ssh ?? "<none>")`
        - Working directory: `\(machine.workingDirectory ?? "<none>")`
        - Command: `\(command)`
        - Process ID: `\(processIdentifier ?? "<unavailable>")`
        - Termination status: `\(terminationStatus)`
        - Duration: `\(duration)`

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
        self.id
    }
}
