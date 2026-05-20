import ArgumentParser
import Foundation

#if canImport(FoundationXML)
    import FoundationXML
#endif

struct ReviewCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "review",
        abstract: "Review Swift E2E recordings with an AI coding agent."
    )

    @Option(name: .long, help: "Swift package directory.")
    var packageDir = "."

    @Option(name: .long, help: "Directory containing Swift E2E test sources.")
    var testsDir: String?

    @Option(name: .long, help: "E2E run directory. Reads tests/ and writes AI review files.")
    var runDir: String

    @Option(name: .long, help: "AI agent: auto, claude, codex, or none.")
    var provider: AIProvider = .auto

    @Option(name: .long, help: "Model name. Passed through as provider-specific environment.")
    var model: String?

    @Option(name: .long, help: "Tests at or above this duration are reviewed as slow-ish.")
    var slowTestSeconds = 5.0

    @Flag(name: .long, help: "Overwrite existing per-test review.md files.")
    var overwrite = false

    mutating func run() async throws {
        let packageURL = URL(fileURLWithPath: packageDir).standardizedFileURL
        let testsURL = URL(
            fileURLWithPath: testsDir ?? defaultReviewTestsDir(packageURL: packageURL).path
        ).standardizedFileURL
        let runURL = URL(fileURLWithPath: runDir, isDirectory: true).standardizedFileURL
        let recordingURL = defaultReviewRecordingDirectory(runURL: runURL)
        let repoURL = packageURL.deletingLastPathComponent().deletingLastPathComponent()
            .standardizedFileURL

        let records = try loadReviewRecords(in: recordingURL)
        let testResults = try loadReviewTestResults(
            in: recordingURL,
            outputDirectoryURL: runURL
        )
        var tests = try parseReviewTests(in: testsURL, records: records, testResults: testResults)
        if FileManager.default.fileExists(
            atPath: recordingURL.appendingPathComponent("recording.md").path
        ) {
            tests = tests.filter { $0.recordURL != nil }
        }
        let reviewableTests = tests.filter {
            $0.requiresAgentReview(slowTestSeconds: slowTestSeconds)
        }

        if overwrite {
            try removeExistingPerTestReviews(in: recordingURL)
        }
        try? FileManager.default.removeItem(at: runURL.appendingPathComponent("review.md"))

        guard !reviewableTests.isEmpty else {
            try removeEmptyReviewSummary(runURL: runURL)
            print("==> Swift E2E agent review skipped: no failed, annotated, or slow-ish tests")
            print("    Tests discovered: \(tests.count)")
            print("    Slow threshold:   \(formatSeconds(slowTestSeconds))s")
            return
        }

        let agent = try makeAgent(provider: provider, model: model)
        guard agent.isConfigured else {
            try removeEmptyReviewSummary(runURL: runURL)
            print("==> Swift E2E agent review skipped: no agent API key configured")
            print("    Tests discovered: \(tests.count)")
            print("    Tests selected:   \(reviewableTests.count)")
            return
        }

        print("==> Running Swift E2E agent review")
        print("    Agent:    \(agent.providerName)")
        print("    Model:    \(agent.modelName)")
        print("    Repo:     \(repoURL.path)")
        print("    Run dir:  \(runURL.path)")
        print("    Tests:    \(reviewableTests.count)")

        let request = ReviewAgentRequest(
            repoURL: repoURL,
            packageURL: packageURL,
            testsURL: testsURL,
            runURL: runURL,
            recordingURL: recordingURL,
            tests: tests,
            reviewableTests: reviewableTests,
            slowTestSeconds: slowTestSeconds,
            overwrite: overwrite
        )
        try agent.review(request: request)

        let reviewFiles = try enforceConcernOnlyReviews(in: recordingURL)
        if reviewFiles.isEmpty {
            try removeEmptyReviewSummary(runURL: runURL)
            print("==> Swift E2E agent review found no concern-level issues")
            return
        }

        print("==> Swift E2E agent review wrote per-test concern reviews")
        print("    Per-test concern reviews: \(reviewFiles.count)")
    }
}

enum AIProvider: String, ExpressibleByArgument {
    case auto
    case claude
    case codex
    case none

