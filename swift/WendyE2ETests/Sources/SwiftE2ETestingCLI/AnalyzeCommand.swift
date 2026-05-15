import ArgumentParser
import Foundation

#if canImport(FoundationNetworking)
    import FoundationNetworking
#endif

#if canImport(FoundationXML)
    import FoundationXML
#endif

struct AnalyzeCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "analyze",
        abstract: "Analyze Swift E2E recordings with an AI provider."
    )

    @Option(name: .long, help: "Swift package directory.")
    var packageDir = "."

    @Option(name: .long, help: "Directory containing Swift E2E test sources.")
    var testsDir: String?

    @Option(name: .long, help: "E2E run directory. Reads tests/ and writes AI analysis files.")
    var runDir: String

    @Option(name: .long, help: "AI provider: auto, anthropic, claude, openai, or none.")
    var provider: AIProvider = .auto

    @Option(name: .long, help: "Model name. Defaults depend on provider.")
    var model: String?

    @Option(name: .long, help: "Maximum recording characters to include per test.")
    var maxRecordingCharacters = 60_000

    @Option(name: .long, help: "Maximum source characters to include per test.")
    var maxSourceCharacters = 20_000

    @Flag(name: .long, help: "Overwrite existing per-test ai-analysis.md files.")
    var overwrite = false

    mutating func run() async throws {
        let packageURL = URL(fileURLWithPath: packageDir)
        let testsURL = URL(
            fileURLWithPath: testsDir ?? defaultAnalyzeTestsDir(packageURL: packageURL).path
        )
        let runURL = URL(fileURLWithPath: runDir, isDirectory: true)
        let recordingURL = runURL.appendingPathComponent("tests", isDirectory: true)
        let outputDirectoryURL = runURL

        let records = try loadAnalyzeRecords(in: recordingURL)
        let testResults = try loadAnalyzeTestResults(
            in: recordingURL,
            outputDirectoryURL: outputDirectoryURL
        )
        let tests = try parseAnalyzeTests(in: testsURL, records: records, testResults: testResults)
        let reviewableTests = tests.filter { !$0.aiComments.isEmpty }

        let analyzer = try makeAnalyzer(provider: provider, model: model)
        if analyzer.isConfigured {
            print("==> Running Swift E2E AI analysis")
            print("    Provider: \(analyzer.providerName)")
            print("    Model:    \(analyzer.modelName)")
            print("    Tests:    \(reviewableTests.count)")
        } else {
            print("==> Swift E2E AI analysis skipped: no provider API key configured")
        }

        var results: [AnalyzeTestAIResult] = []
        for test in reviewableTests {
            guard let recordURL = test.recordURL else {
                results.append(.missingRecord(test: test))
                continue
            }
            let analysisURL = recordURL.deletingLastPathComponent()
                .appendingPathComponent("ai-analysis.md")
            if FileManager.default.fileExists(atPath: analysisURL.path), !overwrite {
                let existing = try String(contentsOf: analysisURL, encoding: .utf8)
                    .trimmingCharacters(in: .whitespacesAndNewlines)
                if !existing.isEmpty {
                    results.append(.existing(test: test, path: analysisURL))
                    continue
                }
            }

            guard analyzer.isConfigured else {
                results.append(.skipped(test: test))
                continue
            }

            let request = try AnalyzeAIRequest(
                test: test,
                source: clipped(test.sourceBody, limit: maxSourceCharacters),
                recording: clipped(
                    String(contentsOf: recordURL, encoding: .utf8),
                    limit: maxRecordingCharacters
                )
            )
            let markdown = try await analyzer.analyze(request: request)
            try markdown.write(to: analysisURL, atomically: true, encoding: .utf8)
            results.append(.written(test: test, path: analysisURL, markdown: markdown))
        }

        try writeAnalyzeSummary(
            runURL: runURL,
            analyzer: analyzer,
            tests: tests,
            reviewableTests: reviewableTests,
            results: results
        )
        updateAnalyzeReadmeBlock(runURL: runURL)

        let written = results.filter(\.isWritten).count
        let skipped = results.filter(\.isSkipped).count
        print(
            "==> Wrote Swift E2E AI analysis summary: \(runURL.appendingPathComponent("ai-analysis.md").path)"
        )
        print("    Per-test analyses written: \(written)")
        if skipped > 0 {
            print("    Per-test analyses skipped: \(skipped)")
        }
    }
}

