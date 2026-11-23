import ArgumentParser
import Foundation
import Testing
import WendyE2ETesting

@testable import SwiftE2ETestingCLI

@Suite
struct `diff comparison rendering` {
    private let before = """
        @Suite
        struct `'wendy example'` {
            @Test
            func `unchanged`() { let value = 1 }
            @Test
            func `edited`() { let value = 1 }
            func helper() { let helperValue = 1 }
        }
        """

    private var after: String {
        before.replacingOccurrences(
            of: "func `edited`() { let value = 1 }",
            with: "func `edited`() { let value = 2 }"
        )
        .replacingOccurrences(
            of: "    func helper()",
            with: "    @Test\n    func `added`() {}\n    func helper()"
        )
    }

    private func comparison(
        at root: URL,
        source: URL,
        selected: [Int],
        baseSource: String? = nil,
        currentSource: String? = nil
    ) throws -> URL {
        let file = root.appendingPathComponent("diff.json")
        let data: [String: Any] = [
            "status": "not-applicable",
            "provenance": [
                "baseRevision": String(repeating: "a", count: 40),
                "sourceRevision": String(repeating: "b", count: 40),
                "sourceID": String(repeating: "c", count: 64),
                "patchSHA256": String(repeating: "d", count: 64),
            ],
            "files": [
                [
                    "path": source.lastPathComponent, "oldPath": source.lastPathComponent,
                    "sourceFilePath": "Tests/" + source.lastPathComponent,
                    "baseSource": baseSource ?? before, "source": currentSource ?? after,
                    "addedLines": selected,
                    "removedLines": [],
                ] as [String: Any]
            ],
        ]
        try JSONSerialization.data(withJSONObject: data).write(to: file)
        return file
    }

