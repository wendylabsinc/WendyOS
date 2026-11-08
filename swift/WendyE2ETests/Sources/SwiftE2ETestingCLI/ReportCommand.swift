import ArgumentParser
import Foundation

#if canImport(FoundationXML)
    import FoundationXML
#endif

struct ReportCommand: ParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "report",
        abstract: "Generate an HTML report from a Swift E2E run.",
        discussion: """
            Generates the static HTML index for a Swift E2E run. When
            WENDY_E2E_REVIEW_OUTCOME is set to anything but success, review.md says
            the AI review did not complete.
            """
    )

    @Option(name: .long, help: "Swift package directory.")
    var packageDir = "."

    @Option(name: .long, help: "Directory containing Swift E2E test sources.")
    var testsDir: String?

    @Option(name: .long, help: "HTML report template path.")
    var template: String?

    @Option(name: .long, help: "Swift E2E run directory. Writes index.html.")
    var runDir: String

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
        let runURL = URL(fileURLWithPath: runDir, isDirectory: true)
        guard try isRunDirectory(runURL) else {
            throw ValidationError("Report input must be a Swift E2E run directory: \(runURL.path)")
        }

        let outputURL = runURL.appendingPathComponent("index.html")
        let records = try loadRecords(in: runURL)
        let aiReviews = try loadRunAIReviews(in: runURL)
        let testResults = try loadRunTestResults(in: runURL)
        let files = try parseTests(
            in: testsURL,
            packageURL: packageURL,
            runURL: runURL,
            records: records,
            aiReviews: aiReviews,
            testResults: testResults
        )
        try renderReport(
            templateURL: templateURL,
            runURL: runURL,
            files: files,
            aiReviews: aiReviews,
            reviewOutcome: ProcessInfo.processInfo.environment["WENDY_E2E_REVIEW_OUTCOME"],
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

private struct RunPathKey: Hashable {
    var suiteKey: String
    var testKey: String
}

private struct ReportTestDuration {
    var seconds: Double
    var formatted: String
    var color: String
    var barWidth: String
}

private struct ReportTestDurationRange {
    var count: Int
    var minSeconds: Double
    var maxSeconds: Double

    var formatted: String {
        let min = formattedTestDuration(minSeconds)
        let max = formattedTestDuration(maxSeconds)
        if count == 1 || min == max {
            return min
        }
        return "\(min)–\(max)"
    }

    var barLeft: String {
        let minPercent = durationBarPercent(seconds: minSeconds)
        let maxPercent = durationBarPercent(seconds: maxSeconds)
        if minPercent == maxPercent {
            let markerWidth = 2.0
            return percentString(
                Swift.min(Swift.max(minPercent - (markerWidth / 2), 0), 100 - markerWidth)
            )
        }
        return percentString(minPercent)
    }

    var barWidth: String {
        let minPercent = durationBarPercent(seconds: minSeconds)
        let maxPercent = durationBarPercent(seconds: maxSeconds)
        if minPercent == maxPercent {
            return "2.0%"
        }
        return percentString(maxPercent - minPercent)
    }

    var barColor: String {
        let minColor = durationColor(seconds: minSeconds)
        let maxColor = durationColor(seconds: maxSeconds)
        if minSeconds == maxSeconds || minColor == maxColor {
            return minColor
        }
        return "linear-gradient(90deg, \(minColor), \(maxColor))"
    }

    init?(_ seconds: [Double]) {
        let validSeconds = seconds.filter { $0 >= 0 }.sorted()
        guard let min = validSeconds.first, let max = validSeconds.last else {
            return nil
        }
        self.count = validSeconds.count
        self.minSeconds = min
        self.maxSeconds = max
    }
}

private struct ReportRunTestResult {
    var targetOutcomes: ReportTargetOutcomeCounts
    var durationRange: ReportTestDurationRange?
    var observations: [ReportTestObservation]
}

private struct ReportTestObservation {
    var target: String
    var route: TargetRoute
    var attempt: String
    var status: ReportTestStatus
    var recordingPath: String?
    var shellPath: String?

    var duration: ReportTestDuration? {
        status.duration
    }
}

private enum ReportTestStatus {
    case passed(duration: ReportTestDuration?)
    case failed(String?, duration: ReportTestDuration?)
    case skipped(String?, duration: ReportTestDuration?)
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
        case .failed(let message, _):
            return message
        case .skipped(let reason, _):
            return reason
        case .unknown:
            return "No Swift Testing result was found for this test in the recording."
        case .passed:
            return nil
        }
    }

    var duration: ReportTestDuration? {
        switch self {
        case .passed(let duration):
            return duration
        case .failed(_, let duration):
            return duration
        case .skipped(_, let duration):
            return duration
        case .unknown:
            return nil
        }
    }
}

private struct ReportTargetOutcomeCounts {
    var passed = 0
    var flaked = 0
    var skipped = 0
    var failed = 0
    var unknown = 0

    var isEmpty: Bool {
        passed == 0 && flaked == 0 && skipped == 0 && failed == 0 && unknown == 0
    }

    var primaryStatusClass: String {
        if failed > 0 { return "fail" }
        if flaked > 0 { return "flaked" }
        if skipped > 0 { return "skipped" }
        if passed > 0 { return "pass" }
        return "unknown"
    }

    var filterStatusClasses: [String] {
        var classes: [String] = []
        if passed > 0 { classes.append("pass") }
        if flaked > 0 { classes.append("flaked") }
        if failed > 0 { classes.append("fail") }
        if skipped > 0 { classes.append("skipped") }
        if unknown > 0 { classes.append("unknown") }
        return classes.isEmpty ? ["unknown"] : classes
    }

    mutating func add(_ outcome: ReportTargetOutcome) {
        switch outcome {
        case .passed:
            passed += 1
        case .flaked:
            flaked += 1
        case .skipped:
            skipped += 1
        case .failed:
            failed += 1
        case .unknown:
            unknown += 1
        }
    }

    static func fallback(for status: ReportTestStatus) -> ReportTargetOutcomeCounts {
        var counts = ReportTargetOutcomeCounts()
        switch status {
        case .passed:
            counts.passed = 1
        case .failed:
            counts.failed = 1
        case .skipped:
            counts.skipped = 1
        case .unknown:
            counts.unknown = 1
        }
        return counts
    }
}

private enum ReportTargetOutcome {
    case passed
    case flaked
    case skipped
    case failed
    case unknown

    var statusClass: String {
        switch self {
        case .passed:
            return "pass"
        case .flaked:
            return "flaked"
        case .skipped:
            return "skipped"
        case .failed:
            return "fail"
        case .unknown:
            return "unknown"
        }
    }

    var statusText: String {
        switch self {
        case .passed:
            return "Passed"
        case .flaked:
            return "Flaked"
        case .skipped:
            return "Skipped"
        case .failed:
            return "Failed"
        case .unknown:
            return "Unknown"
        }
    }
}

private struct ReportTargetOverviewRow {
    var target: String
    var route: TargetRoute
    var attempts: Int
    var tests: Int
    var counts: ReportTargetOutcomeCounts

    var outcome: ReportTargetOutcome {
        targetOverviewOutcome(for: counts)
    }
}