enum AIProvider: String, ExpressibleByArgument {
    case auto
    case anthropic
    case openai
    case none

    init?(argument: String) {
        switch argument.lowercased() {
        case "claude":
            self = .anthropic
        default:
            self.init(rawValue: argument.lowercased())
        }
    }
}

private struct AnalyzeTestCase {
    var fileName: String
    var suite: String
    var name: String
    var funcLine: Int
    var nextLine: Int
    var sourceBody: String
    var aiComments: [String]
    var status: AnalyzeTestStatus
    var recordName: String
    var recordURL: URL?
}

private enum AnalyzeTestStatus {
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

private struct AnalyzeCommandRun {
    var recordURL: URL
    var sourceFile: String
    var sourceLine: Int
}

private struct AnalyzeResultKey: Hashable {
    var suite: String
    var name: String
}

private struct AnalyzeAIRequest {
    var test: AnalyzeTestCase
    var source: String
    var recording: String

    var prompt: String {
        var lines: [String] = []
        lines.append("You are analyzing a WendyAgent Swift end-to-end test recording.")
        lines.append("Pay special attention to every // AI: comment in the test source.")
        lines.append(
            "Treat // AI: comments as prompts, notes, or instructions; they are not necessarily checklist items."
        )
        lines.append(
            "Use the full test source, captured recording, and failure text as context when needed."
        )
        lines.append("Return concise Markdown only using this shape:")
        lines.append("# AI Analysis")
        lines.append("")
        lines.append("Status: pass|concern|fail")
        lines.append("Source: `File.swift:line`")
        lines.append("Record: `recording.md`")
        lines.append("")
        lines.append("## AI comments")
        lines.append("- pass|concern|fail: address each // AI: instruction or note")
        lines.append("  Evidence: brief evidence")
        lines.append("")
        lines.append("## Failure investigation")
        lines.append("Only include if the test failed.")
        lines.append("")
        lines.append("## Notes")
        lines.append("Optional. Keep it brief. Quote only relevant lines.")
        lines.append("")
        lines.append("Test: \(test.suite) › \(test.name)")
        lines.append("Source: \(test.fileName):\(test.funcLine)")
        lines.append("Status: \(test.status.statusText)")
        if let detail = test.status.detail, !detail.isEmpty {
            lines.append("Failure detail:\n\(detail)")
        }
        lines.append("// AI comments:")
        lines.append(formattedAIComments)
        lines.append("")
        lines.append("Swift source excerpt:")
        lines.append("```swift")
        lines.append(source)
        lines.append("```")
        lines.append("")
        lines.append("Recording:")
        lines.append("```markdown")
        lines.append(recording)
        lines.append("```")
        return lines.joined(separator: "\n")
    }

    private var formattedAIComments: String {
        guard !test.aiComments.isEmpty else {
            return "<none>"
        }
        return test.aiComments.joined(separator: "\n\n")
    }
}

private enum AnalyzeTestAIResult {
    case written(test: AnalyzeTestCase, path: URL, markdown: String)
    case existing(test: AnalyzeTestCase, path: URL)
    case skipped(test: AnalyzeTestCase)
    case missingRecord(test: AnalyzeTestCase)

    var test: AnalyzeTestCase {
        switch self {
        case .written(let test, _, _), .existing(let test, _), .skipped(let test),
            .missingRecord(let test):
            test
        }
    }

    var isWritten: Bool {
        if case .written = self { return true }
        return false
    }

    var isSkipped: Bool {
        if case .skipped = self { return true }
        return false
    }
}

private protocol E2EAIAnalyzer {
    var isConfigured: Bool { get }
    var providerName: String { get }
    var modelName: String { get }

    func analyze(request: AnalyzeAIRequest) async throws -> String
}

private struct UnconfiguredAnalyzer: E2EAIAnalyzer {
    var isConfigured: Bool { false }
    var providerName: String { "none" }
    var modelName: String { "none" }

    func analyze(request _: AnalyzeAIRequest) async throws -> String {
        throw ValidationError("AI analyzer is not configured.")
    }
}

private struct AnthropicAnalyzer: E2EAIAnalyzer {
    var apiKey: String
    var modelName: String
    var isConfigured: Bool { !apiKey.isEmpty }
    var providerName: String { "anthropic" }

