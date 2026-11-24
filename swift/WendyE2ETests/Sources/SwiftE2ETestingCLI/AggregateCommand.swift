import ArgumentParser
import Foundation

private let e2eSourceArtifactMaxLines = 500
private let e2eSourceArtifactMaxBytes = 1_048_576

struct AggregateCommand: ParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "aggregate",
        abstract: "Aggregate Swift E2E attempts into a run layout."
    )

    @Option(
        name: .long,
        help: "Directory where the E2E run is written. Defaults to the first attempt's parent."
    )
    var outputDir: String?

    @Option(
        name: .long,
        help:
            "Swift package directory used to resolve test source paths. Defaults to the current directory."
    )
    var packageDir: String?

    @Option(
        help:
            "Optional Git range (BASE...HEAD or BASE..HEAD) for changed Go blocks and Added/Removed/Changed specs."
    )
    var diff: String?
    @Option(help: "Matching source repository; defaults to the package repository when needed.")
    var repo: String?

    @Argument(help: "Swift E2E attempt directories to aggregate.")
    var attemptDirs: [String] = []

    mutating func run() throws {
        guard !attemptDirs.isEmpty else {
            throw ValidationError("Missing attempt directory.")
        }

        let attemptURLs = attemptDirs.map {
            URL(fileURLWithPath: $0, isDirectory: true).standardizedFileURL
        }
        let firstAttemptURL = attemptURLs[0]
        let packageURL = URL(
            fileURLWithPath: packageDir ?? FileManager.default.currentDirectoryPath,
            isDirectory: true
        ).standardizedFileURL
        let outputURL = URL(
            fileURLWithPath: outputDir ?? firstAttemptURL.deletingLastPathComponent().path,
            isDirectory: true
        ).standardizedFileURL

        try FileManager.default.createDirectory(at: outputURL, withIntermediateDirectories: true)

        var planned: [URL: Set<String>] = [:]
        for input in attemptURLs {
            let id = try AttemptID(input.lastPathComponent)
            let root = outputURL.appendingPathComponent("\(id.workflowName).\(id.runID)")
            planned[root, default: []].insert(id.targetName + "/" + id.attempt)
        }
        let hasCoverage = attemptURLs.contains {
            CoverageIO.exists($0.appendingPathComponent("coverage/source.json"))
        }
        for (root, expected) in planned
        where hasCoverage || diff != nil
            || CoverageIO.exists(root.appendingPathComponent("coverage/summary.json"))
        {
            let existing = try runAttemptArtifactURLsByTarget(in: root)
            let names = Set(
                existing.flatMap { target, attempts in
                    attempts.map { target + "/" + $0.lastPathComponent }
                }
            )
            guard names.isSubset(of: expected) else {
                throw ValidationError(
                    "Coverage reaggregation requires all run attempts; supply every raw attempt or use a fresh output directory"
                )
            }
        }

        var runURLs: Set<URL> = []
        var inputsByRun: [URL: [URL]] = [:]
        var coverageByRun: [URL: [AggregateCoverageAttempt]] = [:]
        var errorsByRun: [URL: [String]] = [:]
        var repositories: [URL: URL] = [:]
        var scratch: [URL] = []
        defer { for url in scratch { try? FileManager.default.removeItem(at: url) } }
        var failed = false
        for attemptURL in attemptURLs {
            let attemptID = attemptURL.lastPathComponent
            let components = try AttemptID(attemptID)
            let runURL = outputURL.appendingPathComponent(
                "\(components.workflowName).\(components.runID)",
                isDirectory: true
            )
            runURLs.insert(runURL)
            try FileManager.default.createDirectory(
                at: runURL,
                withIntermediateDirectories: true
            )

            inputsByRun[runURL, default: []].append(attemptURL)
            if CoverageIO.exists(attemptURL.appendingPathComponent("coverage/source.json"))
                || diff != nil
            {
                do {
                    let repository =
                        try repo.map { URL(fileURLWithPath: $0) }
                        ?? URL(
                            fileURLWithPath: CoverageIO.command(
                                ["git", "rev-parse", "--show-toplevel"],
                                cwd: packageURL
                            ).trimmingCharacters(in: .whitespacesAndNewlines)
                        )
                    repositories[runURL] = repository
                    if CoverageIO.exists(attemptURL.appendingPathComponent("coverage/source.json"))
                    {
                        let capture = try AggregateCoverageAttempt(
                            input: attemptURL,
                            repo: repository
                        )
                        scratch.append(capture.scratch)
                        coverageByRun[runURL, default: []].append(capture)
                    }
                } catch { errorsByRun[runURL, default: []].append(String(describing: error)) }
            }
            if CoverageIO.exists(attemptURL.appendingPathComponent("coverage/source.json")) {
                do { try GoCoverage.pack(run: attemptURL) } catch {
                    errorsByRun[runURL, default: []].append(String(describing: error))
                }
            }

            try mapAttempt(
                attemptURL: attemptURL,
                components: components,
                runURL: runURL,
                packageURL: packageURL
            )
        }

        for root in runURLs.sorted(by: { $0.path < $1.path }) {
            if try aggregateGoCoverage(
                inputs: inputsByRun[root] ?? [],
                captured: coverageByRun[root] ?? [],
                errors: errorsByRun[root] ?? [],
                run: root,
                repo: repositories[root],
                diff: diff
            ) {
                failed = true
            }
            for input in inputsByRun[root] ?? [] {
                if CoverageIO.exists(input.appendingPathComponent("coverage/source.json")) {
                    try CoverageIO.remove(input.appendingPathComponent("coverage/coverage.out"))
                }
            }
            _ = try writeRunOverview(in: root)
            try writeRunSourceIndex(in: root)
            print("==> Wrote Swift E2E run: \(root.path)")
            print("    Overview: \(runOverviewURL(in: root).path)")
        }
        if failed { throw ExitCode.failure }
    }

    private func mapAttempt(
        attemptURL: URL,
        components: AttemptID,
        runURL: URL,
        packageURL: URL
    ) throws {
        guard FileManager.default.fileExists(atPath: attemptURL.path) else {
            throw ValidationError("Attempt directory does not exist: \(attemptURL.path)")
        }

        let attemptArtifactsURL = e2eAttemptArtifactsURL(
            in: runURL,
            targetName: components.targetName,
            attempt: components.attempt
        )
        try? FileManager.default.removeItem(at: attemptArtifactsURL)
        try FileManager.default.createDirectory(
            at: attemptArtifactsURL,
            withIntermediateDirectories: true
        )
        try copyAttemptLevelArtifacts(from: attemptURL, to: attemptArtifactsURL)

        let testDirectories = try attemptTestDirectories(in: attemptURL)
        for testDirectory in testDirectories {
            let suiteKey = testDirectory.deletingLastPathComponent().lastPathComponent
            let testKey = testDirectory.lastPathComponent
            let destinationURL =
                e2eObservationsRootURL(in: runURL)
                .appendingPathComponent(suiteKey, isDirectory: true)
                .appendingPathComponent(testKey, isDirectory: true)
                .appendingPathComponent(components.targetName, isDirectory: true)
                .appendingPathComponent(components.attempt, isDirectory: true)

            try? FileManager.default.removeItem(at: destinationURL)
            try FileManager.default.createDirectory(
                at: destinationURL.deletingLastPathComponent(),
                withIntermediateDirectories: true
            )
            // HTML/AI inputs need recordings and metadata, not a second copy of
            // native Go evidence or reproducible full-source profiles.
            try copyDirectory(at: testDirectory, to: destinationURL, excluding: ["coverage"])
            try copyTestMetadataIfPresent(
                from: testDirectory,
                to: destinationURL.deletingLastPathComponent().deletingLastPathComponent(),
                packageURL: packageURL
            )
        }
    }
}