    init?(argument: String) {
        switch argument.lowercased() {
        case "claude", "claude-code", "anthropic":
            self = .claude
        case "codex", "openai":
            self = .codex
        default:
            self.init(rawValue: argument.lowercased())
        }
    }
}

private struct ReviewTestCase {
    var sourcePath: String
    var fileName: String
    var suite: String
    var name: String
    var funcLine: Int
    var nextLine: Int
    var sourceBody: String
    var aiComments: [String]
    var status: ReviewTestStatus
    var durationSeconds: Double?
    var recordName: String
    var recordURL: URL?

    func requiresAgentReview(slowTestSeconds: Double) -> Bool {
        status.isFailed || !aiComments.isEmpty || (durationSeconds ?? 0) >= slowTestSeconds
    }
}

private enum ReviewTestStatus {
    case passed
    case failed(String?)
    case skipped(String?)
    case unknown

    var isFailed: Bool {
        if case .failed = self { return true }
        return false
    }

    var statusText: String {
        switch self {
        case .passed:
            "passed"
        case .failed:
            "failed"
        case .skipped:
            "skipped"
        case .unknown:
            "unknown"
        }
    }

    var detail: String? {
        switch self {
        case .failed(let detail), .skipped(let detail):
            detail
        case .passed, .unknown:
            nil
        }
    }
}

private struct ReviewTestObservation {
    var status: ReviewTestStatus
    var durationSeconds: Double?
}

private struct ReviewResultKey: Hashable {
    var suite: String
    var name: String
}

private struct ReviewAgentRequest {
    var repoURL: URL
    var packageURL: URL
    var testsURL: URL
    var runURL: URL
    var recordingURL: URL
    var tests: [ReviewTestCase]
    var reviewableTests: [ReviewTestCase]
    var slowTestSeconds: Double
    var overwrite: Bool