    func analyze(request: AnalyzeAIRequest) async throws -> String {
        let payload = AnthropicMessagesRequest(
            model: modelName,
            maxTokens: 2_000,
            messages: [
                .init(role: "user", content: request.prompt)
            ]
        )
        var urlRequest = URLRequest(url: URL(string: "https://api.anthropic.com/v1/messages")!)
        urlRequest.httpMethod = "POST"
        urlRequest.setValue("application/json", forHTTPHeaderField: "Content-Type")
        urlRequest.setValue(apiKey, forHTTPHeaderField: "x-api-key")
        urlRequest.setValue("2023-06-01", forHTTPHeaderField: "anthropic-version")
        urlRequest.httpBody = try JSONEncoder().encode(payload)

        let (data, response) = try await URLSession.shared.data(for: urlRequest)
        try validateHTTP(response: response, data: data, provider: providerName)
        let decoded = try JSONDecoder().decode(AnthropicMessagesResponse.self, from: data)
        let text = decoded.content.map(\.text).joined(separator: "\n").trimmingCharacters(
            in: .whitespacesAndNewlines
        )
        guard !text.isEmpty else {
            throw ValidationError("Anthropic returned an empty analysis.")
        }
        return text
    }
}

private struct OpenAIAnalyzer: E2EAIAnalyzer {
    var apiKey: String
    var modelName: String
    var isConfigured: Bool { !apiKey.isEmpty }
    var providerName: String { "openai" }

    func analyze(request: AnalyzeAIRequest) async throws -> String {
        let payload = OpenAIChatRequest(
            model: modelName,
            messages: [
                .init(
                    role: "system",
                    content: "You write concise Markdown analysis of E2E test recordings."
                ),
                .init(role: "user", content: request.prompt),
            ]
        )
        var urlRequest = URLRequest(url: URL(string: "https://api.openai.com/v1/chat/completions")!)
        urlRequest.httpMethod = "POST"
        urlRequest.setValue("application/json", forHTTPHeaderField: "Content-Type")
        urlRequest.setValue("Bearer \(apiKey)", forHTTPHeaderField: "Authorization")
        urlRequest.httpBody = try JSONEncoder().encode(payload)

        let (data, response) = try await URLSession.shared.data(for: urlRequest)
        try validateHTTP(response: response, data: data, provider: providerName)
        let decoded = try JSONDecoder().decode(OpenAIChatResponse.self, from: data)
        let text =
            decoded.choices.first?.message.content.trimmingCharacters(
                in: .whitespacesAndNewlines
            ) ?? ""
        guard !text.isEmpty else {
            throw ValidationError("OpenAI returned an empty analysis.")
        }
        return text
    }
}

private struct AnthropicMessagesRequest: Encodable {
    struct Message: Encodable {
        var role: String
        var content: String
    }

    var model: String
    var maxTokens: Int
    var messages: [Message]

    enum CodingKeys: String, CodingKey {
        case model
        case maxTokens = "max_tokens"
        case messages
    }
}

private struct AnthropicMessagesResponse: Decodable {
    struct Content: Decodable {
        var type: String
        var text: String
    }

    var content: [Content]
}

private struct OpenAIChatRequest: Encodable {
    struct Message: Encodable {
        var role: String
        var content: String
    }

    var model: String
    var messages: [Message]
}

private struct OpenAIChatResponse: Decodable {
    struct Choice: Decodable {
        struct Message: Decodable {
            var content: String
        }

        var message: Message
    }