private struct ReportTargetOverviewAccumulator {
    var route: TargetRoute?
    var attempts: Set<String> = []
    var tests = 0
    var counts = ReportTargetOutcomeCounts()
}

private struct ReportTestCase {
    var fileName: String
    var suite: String
    var name: String
    var funcLine: Int
    var disabled: String?
    var status: ReportTestStatus
    var targetOutcomes = ReportTargetOutcomeCounts()
    var durationRange: ReportTestDurationRange?
    var observations: [ReportTestObservation] = []
    var nextLine = 0
    var aiItems: [String] = []
    var recordName = ""
    var aiReviews: [E2EReview] = []
    var commands: [CommandRun] = []

    var aiReviewMarkdown: String {
        aiReviews.map { review in
            "# \(review.title)\n\n\(review.summaryMarkdown)"
        }.joined(separator: "\n\n")
    }
}

private struct RunAIReviews {
    var root: [E2EReview] = []
    var suites: [String: [E2EReview]] = [:]
    var tests: [RunPathKey: [E2EReview]] = [:]
}

private struct ReportTestFile {
    var url: URL
    var suiteKey: String
    var suiteReviews: [E2EReview]
    var tests: [ReportTestCase]
}

private func defaultTestsDir(packageURL: URL) -> URL {
    let e2eTestsURL = packageURL.appendingPathComponent("Tests/WendyE2ETests")
    if FileManager.default.fileExists(atPath: e2eTestsURL.path) {
        return e2eTestsURL
    }
    return packageURL.appendingPathComponent("Tests")
}

private func loadRecords(in runURL: URL) throws -> [E2ETestIdentityKey: [CommandRun]] {
    let recordURLs = try commandRecordURLs(in: runURL)

    var records: [E2ETestIdentityKey: [CommandRun]] = [:]
    for recordURL in recordURLs {
        let observationURL = recordURL.deletingLastPathComponent()
        let metadata = try loadE2ETestMetadata(in: observationURL)
        records[metadata.identityKey, default: []] +=
            try parseRecord(
                at: recordURL,
                relativeTo: runURL
            )
    }
    return records
}

private func loadRunAIReviews(in runURL: URL) throws -> RunAIReviews {
    guard FileManager.default.fileExists(atPath: runURL.path) else {
        return RunAIReviews()
    }

    var reviews = RunAIReviews(
        root: try loadE2EReviews(in: runURL, expectedScope: "report", relativeTo: runURL)
    )

    for suiteURL in try directoryChildren(of: e2eObservationsRootURL(in: runURL)) {
        let suiteKey = suiteURL.lastPathComponent
        guard !isE2EReviewDirectoryName(suiteKey) else { continue }
        let suiteReviews = try loadE2EReviews(
            in: suiteURL,
            expectedScope: "suite",
            relativeTo: runURL
        )
        if !suiteReviews.isEmpty {
            reviews.suites[suiteKey] = suiteReviews
        }

        for testURL in try directoryChildren(of: suiteURL) {
            let testKey = testURL.lastPathComponent
            let testReviews = try loadE2EReviews(
                in: testURL,
                expectedScope: "test",
                relativeTo: runURL
            )
            if !testReviews.isEmpty {
                reviews.tests[RunPathKey(suiteKey: suiteKey, testKey: testKey)] = testReviews
            }
        }
    }

    return reviews
}

