import Foundation

func writeE2EReviewAggregate(in runURL: URL, reviewOutcome: String? = nil) throws {
    let issues = try loadE2EReviewAggregateIssues(in: runURL)
    let markdown = renderE2EReviewAggregate(issues: issues, reviewOutcome: reviewOutcome)
    let outputURL = runURL.appendingPathComponent("review.md")
    try markdown.write(to: outputURL, atomically: true, encoding: .utf8)
    print("==> Wrote Swift E2E review aggregate")
    print("    Review: \(outputURL.path)")
}

private struct E2EReviewAggregateIssue {
    var scope: E2EReviewAggregateScope
    var suiteKey: String?
    var testKey: String?
    var review: E2EReview

    var severity: E2EReviewSeverity {
        review.metadata.severity
    }
}

private enum E2EReviewAggregateScope: Int {
    case report = 0
    case suite = 1
    case test = 2

    var metadataValue: String {
        switch self {
        case .report:
            "run"
        case .suite:
            "suite"
        case .test:
            "test"
        }
    }
}

private func loadE2EReviewAggregateIssues(in runURL: URL) throws -> [E2EReviewAggregateIssue] {
    var issues = try loadE2EReviews(
        in: runURL,
        expectedScope: "report",
        relativeTo: runURL
    ).map { review in
        E2EReviewAggregateIssue(scope: .report, suiteKey: nil, testKey: nil, review: review)
    }

    for suiteURL in try reviewAggregateDirectoryChildren(of: e2eObservationsRootURL(in: runURL)) {
        let suiteKey = suiteURL.lastPathComponent
        guard !isE2EReviewDirectoryName(suiteKey) else { continue }

        issues += try loadE2EReviews(
            in: suiteURL,
            expectedScope: "suite",
            relativeTo: runURL
        ).map { review in
            E2EReviewAggregateIssue(scope: .suite, suiteKey: suiteKey, testKey: nil, review: review)
        }

        for testURL in try reviewAggregateDirectoryChildren(of: suiteURL) {
            let testKey = testURL.lastPathComponent
            guard !isE2EReviewDirectoryName(testKey) else { continue }

            issues += try loadE2EReviews(
                in: testURL,
                expectedScope: "test",
                relativeTo: runURL
            ).map { review in
                E2EReviewAggregateIssue(
                    scope: .test,
                    suiteKey: suiteKey,
                    testKey: testKey,
                    review: review
                )
            }
        }
    }

    return issues.sorted { lhs, rhs in
        if lhs.severity.sortRank != rhs.severity.sortRank {
            return lhs.severity.sortRank < rhs.severity.sortRank
        }
        if lhs.scope.rawValue != rhs.scope.rawValue {
            return lhs.scope.rawValue < rhs.scope.rawValue
        }
        if lhs.suiteKey != rhs.suiteKey {
            return (lhs.suiteKey ?? "") < (rhs.suiteKey ?? "")
        }
        if lhs.testKey != rhs.testKey {
            return (lhs.testKey ?? "") < (rhs.testKey ?? "")
        }
        return lhs.review.path < rhs.review.path
    }
}

private func renderE2EReviewAggregate(
    issues: [E2EReviewAggregateIssue],
    reviewOutcome: String?
) -> String {
    var lines: [String] = [
        "# Swift E2E Review",
        "",
    ]

    // CI and E2EAnalyze pass on the review's outcome. A report rendered on its own
    // has none, so the absence of issues keeps its usual meaning there.
    let incomplete = reviewOutcome != nil && reviewOutcome != "success"
    if incomplete {
        lines.append(
            "⚠️ AI review did not complete, so this is not a clean result. Any issues listed are partial; check the review step's output for the reason."
        )
        lines.append("")
    }

    guard !issues.isEmpty else {
        if !incomplete {
            lines.append("No Swift E2E review issues were generated for this run.")
            lines.append("")
        }
        return lines.joined(separator: "\n")
    }

    let runIssues =
        issues
        .filter { $0.scope == .report }
        .sorted(by: reviewAggregateIssueSort)
    for issue in runIssues {
        appendE2EReviewAggregateIssue(issue, headingLevel: 2, to: &lines)
    }

    var wroteSuite = false
    let suiteKeys = Set(issues.compactMap(\.suiteKey)).sorted()
    for suiteKey in suiteKeys {
        let suiteIssues =
            issues
            .filter { $0.scope == .suite && $0.suiteKey == suiteKey }
            .sorted(by: reviewAggregateIssueSort)
        let testIssues =
            issues
            .filter { $0.scope == .test && $0.suiteKey == suiteKey }
        guard !suiteIssues.isEmpty || !testIssues.isEmpty else { continue }

        if !runIssues.isEmpty || wroteSuite {
            lines.append("---")
            lines.append("")
        }
        lines.append("## `\(suiteKey)`")
        lines.append("")

        for issue in suiteIssues {
            appendE2EReviewAggregateIssue(issue, headingLevel: 3, to: &lines)
        }
        for issue in testIssues.sorted(by: reviewAggregateIssueSort) {
            appendE2EReviewAggregateIssue(issue, headingLevel: 3, to: &lines)
        }

        wroteSuite = true
    }

    return lines.joined(separator: "\n")
}

