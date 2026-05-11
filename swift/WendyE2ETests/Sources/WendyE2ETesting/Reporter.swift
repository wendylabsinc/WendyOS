import Foundation

public struct Reporter: Sendable {
    public let reportPath: String

    public init(
        filePath: String,
        function: String,
        line: Int
    ) throws {
        self.reportPath = try Self.reportURL(filePath: filePath, function: function).path
        self.source = Source(
            filePath: filePath,
            function: function,
            line: line
        )
    }

    public func record(
        session: Session,
        command: String,
        processIdentifier: String?,
        terminationStatus: String,
        duration: Duration,
        standardOutput: String,
        standardError: String
    ) {
        do {
            let reportURL = URL(fileURLWithPath: self.reportPath, isDirectory: false)
            let fileExists = FileManager.default.fileExists(atPath: reportURL.path)

            if !fileExists {
                try Self.reportHeader(
                    filePath: self.source.filePath,
                    function: self.source.function
                )
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
                        filePath: self.source.filePath,
                        line: self.source.line,
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

    public static func slug(_ value: String) -> String {
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

    // MARK: - Private

    private struct Source: Sendable {
        let filePath: String
        let function: String
        let line: Int
    }

    private let source: Source

    private static let e2eTestRecordsDirectoryName: String = {
        let formatter = DateFormatter()
        formatter.calendar = Calendar(identifier: .gregorian)
        formatter.locale = Locale(identifier: "en_US_POSIX")
        formatter.timeZone = TimeZone(secondsFromGMT: 0)
        formatter.dateFormat = "yyyy-MM-dd.HH-mm-ss"

        return "e2e-test-records.\(formatter.string(from: Date()))"
    }()

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
            - User: `\(machine.user ?? "<none>")`
            - Address: `\(machine.address)`
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

    private static func printToStandardError(_ message: String) {
        try? FileHandle.standardError.write(contentsOf: Data(message.utf8))
    }
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
