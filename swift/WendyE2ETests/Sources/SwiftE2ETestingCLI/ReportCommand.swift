import ArgumentParser
import Foundation

#if canImport(FoundationXML)
    import FoundationXML
#endif

struct ReportCommand: ParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "report",
        abstract: "Generate an HTML report from a Swift E2E recording.",
        discussion: """
            Generates the same static HTML report used by the E2E review skill,
            using Swift test sources and an existing E2E recording directory.
            """
    )

    @Option(name: .long, help: "Swift package directory.")
    var packageDir = "."

    @Option(name: .long, help: "Directory containing Swift E2E test sources.")
    var testsDir: String?

    @Option(name: .long, help: "HTML report template path.")
    var template: String?

    @Option(name: .long, help: "E2E run directory. Reads tests/ and writes report.html.")
    var runDir: String?

    @Option(
        name: [.customLong("recording-dir"), .customLong("records-dir")],
        help: "Directory containing E2E command recordings and Swift Testing results."
    )
    var recordingDir: String?

    @Option(name: [.short, .long], help: "Output HTML file path.")
    var output: String?

    mutating func run() throws {
        let packageURL = URL(fileURLWithPath: packageDir)
        let testsURL = URL(
            fileURLWithPath: testsDir ?? defaultTestsDir(packageURL: packageURL).path
        )
        let templateURL = URL(
            fileURLWithPath: template
                ?? packageURL.appendingPathComponent("Support/e2e-report.template.html")
                .path
        )
        let runURL = runDir.map { URL(fileURLWithPath: $0, isDirectory: true) }
        let recordingURL = try resolvedRecordingDirectory(
            recordingDir.map { URL(fileURLWithPath: $0, isDirectory: true) }
                ?? runURL?.appendingPathComponent("tests", isDirectory: true)
                ?? latestRecordingDirectory(packageURL: packageURL)
        )
        let outputURL = URL(
            fileURLWithPath: output
                ?? runURL?.appendingPathComponent("report.html").path
                ?? recordingURL.appendingPathComponent("index.html").path
        )

        let records = try loadRecords(in: recordingURL)
        let testResults = try loadTestResults(
            in: recordingURL,
            outputDirectoryURL: outputURL.deletingLastPathComponent()
        )
        let files = try parseTests(in: testsURL, records: records, testResults: testResults)
        try renderReport(
            templateURL: templateURL,
            recordingURL: recordingURL,
            files: files,
            outputURL: outputURL
        )
    }
}

private struct CommandRun {
    var record: String
    var sourcePath: String
    var sourceFile: String
    var sourceLine: Int
    var machine = ""
    var command = ""
    var status = ""
    var duration = ""
    var stdout = ""
    var stderr = ""
}

private struct TestResultKey: Hashable {
    var suite: String
    var name: String
}

private enum ReportTestStatus {
    case passed
    case failed(String?)
    case skipped(String?)
    case unknown

    var statusClass: String {
        switch self {
        case .passed:
            return "pass"
        case .failed:
            return "fail"
        case .skipped:
            return "skipped"
        case .unknown:
            return "unknown"
        }
    }

    var statusText: String {
        switch self {
        case .passed:
            return "Passed"
        case .failed:
            return "Failed"
        case .skipped:
            return "Skipped"
        case .unknown:
            return "Unknown"
        }
    }

    var detail: String? {
        switch self {
        case .failed(let message):
            return message
        case .skipped(let reason):
            return reason
        case .unknown:
            return "No Swift Testing result was found for this test in the recording."
        case .passed:
            return nil
        }
    }
}

private struct ReportTestCase {
    var fileName: String
    var suite: String
    var name: String
    var funcLine: Int
    var disabled: String?
    var status: ReportTestStatus
    var nextLine = 0
    var aiItems: [String] = []
    var recordName = ""
    var commands: [CommandRun] = []
}