private struct AttemptID {
    var workflowName: String
    var runID: String
    var targetName: String
    var attempt: String

    init(_ value: String) throws {
        let parts = value.split(separator: ".", omittingEmptySubsequences: false).map(String.init)
        guard parts.count >= 4 else {
            throw ValidationError(
                "Attempt ID must have shape <workflow-name>.<run-id>.<target-name>.<attempt>: \(value)"
            )
        }
        self.workflowName = parts[0]
        self.runID = parts[1]
        self.targetName = parts.dropFirst(2).dropLast().joined(separator: ".")
        self.attempt = parts[parts.count - 1]
        guard !workflowName.isEmpty, !runID.isEmpty, !targetName.isEmpty, !attempt.isEmpty else {
            throw ValidationError("Attempt ID contains an empty component: \(value)")
        }
    }
}

private func attemptTestDirectories(in attemptURL: URL) throws -> [URL] {
    let observationsURL = attemptURL.appendingPathComponent(
        e2eObservationsDirectoryName,
        isDirectory: true
    )
    guard FileManager.default.fileExists(atPath: observationsURL.path) else {
        return []
    }

    let suiteURLs = try FileManager.default.contentsOfDirectory(
        at: observationsURL,
        includingPropertiesForKeys: [.isDirectoryKey],
        options: [.skipsHiddenFiles]
    )

    var directories: [URL] = []
    for suiteURL in suiteURLs where try isDirectory(suiteURL) {
        let testURLs = try FileManager.default.contentsOfDirectory(
            at: suiteURL,
            includingPropertiesForKeys: [.isDirectoryKey],
            options: [.skipsHiddenFiles]
        )
        for testURL in testURLs where try isDirectory(testURL) {
            let recordURL = testURL.appendingPathComponent("recording.md")
            if FileManager.default.fileExists(atPath: recordURL.path) {
                directories.append(testURL)
            }
        }
    }
    return directories.sorted { $0.path < $1.path }
}

