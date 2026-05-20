import ArgumentParser
import Foundation

struct AggregateCommand: ParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "aggregate",
        abstract: "Transpose Swift E2E runs into an aggregate layout."
    )

    @Option(name: .long, help: "Directory where the aggregate root is written. Defaults to the first run's parent.")
    var outputDir: String?

    @Argument(help: "Raw Swift E2E run directories to aggregate.")
    var runDirs: [String] = []

    mutating func run() throws {
        guard !runDirs.isEmpty else {
            throw ValidationError("Missing run directory.")
        }

        let rawRunURLs = runDirs.map { URL(fileURLWithPath: $0, isDirectory: true).standardizedFileURL }
        let firstRunURL = rawRunURLs[0]
        let outputURL = URL(
            fileURLWithPath: outputDir ?? firstRunURL.deletingLastPathComponent().path,
            isDirectory: true
        ).standardizedFileURL

        try FileManager.default.createDirectory(at: outputURL, withIntermediateDirectories: true)

        var aggregateRoots: Set<URL> = []
        var mappedRuns: [AggregateMappedRun] = []
        for rawRunURL in rawRunURLs {
            let runID = rawRunURL.lastPathComponent
            let components = try AggregateRunID(runID)
            let aggregateRootURL = outputURL.appendingPathComponent(
                "\(components.workflowName).\(components.runID)",
                isDirectory: true
            )
            aggregateRoots.insert(aggregateRootURL)
            try FileManager.default.createDirectory(
                at: aggregateRootURL,
                withIntermediateDirectories: true
            )

            let mappedRun = try aggregateRun(
                rawRunURL: rawRunURL,
                runID: runID,
                components: components,
                aggregateRootURL: aggregateRootURL
            )
            mappedRuns.append(mappedRun)
        }

        for aggregateRootURL in aggregateRoots {
            try writeAggregateInfo(at: aggregateRootURL, mappedRuns: mappedRuns.filter { run in
                run.aggregateRootURL == aggregateRootURL
            })
        }

        for root in aggregateRoots.sorted(by: { $0.path < $1.path }) {
            print("==> Wrote Swift E2E aggregate: \(root.path)")
        }
    }

    private func aggregateRun(
        rawRunURL: URL,
        runID: String,
        components: AggregateRunID,
        aggregateRootURL: URL
    ) throws -> AggregateMappedRun {
        guard FileManager.default.fileExists(atPath: rawRunURL.path) else {
            throw ValidationError("Run directory does not exist: \(rawRunURL.path)")
        }

        let rawRunsURL = aggregateRootURL.appendingPathComponent("_runs", isDirectory: true)
        let rawRunCopyURL = rawRunsURL.appendingPathComponent(runID, isDirectory: true)
        try FileManager.default.createDirectory(at: rawRunsURL, withIntermediateDirectories: true)
        if rawRunURL.standardizedFileURL != rawRunCopyURL.standardizedFileURL {
            try? FileManager.default.removeItem(at: rawRunCopyURL)
            try copyItem(at: rawRunURL, to: rawRunCopyURL)
        }

        let testsURL = rawRunURL.appendingPathComponent("tests", isDirectory: true)
        let testDirectories = try aggregateTestDirectories(in: testsURL)
        var mappedTests: [AggregateMappedTest] = []
        for testDirectory in testDirectories {
            let suiteKey = testDirectory.deletingLastPathComponent().lastPathComponent
            let testKey = testDirectory.lastPathComponent
            let destinationURL = aggregateRootURL
                .appendingPathComponent(suiteKey, isDirectory: true)
                .appendingPathComponent(testKey, isDirectory: true)
                .appendingPathComponent(components.targetName, isDirectory: true)
                .appendingPathComponent(components.attempt, isDirectory: true)

            try? FileManager.default.removeItem(at: destinationURL)
            try FileManager.default.createDirectory(
                at: destinationURL.deletingLastPathComponent(),
                withIntermediateDirectories: true
            )
            try copyItem(at: testDirectory, to: destinationURL)
            try copyRunLevelFiles(from: rawRunURL, to: destinationURL)
            mappedTests.append(
                AggregateMappedTest(
                    suiteKey: suiteKey,
                    testKey: testKey,
                    path: aggregateRelativePath(destinationURL, base: aggregateRootURL)
                )
            )
        }

        return AggregateMappedRun(
            runID: runID,
            aggregateRootURL: aggregateRootURL,
            workflowName: components.workflowName,
            workflowRunID: components.runID,
            targetName: components.targetName,
            attempt: components.attempt,
            rawRunPath: aggregateRelativePath(rawRunCopyURL, base: aggregateRootURL),
            mappedTests: mappedTests
        )
    }
}

