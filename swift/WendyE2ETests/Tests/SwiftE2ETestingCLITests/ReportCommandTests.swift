import ArgumentParser
import Foundation
import Testing

@testable import SwiftE2ETestingCLI

@Suite
struct `report command` {
    @Test
    func `escapes target names in overview HTML`() throws {
        let rootURL = temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: rootURL) }

        let packageURL = rootURL.appendingPathComponent("Package", isDirectory: true)
        let testsURL = packageURL.appendingPathComponent("Tests", isDirectory: true)
        let supportURL = packageURL.appendingPathComponent("Support", isDirectory: true)
        let runURL = rootURL.appendingPathComponent("Run", isDirectory: true)
        let attemptArtifactsURL =
            runURL
            .appendingPathComponent("attempts", isDirectory: true)
            .appendingPathComponent("macos-to-<img src=x onerror=\"alert(1)\">", isDirectory: true)
            .appendingPathComponent("attempt-1", isDirectory: true)
        let observationURL =
            runURL
            .appendingPathComponent("observations", isDirectory: true)
            .appendingPathComponent("report-security", isDirectory: true)
            .appendingPathComponent("escapes-malicious-target", isDirectory: true)
            .appendingPathComponent("macos-to-<img src=x onerror=\"alert(1)\">", isDirectory: true)
            .appendingPathComponent("attempt-1", isDirectory: true)
        let noObservationAttemptURL =
            runURL
            .appendingPathComponent("attempts", isDirectory: true)
            .appendingPathComponent("macos-to-no-observations", isDirectory: true)
            .appendingPathComponent("attempt-1", isDirectory: true)

        try FileManager.default.createDirectory(at: testsURL, withIntermediateDirectories: true)
        try FileManager.default.createDirectory(at: supportURL, withIntermediateDirectories: true)
        try FileManager.default.createDirectory(
            at: attemptArtifactsURL,
            withIntermediateDirectories: true
        )
        try FileManager.default.createDirectory(
            at: observationURL,
            withIntermediateDirectories: true
        )
        try FileManager.default.createDirectory(
            at: noObservationAttemptURL,
            withIntermediateDirectories: true
        )
        try writeReportTestMetadata(to: observationURL)

        try """
        import Testing

        @Suite
        struct `report security` {
            @Test
            func `escapes malicious target`() async throws {}
        }
        """.write(
            to: testsURL.appendingPathComponent("ReportSecurityTests.swift"),
            atomically: true,
            encoding: .utf8
        )

        try """
        <?xml version="1.0" encoding="UTF-8"?>
        <testsuite tests="1" failures="0" skipped="0">
          <testcase classname="WendyE2ETests.`report security`" name="escapes malicious target()" time="0.01" />
        </testsuite>
        """.write(
            to: attemptArtifactsURL.appendingPathComponent("test-results.xml"),
            atomically: true,
            encoding: .utf8
        )

        try FileManager.default.copyItem(
            at: productionReportTemplateURL(),
            to: supportURL.appendingPathComponent("e2e-report.template.html")
        )

        var command = try ReportCommand.parse([
            "--package-dir", packageURL.path,
            "--run-dir", runURL.path,
        ])
        try command.run()

        let html = try String(
            contentsOf: runURL.appendingPathComponent("index.html"),
            encoding: .utf8
        )

        #expect(!html.contains("<img src=x onerror="))
        #expect(html.contains("macos-to-&lt;img src=x onerror=&quot;alert(1)&quot;&gt;"))
        #expect(html.contains("title=\"macos-to-&lt;img src=x onerror=&quot;alert(1)&quot;&gt;\""))
        #expect(html.contains("macos-to-no-observations"))
        #expect(html.contains("--background: #F1EEE7"))
        #expect(html.contains("--foreground: #171C23"))
        #expect(html.contains("--seafoam: #9FE2BF"))
        #expect(html.contains("font: 16px/1.6 \"Geist\""))
        #expect(html.contains("font-family: \"Geist Mono\""))
        #expect(html.contains("viewBox=\"0 0 749.97 181.81\""))
        #expect(html.contains("class=\"brand-mark\" role=\"img\" aria-label=\"Wendy\""))
        #expect(html.contains("Swift E2E · Run report"))
        #expect(html.contains(":where(a, button, input, summary):focus-visible"))
        #expect(!html.contains("fonts.googleapis.com"))
        #expect(!html.contains("<script src="))
        #expect(!html.contains("#000"))

        let reviewHTML = try String(
            contentsOf: runURL.appendingPathComponent("review.html"),
            encoding: .utf8
        )
        #expect(reviewHTML.contains("--background: #F1EEE7"))
        #expect(reviewHTML.contains("--foreground: #171C23"))
        #expect(reviewHTML.contains("font: 15px/1.55 \"Geist\""))
        #expect(reviewHTML.contains("font-family: \"Geist Mono\""))
    }

    @Test
    func `renders failed target for failed attempt artifact without observations`() throws {
        let rootURL = temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: rootURL) }

        let packageURL = rootURL.appendingPathComponent("Package", isDirectory: true)
        let testsURL = packageURL.appendingPathComponent("Tests", isDirectory: true)
        let supportURL = packageURL.appendingPathComponent("Support", isDirectory: true)
        let runURL = rootURL.appendingPathComponent("Run", isDirectory: true)
        let attemptURL =
            runURL
            .appendingPathComponent("attempts", isDirectory: true)
            .appendingPathComponent("macos-jetson-orin-nano", isDirectory: true)
            .appendingPathComponent("0001", isDirectory: true)

        try FileManager.default.createDirectory(at: testsURL, withIntermediateDirectories: true)
        try FileManager.default.createDirectory(at: supportURL, withIntermediateDirectories: true)
        try FileManager.default.createDirectory(at: attemptURL, withIntermediateDirectories: true)
        try """
        {
          "exitStatus": 1
        }
        """.write(
            to: attemptURL.appendingPathComponent("attempt.json"),
            atomically: true,
            encoding: .utf8
        )
        try """
        <!doctype html>
        <html>
          <body>
            {{TARGET_OVERVIEW}}
            <!-- Repeat this .card section once per test file. -->
            <footer></footer>
          </body>
        </html>
        """.write(
            to: supportURL.appendingPathComponent("e2e-report.template.html"),
            atomically: true,
            encoding: .utf8
        )

        var command = try ReportCommand.parse([
            "--package-dir", packageURL.path,
            "--run-dir", runURL.path,
        ])
        try command.run()

        let html = try String(
            contentsOf: runURL.appendingPathComponent("index.html"),
            encoding: .utf8
        )

        #expect(html.contains("macos-jetson-orin-nano"))
        #expect(html.contains("<td><span class=\"badge fail\">Failed</span></td>"))
        #expect(
            html.contains(
                """
                  <td class="numeric">1</td>
                  <td class="numeric">0</td>
                  <td class="numeric">0</td>
                  <td class="numeric">0</td>
                  <td class="numeric">1</td>
                  <td class="numeric">0</td>
                  <td class="numeric">0</td>
                """
            )
        )
    }
}