private func isDirectory(_ url: URL) throws -> Bool {
    try url.resourceValues(forKeys: [.isDirectoryKey]).isDirectory == true
}

private func copyTestMetadataIfPresent(
    from testDirectoryURL: URL,
    to testRootURL: URL,
    packageURL: URL
) throws {
    let sourceURL = testDirectoryURL.appendingPathComponent(e2eTestMetadataFileName)
    guard FileManager.default.fileExists(atPath: sourceURL.path) else { return }
    try copyItem(at: sourceURL, to: testRootURL.appendingPathComponent(e2eTestMetadataFileName))
    try writeTestSourceArtifactIfPossible(testRootURL: testRootURL, packageURL: packageURL)
}

private func writeTestSourceArtifactIfPossible(testRootURL: URL, packageURL: URL) throws {
    let metadata = try loadE2ETestMetadata(in: testRootURL)
    guard let startLine = metadata.sourceStartLine,
        let endLine = metadata.sourceEndLine,
        startLine <= endLine
    else {
        return
    }

    guard
        let sourceURL = resolvedTestSourceURL(
            packageURL: packageURL,
            sourceFilePath: metadata.sourceFilePath
        ), FileManager.default.fileExists(atPath: sourceURL.path)
    else { return }

    guard try sourceFileSize(sourceURL) <= e2eSourceArtifactMaxBytes else { return }

    let source = try String(contentsOf: sourceURL, encoding: .utf8)
    guard source.utf8.count <= e2eSourceArtifactMaxBytes else { return }
    let lines = source.components(separatedBy: .newlines)
    guard startLine <= lines.count else { return }
    let cappedEndLine = min(endLine, lines.count, startLine + e2eSourceArtifactMaxLines - 1)
    let chunk = lines[(startLine - 1)..<cappedEndLine].joined(separator: "\n")
    let truncated = cappedEndLine < endLine ? "yes" : "no"

    let declarationLine = metadata.declarationLine.map(String.init) ?? "unknown"
    let contents = """
        # Wendy E2E test source

        - Source: `\(aggregateMarkdownInline(metadata.sourceFilePath)):\(startLine)-\(cappedEndLine)`
        - Suite: `\(aggregateMarkdownInline(metadata.suiteName))`
        - Test: `\(aggregateMarkdownInline(metadata.testName))`
        - Function: `\(aggregateMarkdownInline(metadata.functionName))`
        - Declaration line: `\(declarationLine)`
        - Truncated: `\(truncated)`

        ```swift
        \(chunk)
        ```

        """
    try contents.write(
        to: testRootURL.appendingPathComponent(e2eSourceArtifactFileName),
        atomically: true,
        encoding: .utf8
    )
}

private func writeRunSourceIndex(in runURL: URL) throws {
    var entries: [String] = []
    for suiteURL in try aggregateDirectoryChildren(of: e2eObservationsRootURL(in: runURL)) {
        for testURL in try aggregateDirectoryChildren(of: suiteURL) {
            let sourceURL = testURL.appendingPathComponent(e2eSourceArtifactFileName)
            guard FileManager.default.fileExists(atPath: sourceURL.path),
                let metadata = try? loadE2ETestMetadata(in: testURL)
            else {
                continue
            }

            let startLine = metadata.sourceStartLine.map(String.init) ?? "?"
            let endLine = metadata.sourceEndLine.map(String.init) ?? "?"
            let sourceArtifactPath = aggregateRelativePath(sourceURL, base: runURL)
            let sourceRange =
                "\(aggregateMarkdownInline(metadata.sourceFilePath)):\(startLine)-\(endLine)"
            let suiteName = aggregateMarkdownInline(metadata.suiteName)
            let testName = aggregateMarkdownInline(metadata.testName)
            entries.append(
                "- `\(sourceArtifactPath)` — `\(sourceRange)` — `\(suiteName)` / `\(testName)`"
            )
        }
    }

    let body: String
    if entries.isEmpty {
        body = "- No test source artifacts were recorded.\n"
    } else {
        body = entries.sorted().joined(separator: "\n") + "\n"
    }

    let contents = """
        # Wendy E2E test source index

        Each entry points to the extracted test source, including the DocC/spec comment above the `@Test` declaration when present.

        \(body)
        """
    try contents.write(
        to: runURL.appendingPathComponent(e2eSourceIndexFileName),
        atomically: true,
        encoding: .utf8
    )
}