    var prompt: String {
        var lines: [String] = []
        lines.append(
            "You are reviewing WendyAgent Swift end-to-end test artifacts as a coding agent."
        )
        lines.append("")
        lines.append("You are running from the repository root and may inspect:")
        lines.append("- the full source tree")
        lines.append("- git history via git log / git blame / git diff")
        lines.append("- all E2E run artifacts under the run directory")
        lines.append("- recordings, xUnit XML, report metadata, and generated logs")
        lines.append("")
        lines.append("Repository root: `\(repoURL.path)`")
        lines.append("Swift package: `\(packageURL.path)`")
        lines.append("Swift E2E tests: `\(testsURL.path)`")
        lines.append("E2E run directory: `\(runURL.path)`")
        lines.append("Recorded tests directory: `\(recordingURL.path)`")
        lines.append("Slow-ish threshold: `\(formatSeconds(slowTestSeconds))s`")
        lines.append("")
        lines.append("## Progress output")
        lines.append("")
        lines.append(
            "Print brief progress updates to stdout while you work so CI shows a health signal. Mention what test, artifact, source file, or hypothesis you are currently inspecting. Keep updates short and useful, for example:"
        )
        lines.append(
            "- `Progress: inspecting wendy-device-info.prints-human-readable-device-information recording.md`"
        )
        lines.append("- `Progress: checking device info implementation for terminal probe output`")
        lines.append("- `Progress: writing concern for tests/<suite-key>/<test-key>/review.md`")
        lines.append(
            "Print a progress line at least before each selected test and before writing each review file. Do not wait until the final summary to report progress."
        )
        lines.append("")
        lines.append("## Task")
        lines.append("")
        lines.append(
            "Investigate the selected tests below. They were selected because they failed, contain `// AI:` review notes, or are slow-ish."
        )
        lines.append(
            "For each selected test, inspect its recording, source, relevant implementation code, nearby tests, and git history as needed."
        )
        lines.append(
            "Look for real issues that a human reviewer should discuss: regressions, product bugs, test bugs, flaky or infrastructure problems, suspicious slowness, misleading output, missing assertions, or unresolved `// AI:` concerns."
        )
        lines.append("")
        lines.append("## Output contract")
        lines.append("")
        lines.append(
            "Write Markdown review files only for tests with at least a concern-level issue."
        )
        lines.append(
            "Do not write any file for tests that are OK, expected, or purely informational."
        )
        lines.append(
            "Do not write `Status: pass` reviews. A missing `review.md` means no concern."
        )
        lines.append(
            "Do not edit product code, tests, or generated recordings. Only create, replace, or remove per-test `review.md` files under the E2E run directory."
        )
        if !overwrite {
            lines.append(
                "If a non-empty `review.md` already exists, leave it in place unless it is clearly stale or says the test passed."
            )
        }
        lines.append("")
        lines.append("Each concern file must use this shape:")
        lines.append("")
        lines.append("```markdown")
        lines.append("# AI Review")
        lines.append("")
        lines.append("Status: concern|fail")
        lines.append("Source: `path/to/TestFile.swift:line`")
        lines.append("Record: `tests/<suite-key>/<test-key>/recording.md`")
        lines.append("")
        lines.append("## Issue")
        lines.append("One concise paragraph naming the issue.")
        lines.append("")
        lines.append("## Evidence")
        lines.append("- Quote or cite exact recording/source/result evidence.")
        lines.append("")
        lines.append("## Likely cause")
        lines.append("Best current hypothesis. Say when uncertain.")
        lines.append("")
        lines.append("## Suggested actions")
        lines.append("- Concrete options for a human or follow-up agent.")
        lines.append("```")
        lines.append("")
        lines.append(
            "Prefer `Status: fail` when the evidence points to a real regression or broken required behavior. Use `Status: concern` for flakes, slowness, ambiguous behavior, missing evidence, or follow-up-worthy test quality issues."
        )
        lines.append("")
        lines.append("## Selected tests")
        lines.append("")
        for test in reviewableTests {
            lines.append("### \(test.suite) › \(test.name)")
            lines.append("- Status: `\(test.status.statusText)`")
            if let duration = test.durationSeconds {
                lines.append("- Duration: `\(formatSeconds(duration))s`")
            }
            if let detail = test.status.detail, !detail.isEmpty {
                lines.append("- Failure/skipped detail:")
                lines.append("  ```")
                lines.append(indent(detail, prefix: "  "))
                lines.append("  ```")
            }
            lines.append("- Source: `\(test.sourcePath):\(test.funcLine)`")
            lines.append("- Record: `\(test.recordName)`")
            if let recordURL = test.recordURL {
                lines.append("- Recording path: `\(recordURL.path)`")
                lines.append(
                    "- Review path: `\(recordURL.deletingLastPathComponent().appendingPathComponent("review.md").path)`"
                )
            } else {
                lines.append("- Recording path: `<missing>`")
            }
            if !test.aiComments.isEmpty {
                lines.append("- `// AI:` comments:")
                for comment in test.aiComments {
                    lines.append("  - \(comment.replacingOccurrences(of: "\n", with: "\n    "))")
                }
            }
            lines.append("")
        }
        lines.append("## All-run context")
        lines.append("")
        lines.append(
            "The full xUnit result file and all other test recordings are available in the run directory. You may inspect non-selected tests to compare behavior or identify shared causes, but only write per-test review files for concern-level findings."
        )
        return lines.joined(separator: "\n")
    }
}

private protocol E2EAgentReviewer {
    var isConfigured: Bool { get }
    var providerName: String { get }
    var modelName: String { get }

    func review(request: ReviewAgentRequest) throws
}

private struct UnconfiguredAgentReviewer: E2EAgentReviewer {
    var reason: String
    var isConfigured: Bool { false }
    var providerName: String { "none" }
    var modelName: String { "none" }

    func review(request _: ReviewAgentRequest) throws {
        throw ValidationError(reason)
    }
}

private struct ShellAgentReviewer: E2EAgentReviewer {
    var providerName: String
    var modelName: String
    var shellCommand: String
    var modelEnvironmentKey: String?

    var isConfigured: Bool { true }