    @Test
    func `reference and report show changes independently of unknown outcomes`() throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: root) }
        let package = root.appendingPathComponent("Package")
        let tests = package.appendingPathComponent("Tests")
        let support = package.appendingPathComponent("Support")
        let run = root.appendingPathComponent("Run")
        let attempt = run.appendingPathComponent("attempts/local/0001")
        for directory in [tests, support, attempt] {
            try FileManager.default.createDirectory(
                at: directory,
                withIntermediateDirectories: true
            )
        }
        let source = tests.appendingPathComponent("ExampleTests.swift")
        try after.write(to: source, atomically: true, encoding: .utf8)
        let selected = Array(1...after.components(separatedBy: .newlines).count)
        let comparisonURL = try comparison(at: root, source: source, selected: selected)
        let reference = root.appendingPathComponent("reference")
        var referenceCommand = try ReferenceCommand.parse([
            "--format", "html", "--comparison", comparisonURL.path,
            "--output", reference.path, source.path,
        ])
        try referenceCommand.run()
        let referenceHTML = try String(
            contentsOf: reference.appendingPathComponent("wendy-example.html"),
            encoding: .utf8
        )
        #expect(referenceHTML.contains("<code>NEW</code>"))
        #expect(referenceHTML.contains("<code>MODIFIED</code>"))
        #expect(referenceHTML.contains("unchanged"))
        // The packaged source retains the package-relative test identity even
        // when it lacks the original repository path prefix.
        var packaged =
            try JSONSerialization.jsonObject(with: Data(contentsOf: comparisonURL))
            as! [String: Any]
        var sources = packaged["files"] as! [[String: Any]]
        sources[0]["path"] = "swift/WendyE2ETests/Tests/ExampleTests.swift"
        packaged["files"] = sources
        try JSONSerialization.data(withJSONObject: packaged).write(to: comparisonURL)

        let metadata: [String: Any] = [
            "git": ["commit": String(repeating: "b", count: 40)], "exitStatus": 0,
            "coverage": [
                "status": "collected", "summary": ["percent": 25.0],
                "diff": ["status": "not-applicable", "provenance": packaged["provenance"]!],
            ] as [String: Any],
        ]
        try JSONSerialization.data(withJSONObject: metadata).write(
            to: attempt.appendingPathComponent("attempt.json")
        )
        let template = URL(fileURLWithPath: #filePath).deletingLastPathComponent()
            .deletingLastPathComponent().deletingLastPathComponent()
            .appendingPathComponent("Support/e2e-report.template.html")
        try FileManager.default.copyItem(
            at: template,
            to: support.appendingPathComponent(template.lastPathComponent)
        )
        var reportCommand = try ReportCommand.parse([
            "--package-dir", package.path, "--run-dir", run.path, "--comparison",
            comparisonURL.path,
        ])
        try reportCommand.run()
        let report = try String(
            contentsOf: run.appendingPathComponent("index.html"),
            encoding: .utf8
        )
        #expect(report.contains("data-spec-change=\"NEW\""))
        #expect(report.contains("data-spec-change=\"MODIFIED\""))
        #expect(report.contains("Not applicable (no eligible changed Go statements)"))
        #expect(report.contains("Go source coverage:"))
        #expect(report.contains("unchanged"))
        #expect(report.contains("Unknown"))
    }

    @Test
    func `filtered helper edits do not select omitted declaration edits and wrong source fails`()
        throws
    {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: root) }
        let source = root.appendingPathComponent("ExampleTests.swift")
        try after.write(to: source, atomically: true, encoding: .utf8)
        let helperLine =
            try #require(
                after.components(separatedBy: .newlines).firstIndex { $0.contains("func helper") }
            ) + 1
        let url = try comparison(at: root, source: source, selected: [helperLine])
        let diff = try E2EDiffComparison(at: url)
        let marked = try diff.annotate(
            WendyE2EReference.parseFile(at: source.path),
            at: source.path
        )
        #expect(marked.flatMap(\.sections).flatMap(\.entries).allSatisfy { $0.change == nil })
        try (after + "// wrong source\n").write(to: source, atomically: true, encoding: .utf8)
        #expect(throws: (any Error).self) { try diff.annotate([], at: source.path) }
    }

    @Test
    func `selected inherited suite traits modify specs while omitted trait hunks do not`() throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: root) }
        let source = root.appendingPathComponent("ExampleTests.swift")
        let base = before.replacingOccurrences(
            of: "@Suite",
            with: "@Suite(.disabled(\"maintenance\"))"
        )
        try before.write(to: source, atomically: true, encoding: .utf8)
        let url = try comparison(
            at: root,
            source: source,
            selected: [1],
            baseSource: base,
            currentSource: before
        )
        let diff = try E2EDiffComparison(at: url)
        let marked = try diff.annotate(
            WendyE2EReference.parseFile(at: source.path),
            at: source.path
        )
        #expect(
            marked.flatMap(\.sections).flatMap(\.entries).map(\.change) == [.modified, .modified]
        )
        #expect(
            WendyE2EReference.renderHTML(marked).components(separatedBy: "<code>MODIFIED</code>")
                .count == 3
        )
        let omitted = try comparison(
            at: root,
            source: source,
            selected: [],
            baseSource: base,
            currentSource: before
        )
        let unchanged = try E2EDiffComparison(at: omitted).annotate(
            WendyE2EReference.parseFile(at: source.path),
            at: source.path
        )
        #expect(unchanged.flatMap(\.sections).flatMap(\.entries).allSatisfy { $0.change == nil })
        try after.write(to: source, atomically: true, encoding: .utf8)
        let onlySuite = try comparison(at: root, source: source, selected: [1], baseSource: base)
        let filtered = try E2EDiffComparison(at: onlySuite).annotate(
            WendyE2EReference.parseFile(at: source.path),
            at: source.path
        )
        #expect(
            filtered.flatMap(\.sections).flatMap(\.entries).map(\.change) == [
                .modified, .modified, nil,
            ]
        )
    }

    @Test
    func `explicit and automatic report comparisons reject the same mismatched run revision`()
        throws
    {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: root) }
        let attempt = root.appendingPathComponent("attempts/local/0001")
        let coverage = attempt.appendingPathComponent("coverage")
        try FileManager.default.createDirectory(at: coverage, withIntermediateDirectories: true)
        let source = root.appendingPathComponent("ExampleTests.swift")
        try after.write(to: source, atomically: true, encoding: .utf8)
        let input = try comparison(at: root, source: source, selected: [6])
        let automatic = coverage.appendingPathComponent("diff.json")
        try FileManager.default.copyItem(at: input, to: automatic)
        func setRevision(_ character: String) throws {
            try JSONSerialization.data(withJSONObject: [
                "git": ["commit": String(repeating: character, count: 40)]
            ])
            .write(to: attempt.appendingPathComponent("attempt.json"))
        }
        try setRevision("e")
        #expect(throws: (any Error).self) { try e2eRunComparison(at: root, explicit: nil) }
        #expect(throws: (any Error).self) { try e2eRunComparison(at: root, explicit: input.path) }
        try setRevision("b")
        #expect(try e2eRunComparison(at: root, explicit: nil) != nil)
        #expect(try e2eRunComparison(at: root, explicit: input.path) != nil)
    }
}