    var choices: [Choice]
}

private func makeAnalyzer(provider: AIProvider, model: String?) throws -> any E2EAIAnalyzer {
    let environment = ProcessInfo.processInfo.environment
    let anthropicKey = environment["ANTHROPIC_API_KEY", default: ""]
    let openAIKey = environment["OPENAI_API_KEY", default: ""]

    switch provider {
    case .none:
        return UnconfiguredAnalyzer()
    case .anthropic:
        guard !anthropicKey.isEmpty else {
            throw ValidationError("ANTHROPIC_API_KEY is required for --provider anthropic.")
        }
        return AnthropicAnalyzer(
            apiKey: anthropicKey,
            modelName: model ?? environment["ANTHROPIC_MODEL", default: "claude-3-5-sonnet-latest"]
        )
    case .openai:
        guard !openAIKey.isEmpty else {
            throw ValidationError("OPENAI_API_KEY is required for --provider openai.")
        }
        return OpenAIAnalyzer(
            apiKey: openAIKey,
            modelName: model ?? environment["OPENAI_MODEL", default: "gpt-4o-mini"]
        )
    case .auto:
        if !anthropicKey.isEmpty {
            return AnthropicAnalyzer(
                apiKey: anthropicKey,
                modelName: model
                    ?? environment["ANTHROPIC_MODEL", default: "claude-3-5-sonnet-latest"]
            )
        }
        if !openAIKey.isEmpty {
            return OpenAIAnalyzer(
                apiKey: openAIKey,
                modelName: model ?? environment["OPENAI_MODEL", default: "gpt-4o-mini"]
            )
        }
        return UnconfiguredAnalyzer()
    }
}

private func validateHTTP(response: URLResponse, data: Data, provider: String) throws {
    guard let httpResponse = response as? HTTPURLResponse else {
        throw ValidationError("\(provider) returned a non-HTTP response.")
    }
    guard 200..<300 ~= httpResponse.statusCode else {
        let body = String(data: data, encoding: .utf8) ?? "<non-UTF8 body>"
        throw ValidationError("\(provider) returned HTTP \(httpResponse.statusCode): \(body)")
    }
}

private func defaultAnalyzeTestsDir(packageURL: URL) -> URL {
    let e2eTestsURL = packageURL.appendingPathComponent("Tests/WendyE2ETests")
    if FileManager.default.fileExists(atPath: e2eTestsURL.path) {
        return e2eTestsURL
    }
    return packageURL.appendingPathComponent("Tests")
}

private func loadAnalyzeRecords(in recordingURL: URL) throws -> [String: URL] {
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
        records[recordURL.deletingLastPathComponent().lastPathComponent] = recordURL
    }
    return records
}