    func review(request: ReviewAgentRequest) throws {
        let promptURL = request.runURL.appendingPathComponent(
            ".agent-review-prompt-\(UUID().uuidString).md"
        )
        try request.prompt.write(to: promptURL, atomically: true, encoding: .utf8)
        defer { try? FileManager.default.removeItem(at: promptURL) }

        var environment = ProcessInfo.processInfo.environment
        environment["WENDY_E2E_AGENT_PROMPT"] = promptURL.path
        environment["WENDY_E2E_REVIEW_RUN_DIR"] = request.runURL.path
        environment["WENDY_E2E_REVIEW_RECORDINGS_DIR"] = request.recordingURL.path
        environment["WENDY_E2E_REVIEW_REPO_DIR"] = request.repoURL.path
        if let modelEnvironmentKey {
            if usesAgentDefaultModel(modelName) {
                environment.removeValue(forKey: modelEnvironmentKey)
            } else {
                environment[modelEnvironmentKey] = modelName
            }
        }

        let process = Process()
        process.executableURL = URL(fileURLWithPath: "/bin/bash")
        process.arguments = ["-lc", shellCommand]
        process.currentDirectoryURL = request.repoURL
        process.environment = environment

        try process.run()
        process.waitUntilExit()

        guard process.terminationStatus == 0 else {
            throw ValidationError(
                "\(providerName) agent review failed with exit status \(process.terminationStatus)."
            )
        }
    }
}

private func makeAgent(provider: AIProvider, model: String?) throws -> any E2EAgentReviewer {
    let environment = ProcessInfo.processInfo.environment
    let anthropicKey = environment["ANTHROPIC_API_KEY", default: ""]
    let openAIKey = environment["OPENAI_API_KEY", default: ""]

    switch provider {
    case .none:
        return UnconfiguredAgentReviewer(reason: "AI review disabled with --provider none.")
    case .claude:
        guard !anthropicKey.isEmpty else {
            throw ValidationError("ANTHROPIC_API_KEY is required for --provider claude.")
        }
        if environment["WENDY_E2E_CLAUDE_COMMAND", default: ""].isEmpty {
            try requireExecutable("claude", provider: "claude")
        }
        return claudeAgent(model: model, environment: environment)
    case .codex:
        guard !openAIKey.isEmpty else {
            throw ValidationError("OPENAI_API_KEY is required for --provider codex.")
        }
        if environment["WENDY_E2E_CODEX_COMMAND", default: ""].isEmpty {
            try requireExecutable("codex", provider: "codex")
        }
        return codexAgent(model: model, environment: environment)
    case .auto:
        if !anthropicKey.isEmpty {
            if environment["WENDY_E2E_CLAUDE_COMMAND", default: ""].isEmpty {
                try requireExecutable("claude", provider: "claude")
            }
            return claudeAgent(model: model, environment: environment)
        }
        if !openAIKey.isEmpty {
            if environment["WENDY_E2E_CODEX_COMMAND", default: ""].isEmpty {
                try requireExecutable("codex", provider: "codex")
            }
            return codexAgent(model: model, environment: environment)
        }
        return UnconfiguredAgentReviewer(reason: "No agent API key configured.")
    }
}

private func claudeAgent(model: String?, environment: [String: String]) -> ShellAgentReviewer {
    ShellAgentReviewer(
        providerName: "claude",
        modelName: model ?? environment["ANTHROPIC_MODEL", default: "default"],
        shellCommand: environment[
            "WENDY_E2E_CLAUDE_COMMAND",
            default:
                #"prompt="Read and follow the E2E review instructions in $WENDY_E2E_AGENT_PROMPT."; if [[ -n "${ANTHROPIC_MODEL:-}" ]]; then claude --model "$ANTHROPIC_MODEL" -p "$prompt" --dangerously-skip-permissions; else claude -p "$prompt" --dangerously-skip-permissions; fi"#
        ],
        modelEnvironmentKey: "ANTHROPIC_MODEL"
    )
}

private func codexAgent(model: String?, environment: [String: String]) -> ShellAgentReviewer {
    ShellAgentReviewer(
        providerName: "codex",
        modelName: model ?? environment["OPENAI_MODEL", default: "default"],
        shellCommand: environment[
            "WENDY_E2E_CODEX_COMMAND",
            default:
                #"prompt="Read and follow the E2E review instructions in $WENDY_E2E_AGENT_PROMPT."; if [[ -n "${OPENAI_MODEL:-}" ]]; then codex exec --model "$OPENAI_MODEL" --sandbox workspace-write --ask-for-approval never "$prompt"; else codex exec --sandbox workspace-write --ask-for-approval never "$prompt"; fi"#
        ],
        modelEnvironmentKey: "OPENAI_MODEL"
    )
}

private func usesAgentDefaultModel(_ modelName: String) -> Bool {
    let normalized = modelName.trimmingCharacters(in: .whitespacesAndNewlines).lowercased()
    return normalized.isEmpty || normalized == "default" || normalized == "latest"
}