private struct ReportTestFile {
    var url: URL
    var tests: [ReportTestCase]
}

private func defaultTestsDir(packageURL: URL) -> URL {
    let e2eTestsURL = packageURL.appendingPathComponent("Tests/WendyE2ETests")
    if FileManager.default.fileExists(atPath: e2eTestsURL.path) {
        return e2eTestsURL
    }
    return packageURL.appendingPathComponent("Tests")
}

private func latestRecordingDirectory(packageURL: URL) throws -> URL {
    let buildURL = packageURL.appendingPathComponent(".build")
    let currentURL = buildURL.appendingPathComponent("e2e-recording.current")
    if FileManager.default.fileExists(atPath: currentURL.path) {
        return currentURL
    }
    let legacyCurrentURL = buildURL.appendingPathComponent("e2e-test-records.current")
    if FileManager.default.fileExists(atPath: legacyCurrentURL.path) {
        return legacyCurrentURL
    }

    let contents = try FileManager.default.contentsOfDirectory(
        at: buildURL,
        includingPropertiesForKeys: [.isDirectoryKey]
    )
    let candidates = contents.filter { url in
        guard
            url.lastPathComponent.hasPrefix("e2e-recording.")
                || url.lastPathComponent.hasPrefix("e2e-test-records.")
        else {
            return false
        }
        return (try? url.resourceValues(forKeys: [.isDirectoryKey]).isDirectory) == true
    }.sorted { $0.path < $1.path }

    guard let latest = candidates.last else {
        throw ValidationError("No e2e-recording.* directory found in \(buildURL.path)")
    }
    return latest
}

private func loadRecords(in recordingURL: URL) throws -> [String: [CommandRun]] {
    let recordURLs = try commandRecordURLs(in: recordingURL)

    var records: [String: [CommandRun]] = [:]
    for recordURL in recordURLs {
        records[recordKey(for: recordURL)] = try parseRecord(
            at: recordURL,
            relativeTo: recordingURL
        )
    }
    return records
}

private func commandRecordURLs(in recordingURL: URL) throws -> [URL] {
    guard FileManager.default.fileExists(atPath: recordingURL.path) else {
        return []
    }

    guard
        let enumerator = FileManager.default.enumerator(
            at: recordingURL,
            includingPropertiesForKeys: nil
        )
    else {
        throw ValidationError("Recording directory cannot be read: \(recordingURL.path)")
    }

    return enumerator.compactMap { element -> URL? in
        guard let url = element as? URL, isCommandRecord(url) else {
            return nil
        }
        return url
    }.sorted { $0.path < $1.path }
}

private func recordKey(for recordURL: URL) -> String {
    if recordURL.lastPathComponent == "recording.md" {
        return recordURL.deletingLastPathComponent().lastPathComponent
    }
    return recordURL.deletingPathExtension().lastPathComponent
}

private func relativePath(from baseURL: URL, to url: URL) -> String {
    let basePath = baseURL.standardizedFileURL.path
    let path = url.standardizedFileURL.path
    let prefix = basePath.hasSuffix("/") ? basePath : basePath + "/"
    guard path.hasPrefix(prefix) else {
        return url.lastPathComponent
    }
    return String(path.dropFirst(prefix.count))
}