private func commandRecordURLs(in runURL: URL) throws -> [URL] {
    guard FileManager.default.fileExists(atPath: runURL.path) else {
        return []
    }

    return try runObservationFileURLs(in: runURL, fileName: "recording.md")
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

private func parseRecord(at recordURL: URL, relativeTo runURL: URL) throws -> [CommandRun] {
    let text = try String(contentsOf: recordURL, encoding: .utf8)
    var commands: [CommandRun] = []

    for part in text.components(separatedBy: "\n---\n") where part.contains("## Command") {
        let sourcePath = firstMatch(#"- Source: `([^`]+):(\d+)`"#, in: part, group: 1) ?? ""
        let sourceLine =
            Int(firstMatch(#"- Source: `([^`]+):(\d+)`"#, in: part, group: 2) ?? "") ?? -1
        var command = CommandRun(
            record: relativePath(from: runURL, to: recordURL),
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

private func isRunDirectory(_ runURL: URL) throws -> Bool {
    var isDirectory: ObjCBool = false
    guard FileManager.default.fileExists(atPath: runURL.path, isDirectory: &isDirectory),
        isDirectory.boolValue
    else {
        return false
    }
    return !FileManager.default.fileExists(
        atPath: runURL.appendingPathComponent("attempt.json").path
    )
}

private func parseXUnitResults(at resultURL: URL) throws -> [TestResultKey: ReportTestStatus] {
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

private func loadRunTestResults(
    in runURL: URL
) throws -> [E2ETestIdentityKey: ReportRunTestResult] {
    var observed: [E2ETestIdentityKey: [String: [ReportTestStatus]]] = [:]
    var durations: [E2ETestIdentityKey: [Double]] = [:]
    var observations: [E2ETestIdentityKey: [ReportTestObservation]] = [:]

    for suiteURL in try directoryChildren(of: e2eObservationsRootURL(in: runURL)) {
        let suiteKey = suiteURL.lastPathComponent
        guard !isE2EReviewDirectoryName(suiteKey) else { continue }
        for testURL in try directoryChildren(of: suiteURL) {
            for targetURL in try directoryChildren(of: testURL) {
                let targetName = targetURL.lastPathComponent
                for attemptURL in try directoryChildren(of: targetURL) {
                    let attemptName = attemptURL.lastPathComponent
                    let attemptArtifactsURL = e2eAttemptArtifactsURL(
                        in: runURL,
                        targetName: targetName,
                        attempt: attemptName
                    )
                    if e2eAttemptInfrastructureFailureDetail(at: attemptArtifactsURL) != nil {
                        continue
                    }
                    let metadata = try loadE2ETestMetadata(in: attemptURL)
                    let identityKey = metadata.identityKey
                    let status = try runObservationStatus(
                        metadata: metadata,
                        attemptURL: attemptArtifactsURL
                    )
                    observed[identityKey, default: [:]][targetName, default: []].append(status)
                    observations[identityKey, default: []].append(
                        ReportTestObservation(
                            target: targetName,
                            route: try targetRoute(
                                for: targetName,
                                attemptURL: attemptArtifactsURL
                            ),
                            attempt: attemptName,
                            status: status,
                            recordingPath: observationFilePath(
                                fileName: "recording.md",
                                attemptURL: attemptURL,
                                runURL: runURL
                            ),
                            shellPath: observationFilePath(
                                fileName: "recording.sh.txt",
                                attemptURL: attemptURL,
                                runURL: runURL
                            )
                        )
                    )
                    if let duration = status.duration {
                        durations[identityKey, default: []].append(duration.seconds)
                    }
                }
            }
        }
    }

    let keys = Set(observed.keys).union(durations.keys).union(observations.keys)
    return Dictionary(
        uniqueKeysWithValues: keys.map { key in
            var counts = ReportTargetOutcomeCounts()
            for statuses in observed[key, default: [:]].values {
                counts.add(targetOutcome(for: statuses))
            }
            return (
                key,
                ReportRunTestResult(
                    targetOutcomes: counts,
                    durationRange: ReportTestDurationRange(durations[key, default: []]),
                    observations: observations[key, default: []].sorted(by: observationSort)
                )
            )
        }
    )
}

private func observationFilePath(fileName: String, attemptURL: URL, runURL: URL) -> String? {
    let fileURL = attemptURL.appendingPathComponent(fileName)
    guard FileManager.default.fileExists(atPath: fileURL.path) else {
        return nil
    }
    return relativePath(from: runURL, to: fileURL)
}

private func runObservationStatus(
    metadata: E2ETestMetadata,
    attemptURL: URL
) throws -> ReportTestStatus {
    guard let resultURL = e2eAttemptXUnitURL(at: attemptURL) else {
        return .unknown
    }

    let results = try parseXUnitResults(at: resultURL)
    return results[TestResultKey(suite: metadata.suiteName, name: metadata.testName)] ?? .unknown
}

private func observationSort(_ lhs: ReportTestObservation, _ rhs: ReportTestObservation) -> Bool {
    if lhs.target != rhs.target {
        return lhs.target < rhs.target
    }
    return lhs.attempt < rhs.attempt
}

private func runObservationFileURLs(in runURL: URL, fileName: String) throws -> [URL] {
    guard FileManager.default.fileExists(atPath: runURL.path) else {
        return []
    }

    var urls: [URL] = []
    for suiteURL in try directoryChildren(of: e2eObservationsRootURL(in: runURL)) {
        for testURL in try directoryChildren(of: suiteURL) {
            for targetURL in try directoryChildren(of: testURL) {
                for attemptURL in try directoryChildren(of: targetURL) {
                    let candidate = attemptURL.appendingPathComponent(fileName)
                    if FileManager.default.fileExists(atPath: candidate.path) {
                        urls.append(candidate)
                    }
                }
            }
        }
    }
    return urls.sorted { $0.path < $1.path }
}

private func directoryChildren(of url: URL) throws -> [URL] {
    guard FileManager.default.fileExists(atPath: url.path) else {
        return []
    }
    return try FileManager.default.contentsOfDirectory(
        at: url,
        includingPropertiesForKeys: [.isDirectoryKey],
        options: [.skipsHiddenFiles]
    )
    .filter { isDirectory($0) }
    .sorted { $0.path < $1.path }
}

private func isDirectory(_ url: URL) -> Bool {
    (try? url.resourceValues(forKeys: [.isDirectoryKey]).isDirectory) == true
}

private func targetOutcome(for statuses: [ReportTestStatus]) -> ReportTargetOutcome {
    guard !statuses.isEmpty else {
        return .unknown
    }

    let passed = statuses.contains { $0.statusClass == "pass" }
    let failed = statuses.contains { $0.statusClass == "fail" }
    let skipped = statuses.contains { $0.statusClass == "skipped" }
    let unknown = statuses.contains { $0.statusClass == "unknown" }

    if skipped && !passed && !failed && !unknown {
        return .skipped
    }
    if passed && failed {
        return .flaked
    }
    if passed && !failed && !skipped && !unknown {
        return .passed
    }
    if failed && !passed && !skipped && !unknown {
        return .failed
    }
    return .unknown
}

private func targetOverviewOutcome(for counts: ReportTargetOutcomeCounts) -> ReportTargetOutcome {
    if counts.failed > 0 { return .failed }
    if counts.unknown > 0 { return .unknown }
    if counts.flaked > 0 { return .flaked }
    if counts.passed > 0 { return .passed }
    if counts.skipped > 0 { return .skipped }
    return .unknown
}

private final class XUnitResultParser: NSObject, XMLParserDelegate {
    var results: [TestResultKey: ReportTestStatus] = [:]

    private var current:
        (key: TestResultKey, duration: ReportTestDuration?, failure: String?, skipped: String?)?
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
            current = (
                key: key,
                duration: parsedTestDuration(attributeDict["time"]),
                failure: nil,
                skipped: nil
            )
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
                results[current.key] = .skipped(
                    skipped.isEmpty ? nil : skipped,
                    duration: current.duration
                )
            } else if let failure = current.failure {
                results[current.key] = .failed(
                    failure.isEmpty ? nil : failure,
                    duration: current.duration
                )
            } else {
                results[current.key] = .passed(duration: current.duration)
            }
            self.current = nil
        default:
            break
        }
    }
}

private func parsedTestDuration(_ value: String?) -> ReportTestDuration? {
    guard let value, let seconds = Double(value), seconds >= 0 else {
        return nil
    }

    return ReportTestDuration(
        seconds: seconds,
        formatted: formattedTestDuration(seconds),
        color: durationColor(seconds: seconds),
        barWidth: durationBarWidth(seconds: seconds)
    )
}

private func formattedTestDuration(_ seconds: Double) -> String {
    if seconds < 0.01 {
        return "<0.01s"
    }
    if seconds < 10 {
        return String(format: "%.2fs", seconds)
    }
    if seconds < 60 {
        return String(format: "%.1fs", seconds)
    }

    let minutes = Int(seconds / 60)
    let remainingSeconds = Int(seconds.rounded()) % 60
    return "\(minutes)m \(remainingSeconds)s"
}

private func durationBarWidth(seconds: Double) -> String {
    percentString(durationBarPercent(seconds: seconds))
}

private func durationBarPercent(seconds: Double) -> Double {
    min(max(seconds / 30, 0), 1) * 100
}

private func percentString(_ percent: Double) -> String {
    String(format: "%.1f%%", locale: Locale(identifier: "en_US_POSIX"), percent)
}

private func durationColor(seconds: Double) -> String {
    let cream = RGB(red: 241, green: 238, blue: 231)
    let orange = RGB(red: 245, green: 158, blue: 11)
    let deepRed = RGB(red: 153, green: 27, blue: 27)
    let slate = RGB(red: 23, green: 28, blue: 35)

    let color: RGB
    if seconds <= 0 {
        color = cream
    } else if seconds <= 1 {
        color = interpolateRGB(from: cream, to: orange, t: seconds)
    } else if seconds <= 10 {
        color = interpolateRGB(from: orange, to: deepRed, t: (seconds - 1) / 9)
    } else if seconds < 30 {
        color = interpolateRGB(from: deepRed, to: slate, t: (seconds - 10) / 20)
    } else {
        color = slate
    }

    return "rgb(\(color.red), \(color.green), \(color.blue))"
}

private struct RGB {
    var red: Int
    var green: Int
    var blue: Int
}

private func interpolateRGB(from start: RGB, to end: RGB, t: Double) -> RGB {
    let amount = min(max(t, 0), 1)

    func component(_ start: Int, _ end: Int) -> Int {
        Int((Double(start) + (Double(end - start) * amount)).rounded())
    }

    return RGB(
        red: component(start.red, end.red),
        green: component(start.green, end.green),
        blue: component(start.blue, end.blue)
    )
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
    packageURL: URL,
    runURL: URL,
    records: [E2ETestIdentityKey: [CommandRun]],
    aiReviews: RunAIReviews,
    testResults: [E2ETestIdentityKey: ReportRunTestResult]
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
                        status: test.disabled.map { .skipped($0, duration: nil) } ?? .unknown
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
            let recordSuiteKey = recordFileStem(sourceURL)
            let recordTestKey = slug(tests[testIndex].name)
            let identityKey = E2ETestIdentityKey(
                sourceFilePath: e2ePackageRelativePath(from: packageURL, to: sourceURL),
                suiteName: tests[testIndex].suite,
                testName: tests[testIndex].name
            )
            let directRecordName = "recording.md"
            let nestedRecordName = [
                e2eObservationsDirectoryName,
                recordSuiteKey,
                recordTestKey,
                "recording.md",
            ].joined(separator: "/")
            if records[identityKey] != nil,
                FileManager.default.fileExists(
                    atPath: runURL.appendingPathComponent(directRecordName).path
                )
            {
                tests[testIndex].recordName = directRecordName
            } else {
                tests[testIndex].recordName = nestedRecordName
            }
            let key = RunPathKey(suiteKey: recordSuiteKey, testKey: recordTestKey)
            tests[testIndex].aiReviews = aiReviews.tests[key, default: []]
            tests[testIndex].commands = records[identityKey, default: []].filter { command in
                command.sourceFile == sourceURL.lastPathComponent
                    && tests[testIndex].funcLine <= command.sourceLine
                    && command.sourceLine < nextLine
            }
            if let result = testResults[identityKey] {
                tests[testIndex].targetOutcomes = result.targetOutcomes
                tests[testIndex].durationRange = result.durationRange
                tests[testIndex].observations = result.observations
            }
        }

        if !tests.isEmpty {
            let suiteKey = recordFileStem(sourceURL)
            files.append(
                ReportTestFile(
                    url: sourceURL,
                    suiteKey: suiteKey,
                    suiteReviews: aiReviews.suites[suiteKey, default: []],
                    tests: tests
                )
            )
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

private func buildTargetOverview(
    runURL: URL,
    files: [ReportTestFile]
) throws -> [ReportTargetOverviewRow] {
    var rowsByTarget: [String: ReportTargetOverviewAccumulator] = [:]
    var observedAttemptsByTarget: [String: Set<String>] = [:]
    let attemptURLsByTarget = try runAttemptArtifactURLsByTarget(in: runURL)

    for (target, attemptURLs) in attemptURLsByTarget {
        var row = rowsByTarget[target] ?? ReportTargetOverviewAccumulator()
        row.attempts.formUnion(attemptURLs.map(\.lastPathComponent))
        if row.route == nil, let attemptURL = attemptURLs.first {
            row.route = try? targetRoute(for: target, attemptURL: attemptURL)
        }
        rowsByTarget[target] = row
    }

    for test in files.flatMap(\.tests) {
        let observationsByTarget = Dictionary(grouping: test.observations, by: \.target)
        for (target, observations) in observationsByTarget {
            var row = rowsByTarget[target] ?? ReportTargetOverviewAccumulator()
            row.route = row.route ?? observations.first?.route
            row.attempts.formUnion(observations.map(\.attempt))
            observedAttemptsByTarget[target, default: []].formUnion(
                observations.map(\.attempt)
            )
            row.tests += 1
            row.counts.add(targetOutcome(for: observations.map(\.status)))
            rowsByTarget[target] = row
        }
    }

    for (target, attemptURLs) in attemptURLsByTarget {
        let observedAttempts = observedAttemptsByTarget[target, default: []]
        for attemptURL in attemptURLs
        where !observedAttempts.contains(attemptURL.lastPathComponent) {
            let infrastructureFailure = e2eAttemptInfrastructureFailureDetail(at: attemptURL)
            let exitStatus = e2eAttemptExitStatus(at: attemptURL)
            guard infrastructureFailure != nil || (exitStatus != nil && exitStatus != 0) else {
                continue
            }
            var row = rowsByTarget[target] ?? ReportTargetOverviewAccumulator()
            row.counts.failed += 1
            rowsByTarget[target] = row
        }
    }

    return rowsByTarget.map { target, row in
        ReportTargetOverviewRow(
            target: target,
            route: row.route ?? TargetRoute(cli: .unknown, agent: nil),
            attempts: row.attempts.count,
            tests: row.tests,
            counts: row.counts
        )
    }
    .sorted { $0.target < $1.target }
}

private func runAttemptArtifactURLsByTarget(in runURL: URL) throws -> [String: [URL]] {
    var urlsByTarget: [String: [URL]] = [:]
    for targetURL in try directoryChildren(of: e2eAttemptArtifactsRootURL(in: runURL)) {
        let target = targetURL.lastPathComponent
        urlsByTarget[target] = try directoryChildren(of: targetURL)
    }
    return urlsByTarget
}

private func renderReport(
    templateURL: URL,
    runURL: URL,
    files: [ReportTestFile],
    aiReviews: RunAIReviews,
    reviewOutcome: String?,
    outputURL: URL
) throws {
    let tests = files.flatMap(\.tests)
    let outcomeCounts = tests.map(displayOutcomeCounts(for:))
    let passed = outcomeCounts.filter { $0.passed > 0 }.count
    let flaked = outcomeCounts.filter { $0.flaked > 0 }.count
    let skipped = outcomeCounts.filter { $0.skipped > 0 }.count
    let failed = outcomeCounts.filter { $0.failed > 0 }.count
    let unknown = outcomeCounts.filter { $0.unknown > 0 }.count
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

    try writeE2EReviewAggregate(in: runURL, reviewOutcome: reviewOutcome)
    try renderReviewAggregateHTMLIfPresent(runURL: runURL)

    let reviewHTML = renderRunAIReview(aiReviews.root)
    let targetOverview = try renderTargetOverview(buildTargetOverview(runURL: runURL, files: files))
    let testCards = renderCards(files: files)

    template.replaceSubrange(
        start.lowerBound..<footerStart.lowerBound,
        with: reviewHTML + testCards + "\n\n"
    )

    let replacements: [String: String] = [
        "{{REPORT_TITLE}}": "Swift E2E run report",
        "{{REPORT_HEADING}}": "Swift E2E run report",
        "{{REPORT_SUMMARY}}":
            "Test outcomes, review findings, and captured diagnostics from this Swift E2E run.",
        "{{RUN_ID}}": runID(outputURL: outputURL),
        "{{REVIEW_AGGREGATE_LINK}}": renderReviewAggregateLink(runURL: runURL),
        "{{TARGET_OVERVIEW}}": targetOverview,
        "{{TESTS_PASSED_COUNT}}": String(passed),
        "{{TESTS_FLAKED_COUNT}}": String(flaked),
        "{{TESTS_SKIPPED_COUNT}}": String(skipped),
        "{{TESTS_FAILED_COUNT}}": String(failed),
        "{{TESTS_UNKNOWN_COUNT}}": String(unknown),
        "{{COMMAND_RUN_COUNT}}": String(commandCount),
        "{{VISIBLE_TEST_COUNT}}": String(total),
        "{{TOTAL_TEST_COUNT}}": String(total),
        "{{RUN_DIRECTORY}}": runURL.path,
    ]
    let rawPlaceholders: Set<String> = [
        "{{REPORT_TITLE}}",
        "{{TESTS_PASSED_COUNT}}",
        "{{TESTS_FLAKED_COUNT}}",
        "{{TESTS_SKIPPED_COUNT}}",
        "{{TESTS_FAILED_COUNT}}",
        "{{TESTS_UNKNOWN_COUNT}}",
        "{{COMMAND_RUN_COUNT}}",
        "{{VISIBLE_TEST_COUNT}}",
        "{{TOTAL_TEST_COUNT}}",
        "{{REVIEW_AGGREGATE_LINK}}",
        "{{TARGET_OVERVIEW}}",
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
        "tests=\(total) passed=\(passed) flaked=\(flaked) skipped=\(skipped) failed=\(failed) unknown=\(unknown) commands=\(commandCount)"
    )
}

private func runID(outputURL: URL) -> String {
    outputURL.deletingLastPathComponent().lastPathComponent
}

private func renderReviewAggregateLink(runURL: URL) -> String {
    let reviewURL = runURL.appendingPathComponent("review.md")
    guard FileManager.default.fileExists(atPath: reviewURL.path) else {
        return ""
    }
    return " · Review: <a href=\"review.html\">HTML</a>, <a href=\"review.md\">Markdown</a>"
}

private func renderReviewAggregateHTMLIfPresent(runURL: URL) throws {
    let markdownURL = runURL.appendingPathComponent("review.md")
    guard FileManager.default.fileExists(atPath: markdownURL.path) else {
        return
    }

    let markdown = try String(contentsOf: markdownURL, encoding: .utf8)
    let html = renderReviewAggregateHTML(
        markdown: markdown,
        title: "Swift E2E Review",
        runID: runURL.lastPathComponent
    )
    try html.write(
        to: runURL.appendingPathComponent("review.html"),
        atomically: true,
        encoding: .utf8
    )
}

private func renderReviewAggregateHTML(markdown: String, title: String, runID: String) -> String {
    """
    <!doctype html>
    <html lang="en">
    <head>
      <meta charset="utf-8">
      <meta name="viewport" content="width=device-width, initial-scale=1">
      <title>\(escapeHTML(title))</title>
      <style>
        :root {
          color-scheme: light;
          --background: #F1EEE7;
          --foreground: #171C23;
          --card: #F1EEE7;
          --secondary: #E6E2D8;
          --muted-foreground: #5B5A56;
          --border: #DEDEDE;
          --link: #2A7050;
        }
        @media (prefers-color-scheme: dark) {
          :root {
            color-scheme: dark;
            --background: #171C23;
            --foreground: #F1EEE7;
            --card: #1E242D;
            --secondary: #242B35;
            --muted-foreground: #C7C2B7;
            --border: rgba(241, 238, 231, .18);
            --link: #9FE2BF;
          }
        }
        body {
          margin: 0;
          padding: 32px;
          background: var(--background);
          color: var(--foreground);
          font: 15px/1.55 "Geist", ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
        }
        main {
          max-width: 920px;
          margin: 0 auto;
          padding: 28px;
          border: 1px solid var(--border);
          background: var(--card);
        }
        h1 { margin: 0 0 8px; font-size: 32px; font-weight: 500; }
        h2 { margin: 22px 0 10px; font-size: 18px; font-weight: 500; }
        p { margin: 8px 0; }
        a { color: var(--link); text-underline-offset: 2px; }
        :where(a, summary):focus-visible { outline: 3px solid var(--link); outline-offset: 3px; }
        code {
          padding: .12em .34em;
          border: 1px solid var(--border);
          border-radius: 5px;
          background: var(--secondary);
          font-family: "Geist Mono", ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
          font-size: .9em;
        }
        ul, ol { padding-left: 22px; }
        details {
          margin: 12px 0;
          padding: 12px 14px;
          border: 1px solid var(--border);
          background: var(--secondary);
        }
        summary {
          cursor: pointer;
          font-weight: 500;
        }
        .run-meta {
          margin: 0 0 22px;
          color: var(--muted-foreground);
          font-family: "Geist Mono", ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
          font-size: 12px;
        }
      </style>
    </head>
    <body>
      <main>
        <p class="run-meta">Run ID: <code>\(escapeHTML(runID))</code> · <a href="index.html">index.html</a> · <a href="review.md">review.md</a></p>
        \(renderMarkdown(
            markdown,
            headingBase: 1,
            allowDisclosureHTML: true
        ))
      </main>
    </body>
    </html>
    """
}

private func renderRunAIReview(_ reviews: [E2EReview]) -> String {
    renderScopedAIReviews(
        reviews,
        className: "ai-review-inline ai-review-report",
        heading: "AI report review"
    )
}

private func renderSuiteAIReview(_ reviews: [E2EReview]) -> String {
    renderScopedAIReviews(
        reviews,
        className: "ai-review-inline ai-review-suite",
        heading: "AI suite review"
    )
}

private func renderTestAIReview(_ reviews: [E2EReview]) -> String {
    renderScopedAIReviews(
        reviews,
        className: "ai-review-inline ai-review-test",
        heading: "AI test review"
    )
}

private func renderScopedAIReviews(
    _ reviews: [E2EReview],
    className: String,
    heading: String
) -> String {
    guard !reviews.isEmpty else {
        return ""
    }

    let renderedReviews = reviews.map { review in
        """
        <article class="ai-review">
        <h5><span class="ai-review-title">\(escapeHTML(review.title))</span>\(renderAIReviewerBadge(review.metadata.reviewer))\(renderAIReviewSeverityBadge(review.metadata.severity))\(renderAIReviewDetailsLink(review.path))</h5>
        <div class="ai-review-markdown">\(renderMarkdown(review.summaryMarkdown))</div>
        </article>
        """
    }.joined(separator: "\n")

    return """
        <section class="\(className)">
        <h4>\(escapeHTML(heading))</h4>
        \(renderedReviews)
        </section>
        """
}

private func renderAIReviewDetailsLink(_ detailsPath: String) -> String {
    "<a class=\"ai-review-details-link\" href=\"\(escapeHTML(detailsPath))\">Details</a>"
}

private func renderAIReviewSeverityBadge(_ severity: E2EReviewSeverity) -> String {
    "<span class=\"ai-review-severity-badge \(severity.rawValue)\">\(escapeHTML(severity.displayName))</span>"
}

private func renderAIReviewerBadge(_ reviewer: String) -> String {
    let identity = aiReviewerIdentity(reviewer: reviewer)
    let label = "\(identity.harnessLabel) \(identity.modelLabel)"
    return """
        <span class=\"ai-reviewer-badge \(identity.cssClass)\" title=\"\(escapeHTML(label))\"><span class=\"ai-reviewer-logo\" aria-hidden=\"true\">\(escapeHTML(identity.logo))</span><span class=\"ai-reviewer-model\">\(escapeHTML(identity.modelLabel))</span></span>
        """
}

private func aiReviewerIdentity(
    reviewer: String
) -> (cssClass: String, logo: String, harnessLabel: String, modelLabel: String) {
    return ("pi", "PI", "Pi", reviewer.isEmpty ? "default" : reviewer)
}

private func renderCards(files: [ReportTestFile]) -> String {
    var cards: [String] = []

    for file in files {
        let hasSuiteAIReview = file.suiteReviews.isEmpty ? "false" : "true"
        cards.append(
            "<section class=\"card\" data-test-file-card data-has-ai-review=\"\(hasSuiteAIReview)\">"
        )
        cards.append(
            "<div class=\"card-title\"><h2>\(escapeHTML(displayName(file.url.lastPathComponent)))</h2></div>"
        )
        cards.append(renderSuiteAIReview(file.suiteReviews))
        cards.append("<div class=\"suite-group\">")

        for test in file.tests {
            let targetOutcomes = displayOutcomeCounts(for: test)
            let statusClass = targetOutcomes.primaryStatusClass
            let statusClasses = targetOutcomes.filterStatusClasses.joined(separator: " ")
            let outcomeBadges = renderTargetOutcomeBadges(targetOutcomes)
            let hasAI = test.aiItems.isEmpty ? "false" : "true"
            let hasAIReview = test.aiReviewMarkdown.isEmpty ? "false" : "true"
            let aiBadge = hasAIReview == "true" ? renderAIReviewBadge() : ""
            let pathText = "\(test.suite) › \(test.name)"

            cards.append(
                "<details class=\"test-details\" data-test-status=\"\(statusClass)\" data-test-statuses=\"\(escapeHTML(statusClasses))\" data-has-ai=\"\(hasAI)\" data-has-ai-review=\"\(hasAIReview)\">"
            )
            cards.append(
                "<summary class=\"test-summary\"><span class=\"test-title\"><span class=\"test-path\">\(escapeHTML(pathText))</span>\(outcomeBadges)\(aiBadge)</span>\(runDurationBadge(test.durationRange))<span class=\"report-links\"></span></summary>"
            )
            cards.append(renderObservations(test.observations, aiReviews: test.aiReviews))
            cards.append("</details>")
        }

        cards.append("</div></section>")
    }

    return cards.joined(separator: "\n")
}

private func displayOutcomeCounts(for test: ReportTestCase) -> ReportTargetOutcomeCounts {
    test.targetOutcomes.isEmpty ? .fallback(for: test.status) : test.targetOutcomes
}

private func renderTargetOverview(_ rows: [ReportTargetOverviewRow]) -> String {
    guard !rows.isEmpty else {
        return ""
    }

    let tableRows = rows.map { row in
        let outcome = row.outcome
        let escapedTarget = escapeHTML(row.target)
        let route = renderTargetRoute(row.route, escapedTitle: escapedTarget)
        return """
            <tr>
              <td><span class="target-overview-target">\(route)<span>\(escapedTarget)</span></span></td>
              <td><span class="badge \(outcome.statusClass)">\(outcome.statusText)</span></td>
              <td class="numeric">\(row.attempts)</td>
              <td class="numeric">\(row.tests)</td>
              <td class="numeric">\(row.counts.passed)</td>
              <td class="numeric">\(row.counts.flaked)</td>
              <td class="numeric">\(row.counts.failed)</td>
              <td class="numeric">\(row.counts.skipped)</td>
              <td class="numeric">\(row.counts.unknown)</td>
            </tr>
            """
    }.joined(separator: "\n")

    return """
        <section class="target-overview" aria-label="CLI to Agent run overview">
          <div class="target-overview-table-wrapper">
            <table class="target-overview-table">
              <thead>
                <tr>
                  <th scope="col" aria-label="CLI to Agent"></th>
                  <th scope="col" aria-label="Outcome"></th>
                  <th class="numeric" scope="col">Attempts</th>
                  <th class="numeric" scope="col">Tests</th>
                  <th class="numeric" scope="col">Passed</th>
                  <th class="numeric" scope="col">Flaked</th>
                  <th class="numeric" scope="col">Failed</th>
                  <th class="numeric" scope="col">Skipped</th>
                  <th class="numeric" scope="col">Unknown</th>
                </tr>
              </thead>
              <tbody>
        \(tableRows)
              </tbody>
            </table>
          </div>
        </section>
        """
}

private func renderTargetOutcomeBadges(_ counts: ReportTargetOutcomeCounts) -> String {
    let buckets: [(className: String, label: String, count: Int)] = [
        ("pass", "Passed", counts.passed),
        ("flaked", "Flaked", counts.flaked),
        ("fail", "Failed", counts.failed),
        ("skipped", "Skipped", counts.skipped),
        ("unknown", "Unknown", counts.unknown),
    ].filter { $0.count > 0 }

    let includeCounts = buckets.count > 1 || (buckets.first?.count ?? 0) > 1
    return buckets.map { bucket in
        let text = includeCounts ? "\(bucket.label) (\(bucket.count))" : bucket.label
        return "<span class=\"badge \(bucket.className)\">\(text)</span>"
    }.joined()
}

private func renderObservations(
    _ observations: [ReportTestObservation],
    aiReviews: [E2EReview]
) -> String {
    guard !observations.isEmpty else {
        return
            "<div class=\"test-body\"><p class=\"note\">No attempt results were found for this test.</p>\(renderTestAIReview(aiReviews))</div>"
    }

    var chunks: [String] = [
        "<div class=\"test-body\">",
        renderTestAIReview(aiReviews),
        "<section class=\"observations\" aria-label=\"Attempt results\">",
    ]
    var previousTarget: String?
    for observation in observations.sorted(by: observationSort) {
        let isFirstTargetRow = observation.target != previousTarget
        previousTarget = observation.target
        let escapedTarget = isFirstTargetRow ? escapeHTML(observation.target) : ""
        let target = escapedTarget
        let route =
            isFirstTargetRow
            ? renderTargetRoute(observation.route, escapedTitle: escapedTarget) : ""
        let rowClass = isFirstTargetRow ? "observation-row" : "observation-row same-target"
        chunks.append(
            "<div class=\"\(rowClass)\"><span class=\"observation-route-cell\">\(route)</span><span class=\"observation-target\">\(target)</span><span class=\"observation-spacer\" aria-hidden=\"true\"></span>\(renderObservationLinks(observation))<span class=\"observation-attempt\">\(escapeHTML(observation.attempt))</span><span class=\"badge \(observation.status.statusClass)\">\(observation.status.statusText)</span>\(observationDurationBadge(observation.duration))</div>"
        )
    }
    chunks.append("</section>")
    chunks.append("</div>")
    return chunks.joined(separator: "\n")
}

private func renderObservationLinks(_ observation: ReportTestObservation) -> String {
    var links: [String] = []
    if let shellPath = observation.shellPath {
        links.append(
            "<a class=\"observation-button\" href=\"\(escapeHTML(shellPath))\">Shell</a>"
        )
    }
    if let recordingPath = observation.recordingPath {
        links.append(
            "<a class=\"observation-button\" href=\"\(escapeHTML(recordingPath))\">Record</a>"
        )
    }
    return "<span class=\"observation-actions\">\(links.joined())</span>"
}

private func renderTargetRoute(_ route: TargetRoute, escapedTitle: String) -> String {
    // Returns a trusted HTML fragment. Callers must pass an already-escaped title
    // so untrusted target names are encoded before entering this fragment.
    let cli = "<span class=\"target-route-cli\">\(renderTargetLogo(route.cli))</span>"
    let arrow = "<span class=\"target-route-arrow\" aria-hidden=\"true\">›</span>"
    let agent =
        "<span class=\"target-route-agent\">\(route.agent.map(renderTargetLogo) ?? "")</span>"
    return
        "<span class=\"target-route\" title=\"\(escapedTitle)\">\(cli)\(arrow)\(agent)</span>"
}

private func targetRoute(for target: String, attemptURL: URL) throws -> TargetRoute {
    let components = targetRouteComponents(for: target)
    let info = try targetInfo(at: attemptURL)
    let cliPlatform = info.cliOS.map(targetPlatformForOS) ?? targetPlatform(for: components.cli)
    let agentPlatform = targetPlatformForAgent(
        component: components.agent,
        info: info
    )
    return TargetRoute(cli: cliPlatform, agent: agentPlatform)
}

private func targetRouteComponents(for target: String) -> (cli: String, agent: String?) {
    let separator = "-to-"
    if let range = target.range(of: separator) {
        return (String(target[..<range.lowerBound]), String(target[range.upperBound...]))
    }

    for prefix in ["macos", "ubuntu", "windows", "linux"] {
        let routePrefix = "\(prefix)-"
        if target.hasPrefix(routePrefix) {
            return (prefix, String(target.dropFirst(routePrefix.count)))
        }
    }

    return (target, nil)
}

private func targetInfo(at attemptURL: URL) throws -> TargetInfo {
    let attemptInfoURL = attemptURL.appendingPathComponent("attempt.json")
    guard FileManager.default.fileExists(atPath: attemptInfoURL.path) else {
        return TargetInfo()
    }

    let data = try Data(contentsOf: attemptInfoURL)
    guard
        let object = try JSONSerialization.jsonObject(with: data) as? [String: Any],
        let target = object["target"] as? [String: Any]
    else {
        return TargetInfo()
    }

    let agentInfo = target["agentInfo"] as? [String: Any]
    return TargetInfo(
        cliOS: nonEmptyString(target["cliOS"]),
        agentOS: nonEmptyString(target["agentOS"]),
        agentDeviceType: nonEmptyString(target["agentDeviceType"])
            ?? nonEmptyString(agentInfo?["deviceType"]),
        agentHardwarePlatform: nonEmptyString(target["agentHardwarePlatform"])
            ?? nonEmptyString(agentInfo?["hardwarePlatform"]),
        agentGPUVendor: nonEmptyString(target["agentGPUVendor"])
            ?? nonEmptyString(agentInfo?["gpuVendor"]),
        agentJetPackVersion: nonEmptyString(target["agentJetPackVersion"])
            ?? nonEmptyString(agentInfo?["jetpackVersion"])
    )
}

private func nonEmptyString(_ value: Any?) -> String? {
    guard let string = value as? String, !string.isEmpty else {
        return nil
    }
    return string
}

private func renderTargetLogo(_ platform: TargetPlatform) -> String {
    "<span class=\"target-logo \(platform.cssClass)\" role=\"img\" aria-label=\"\(escapeHTML(platform.label))\">\(platform.symbol)</span>"
}

private func targetPlatformForAgent(component: String?, info: TargetInfo) -> TargetPlatform? {
    if let hardwarePlatform = targetHardwarePlatform(for: info), hardwarePlatform != .unknown {
        return hardwarePlatform
    }
    if let component {
        let hardwarePlatform = targetHardwarePlatform(for: component)
        if hardwarePlatform != .unknown {
            return hardwarePlatform
        }
    }
    if let os = info.agentOS {
        return targetPlatformForOS(os)
    }
    if let component {
        return targetPlatform(for: component)
    }
    return nil
}

private func targetPlatformForOS(_ value: String) -> TargetPlatform {
    let normalized = value.lowercased()
    if normalized.contains("macos") || normalized.contains("darwin") || normalized == "mac" {
        return .apple
    }
    if normalized.contains("windows") || normalized == "win" {
        return .windows
    }
    if normalized.contains("linux") || normalized.contains("ubuntu")
        || normalized.contains("debian")
        || normalized.contains("wendyos")
    {
        return .linux
    }
    return .unknown
}

private func targetHardwarePlatform(for info: TargetInfo) -> TargetPlatform? {
    if let agentHardwarePlatform = info.agentHardwarePlatform {
        let hardwarePlatform = targetHardwarePlatform(for: agentHardwarePlatform)
        if hardwarePlatform != .unknown {
            return hardwarePlatform
        }
    }
    if let agentDeviceType = info.agentDeviceType {
        let hardwarePlatform = targetHardwarePlatform(for: agentDeviceType)
        if hardwarePlatform != .unknown {
            return hardwarePlatform
        }
    }
    if info.agentJetPackVersion != nil {
        return .nvidia
    }
    return nil
}

private func targetHardwarePlatform(for value: String) -> TargetPlatform {
    let normalized = value.lowercased()
    if normalized.contains("jetson") || normalized.contains("nvidia")
        || normalized.contains("tegra")
    {
        return .nvidia
    }
    if normalized.contains("raspberry") || normalized.contains("raspi")
        || normalized.contains("rpi")
    {
        return .raspberryPi
    }
    return .unknown
}

private func targetPlatform(for value: String) -> TargetPlatform {
    let normalized = value.lowercased()
    let hardwarePlatform = targetHardwarePlatform(for: value)
    if hardwarePlatform != .unknown {
        return hardwarePlatform
    }
    if normalized.contains("macos") || normalized.contains("darwin") || normalized.contains("mac") {
        return .apple
    }
    if normalized.contains("windows") || normalized.contains("win") {
        return .windows
    }
    if normalized.contains("linux") || normalized.contains("ubuntu")
        || normalized.contains("debian")
    {
        return .linux
    }
    return .unknown
}

private struct TargetInfo {
    var cliOS: String? = nil
    var agentOS: String? = nil
    var agentDeviceType: String? = nil
    var agentHardwarePlatform: String? = nil
    var agentGPUVendor: String? = nil
    var agentJetPackVersion: String? = nil
}

private struct TargetRoute {
    var cli: TargetPlatform
    var agent: TargetPlatform?
}

private enum TargetPlatform {
    case apple
    case linux
    case nvidia
    case raspberryPi
    case windows
    case unknown

    var cssClass: String {
        switch self {
        case .apple:
            return "apple"
        case .linux:
            return "linux"
        case .nvidia:
            return "nvidia"
        case .raspberryPi:
            return "raspberry-pi"
        case .windows:
            return "windows"
        case .unknown:
            return "unknown"
        }
    }

    var label: String {
        switch self {
        case .apple:
            return "Apple"
        case .linux:
            return "Linux"
        case .nvidia:
            return "NVIDIA Jetson"
        case .raspberryPi:
            return "Raspberry Pi"
        case .windows:
            return "Windows"
        case .unknown:
            return "Unknown target"
        }
    }

    var symbol: String {
        switch self {
        case .apple:
            return ""
        case .linux:
            return "🐧"
        case .nvidia:
            return "NV"
        case .raspberryPi:
            return "RPi"
        case .windows:
            return "⊞"
        case .unknown:
            return "?"
        }
    }
}

private func observationDurationBadge(_ duration: ReportTestDuration?) -> String {
    guard let duration else {
        return
            "<span class=\"badge duration empty\" aria-hidden=\"true\"><span class=\"duration-bar\"><span class=\"duration-bar-fill\"></span></span><span class=\"duration-value\"></span></span>"
    }

    return
        "<span class=\"badge duration\" title=\"Test duration: \(escapeHTML(duration.formatted))\" style=\"--duration-bar-color: \(duration.color); --duration-bar-width: \(duration.barWidth)\"><span class=\"duration-bar\" aria-hidden=\"true\"><span class=\"duration-bar-fill\"></span></span><span class=\"duration-value\">\(escapeHTML(duration.formatted))</span></span>"
}

private func runDurationBadge(_ duration: ReportTestDurationRange?) -> String {
    guard let duration else {
        return
            "<span class=\"badge duration empty\" aria-hidden=\"true\"><span class=\"duration-bar\"><span class=\"duration-bar-fill\"></span></span><span class=\"duration-value\"></span></span>"
    }

    return
        "<span class=\"badge duration\" title=\"Test duration: \(escapeHTML(duration.formatted))\" style=\"--duration-bar-left: \(duration.barLeft); --duration-bar-width: \(duration.barWidth); --duration-bar-color: \(duration.barColor)\"><span class=\"duration-bar\" aria-hidden=\"true\"><span class=\"duration-bar-fill\"></span></span><span class=\"duration-value\">\(escapeHTML(duration.formatted))</span></span>"
}

private func renderAIReviewBadge() -> String {
    "<span class=\"badge ai\">AI</span>"
}

private func renderAIChecklist(_ test: ReportTestCase) -> String {
    guard !test.aiItems.isEmpty else {
        return ""
    }

    let items = test.aiItems.map { item in
        "<li><span>\(escapeHTML(item))</span><span class=\"status pass\" aria-label=\"pass\"></span></li>"
    }.joined()

    return
        "<section class=\"ai-review-checklist\"><h4>AI review checklist</h4><ul class=\"checks\">\(items)</ul></section>"
}

private func renderAIReview(_ markdown: String) -> String {
    guard !markdown.isEmpty else {
        return ""
    }

    return """
        <section class="ai-review-inline">
        <h4>AI review</h4>
        <div class="ai-review-markdown">\(renderMarkdown(markdown))</div>
        </section>
        """
}

private func renderMarkdown(
    _ markdown: String,
    headingBase: Int = 5,
    allowDisclosureHTML: Bool = false
) -> String {
    let lines = markdown.components(separatedBy: .newlines)
    var chunks: [String] = []
    var paragraph: [String] = []
    var unorderedItems: [String] = []
    var orderedItems: [String] = []
    var codeLines: [String] = []
    var inCodeFence = false

    func flushParagraph() {
        guard !paragraph.isEmpty else {
            return
        }
        chunks.append("<p>\(renderInlineMarkdown(paragraph.joined(separator: " ")))</p>")
        paragraph = []
    }

    func flushUnorderedList() {
        guard !unorderedItems.isEmpty else {
            return
        }
        chunks.append("<ul>\(unorderedItems.joined())</ul>")
        unorderedItems = []
    }

    func flushOrderedList() {
        guard !orderedItems.isEmpty else {
            return
        }
        chunks.append("<ol>\(orderedItems.joined())</ol>")
        orderedItems = []
    }

    func flushLists() {
        flushUnorderedList()
        flushOrderedList()
    }

    func flushCodeFence() {
        chunks.append("<pre><code>\(escapeHTML(codeLines.joined(separator: "\n")))</code></pre>")
        codeLines = []
    }

    for rawLine in lines {
        let trimmed = rawLine.trimmingCharacters(in: .whitespaces)

        if trimmed.hasPrefix("```") {
            if inCodeFence {
                flushCodeFence()
                inCodeFence = false
            } else {
                flushParagraph()
                flushLists()
                inCodeFence = true
                codeLines = []
            }
            continue
        }

        if inCodeFence {
            codeLines.append(rawLine)
            continue
        }

        guard !trimmed.isEmpty else {
            flushParagraph()
            flushLists()
            continue
        }

        if trimmed == "# AI Review" {
            continue
        }

        if allowDisclosureHTML, isDisclosureHTMLLine(trimmed) {
            flushParagraph()
            flushLists()
            chunks.append(trimmed)
            continue
        }

        if let heading = markdownHeading(from: trimmed, base: headingBase) {
            flushParagraph()
            flushLists()
            chunks.append("<\(heading.tag)>\(renderInlineMarkdown(heading.text))</\(heading.tag)>")
            continue
        }

        if let item = markdownUnorderedListItem(from: trimmed) {
            flushParagraph()
            flushOrderedList()
            unorderedItems.append("<li>\(renderInlineMarkdown(item))</li>")
            continue
        }

        if let item = markdownOrderedListItem(from: trimmed) {
            flushParagraph()
            flushUnorderedList()
            orderedItems.append("<li>\(renderInlineMarkdown(item))</li>")
            continue
        }

        flushLists()
        paragraph.append(trimmed)
    }

    if inCodeFence {
        flushCodeFence()
    }
    flushParagraph()
    flushLists()

    return chunks.joined(separator: "\n")
}

private func isDisclosureHTMLLine(_ line: String) -> Bool {
    line == "</details>"
        || line == "</summary>"
        || line.hasPrefix("<details")
        || line.hasPrefix("<summary>")
}

private func markdownHeading(from line: String, base: Int) -> (tag: String, text: String)? {
    let hashes = line.prefix { $0 == "#" }.count
    guard hashes > 0, hashes <= 6 else {
        return nil
    }
    let text = line.dropFirst(hashes).trimmingCharacters(in: .whitespaces)
    guard !text.isEmpty else {
        return nil
    }
    let level = min(6, max(1, base + hashes - 1))
    return ("h\(level)", text)
}

private func markdownUnorderedListItem(from line: String) -> String? {
    guard line.hasPrefix("- ") || line.hasPrefix("* ") else {
        return nil
    }
    return String(line.dropFirst(2)).trimmingCharacters(in: .whitespaces)
}

private func markdownOrderedListItem(from line: String) -> String? {
    guard let match = firstMatch(#"^\d+\.\s+(.+)$"#, in: line) else {
        return nil
    }
    return match.trimmingCharacters(in: .whitespaces)
}

private func renderInlineMarkdown(_ text: String) -> String {
    var output = ""
    var index = text.startIndex
    var strong = false

    while index < text.endIndex {
        if text[index] == "`" {
            let afterOpening = text.index(after: index)
            if let closing = text[afterOpening...].firstIndex(of: "`") {
                output += "<code>\(escapeHTML(String(text[afterOpening..<closing])))</code>"
                index = text.index(after: closing)
                continue
            }
        }

        if text[index...].hasPrefix("**") {
            output += strong ? "</strong>" : "<strong>"
            strong.toggle()
            index = text.index(index, offsetBy: 2)
            continue
        }

        output += escapeHTML(String(text[index]))
        index = text.index(after: index)
    }

    if strong {
        output += "</strong>"
    }
    return output
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
        .replacingOccurrences(of: "'", with: "&#39;")
}