// setenv is process-wide, so these cases must not overlap. The other report tests
// do not assert on review.md's wording, so the variable cannot disturb them.
@Suite(.serialized)
struct `report review outcome` {
    @Test(
        arguments: [
            ("failure", true),
            ("skipped", true),
            ("success", false),
            (nil, false),
        ] as [(String?, Bool)]
    )
    func `comes from the environment`(outcome: String?, incomplete: Bool) throws {
        let rootURL = temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: rootURL) }

        let packageURL = rootURL.appendingPathComponent("Package", isDirectory: true)
        let supportURL = packageURL.appendingPathComponent("Support", isDirectory: true)
        let runURL = rootURL.appendingPathComponent("Run", isDirectory: true)
        try FileManager.default.createDirectory(
            at: packageURL.appendingPathComponent("Tests", isDirectory: true),
            withIntermediateDirectories: true
        )
        try FileManager.default.createDirectory(at: supportURL, withIntermediateDirectories: true)
        try FileManager.default.createDirectory(at: runURL, withIntermediateDirectories: true)
        try FileManager.default.copyItem(
            at: productionReportTemplateURL(),
            to: supportURL.appendingPathComponent("e2e-report.template.html")
        )

        if let outcome {
            setenv("WENDY_E2E_REVIEW_OUTCOME", outcome, 1)
        } else {
            unsetenv("WENDY_E2E_REVIEW_OUTCOME")
        }
        defer { unsetenv("WENDY_E2E_REVIEW_OUTCOME") }

        var command = try ReportCommand.parse([
            "--package-dir", packageURL.path,
            "--run-dir", runURL.path,
        ])
        try command.run()

        let review = try String(
            contentsOf: runURL.appendingPathComponent("review.md"),
            encoding: .utf8
        )
        #expect(review.contains("AI review did not complete") == incomplete)
        #expect(review.contains("No Swift E2E review issues were generated") == !incomplete)
    }
}

private func productionReportTemplateURL() -> URL {
    URL(fileURLWithPath: #filePath)
        .deletingLastPathComponent()
        .deletingLastPathComponent()
        .deletingLastPathComponent()
        .appendingPathComponent("Support/e2e-report.template.html")
}

private func writeReportTestMetadata(to observationURL: URL) throws {
    let metadata = E2ETestMetadata(
        schema: e2eTestMetadataSchemaID,
        sourceFilePath: "Tests/ReportSecurityTests.swift",
        sourceFileName: "ReportSecurityTests",
        suiteName: "report security",
        testName: "escapes malicious target",
        functionName: "`escapes malicious target`()",
        line: 5
    )
    let encoder = JSONEncoder()
    encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
    try encoder.encode(metadata).write(
        to: observationURL.appendingPathComponent(e2eTestMetadataFileName),
        options: .atomic
    )
}

private func temporaryDirectory() -> URL {
    URL(fileURLWithPath: NSTemporaryDirectory(), isDirectory: true)
        .appendingPathComponent("wendy-e2e-cli-tests-\(UUID().uuidString)", isDirectory: true)
}