private func parseAnalyzeTests(
    in testsURL: URL,
    records: [String: URL],
    testResults: [AnalyzeResultKey: AnalyzeTestStatus]
) throws -> [AnalyzeTestCase] {
    let sourceURLs = try analyzeSwiftTestFiles(in: testsURL)
    var tests: [AnalyzeTestCase] = []

    for sourceURL in sourceURLs {
        let source = try String(contentsOf: sourceURL, encoding: .utf8)
        let lines = source.components(separatedBy: .newlines)
        var suite = sourceURL.deletingPathExtension().lastPathComponent
        var pendingTest: (line: Int, disabled: String?)?
        var discovered: [(name: String, funcLine: Int, disabled: String?)] = []

        for (offset, line) in lines.enumerated() {
            let lineNumber = offset + 1
            if let suiteName = analyzeFirstMatch(#"\bstruct\s+`([^`]+)`\s*\{"#, in: line)
                ?? analyzeFirstMatch(#"\bstruct\s+([A-Za-z_][A-Za-z0-9_]*)\s*\{"#, in: line)
            {
                suite = suiteName
            }
            if line.contains("@Test") {
                pendingTest = (
                    line: lineNumber,
                    disabled: analyzeFirstMatch(#"\.disabled\(\"([^\"]*)\"\)"#, in: line)
                )
            }
            if let functionName = analyzeFirstMatch(#"\bfunc\s+`([^`]+)`\s*\("#, in: line)
                ?? analyzeFirstMatch(#"\bfunc\s+([A-Za-z_][A-Za-z0-9_]*)\s*\("#, in: line),
                let test = pendingTest
            {
                discovered.append(
                    (name: functionName, funcLine: lineNumber, disabled: test.disabled)
                )
                pendingTest = nil
            }
        }

        for index in discovered.indices {
            let test = discovered[index]
            let nextLine =
                index + 1 < discovered.count ? discovered[index + 1].funcLine : lines.count + 1
            let bodyLines = Array(lines[(test.funcLine - 1)..<(nextLine - 1)])
            let aiComments = extractAnalyzeAIComments(from: bodyLines)
            let recordKey = "\(analyzeRecordFileStem(sourceURL)).\(analyzeSlug(test.name))"
            let key = AnalyzeResultKey(suite: suite, name: test.name)
            let status =
                test.disabled.map { AnalyzeTestStatus.skipped($0) }
                ?? testResults[key]
                ?? .unknown
            tests.append(
                AnalyzeTestCase(
                    fileName: sourceURL.lastPathComponent,
                    suite: suite,
                    name: test.name,
                    funcLine: test.funcLine,
                    nextLine: nextLine,
                    sourceBody: bodyLines.joined(separator: "\n"),
                    aiComments: aiComments,
                    status: status,
                    recordName: "\(recordKey)/recording.md",
                    recordURL: records[recordKey]
                )
            )
        }
    }

    return tests
}

private func analyzeSwiftTestFiles(in testsURL: URL) throws -> [URL] {
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

private func extractAnalyzeAIComments(from lines: [String]) -> [String] {
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

        guard inAI else {
            continue
        }

        if trimmed.hasPrefix("//") {
            currentBlock.append(stripAnalyzeCommentPrefix(from: trimmed))
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

private func stripAnalyzeCommentPrefix(from line: String) -> String {
    var value = line
    if value.hasPrefix("//") {
        value.removeFirst(2)
    }
    if value.hasPrefix(" ") {
        value.removeFirst()
    }
    return value
}

private func loadAnalyzeTestResults(
    in recordingURL: URL,
    outputDirectoryURL: URL
) throws -> [AnalyzeResultKey: AnalyzeTestStatus] {
    guard
        let resultURL = try analyzeTestResultsURL(
            in: [recordingURL, outputDirectoryURL, recordingURL.deletingLastPathComponent()]
        )
    else {
        return [:]
    }

    let data = try Data(contentsOf: resultURL)
    let parser = AnalyzeXUnitResultParser()
    let xmlParser = XMLParser(data: data)
    xmlParser.delegate = parser
    guard xmlParser.parse() else {
        throw ValidationError("Could not parse Swift Testing xUnit results: \(resultURL.path)")
    }
    return parser.results
}

private func analyzeTestResultsURL(in searchURLs: [URL]) throws -> URL? {
    var seen: Set<String> = []
    for searchURL in searchURLs {
        let path = searchURL.standardizedFileURL.path
        guard !seen.contains(path) else { continue }
        seen.insert(path)
        let defaultURL = searchURL.appendingPathComponent("test-results-swift-testing.xml")
        if FileManager.default.fileExists(atPath: defaultURL.path) {
            return defaultURL
        }
        guard FileManager.default.fileExists(atPath: searchURL.path) else { continue }
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

private final class AnalyzeXUnitResultParser: NSObject, XMLParserDelegate {
    var results: [AnalyzeResultKey: AnalyzeTestStatus] = [:]

    private var current: (key: AnalyzeResultKey, failure: String?, skipped: String?)?
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
                let key = analyzeTestResultKey(classname: classname, name: name)
            else {
                current = nil
                return
            }
            current = (key: key, failure: nil, skipped: nil)
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

private func analyzeTestResultKey(classname: String, name: String) -> AnalyzeResultKey? {
    let suite = analyzeNormalizedClassname(classname)
    let testName = analyzeNormalizedTestName(name)
    guard !suite.isEmpty, !testName.isEmpty else { return nil }
    return AnalyzeResultKey(suite: suite, name: testName)
}

private func analyzeNormalizedClassname(_ classname: String) -> String {
    if classname.last == "`", let start = classname.dropLast().lastIndex(of: "`") {
        let suiteStart = classname.index(after: start)
        return String(classname[suiteStart..<classname.index(before: classname.endIndex)])
    }
    return analyzeStripBackticks(String(classname.split(separator: ".").last ?? ""))
}

private func analyzeNormalizedTestName(_ name: String) -> String {
    var value = name
    if value.hasSuffix("()") {
        value.removeLast(2)
    }
    return analyzeStripBackticks(value)
}

private func analyzeStripBackticks(_ value: String) -> String {
    if value.first == "`", value.last == "`" {
        return String(value.dropFirst().dropLast())
    }
    return value
}

private func writeAnalyzeSummary(
    runURL: URL,
    analyzer: any E2EAIAnalyzer,
    tests: [AnalyzeTestCase],
    reviewableTests: [AnalyzeTestCase],
    results: [AnalyzeTestAIResult]
) throws {
    let markdownURL = runURL.appendingPathComponent("ai-analysis.md")
    let written = results.filter(\.isWritten).count
    let skipped = results.filter(\.isSkipped).count
    let missingRecords = results.filter { result in
        if case .missingRecord = result { return true }
        return false
    }.count
    let existing = results.filter { result in
        if case .existing = result { return true }
        return false
    }.count
    let aiTestCount = tests.filter { !$0.aiComments.isEmpty }.count
    let failedTestCount = tests.filter(\.status.isFailed).count

    var markdown: [String] = []
    markdown.append("# Swift E2E AI Analysis")
    markdown.append("")
    if analyzer.isConfigured {
        markdown.append("Status: complete")
    } else {
        markdown.append("Status: skipped")
        markdown.append("")
        markdown.append("AI analysis skipped: ANTHROPIC_API_KEY or OPENAI_API_KEY not configured.")
    }
    markdown.append("")
    markdown.append("- Provider: `\(analyzer.providerName)`")
    markdown.append("- Model: `\(analyzer.modelName)`")
    markdown.append("- Tests discovered: `\(tests.count)`")
    markdown.append("- Tests with `// AI:` comments: `\(aiTestCount)`")
    markdown.append("- Failed tests: `\(failedTestCount)`")
    markdown.append("- Tests selected for analysis: `\(reviewableTests.count)`")
    markdown.append("- Per-test analyses written: `\(written)`")
    markdown.append("- Existing analyses kept: `\(existing)`")
    markdown.append("- Missing recordings: `\(missingRecords)`")
    markdown.append("- Skipped: `\(skipped)`")
    markdown.append("")
    markdown.append("## Tests")
    markdown.append("")
    for result in results {
        markdown.append(
            "- `\(result.test.suite) › \(result.test.name)` — \(result.test.status.statusText)"
        )
    }
    try markdown.joined(separator: "\n").appending("\n").write(
        to: markdownURL,
        atomically: true,
        encoding: .utf8
    )

}

private func updateAnalyzeReadmeBlock(runURL: URL) {
    let readmeURL = runURL.appendingPathComponent("README.md")
    let start = "<!-- swift-e2e-analyze:start -->"
    let end = "<!-- swift-e2e-analyze:end -->"
    let original = (try? String(contentsOf: readmeURL, encoding: .utf8)) ?? ""
    let stripped = stripAnalyzeBlock(from: original, start: start, end: end).trimmingCharacters(
        in: .whitespacesAndNewlines
    )
    let block = """

        \(start)
        ## AI Analysis

        - Markdown: `\(runURL.appendingPathComponent("ai-analysis.md").path)`
        \(end)
        """
    let output =
        stripped.isEmpty
        ? block.trimmingCharacters(in: .whitespacesAndNewlines) : stripped + "\n" + block
    try? output.appending("\n").write(to: readmeURL, atomically: true, encoding: .utf8)
}

private func stripAnalyzeBlock(from text: String, start: String, end: String) -> String {
    guard let startRange = text.range(of: start), let endRange = text.range(of: end) else {
        return text
    }
    var output = text
    output.removeSubrange(startRange.lowerBound..<endRange.upperBound)
    return output
}

private func clipped(_ value: String, limit: Int) -> String {
    guard value.count > limit else {
        return value
    }
    let prefix = value.prefix(limit)
    return String(prefix) + "\n\n[... clipped to \(limit) characters ...]"
}

private func analyzeRecordFileStem(_ sourceURL: URL) -> String {
    var fileName = sourceURL.deletingPathExtension().lastPathComponent
    if fileName.hasSuffix("Tests") {
        fileName.removeLast("Tests".count)
    }
    return analyzeSlug(fileName)
}

private func analyzeSlug(_ value: String) -> String {
    var result = ""
    var needsSeparator = false
    var previousKind: AnalyzeSlugCharacterKind?
    let scalars = Array(value.unicodeScalars)

    for index in scalars.indices {
        let scalar = scalars[index]
        guard let kind = AnalyzeSlugCharacterKind(scalar) else {
            needsSeparator = !result.isEmpty
            previousKind = nil
            continue
        }
        let nextKind =
            scalars.index(after: index) < scalars.endIndex
            ? AnalyzeSlugCharacterKind(scalars[scalars.index(after: index)]) : nil
        if !result.isEmpty,
            needsSeparator
                || analyzeNeedsCamelCaseSeparator(
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

private func analyzeNeedsCamelCaseSeparator(
    previousKind: AnalyzeSlugCharacterKind?,
    currentKind: AnalyzeSlugCharacterKind,
    nextKind: AnalyzeSlugCharacterKind?
) -> Bool {
    switch (previousKind, currentKind, nextKind) {
    case (.lower?, .upper, _), (.digit?, .upper, _), (.upper?, .upper, .lower?):
        true
    default:
        false
    }
}

private enum AnalyzeSlugCharacterKind {
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

private func analyzeFirstMatch(_ pattern: String, in text: String, group: Int = 1) -> String? {
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