private func aggregateDirectoryChildren(of url: URL) throws -> [URL] {
    guard FileManager.default.fileExists(atPath: url.path) else { return [] }
    return try FileManager.default.contentsOfDirectory(
        at: url,
        includingPropertiesForKeys: [.isDirectoryKey],
        options: [.skipsHiddenFiles]
    )
    .filter { (try? $0.resourceValues(forKeys: [.isDirectoryKey]).isDirectory) == true }
    .sorted { $0.path < $1.path }
}

private func aggregateRelativePath(_ url: URL, base: URL) -> String {
    let path = url.path
    let basePath = base.path
    if path.hasPrefix(basePath + "/") {
        return String(path.dropFirst(basePath.count + 1))
    }
    return path
}

private func resolvedTestSourceURL(
    packageURL: URL,
    sourceFilePath rawSourceFilePath: String
) -> URL? {
    let sourceFilePath = rawSourceFilePath.precomposedStringWithCanonicalMapping
    let lowercasedPath = sourceFilePath.lowercased()
    let sourcePathComponents = sourceFilePath.split(
        separator: "/",
        omittingEmptySubsequences: false
    )
    guard !sourceFilePath.hasPrefix("/"),
        !sourceFilePath.contains("\0"),
        !sourceFilePath.contains("\\"),
        !lowercasedPath.contains("%2f"),
        !lowercasedPath.contains("%5c"),
        !sourcePathComponents.contains(""),
        !sourcePathComponents.contains("."),
        !sourcePathComponents.contains("..")
    else {
        return nil
    }

    let packageURL = packageURL.resolvingSymlinksInPath().standardizedFileURL
    let sourceURL = packageURL.appendingPathComponent(sourceFilePath, isDirectory: false)
        .resolvingSymlinksInPath()
        .standardizedFileURL
    guard pathComponents(of: sourceURL).starts(with: pathComponents(of: packageURL)) else {
        return nil
    }
    return sourceURL
}

private func pathComponents(of url: URL) -> [String] {
    url.standardizedFileURL.pathComponents.map { $0.precomposedStringWithCanonicalMapping }
}

private func sourceFileSize(_ sourceURL: URL) throws -> Int {
    try sourceURL.resourceValues(forKeys: [.fileSizeKey]).fileSize ?? 0
}

private func aggregateMarkdownInline(_ value: String) -> String {
    let withoutControlCharacters = String(
        value.unicodeScalars.map { scalar in
            CharacterSet.controlCharacters.contains(scalar) ? " " : Character(scalar)
        }
    )
    return
        withoutControlCharacters
        .replacingOccurrences(of: "`", with: "'")
        .components(separatedBy: .whitespacesAndNewlines)
        .filter { !$0.isEmpty }
        .joined(separator: " ")
}

private func copyAttemptLevelArtifacts(from attemptURL: URL, to destinationURL: URL) throws {
    let entries = try FileManager.default.contentsOfDirectory(
        at: attemptURL,
        includingPropertiesForKeys: nil,
        options: [.skipsHiddenFiles]
    )
    for sourceURL in entries
    where ![e2eObservationsDirectoryName, "report"].contains(sourceURL.lastPathComponent) {
        if sourceURL.lastPathComponent == "coverage" {
            try copyDirectory(
                at: sourceURL,
                to: destinationURL.appendingPathComponent("coverage"),
                excluding: [
                    "bin", "bootstrap", "go-native.tar.gz", "native.sha256", "coverage.out",
                    "index.html", "diff.patch", "diff.patch.gz", "diff.json",
                ]
            )
            continue
        }
        try copyItem(
            at: sourceURL,
            to: destinationURL.appendingPathComponent(sourceURL.lastPathComponent)
        )
    }
}

private func copyDirectory(
    at sourceURL: URL,
    to destinationURL: URL,
    excluding names: Set<String>
) throws {
    try FileManager.default.createDirectory(at: destinationURL, withIntermediateDirectories: true)
    for entry in try FileManager.default.contentsOfDirectory(
        at: sourceURL,
        includingPropertiesForKeys: nil
    ) {
        if !names.contains(entry.lastPathComponent) {
            try copyItem(
                at: entry,
                to: destinationURL.appendingPathComponent(entry.lastPathComponent)
            )
        }
    }
}

private func copyItem(at sourceURL: URL, to destinationURL: URL) throws {
    if FileManager.default.fileExists(atPath: destinationURL.path) {
        try FileManager.default.removeItem(at: destinationURL)
    }
    try FileManager.default.copyItem(at: sourceURL, to: destinationURL)
}
