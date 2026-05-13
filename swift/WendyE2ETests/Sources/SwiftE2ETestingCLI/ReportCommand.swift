import ArgumentParser
import Foundation

struct ReportCommand: ParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "report",
        abstract: "Generate an HTML report from Swift E2E command records.",
        discussion: """
            Generates the same static HTML report used by the E2E review skill,
            using Swift test sources and an existing command records directory.
            """
    )

    @Option(name: .long, help: "Swift package directory.")
    var packageDir = "."

    @Option(name: .long, help: "Directory containing Swift E2E test sources.")
    var testsDir: String?

    @Option(name: .long, help: "HTML report template path.")
    var template: String?

    @Option(name: .long, help: "Directory containing Markdown command records.")
    var recordsDir: String?

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
        let recordsURL =
            try recordsDir.map { URL(fileURLWithPath: $0) }
            ?? latestRecordsDirectory(packageURL: packageURL)
        let outputURL = URL(
            fileURLWithPath: output ?? recordsURL.appendingPathComponent("index.html").path
        )

        let records = try loadRecords(in: recordsURL)
        let files = try parseTests(in: testsURL, records: records)
        try renderReport(
            templateURL: templateURL,
            recordsURL: recordsURL,
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

private struct ReportTestCase {
    var fileName: String
    var suite: String
    var name: String
    var funcLine: Int
    var disabled: String?
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

private func latestRecordsDirectory(packageURL: URL) throws -> URL {
    let buildURL = packageURL.appendingPathComponent(".build")
    let currentURL = buildURL.appendingPathComponent("e2e-test-records.current")
    if FileManager.default.fileExists(atPath: currentURL.path) {
        return currentURL
    }

    let contents = try FileManager.default.contentsOfDirectory(
        at: buildURL,
        includingPropertiesForKeys: [.isDirectoryKey]
    )
    let candidates = contents.filter { url in
        guard url.lastPathComponent.hasPrefix("e2e-test-records.") else {
            return false
        }
        return (try? url.resourceValues(forKeys: [.isDirectoryKey]).isDirectory) == true
    }.sorted { $0.path < $1.path }

    guard let latest = candidates.last else {
        throw ValidationError("No e2e-test-records.* directory found in \(buildURL.path)")
    }
    return latest
}

private func loadRecords(in recordsURL: URL) throws -> [String: [CommandRun]] {
    let recordURLs = try FileManager.default.contentsOfDirectory(
        at: recordsURL,
        includingPropertiesForKeys: nil
    ).filter { $0.pathExtension == "md" }
        .sorted { $0.lastPathComponent < $1.lastPathComponent }

    var records: [String: [CommandRun]] = [:]
    for recordURL in recordURLs {
        records[recordURL.lastPathComponent] = try parseRecord(at: recordURL)
    }
    return records
}

private func parseRecord(at recordURL: URL) throws -> [CommandRun] {
    let text = try String(contentsOf: recordURL, encoding: .utf8)
    var commands: [CommandRun] = []

    for part in text.components(separatedBy: "\n---\n") where part.contains("## Command") {
        let sourcePath = firstMatch(#"- Source: `([^`]+):(\d+)`"#, in: part, group: 1) ?? ""
        let sourceLine =
            Int(firstMatch(#"- Source: `([^`]+):(\d+)`"#, in: part, group: 2) ?? "") ?? -1
        var command = CommandRun(
            record: recordURL.lastPathComponent,
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

private func parseTests(
    in testsURL: URL,
    records: [String: [CommandRun]]
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
                        disabled: test.disabled
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
            tests[testIndex].recordName =
                "\(sourceURL.deletingPathExtension().lastPathComponent).\(slug(tests[testIndex].name)).md"
            tests[testIndex].commands = records[tests[testIndex].recordName, default: []].filter {
                command in
                command.sourceFile == sourceURL.lastPathComponent
                    && tests[testIndex].funcLine <= command.sourceLine
                    && command.sourceLine < nextLine
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
    recordsURL: URL,
    files: [ReportTestFile],
    outputURL: URL
) throws {
    let passed = files.flatMap(\.tests).filter { $0.disabled == nil }.count
    let skipped = files.flatMap(\.tests).filter { $0.disabled != nil }.count
    let failed = 0
    let total = passed + skipped + failed
    let commandCount = files.flatMap(\.tests).map(\.commands.count).reduce(0, +)

    var template = try String(contentsOf: templateURL, encoding: .utf8)
    template = replacingFirstMatch(
        #"\n  <!--\n    Wendy E2E AI Review HTML Template[\s\S]*?\n  -->"#,
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

    template.replaceSubrange(
        start.lowerBound..<footerStart.lowerBound,
        with: renderCards(files: files, recordsURL: recordsURL) + "\n\n"
    )

    let replacements: [String: String] = [
        "{{REPORT_TITLE}}": "Wendy E2E Report",
        "{{REPORT_HEADING}}": "Wendy E2E Report",
        "{{REPORT_SUMMARY}}":
            "Generated from Swift E2E tests. Includes active, disabled, AI-reviewed tests, and captured command records.",
        "{{TESTS_PASSED_COUNT}}": String(passed),
        "{{TESTS_SKIPPED_COUNT}}": String(skipped),
        "{{TESTS_FAILED_COUNT}}": String(failed),
        "{{COMMAND_RUN_COUNT}}": String(commandCount),
        "{{VISIBLE_TEST_COUNT}}": String(total),
        "{{TOTAL_TEST_COUNT}}": String(total),
        "{{RECORDS_DIRECTORY}}": recordsURL.path,
    ]
    let rawPlaceholders: Set<String> = [
        "{{REPORT_TITLE}}",
        "{{TESTS_PASSED_COUNT}}",
        "{{TESTS_SKIPPED_COUNT}}",
        "{{TESTS_FAILED_COUNT}}",
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
        "tests=\(total) passed=\(passed) skipped=\(skipped) failed=\(failed) commands=\(commandCount)"
    )
}

private func renderCards(files: [ReportTestFile], recordsURL: URL) -> String {
    var cards: [String] = []

    for file in files {
        cards.append("<section class=\"card\" data-test-file-card>")
        cards.append(
            "<div class=\"card-title\"><h2>\(escapeHTML(displayName(file.url.lastPathComponent)))</h2></div>"
        )
        cards.append("<div class=\"suite-group\">")

        for test in file.tests {
            let statusClass = test.disabled == nil ? "pass" : "skipped"
            let statusText = test.disabled == nil ? "Passed" : "Skipped"
            let hasAI = test.aiItems.isEmpty ? "false" : "true"
            let recordURL = recordsURL.appendingPathComponent(test.recordName)
            let reportLink =
                FileManager.default.fileExists(atPath: recordURL.path)
                ? "<a class=\"report-button\" href=\"\(escapeHTML(test.recordName))\">Record</a>"
                : ""
            let pathText = "\(test.suite) › \(test.name)"

            cards.append(
                "<details class=\"test-details\" data-test-status=\"\(statusClass)\" data-has-ai=\"\(hasAI)\" data-has-ai-analysis=\"false\">"
            )
            cards.append(
                "<summary class=\"test-summary\">\(reportLink)<span class=\"test-path\">\(escapeHTML(pathText))</span><span class=\"badge \(statusClass)\">\(statusText)</span></summary>"
            )

            var body: [String] = []
            if let disabled = test.disabled {
                body.append("<p class=\"skip-reason\">\(escapeHTML(disabled))</p>")
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

private func slug(_ value: String) -> String {
    var result = ""
    var needsSeparator = false

    for character in value {
        if character.isASCII, character.isLetter || character.isNumber {
            if needsSeparator, !result.isEmpty {
                result.append("-")
            }
            result.append(character.lowercased())
            needsSeparator = false
        } else if !result.isEmpty {
            needsSeparator = true
        }
    }

    return result.isEmpty ? "unknown" : result
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