private func requireExecutable(_ name: String, provider: String) throws {
    let process = Process()
    process.executableURL = URL(fileURLWithPath: "/usr/bin/env")
    process.arguments = ["which", name]
    process.standardOutput = FileHandle.nullDevice
    process.standardError = FileHandle.nullDevice
    try process.run()
    process.waitUntilExit()
    guard process.terminationStatus == 0 else {
        throw ValidationError("\(provider) requires `\(name)` to be installed on PATH.")
    }
}

private func defaultReviewTestsDir(packageURL: URL) -> URL {
    let e2eTestsURL = packageURL.appendingPathComponent("Tests/WendyE2ETests")
    if FileManager.default.fileExists(atPath: e2eTestsURL.path) {
        return e2eTestsURL
    }
    return packageURL.appendingPathComponent("Tests")
}

private func defaultReviewRecordingDirectory(runURL: URL) -> URL {
    let nestedTestsURL = runURL.appendingPathComponent("tests", isDirectory: true)
    if FileManager.default.fileExists(atPath: nestedTestsURL.path) {
        return nestedTestsURL
    }
    return runURL
}

private func loadReviewRecords(in recordingURL: URL) throws -> [String: URL] {
    guard FileManager.default.fileExists(atPath: recordingURL.path) else {
        return [:]
    }
    guard
        let enumerator = FileManager.default.enumerator(
            at: recordingURL,
            includingPropertiesForKeys: nil
        )
    else {
        throw ValidationError("Recording directory cannot be read: \(recordingURL.path)")
    }

    var records: [String: URL] = [:]
    for case let recordURL as URL in enumerator where recordURL.lastPathComponent == "recording.md"
    {
        if let key = reviewRecordKeyFromRecordingHeader(recordURL) {
            records[key] = recordURL
        }
        let relative = reviewRelativePath(recordURL, base: recordingURL)
        let components = relative.split(separator: "/").map(String.init)
        if components.count >= 3 {
            records["\(components[components.count - 3]).\(components[components.count - 2])"] =
                recordURL
        } else {
            let attemptDirectory = recordURL.deletingLastPathComponent()
            let targetDirectory = attemptDirectory.deletingLastPathComponent()
            let testDirectory = targetDirectory.deletingLastPathComponent()
            let suiteDirectory = testDirectory.deletingLastPathComponent()
            if !suiteDirectory.lastPathComponent.isEmpty,
                !testDirectory.lastPathComponent.isEmpty
            {
                records["\(suiteDirectory.lastPathComponent).\(testDirectory.lastPathComponent)"] =
                    recordURL
            } else {
                records[recordURL.deletingLastPathComponent().lastPathComponent] = recordURL
            }
        }
    }
    return records
}