private func reviewAggregateIssueSort(
    _ lhs: E2EReviewAggregateIssue,
    _ rhs: E2EReviewAggregateIssue
) -> Bool {
    if lhs.severity.sortRank != rhs.severity.sortRank {
        return lhs.severity.sortRank < rhs.severity.sortRank
    }
    if lhs.review.title != rhs.review.title {
        return lhs.review.title < rhs.review.title
    }
    return lhs.review.path < rhs.review.path
}

private func reviewAggregateSeverityMarker(_ severity: E2EReviewSeverity) -> String {
    severity.symbol
}

private func appendE2EReviewAggregateIssue(
    _ issue: E2EReviewAggregateIssue,
    headingLevel: Int,
    to lines: inout [String]
) {
    let review = issue.review
    lines.append(reviewAggregateTitleLine(for: issue, headingLevel: headingLevel))
    lines.append("")
    lines.append(reviewAggregateSummaryMarkdown(review.summaryMarkdown))
    lines.append("")
    lines.append("<details>")
    lines.append("<summary>Details</summary>")
    lines.append("")
    lines.append(review.detailsMarkdown)
    lines.append("")
    appendE2EReviewAggregateMetadata(issue, to: &lines)
    lines.append("")
    lines.append("</details>")
    lines.append("")
}

private func reviewAggregateTitleLine(
    for issue: E2EReviewAggregateIssue,
    headingLevel: Int
) -> String {
    let heading = String(repeating: "#", count: headingLevel)
    let title = reviewAggregateSingleLine(issue.review.title)
    return "\(heading) \(reviewAggregateSeverityMarker(issue.severity)) \(title)"
}

private func appendE2EReviewAggregateMetadata(
    _ issue: E2EReviewAggregateIssue,
    to lines: inout [String]
) {
    let review = issue.review
    lines.append("- Scope: `\(issue.scope.metadataValue)`")
    if let testKey = issue.testKey {
        lines.append("- Test: `\(testKey)`")
    }
    lines.append("- Reviewer: `\(review.metadata.reviewer)`")
    if let confidence = review.metadata.confidence, !confidence.isEmpty {
        lines.append("- Confidence: `\(confidence)`")
    }
    if let locations = review.metadata.locations, !locations.isEmpty {
        lines.append("- Locations: \(reviewAggregateLocations(locations))")
    }
    lines.append("- Full review: `\(review.path)`")
}

private func reviewAggregateLocations(_ locations: [E2EReviewLocation]) -> String {
    locations.map { location in
        var value = "\(location.path):\(location.startLine)"
        if let endLine = location.endLine, endLine != location.startLine {
            value += "-\(endLine)"
        }
        return "`\(value)`"
    }.joined(separator: ", ")
}

private func reviewAggregateDirectoryChildren(of url: URL) throws -> [URL] {
    guard FileManager.default.fileExists(atPath: url.path) else { return [] }
    return try FileManager.default.contentsOfDirectory(
        at: url,
        includingPropertiesForKeys: [.isDirectoryKey],
        options: [.skipsHiddenFiles]
    )
    .filter { child in
        ((try? child.resourceValues(forKeys: [.isDirectoryKey]).isDirectory) == true)
    }
    .sorted { $0.lastPathComponent < $1.lastPathComponent }
}

private func reviewAggregateSingleLine(_ value: String) -> String {
    value
        .replacingOccurrences(of: "\r", with: " ")
        .replacingOccurrences(of: "\n", with: " ")
        .trimmingCharacters(in: .whitespacesAndNewlines)
}

private func reviewAggregateSummaryMarkdown(_ value: String) -> String {
    let original = value.trimmingCharacters(in: .whitespacesAndNewlines)
    let patterns = [
        #"^(?:\*\*|__)?\s*(?:🛑|⚠️|💡)\s*(?:(?:Error|Fail(?:ure)?|Concern|Info)\b)?\s*(?::|[-–—])?\s*(?:\*\*|__)?\s*"#,
        #"^(?:\*\*|__)?\s*(?:Error|Fail(?:ure)?|Concern|Info)\b\s*(?::|[-–—])\s*(?:\*\*|__)?\s*"#,
    ]

    for pattern in patterns {
        guard let regex = try? NSRegularExpression(pattern: pattern, options: [.caseInsensitive])
        else {
            continue
        }
        let range = NSRange(original.startIndex..<original.endIndex, in: original)
        guard let match = regex.firstMatch(in: original, range: range), match.range.location == 0,
            match.range.length > 0,
            let swiftRange = Range(match.range, in: original)
        else {
            continue
        }

        let stripped = String(original[swiftRange.upperBound...])
            .trimmingCharacters(in: .whitespacesAndNewlines)
        if !stripped.isEmpty {
            return stripped
        }
    }

    return original
}