private func parseRecord(at recordURL: URL, relativeTo recordingURL: URL) throws -> [CommandRun] {
    let text = try String(contentsOf: recordURL, encoding: .utf8)
    var commands: [CommandRun] = []

    for part in text.components(separatedBy: "\n---\n") where part.contains("## Command") {
        let sourcePath = firstMatch(#"- Source: `([^`]+):(\d+)`"#, in: part, group: 1) ?? ""
        let sourceLine =
            Int(firstMatch(#"- Source: `([^`]+):(\d+)`"#, in: part, group: 2) ?? "") ?? -1
        var command = CommandRun(
            record: relativePath(from: recordingURL, to: recordURL),
            sourcePath: sourcePath,
            sourceFile: sourcePath.isEmpty
                ? "" : URL(fileURLWithPath: sourcePath).lastPathComponent,
            sourceLine: sourceLine
        )
        command.machine = firstMatch(#"- Machine: `([^`]*)`"#, in: part) ?? ""
        command.command = firstMatch(#"- Command: `([\s\S]*?)`\n- Process ID:"#, in: part) ?? ""
        command.status = firstMatch(#"- Termination status: `([^`]*)`"#, in: part) ?? ""
        command.duration = firstMatch(#"- Duration: `([^`]*)`"#, in: part) ?? ""
        command.stdout = fenced(label: "stdout", in: part)
        command.stderr = fenced(label: "stderr", in: part)
        commands.append(command)
    }

    return commands
}

private func fenced(label: String, in text: String) -> String {
    firstMatch("### \(label)\\n\\n```text\\n([\\s\\S]*?)\\n```", in: text) ?? ""
}

private func resolvedRecordingDirectory(_ url: URL) throws -> URL {
    if try containsRecordingFiles(url) {
        return url
    }

    let nestedRecordingURL = url.appendingPathComponent("recording", isDirectory: true)
    if try containsRecordingFiles(nestedRecordingURL) {
        return nestedRecordingURL
    }

    return url
}

private func containsRecordingFiles(_ url: URL) throws -> Bool {
    guard FileManager.default.fileExists(atPath: url.path) else {
        return false
    }

    return try FileManager.default.contentsOfDirectory(
        at: url,
        includingPropertiesForKeys: nil
    ).contains { candidate in
        isCommandRecord(candidate)
    }
}

private func isCommandRecord(_ url: URL) -> Bool {
    url.pathExtension == "md"
        && url.lastPathComponent != "README.md"
        && url.lastPathComponent != "index.md"
}

private func loadTestResults(
    in recordingURL: URL,
    outputDirectoryURL: URL
) throws -> [TestResultKey: ReportTestStatus] {
    guard
        let resultURL = try testResultsURL(
            in: [
                recordingURL,
                outputDirectoryURL,
                recordingURL.deletingLastPathComponent(),
            ]
        )
    else {
        return [:]
    }

    let data = try Data(contentsOf: resultURL)
    let parser = XUnitResultParser()
    let xmlParser = XMLParser(data: data)
    xmlParser.delegate = parser
    guard xmlParser.parse() else {
        throw ValidationError(
            "Could not parse Swift Testing xUnit results: \(resultURL.path)"
        )
    }
    return parser.results
}

private func testResultsURL(in searchURLs: [URL]) throws -> URL? {
    var seen: Set<String> = []
    for searchURL in searchURLs {
        let path = searchURL.standardizedFileURL.path
        guard !seen.contains(path) else {
            continue
        }
        seen.insert(path)

        let defaultURL = searchURL.appendingPathComponent("test-results-swift-testing.xml")
        if FileManager.default.fileExists(atPath: defaultURL.path) {
            return defaultURL
        }

        guard FileManager.default.fileExists(atPath: searchURL.path) else {
            continue
        }
        let candidates = try FileManager.default.contentsOfDirectory(
            at: searchURL,
            includingPropertiesForKeys: nil
        ).filter { $0.lastPathComponent.hasSuffix("-swift-testing.xml") }
            .sorted { $0.lastPathComponent < $1.lastPathComponent }
        if let candidate = candidates.first {
            return candidate
        }
    }

    return nil
}

private final class XUnitResultParser: NSObject, XMLParserDelegate {
    var results: [TestResultKey: ReportTestStatus] = [:]

    private var current: (key: TestResultKey, failure: String?, skipped: String?)?
    private var currentElement: String?
    private var currentText = ""

    func parser(
        _ parser: XMLParser,
        didStartElement elementName: String,
        namespaceURI: String?,
        qualifiedName qName: String?,
        attributes attributeDict: [String: String]
    ) {
        switch elementName {
        case "testcase":
            guard let classname = attributeDict["classname"], let name = attributeDict["name"],
                let key = testResultKey(classname: classname, name: name)
            else {
                current = nil
                return
            }
            current = (key: key, failure: nil, skipped: nil)
        case "failure", "skipped":
            currentElement = elementName
            currentText = ""
            guard var current else {
                return
            }
            if elementName == "failure" {
                current.failure = attributeDict["message"] ?? ""
            } else {
                current.skipped = attributeDict["message"] ?? ""
            }
            self.current = current
        default:
            break
        }
    }

    func parser(_ parser: XMLParser, foundCharacters string: String) {
        if currentElement == "failure" || currentElement == "skipped" {
            currentText.append(string)
        }
    }

    func parser(
        _ parser: XMLParser,
        didEndElement elementName: String,
        namespaceURI: String?,
        qualifiedName qName: String?
    ) {
        switch elementName {
        case "failure", "skipped":
            guard var current else {
                return
            }
            let text = currentText.trimmingCharacters(in: .whitespacesAndNewlines)
            if elementName == "failure", current.failure?.isEmpty != false, !text.isEmpty {
                current.failure = text
            } else if elementName == "skipped", current.skipped?.isEmpty != false, !text.isEmpty {
                current.skipped = text
            }
            self.current = current
            currentElement = nil
            currentText = ""
        case "testcase":
            guard let current else {
                return
            }
            if let skipped = current.skipped {
                results[current.key] = .skipped(skipped.isEmpty ? nil : skipped)
            } else if let failure = current.failure {
                results[current.key] = .failed(failure.isEmpty ? nil : failure)
            } else {
                results[current.key] = .passed
            }
            self.current = nil
        default:
            break
        }
    }
}

private func testResultKey(classname: String, name: String) -> TestResultKey? {
    let suite = normalizedClassname(classname)
    let testName = normalizedTestName(name)
    guard !suite.isEmpty, !testName.isEmpty else {
        return nil
    }
    return TestResultKey(suite: suite, name: testName)
}

private func normalizedClassname(_ classname: String) -> String {
    if classname.last == "`", let start = classname.dropLast().lastIndex(of: "`") {
        let suiteStart = classname.index(after: start)
        return String(classname[suiteStart..<classname.index(before: classname.endIndex)])
    }

    return stripBackticks(String(classname.split(separator: ".").last ?? ""))
}

private func normalizedTestName(_ name: String) -> String {
    var value = name
    if value.hasSuffix("()") {
        value.removeLast(2)
    }
    return stripBackticks(value)
}

private func stripBackticks(_ value: String) -> String {
    if value.first == "`", value.last == "`" {
        return String(value.dropFirst().dropLast())
    }
    return value
}

private func parseTests(
    in testsURL: URL,
    records: [String: [CommandRun]],
    testResults: [TestResultKey: ReportTestStatus]
) throws -> [ReportTestFile] {
    let sourceURLs = try swiftTestFiles(in: testsURL)
    var files: [ReportTestFile] = []

    for sourceURL in sourceURLs {
        let source = try String(contentsOf: sourceURL, encoding: .utf8)
        let lines = source.components(separatedBy: .newlines)
        var suite = sourceURL.deletingPathExtension().lastPathComponent
        var pendingTest: (line: Int, disabled: String?)?
        var tests: [ReportTestCase] = []

        for (offset, line) in lines.enumerated() {
            let lineNumber = offset + 1
            if let suiteName = firstMatch(#"\bstruct\s+`([^`]+)`\s*\{"#, in: line)
                ?? firstMatch(#"\bstruct\s+([A-Za-z_][A-Za-z0-9_]*)\s*\{"#, in: line)
            {
                suite = suiteName
            }

            if line.contains("@Test") {
                pendingTest = (
                    line: lineNumber,
                    disabled: firstMatch(#"\.disabled\(\"([^\"]*)\"\)"#, in: line)
                )
            }

            if let functionName = firstMatch(#"\bfunc\s+`([^`]+)`\s*\("#, in: line)
                ?? firstMatch(#"\bfunc\s+([A-Za-z_][A-Za-z0-9_]*)\s*\("#, in: line),
                let test = pendingTest
            {
                tests.append(
                    ReportTestCase(
                        fileName: sourceURL.lastPathComponent,
                        suite: suite,
                        name: functionName,
                        funcLine: lineNumber,
                        disabled: test.disabled,
                        status: test.disabled.map { .skipped($0) } ?? .unknown
                    )
                )
                pendingTest = nil
            }
        }

        for testIndex in tests.indices {
            let nextLine =
                testIndex + 1 < tests.count ? tests[testIndex + 1].funcLine : lines.count + 1
            let body = Array(lines[(tests[testIndex].funcLine - 1)..<(nextLine - 1)])
            tests[testIndex].nextLine = nextLine
            tests[testIndex].aiItems = extractAIItems(from: body)
            let recordKey =
                "\(recordFileStem(sourceURL)).\(slug(tests[testIndex].name))"
            tests[testIndex].recordName = "\(recordKey)/recording.md"
            tests[testIndex].commands = records[recordKey, default: []].filter {
                command in
                command.sourceFile == sourceURL.lastPathComponent
                    && tests[testIndex].funcLine <= command.sourceLine
                    && command.sourceLine < nextLine
            }
            let key = TestResultKey(suite: tests[testIndex].suite, name: tests[testIndex].name)
            if let status = testResults[key] {
                tests[testIndex].status = status
            }
        }

        if !tests.isEmpty {
            files.append(ReportTestFile(url: sourceURL, tests: tests))
        }
    }

    return files
}

private func swiftTestFiles(in testsURL: URL) throws -> [URL] {
    var isDirectory: ObjCBool = false
    guard FileManager.default.fileExists(atPath: testsURL.path, isDirectory: &isDirectory) else {
        throw ValidationError("Tests directory not found: \(testsURL.path)")
    }

    if !isDirectory.boolValue {
        return testsURL.lastPathComponent.hasSuffix("Tests.swift") ? [testsURL] : []
    }

    guard let enumerator = FileManager.default.enumerator(atPath: testsURL.path) else {
        throw ValidationError("Tests directory cannot be read: \(testsURL.path)")
    }

    return enumerator.compactMap { element -> URL? in
        guard let relativePath = element as? String, relativePath.hasSuffix("Tests.swift") else {
            return nil
        }
        return testsURL.appendingPathComponent(relativePath)
    }.sorted { $0.path < $1.path }
}

private func extractAIItems(from lines: [String]) -> [String] {
    var items: [String] = []
    var inAI = false

    for line in lines {
        let trimmed = line.trimmingCharacters(in: .whitespaces)
        if line.contains("// AI:") {
            inAI = true
            continue
        }

        guard inAI else {
            continue
        }

        if trimmed.hasPrefix("//") {
            if let item = firstMatch(#"//\s*-\s*(.*)"#, in: line) {
                items.append(item.trimmingCharacters(in: .whitespaces))
            }
        } else if trimmed.isEmpty {
            continue
        } else {
            inAI = false
        }
    }

    return items
}

private func renderReport(
    templateURL: URL,
    recordingURL: URL,
    files: [ReportTestFile],
    outputURL: URL
) throws {
    let tests = files.flatMap(\.tests)
    let passed = tests.filter { $0.status.statusClass == "pass" }.count
    let skipped = tests.filter { $0.status.statusClass == "skipped" }.count
    let failed = tests.filter { $0.status.statusClass == "fail" }.count
    let unknown = tests.filter { $0.status.statusClass == "unknown" }.count
    let total = tests.count
    let commandCount = tests.map(\.commands.count).reduce(0, +)

    var template = try String(contentsOf: templateURL, encoding: .utf8)
    template = replacingFirstMatch(
        #"\n  <!--\n    Wendy E2E Report HTML Template[\s\S]*?\n  -->"#,
        in: template,
        with: ""
    )

    guard let start = template.range(of: "    <!-- Repeat this .card section once per test file."),
        let footerStart = template.range(
            of: "    <footer>",
            range: start.lowerBound..<template.endIndex
        )
    else {
        throw ValidationError("Report template does not contain expected card/footer markers.")
    }

    let testCards = renderCards(
        files: files,
        recordingURL: recordingURL,
        recordLinkPrefix: recordLinkPrefix(recordingURL: recordingURL, outputURL: outputURL)
    )

    template.replaceSubrange(
        start.lowerBound..<footerStart.lowerBound,
        with: testCards + "\n\n"
    )

    let replacements: [String: String] = [
        "{{REPORT_TITLE}}": "Wendy E2E Report",
        "{{REPORT_HEADING}}": "Wendy E2E Report",
        "{{REPORT_SUMMARY}}":
            "Generated from Swift E2E tests, Swift Testing results, and captured command recordings.",
        "{{RUN_ID}}": runID(recordingURL: recordingURL, outputURL: outputURL),
        "{{TESTS_PASSED_COUNT}}": String(passed),
        "{{TESTS_SKIPPED_COUNT}}": String(skipped),
        "{{TESTS_FAILED_COUNT}}": String(failed),
        "{{TESTS_UNKNOWN_COUNT}}": String(unknown),
        "{{COMMAND_RUN_COUNT}}": String(commandCount),
        "{{VISIBLE_TEST_COUNT}}": String(total),
        "{{TOTAL_TEST_COUNT}}": String(total),
        "{{RECORDING_DIRECTORY}}": recordingURL.path,
    ]
    let rawPlaceholders: Set<String> = [
        "{{REPORT_TITLE}}",
        "{{TESTS_PASSED_COUNT}}",
        "{{TESTS_SKIPPED_COUNT}}",
        "{{TESTS_FAILED_COUNT}}",
        "{{TESTS_UNKNOWN_COUNT}}",
        "{{COMMAND_RUN_COUNT}}",
        "{{VISIBLE_TEST_COUNT}}",
        "{{TOTAL_TEST_COUNT}}",
    ]

    for (placeholder, value) in replacements {
        template = template.replacingOccurrences(
            of: placeholder,
            with: rawPlaceholders.contains(placeholder) ? value : escapeHTML(value)
        )
    }

    if let leftover = firstMatch(#"\{\{[A-Z0-9_]+\}\}"#, in: template) {
        throw ValidationError("Unreplaced report template placeholder: \(leftover)")
    }

    try FileManager.default.createDirectory(
        at: outputURL.deletingLastPathComponent(),
        withIntermediateDirectories: true
    )
    try template.write(to: outputURL, atomically: true, encoding: .utf8)

    print(outputURL.path)
    print(
        "tests=\(total) passed=\(passed) skipped=\(skipped) failed=\(failed) unknown=\(unknown) commands=\(commandCount)"
    )
}

private func runID(recordingURL: URL, outputURL: URL) -> String {
    let candidates = [
        outputURL.deletingLastPathComponent(),
        recordingURL.deletingLastPathComponent(),
        recordingURL,
    ]

    for candidate in candidates {
        let name = candidate.lastPathComponent
        if name.hasPrefix("e2e-run.") {
            return String(name.dropFirst("e2e-run.".count))
        }
        if name.hasPrefix("e2e-report.") {
            return String(name.dropFirst("e2e-report.".count))
        }
        if name.hasPrefix("e2e-recording.") {
            return String(name.dropFirst("e2e-recording.".count))
        }
    }

    return outputURL.deletingLastPathComponent().lastPathComponent
}

private func recordLinkPrefix(recordingURL: URL, outputURL: URL) -> String {
    let outputDirectoryPath = outputURL.deletingLastPathComponent().standardizedFileURL.path
    let recordingPath = recordingURL.standardizedFileURL.path
    let prefix =
        outputDirectoryPath.hasSuffix("/") ? outputDirectoryPath : outputDirectoryPath + "/"

    guard recordingPath.hasPrefix(prefix) else {
        return ""
    }

    let relativePath = String(recordingPath.dropFirst(prefix.count))
    return relativePath.isEmpty ? "" : relativePath + "/"
}

private func renderCards(
    files: [ReportTestFile],
    recordingURL: URL,
    recordLinkPrefix: String
) -> String {
    var cards: [String] = []

    for file in files {
        cards.append("<section class=\"card\" data-test-file-card>")
        cards.append(
            "<div class=\"card-title\"><h2>\(escapeHTML(displayName(file.url.lastPathComponent)))</h2></div>"
        )
        cards.append("<div class=\"suite-group\">")

        for test in file.tests {
            let statusClass = test.status.statusClass
            let statusText = test.status.statusText
            let hasAI = test.aiItems.isEmpty ? "false" : "true"
            let recordURL = recordingURL.appendingPathComponent(test.recordName)
            let shellName = test.recordName.replacing(/\.md$/, with: ".sh")
            let shellURL = recordingURL.appendingPathComponent(shellName)
            let recordLinks = [
                FileManager.default.fileExists(atPath: shellURL.path)
                    ? "<a class=\"report-button\" href=\"\(escapeHTML(recordLinkPrefix + shellName))\">Shell</a>"
                    : "",
                FileManager.default.fileExists(atPath: recordURL.path)
                    ? "<a class=\"report-button\" href=\"\(escapeHTML(recordLinkPrefix + test.recordName))\">Record</a>"
                    : "",
            ].joined()
            let pathText = "\(test.suite) › \(test.name)"

            cards.append(
                "<details class=\"test-details\" data-test-status=\"\(statusClass)\" data-has-ai=\"\(hasAI)\" data-has-ai-analysis=\"false\">"
            )
            cards.append(
                "<summary class=\"test-summary\">\(recordLinks)<span class=\"test-path\">\(escapeHTML(pathText))</span><span class=\"badge \(statusClass)\">\(statusText)</span></summary>"
            )

            var body: [String] = []
            if let detail = test.status.detail {
                body.append("<p class=\"skip-reason\">\(escapeHTML(detail))</p>")
            }
            body.append(renderAI(test))
            body.append(renderCommands(test.commands))
            cards.append(
                "<div class=\"test-body\">\(body.filter { !$0.isEmpty }.joined(separator: "\n"))</div>"
            )
            cards.append("</details>")
        }

        cards.append("</div></section>")
    }

    return cards.joined(separator: "\n")
}

private func renderAI(_ test: ReportTestCase) -> String {
    guard !test.aiItems.isEmpty else {
        return ""
    }

    let items = test.aiItems.map { item in
        "<li><span>\(escapeHTML(item))</span><span class=\"status pass\" aria-label=\"pass\"></span></li>"
    }.joined()

    return
        "<section class=\"ai-analysis\"><h4>AI analysis</h4><ul class=\"checks\">\(items)</ul></section>"
}

private func renderCommands(_ commands: [CommandRun]) -> String {
    guard !commands.isEmpty else {
        return ""
    }

    var chunks = ["<div class=\"commands\">"]
    for command in commands {
        chunks.append("<section class=\"command-run\">")
        chunks.append(
            "<div class=\"command-line\"><span class=\"command-prompt\">❯</span><span class=\"command-text\">\(escapeHTML(command.command))</span></div>"
        )

        var output: [String] = []
        for line in command.stdout.components(separatedBy: .newlines) where !line.isEmpty {
            output.append(
                "<div class=\"output-line stdout\"><span class=\"stream-marker\">!</span><span class=\"output-text\">\(escapeHTML(line))</span></div>"
            )
        }
        for line in command.stderr.components(separatedBy: .newlines) where !line.isEmpty {
            output.append(
                "<div class=\"output-line stderr\"><span class=\"stream-marker\">!</span><span class=\"output-text\">\(escapeHTML(line))</span></div>"
            )
        }
        if !output.isEmpty {
            chunks.append("<div class=\"command-output\">\(output.joined())</div>")
        }

        let metadata = [command.machine, command.status, command.duration]
            .filter { !$0.isEmpty }
            .joined(separator: " · ")
        chunks.append("<p class=\"command-run-meta\">\(escapeHTML(metadata))</p>")
        chunks.append("</section>")
    }
    chunks.append("</div>")
    return chunks.joined(separator: "\n")
}

private func recordFileStem(_ sourceURL: URL) -> String {
    var fileName = sourceURL.deletingPathExtension().lastPathComponent
    if fileName.hasSuffix("Tests") {
        fileName.removeLast("Tests".count)
    }
    return slug(fileName)
}

private func slug(_ value: String) -> String {
    var result = ""
    var needsSeparator = false
    var previousKind: SlugCharacterKind?
    let scalars = Array(value.unicodeScalars)

    for index in scalars.indices {
        let scalar = scalars[index]
        guard let kind = SlugCharacterKind(scalar) else {
            needsSeparator = !result.isEmpty
            previousKind = nil
            continue
        }

        let nextKind =
            scalars.index(after: index) < scalars.endIndex
            ? SlugCharacterKind(scalars[scalars.index(after: index)]) : nil
        if !result.isEmpty,
            needsSeparator
                || needsCamelCaseSeparator(
                    previousKind: previousKind,
                    currentKind: kind,
                    nextKind: nextKind
                )
        {
            result.append("-")
        }

        result.append(String(scalar).lowercased())
        needsSeparator = false
        previousKind = kind
    }

    return result.isEmpty ? "unknown" : result
}

private func needsCamelCaseSeparator(
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

private func displayName(_ fileName: String) -> String {
    let stem = URL(fileURLWithPath: fileName).deletingPathExtension().lastPathComponent
    let withoutTests = stem.hasSuffix("Tests") ? String(stem.dropLast("Tests".count)) : stem
    var result = ""
    var previous: Character?
    for character in withoutTests {
        if let previous, previous.isLowercase || previous.isNumber, character.isUppercase {
            result.append(" ")
        }
        result.append(character)
        previous = character
    }
    return result
}

private func firstMatch(_ pattern: String, in text: String, group: Int = 1) -> String? {
    guard let regex = try? NSRegularExpression(pattern: pattern) else {
        return nil
    }
    let range = NSRange(text.startIndex..<text.endIndex, in: text)
    guard let match = regex.firstMatch(in: text, range: range), match.numberOfRanges > group,
        let swiftRange = Range(match.range(at: group), in: text)
    else {
        return nil
    }
    return String(text[swiftRange])
}

private func replacingFirstMatch(
    _ pattern: String,
    in text: String,
    with replacement: String
) -> String {
    guard let regex = try? NSRegularExpression(pattern: pattern) else {
        return text
    }
    let range = NSRange(text.startIndex..<text.endIndex, in: text)
    guard let match = regex.firstMatch(in: text, range: range),
        let swiftRange = Range(match.range, in: text)
    else {
        return text
    }
    var text = text
    text.replaceSubrange(swiftRange, with: replacement)
    return text
}

private func escapeHTML(_ value: String) -> String {
    value
        .replacingOccurrences(of: "&", with: "&amp;")
        .replacingOccurrences(of: "<", with: "&lt;")
        .replacingOccurrences(of: ">", with: "&gt;")
        .replacingOccurrences(of: "\"", with: "&quot;")
}