private func reviewRecordKeyFromRecordingHeader(_ recordURL: URL) -> String? {
    guard let text = try? String(contentsOf: recordURL, encoding: .utf8) else {
        return nil
    }
    guard let sourcePath = reviewFirstMatch(#"(?m)^- Source: `([^`]+)`"#, in: text),
        let testName = reviewFirstMatch(#"(?m)^- Test: `([^`]+)`"#, in: text)
    else {
        return nil
    }
    return "\(reviewRecordFileStem(URL(fileURLWithPath: sourcePath))).\(reviewSlug(testName))"
}

private func parseReviewTests(
    in testsURL: URL,
    records: [String: URL],
    testResults: [ReviewResultKey: ReviewTestObservation]
) throws -> [ReviewTestCase] {
    let sourceURLs = try reviewSwiftTestFiles(in: testsURL)
    var tests: [ReviewTestCase] = []

    for sourceURL in sourceURLs {
        let source = try String(contentsOf: sourceURL, encoding: .utf8)
        let lines = source.components(separatedBy: .newlines)
        var suite = sourceURL.deletingPathExtension().lastPathComponent
        var pendingTest: (line: Int, disabled: String?)?
        var discovered: [(suite: String, name: String, funcLine: Int, disabled: String?)] = []

        for (offset, line) in lines.enumerated() {
            let lineNumber = offset + 1
            if let suiteName = reviewFirstMatch(#"\bstruct\s+`([^`]+)`\s*\{"#, in: line)
                ?? reviewFirstMatch(#"\bstruct\s+([A-Za-z_][A-Za-z0-9_]*)\s*\{"#, in: line)
            {
                suite = suiteName
            }
            if line.contains("@Test") {
                pendingTest = (
                    line: lineNumber,
                    disabled: reviewFirstMatch(#"\.disabled\("([^"]*)"\)"#, in: line)
                )
            }
            if let functionName = reviewFirstMatch(#"\bfunc\s+`([^`]+)`\s*\("#, in: line)
                ?? reviewFirstMatch(#"\bfunc\s+([A-Za-z_][A-Za-z0-9_]*)\s*\("#, in: line),
                let test = pendingTest
            {
                discovered.append(
                    (
                        suite: suite,
                        name: functionName,
                        funcLine: lineNumber,
                        disabled: test.disabled
                    )
                )
                pendingTest = nil
            }
        }

        for index in discovered.indices {
            let test = discovered[index]
            let nextLine =
                index + 1 < discovered.count ? discovered[index + 1].funcLine : lines.count + 1
            let bodyLines = Array(lines[(test.funcLine - 1)..<(nextLine - 1)])
            let aiComments = extractReviewAIComments(from: bodyLines)
            let recordSuiteKey = reviewRecordFileStem(sourceURL)
            let recordTestKey = reviewSlug(test.name)
            let recordKey = "\(recordSuiteKey).\(recordTestKey)"
            let key = ReviewResultKey(suite: test.suite, name: test.name)
            let observation = testResults[key]
            let status =
                test.disabled.map { ReviewTestStatus.skipped($0) }
                ?? observation?.status
                ?? .unknown
            tests.append(
                ReviewTestCase(
                    sourcePath: sourceURL.path,
                    fileName: sourceURL.lastPathComponent,
                    suite: test.suite,
                    name: test.name,
                    funcLine: test.funcLine,
                    nextLine: nextLine,
                    sourceBody: bodyLines.joined(separator: "\n"),
                    aiComments: aiComments,
                    status: status,
                    durationSeconds: observation?.durationSeconds,
                    recordName: "\(recordSuiteKey)/\(recordTestKey)/recording.md",
                    recordURL: records[recordKey]
                )
            )
        }
    }

    return tests
}

private func reviewSwiftTestFiles(in testsURL: URL) throws -> [URL] {
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

private func extractReviewAIComments(from lines: [String]) -> [String] {
    var blocks: [String] = []
    var currentBlock: [String] = []
    var inAI = false

    func finishBlock() {
        let block = currentBlock.joined(separator: "\n")
            .trimmingCharacters(in: .whitespacesAndNewlines)
        if !block.isEmpty {
            blocks.append(block)
        }
        currentBlock = []
    }

    for line in lines {
        let trimmed = line.trimmingCharacters(in: .whitespaces)
        if let range = trimmed.range(of: "// AI:") {
            if inAI {
                finishBlock()
            }
            inAI = true
            let note = trimmed[range.upperBound...]
                .trimmingCharacters(in: .whitespaces)
            if !note.isEmpty {
                currentBlock.append(note)
            }
            continue
        }

        guard inAI else { continue }

        if trimmed.hasPrefix("//") {
            currentBlock.append(stripReviewCommentPrefix(from: trimmed))
        } else {
            finishBlock()
            inAI = false
        }
    }

    if inAI {
        finishBlock()
    }

    return blocks
}

private func stripReviewCommentPrefix(from line: String) -> String {
    var value = line
    if value.hasPrefix("//") {
        value.removeFirst(2)
    }
    if value.hasPrefix(" ") {
        value.removeFirst()
    }
    return value
}

private func loadReviewTestResults(
    in recordingURL: URL,
    outputDirectoryURL: URL
) throws -> [ReviewResultKey: ReviewTestObservation] {
    guard
        let resultURL = try reviewTestResultsURL(
            in: [recordingURL, outputDirectoryURL, recordingURL.deletingLastPathComponent()]
        )
    else {
        return [:]
    }

    let data = try Data(contentsOf: resultURL)
    let parser = ReviewXUnitResultParser()
    let xmlParser = XMLParser(data: data)
    xmlParser.delegate = parser
    guard xmlParser.parse() else {
        throw ValidationError("Could not parse Swift Testing xUnit results: \(resultURL.path)")
    }
    return parser.results
}

private func reviewTestResultsURL(in searchURLs: [URL]) throws -> URL? {
    var seen: Set<String> = []
    for searchURL in searchURLs {
        let path = searchURL.standardizedFileURL.path
        guard !seen.contains(path) else { continue }
        seen.insert(path)
        let defaultURL = searchURL.appendingPathComponent("test-results.xml")
        if FileManager.default.fileExists(atPath: defaultURL.path) {
            return defaultURL
        }
    }
    return nil
}

private final class ReviewXUnitResultParser: NSObject, XMLParserDelegate {
    var results: [ReviewResultKey: ReviewTestObservation] = [:]

    private var current: (key: ReviewResultKey, failure: String?, skipped: String?, time: Double?)?
    private var currentElement: String?
    private var currentText = ""

    func parser(
        _: XMLParser,
        didStartElement elementName: String,
        namespaceURI _: String?,
        qualifiedName _: String?,
        attributes attributeDict: [String: String]
    ) {
        switch elementName {
        case "testcase":
            guard let classname = attributeDict["classname"], let name = attributeDict["name"],
                let key = reviewTestResultKey(classname: classname, name: name)
            else {
                current = nil
                return
            }
            current = (
                key: key,
                failure: nil,
                skipped: nil,
                time: attributeDict["time"].flatMap(Double.init)
            )
        case "failure", "skipped":
            currentElement = elementName
            currentText = ""
            guard var current else { return }
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

    func parser(_: XMLParser, foundCharacters string: String) {
        if currentElement == "failure" || currentElement == "skipped" {
            currentText.append(string)
        }
    }

    func parser(
        _: XMLParser,
        didEndElement elementName: String,
        namespaceURI _: String?,
        qualifiedName _: String?
    ) {
        switch elementName {
        case "failure", "skipped":
            guard var current else { return }
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
            guard let current else { return }
            let status: ReviewTestStatus
            if let skipped = current.skipped {
                status = .skipped(skipped.isEmpty ? nil : skipped)
            } else if let failure = current.failure {
                status = .failed(failure.isEmpty ? nil : failure)
            } else {
                status = .passed
            }
            results[current.key] = ReviewTestObservation(
                status: status,
                durationSeconds: current.time
            )
            self.current = nil
        default:
            break
        }
    }
}

private func reviewTestResultKey(classname: String, name: String) -> ReviewResultKey? {
    let suite = reviewNormalizedClassname(classname)
    let testName = reviewNormalizedTestName(name)
    guard !suite.isEmpty, !testName.isEmpty else { return nil }
    return ReviewResultKey(suite: suite, name: testName)
}

private func reviewNormalizedClassname(_ classname: String) -> String {
    if classname.last == "`", let start = classname.dropLast().lastIndex(of: "`") {
        let suiteStart = classname.index(after: start)
        return String(classname[suiteStart..<classname.index(before: classname.endIndex)])
    }
    return reviewStripBackticks(String(classname.split(separator: ".").last ?? ""))
}

private func reviewNormalizedTestName(_ name: String) -> String {
    var value = name
    if value.hasSuffix("()") {
        value.removeLast(2)
    }
    return reviewStripBackticks(value)
}

private func reviewStripBackticks(_ value: String) -> String {
    if value.first == "`", value.last == "`" {
        return String(value.dropFirst().dropLast())
    }
    return value
}

private struct ReviewFile {
    var testKey: String
    var url: URL
    var status: String
}

private func removeExistingPerTestReviews(in recordingURL: URL) throws {
    guard FileManager.default.fileExists(atPath: recordingURL.path),
        let enumerator = FileManager.default.enumerator(
            at: recordingURL,
            includingPropertiesForKeys: nil
        )
    else { return }

    for case let reviewURL as URL in enumerator where reviewURL.lastPathComponent == "review.md"
    {
        let recordURL = reviewURL.deletingLastPathComponent().appendingPathComponent("recording.md")
        guard FileManager.default.fileExists(atPath: recordURL.path) else { continue }
        try FileManager.default.removeItem(at: reviewURL)
    }
}

private func enforceConcernOnlyReviews(in recordingURL: URL) throws -> [ReviewFile] {
    guard FileManager.default.fileExists(atPath: recordingURL.path),
        let enumerator = FileManager.default.enumerator(
            at: recordingURL,
            includingPropertiesForKeys: nil
        )
    else { return [] }

    var reviewFiles: [ReviewFile] = []
    for case let reviewURL as URL in enumerator where reviewURL.lastPathComponent == "review.md"
    {
        let recordURL = reviewURL.deletingLastPathComponent().appendingPathComponent("recording.md")
        guard FileManager.default.fileExists(atPath: recordURL.path) else { continue }
        let markdown = try String(contentsOf: reviewURL, encoding: .utf8)
            .trimmingCharacters(in: .whitespacesAndNewlines)
        guard !markdown.isEmpty else {
            try FileManager.default.removeItem(at: reviewURL)
            continue
        }
        guard let status = parseReviewStatus(from: markdown), status != "pass" else {
            try FileManager.default.removeItem(at: reviewURL)
            continue
        }
        guard status == "concern" || status == "fail" else {
            try FileManager.default.removeItem(at: reviewURL)
            continue
        }
        reviewFiles.append(
            ReviewFile(
                testKey: reviewURL.deletingLastPathComponent().deletingLastPathComponent()
                    .lastPathComponent
                    + "." + reviewURL.deletingLastPathComponent().lastPathComponent,
                url: reviewURL,
                status: status
            )
        )
    }
    return reviewFiles.sorted { $0.url.path < $1.url.path }
}

private func parseReviewStatus(from markdown: String) -> String? {
    for line in markdown.components(separatedBy: .newlines) {
        let trimmed = line.trimmingCharacters(in: .whitespacesAndNewlines).lowercased()
        guard trimmed.hasPrefix("status:") else { continue }
        let status = trimmed.dropFirst("status:".count)
            .trimmingCharacters(in: .whitespacesAndNewlines)
        return String(status.split(separator: " ").first ?? "")
    }
    return nil
}

private func reviewRelativePath(_ url: URL, base: URL) -> String {
    let path = url.path
    let basePath = base.path
    if path.hasPrefix(basePath + "/") {
        return String(path.dropFirst(basePath.count + 1))
    }
    if let range = path.range(of: "/tests/") {
        return "tests/" + path[range.upperBound...]
    }
    return path
}

private func removeEmptyReviewSummary(runURL: URL) throws {
    try? FileManager.default.removeItem(at: runURL.appendingPathComponent("review.md"))
}

private func reviewRecordFileStem(_ sourceURL: URL) -> String {
    var fileName = sourceURL.deletingPathExtension().lastPathComponent
    if fileName.hasSuffix("Tests") {
        fileName.removeLast("Tests".count)
    }
    return reviewSlug(fileName)
}

private func reviewSlug(_ value: String) -> String {
    var result = ""
    var needsSeparator = false
    var previousKind: ReviewSlugCharacterKind?
    let scalars = Array(value.unicodeScalars)

    for index in scalars.indices {
        let scalar = scalars[index]
        guard let kind = ReviewSlugCharacterKind(scalar) else {
            needsSeparator = !result.isEmpty
            previousKind = nil
            continue
        }
        let nextKind =
            scalars.index(after: index) < scalars.endIndex
            ? ReviewSlugCharacterKind(scalars[scalars.index(after: index)]) : nil
        if !result.isEmpty,
            needsSeparator
                || reviewNeedsCamelCaseSeparator(
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

private func reviewNeedsCamelCaseSeparator(
    previousKind: ReviewSlugCharacterKind?,
    currentKind: ReviewSlugCharacterKind,
    nextKind: ReviewSlugCharacterKind?
) -> Bool {
    switch (previousKind, currentKind, nextKind) {
    case (.lower?, .upper, _), (.digit?, .upper, _), (.upper?, .upper, .lower?):
        true
    default:
        false
    }
}

private enum ReviewSlugCharacterKind {
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

private func reviewFirstMatch(_ pattern: String, in text: String, group: Int = 1) -> String? {
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

private func formatSeconds(_ value: Double) -> String {
    if value.rounded() == value {
        return String(Int(value))
    }
    return String(format: "%.3f", value)
}

private func indent(_ value: String, prefix: String) -> String {
    value.components(separatedBy: .newlines).map { prefix + $0 }.joined(separator: "\n")
}