private struct AggregateRunID {
    var workflowName: String
    var runID: String
    var targetName: String
    var attempt: String

    init(_ value: String) throws {
        let parts = value.split(separator: ".", omittingEmptySubsequences: false).map(String.init)
        guard parts.count >= 4 else {
            throw ValidationError(
                "Run ID must have shape <workflow-name>.<run-id>.<target-name>.<attempt>: \(value)"
            )
        }
        self.workflowName = parts[0]
        self.runID = parts[1]
        self.targetName = parts.dropFirst(2).dropLast().joined(separator: ".")
        self.attempt = parts[parts.count - 1]
        guard !workflowName.isEmpty, !runID.isEmpty, !targetName.isEmpty, !attempt.isEmpty else {
            throw ValidationError("Run ID contains an empty component: \(value)")
        }
    }
}

private struct AggregateMappedRun: Encodable {
    var runID: String
    var aggregateRootURL: URL
    var workflowName: String
    var workflowRunID: String
    var targetName: String
    var attempt: String
    var rawRunPath: String
    var mappedTests: [AggregateMappedTest]

    enum CodingKeys: String, CodingKey {
        case runID
        case workflowName
        case workflowRunID = "runId"
        case targetName
        case attempt
        case rawRunPath
        case mappedTests
    }
}

private struct AggregateMappedTest: Encodable {
    var suiteKey: String
    var testKey: String
    var path: String
}

private struct AggregateInfo: Encodable {
    var kind: String
    var generatedAt: String
    var runs: [AggregateMappedRun]
}

private func aggregateTestDirectories(in testsURL: URL) throws -> [URL] {
    guard FileManager.default.fileExists(atPath: testsURL.path) else {
        return []
    }
    guard
        let enumerator = FileManager.default.enumerator(
            at: testsURL,
            includingPropertiesForKeys: [.isDirectoryKey]
        )
    else {
        throw ValidationError("Tests directory cannot be read: \(testsURL.path)")
    }

    var directories: [URL] = []
    for case let url as URL in enumerator {
        let recordURL = url.appendingPathComponent("recording.md")
        if FileManager.default.fileExists(atPath: recordURL.path) {
            directories.append(url)
        }
    }
    return directories.sorted { $0.path < $1.path }
}

private func copyRunLevelFiles(from rawRunURL: URL, to destinationURL: URL) throws {
    for fileName in ["info.json", "test-results.xml", "test-results.raw.xml"] {
        let sourceURL = rawRunURL.appendingPathComponent(fileName)
        guard FileManager.default.fileExists(atPath: sourceURL.path) else { continue }
        try copyItem(at: sourceURL, to: destinationURL.appendingPathComponent(fileName))
    }
}

private func copyItem(at sourceURL: URL, to destinationURL: URL) throws {
    if FileManager.default.fileExists(atPath: destinationURL.path) {
        try FileManager.default.removeItem(at: destinationURL)
    }
    try FileManager.default.copyItem(at: sourceURL, to: destinationURL)
}

private func writeAggregateInfo(at aggregateRootURL: URL, mappedRuns: [AggregateMappedRun]) throws {
    let info = AggregateInfo(
        kind: "swift-e2e-aggregate",
        generatedAt: ISO8601DateFormatter().string(from: Date()),
        runs: mappedRuns.sorted { $0.runID < $1.runID }
    )
    let encoder = JSONEncoder()
    encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
    let data = try encoder.encode(info)
    try data.write(to: aggregateRootURL.appendingPathComponent("info.json"))
}

private func aggregateRelativePath(_ url: URL, base: URL) -> String {
    let basePath = base.standardizedFileURL.path
    let path = url.standardizedFileURL.path
    let prefix = basePath.hasSuffix("/") ? basePath : basePath + "/"
    guard path.hasPrefix(prefix) else {
        return url.lastPathComponent
    }
    return String(path.dropFirst(prefix.count))
}
