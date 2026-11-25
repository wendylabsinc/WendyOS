import ArgumentParser
import Foundation
import Testing
import WendyE2ETesting

@testable import SwiftE2ETestingCLI

@Suite(.serialized)
struct `aggregate saved coverage` {
    @Test
    func
        `unions compatible attempts reaggregates another base and renders coverage without spec highlights or Git`()
        throws
    {
        let f = try GoCoverageFixture(build: false)
        let file = "swift/WendyE2ETests/Tests/WendyE2ETests/ExampleTests.swift"
        let before = """
            @Suite(.disabled("old"))
            struct `'wendy example'` {
                @Test
                func `existing`() { let value = 1 }
                @Test
                func `removed`() {}
                func helper() { let helper = 1 }
            }
            """
        try f.write(file, before)
        let base = try f.commit()
        let after = before.replacingOccurrences(
            of: ".disabled(\"old\")",
            with: ".enabled(if: true)"
        )
        .replacingOccurrences(of: "func `removed`()", with: "func `added`()")
        try f.write(file, after)
        let head = try f.commit()
        try f.buildBinaries()
        try f.attempt()
        var records: [[String: Any]] = []
        for (index, argument) in ["left", "right"].enumerated() {
            let directory = "observations/suite/test-\(index)"
            let native = try f.native(directory + "/coverage/go")
            try f.invoke(native, argument)
            records.append(try f.record(argument, directory: directory))
        }
        try f.records(records)
        try "<testsuite><testcase name=\"fixture\"/></testsuite>".write(
            to: f.run.appendingPathComponent("test-results.xml"),
            atomically: true,
            encoding: .utf8
        )
        let originals = try GoCoverage.nativeFiles(run: f.run)
        let hashes = try CoverageIO.hashes(originals)
        let nativeNames = try originals.map { try CoverageIO.relative($0, to: f.run) }
        let expected = try GoCoverage.profile(
            originals.map { $0.deletingLastPathComponent() }.reduce(into: Set<URL>()) {
                $0.insert($1)
            }.sorted { $0.path < $1.path },
            destination: f.repo.appendingPathComponent("run-expected"),
            source: f.source
        )
        let expectedProfile = try Data(
            contentsOf: f.repo.appendingPathComponent("run-expected/coverage.out")
        )
        let input = f.repo.appendingPathComponent("run-input/swift.123.local.0001")
        try FileManager.default.createDirectory(
            at: input.deletingLastPathComponent(),
            withIntermediateDirectories: true
        )
        try FileManager.default.copyItem(at: f.run, to: input)
        let output = f.repo.appendingPathComponent("run-output")
        let package = f.repo.appendingPathComponent("swift/WendyE2ETests")
        func aggregate(_ range: String) throws {
            var command = try AggregateCommand.parse([
                "--package-dir", package.path, "--repo", f.repo.path, "--output-dir", output.path,
                "--diff", range, input.path,
            ])
            try command.run()
        }
        try aggregate(base + "..." + head)
        let run = output.appendingPathComponent("swift.123")
        let summary = try CoverageIO.read(run.appendingPathComponent("coverage/summary.json"))
        #expect(summary["status"] as? String == "collected")
        let total = summary["summary"] as! [String: Any]
        #expect(total["statements"] as? Int == expected["statements"] as? Int)
        #expect(total["covered"] as? Int == expected["covered"] as? Int)
        #expect(
            try Data(contentsOf: run.appendingPathComponent("coverage/coverage.out"))
                == expectedProfile
        )
        #expect((summary["diff"] as? [String: Any])?["status"] as? String == "not-applicable")
        let comparison = try #require(try E2ERunComparison.load(in: run))
        #expect(Set(comparison.specs.map(\.change)) == ["Added", "Removed", "Changed"])
        #expect(!CoverageIO.exists(input.appendingPathComponent("coverage/bin")))
        #expect(!CoverageIO.exists(input.appendingPathComponent("coverage/coverage.out")))
        #expect(
            !CoverageIO.exists(
                run.appendingPathComponent("attempts/local/0001/coverage/go-native.tar.gz")
            )
        )
        let recovered = try CoverageIO.temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: recovered) }
        _ = try CoverageIO.command([
            "tar", "-xzf", input.appendingPathComponent("coverage/go-native.tar.gz").path, "-C",
            recovered.path,
        ])
        #expect(
            try CoverageIO.hashes(nativeNames.map { recovered.appendingPathComponent($0) })
                == hashes
        )
        let template = f.product.appendingPathComponent(
            "swift/WendyE2ETests/Support/e2e-report.template.html"
        )
        var report = try ReportCommand.parse([
            "--package-dir", package.path, "--template", template.path, "--run-dir", run.path,
        ])
        try report.run()
        var html = try String(contentsOf: run.appendingPathComponent("index.html"), encoding: .utf8)
        #expect(!html.contains("data-spec-change="))
        #expect(!html.contains("data-comparison-removed"))
        #expect(!html.contains("Removed specs"))
        // Saved Removed entries never enter current test counts/outcomes.
        #expect(html.components(separatedBy: "<summary class=\"test-summary\">").count - 1 == 2)
        let savedComparison = try Data(contentsOf: run.appendingPathComponent("comparison.json"))
        // Separate trusted templates/tooling from the tested source identity root.
        var splitReport = try ReportCommand.parse([
            "--package-dir", f.product.appendingPathComponent("swift/WendyE2ETests").path,
            "--tests-dir", package.appendingPathComponent("Tests/WendyE2ETests").path,
            "--run-dir", run.path,
        ])
        // Rendering has no spec identity or source-hash validation gate.
        try splitReport.run()
        splitReport.repo = f.repo.path
        try splitReport.run()
        let splitHTML = try String(
            contentsOf: run.appendingPathComponent("index.html"),
            encoding: .utf8
        )
        #expect(!splitHTML.contains("data-spec-change="))
        #expect(!splitHTML.contains("data-comparison-removed"))
        try f.write(file, after + "\n// source hash for deferred spec annotations is irrelevant\n")
        try splitReport.run()
        #expect(
            try Data(contentsOf: run.appendingPathComponent("comparison.json")) == savedComparison
        )
        try f.write(file, after)
        // A renderer has no Git access; it displays saved coverage results.
        try f.write("fake-bin/git", "#!/bin/sh\nexit 99\n")
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o755],
            ofItemAtPath: f.repo.appendingPathComponent("fake-bin/git").path
        )
        let binaries = [
            ".build/out/Products/Debug/swift-e2e-testing", ".build/debug/swift-e2e-testing",
        ].map { f.product.appendingPathComponent("swift/WendyE2ETests/" + $0) }
        let cli = try #require(binaries.first { CoverageIO.exists($0) })
        var env = ProcessInfo.processInfo.environment
        env["PATH"] =
            f.repo.appendingPathComponent("fake-bin").path + ":" + env["PATH", default: ""]
        let rendered = try CoverageIO.execute(
            [
                cli.path, "report", "--package-dir",
                f.product.appendingPathComponent("swift/WendyE2ETests").path,
                "--repo", f.repo.path, "--template", template.path,
                "--run-dir", run.path,
            ],
            environment: env
        )
        #expect(rendered.status == 0, Comment(rawValue: rendered.error))
        // Exercise the actual shell wrapper without recursively invoking SwiftPM.
        try f.write(
            "fake-bin/swift",
            "#!/bin/sh\n[ \"$1\" = run ] && [ \"$2\" = swift-e2e-testing ] || exit 91\nshift 2\nexec '"
                + cli.path + "' \"$@\"\n"
        )
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o755],
            ofItemAtPath: f.repo.appendingPathComponent("fake-bin/swift").path
        )
        let wrapped = try CoverageIO.execute(
            [
                "/bin/bash", f.product.appendingPathComponent("swift/Scripts/E2EReport.sh").path,
                "--package-dir", f.product.appendingPathComponent("swift/WendyE2ETests").path,
                "--repo", f.repo.path, "--run-dir", run.path,
            ],
            environment: env
        )
        #expect(wrapped.status == 0, Comment(rawValue: wrapped.error))
        let wrappedHTML = try String(
            contentsOf: run.appendingPathComponent("index.html"),
            encoding: .utf8
        )
        #expect(!wrappedHTML.contains("data-spec-change="))
        #expect(!wrappedHTML.contains("data-comparison-removed"))
        // A different base uses exactly the same native evidence/profile.
        try aggregate(head + ".." + head)
        #expect(
            try Data(contentsOf: run.appendingPathComponent("coverage/coverage.out"))
                == expectedProfile
        )
        #expect(try E2ERunComparison.load(in: run)?.specs.isEmpty == true)
        try report.run()
        html = try String(contentsOf: run.appendingPathComponent("index.html"), encoding: .utf8)
        #expect(!html.contains("data-spec-change="))
        #expect(!html.contains("data-comparison-removed"))
        let files = FileManager.default.enumerator(atPath: run.path)!.allObjects as! [String]
        #expect(!files.contains { $0.hasSuffix("diff.patch") || $0.hasSuffix("diff.patch.gz") })
        // Saved wrong-head Go comparisons still fail provenance validation.
        var bad = comparison
        bad.sourceRevision = base
        try JSONEncoder().encode(bad).write(to: run.appendingPathComponent("comparison.json"))
        #expect(throws: (any Error).self) { try E2ERunComparison.load(in: run) }
    }

    @Test
    func
        `incompatible revisions missing counters and corrupt archives fail without authoritative totals`()
        throws
    {
        let f = try GoCoverageFixture()
        try f.attempt()
        let directory = "observations/suite/test"
        let native = try f.native(directory + "/coverage/go")
        try f.invoke(native, "left")
        try f.records([f.record("test", directory: directory)])
        let first = try AggregateCoverageAttempt(input: f.run, repo: f.repo)
        defer { try? FileManager.default.removeItem(at: first.scratch) }
        var source = try f.source
        try f.write("swift/another.swift", "// another revision, identical Go source\n")
        let otherHead = try f.commit()
        source["revision"] = otherHead
        let other = f.repo.appendingPathComponent("run-other")
        try FileManager.default.copyItem(at: f.run, to: other)
        try CoverageIO.write(source, to: other.appendingPathComponent("coverage/source.json"))
        try CoverageIO.write(
            ["exitStatus": 0, "git": ["commit": otherHead]],
            to: other.appendingPathComponent("attempt.json")
        )
        let second = try AggregateCoverageAttempt(input: other, repo: f.repo)
        defer { try? FileManager.default.removeItem(at: second.scratch) }
        let run = f.repo.appendingPathComponent("run-combined")
        try FileManager.default.createDirectory(at: run, withIntermediateDirectories: true)
        #expect(
            try aggregateGoCoverage(
                inputs: [f.run, other],
                captured: [first, second],
                errors: [],
                run: run,
                repo: f.repo,
                diff: nil
            )
        )
        let bad = try CoverageIO.read(run.appendingPathComponent("coverage/summary.json"))
        #expect(bad["status"] as? String == "incomplete")
        #expect(bad["summary"] == nil)
        #expect(!CoverageIO.exists(run.appendingPathComponent("coverage/coverage.out")))
        for path in try CoverageIO.files(native, prefix: "covmeta.") {
            try FileManager.default.removeItem(at: path)
        }
        let incomplete = try AggregateCoverageAttempt(input: f.run, repo: f.repo)
        defer { try? FileManager.default.removeItem(at: incomplete.scratch) }
        #expect(incomplete.result["status"] as? String == "incomplete")
        #expect(incomplete.result["summary"] == nil)
        let hash = try CoverageIO.hashes(GoCoverage.nativeFiles(run: f.run))
        try Data("corrupt archive".utf8).write(
            to: f.run.appendingPathComponent("coverage/go-native.tar.gz")
        )
        #expect(throws: (any Error).self) {
            try AggregateCoverageAttempt(input: f.run, repo: f.repo)
        }
        #expect(throws: (any Error).self) { try GoCoverage.pack(run: f.run) }
        #expect(try CoverageIO.hashes(GoCoverage.nativeFiles(run: f.run)) == hash)
    }
}

@Suite(.serialized)
struct `aggregate coverage boundaries` {
    @Test func `compatible attempts merge a native source union rather than averaging percentages`()
        throws
    {
        let f = try GoCoverageFixture()
        var captures: [AggregateCoverageAttempt] = []
        defer {
            for capture in captures { try? FileManager.default.removeItem(at: capture.scratch) }
        }
        let head = try f.git(["rev-parse", "HEAD"])
        for argument in ["left", "right"] {
            let directory = "observations/suite/" + argument
            let native = try f.native(directory + "/coverage/go")
            try f.invoke(native, argument)
            let raw = f.repo.appendingPathComponent("run-" + argument)
            try CoverageIO.write(f.source, to: raw.appendingPathComponent("coverage/source.json"))
            try CoverageIO.write(
                [f.record(argument, directory: directory)],
                to: raw.appendingPathComponent("coverage/tests.json")
            )
            try CoverageIO.write(
                ["exitStatus": 0, "git": ["commit": head]],
                to: raw.appendingPathComponent("attempt.json")
            )
            let target = raw.appendingPathComponent(directory)
            try FileManager.default.createDirectory(
                at: target.deletingLastPathComponent(),
                withIntermediateDirectories: true
            )
            try FileManager.default.copyItem(
                at: native.deletingLastPathComponent().deletingLastPathComponent(),
                to: target
            )
            captures.append(try AggregateCoverageAttempt(input: raw, repo: f.repo))
        }
        let package = "example.test/coverage/go/internal/sample"
        for capture in captures {
            let summary = capture.result["summary"] as! [String: Any]
            #expect(
                (summary["packages"] as! [String: [String: Int]])[package] == [
                    "covered": 2, "statements": 3,
                ]
            )
        }
        let run = f.repo.appendingPathComponent("run-union")
        try FileManager.default.createDirectory(at: run, withIntermediateDirectories: true)
        #expect(
            try !aggregateGoCoverage(
                inputs: captures.map(\.input),
                captured: captures,
                errors: [],
                run: run,
                repo: f.repo,
                diff: nil
            )
        )
        let combined =
            try CoverageIO.read(run.appendingPathComponent("coverage/summary.json"))["summary"]
            as! [String: Any]
        #expect(
            (combined["packages"] as! [String: [String: Int]])[package] == [
                "covered": 3, "statements": 3,
            ]
        )
    }

    @Test func `bootstrap-only archived no data aggregates without permanent unpack`() throws {
        let f = try GoCoverageFixture()
        try f.attempt()
        try f.records([["test": "skipped", "status": "skipped", "sourceID": f.source["sourceID"]!]])
        let bootstrap = try f.native("coverage/bootstrap")
        try f.invoke(bootstrap, "left")
        try GoCoverage.pack(run: f.run)
        let attempt = try AggregateCoverageAttempt(input: f.run, repo: f.repo)
        defer { try? FileManager.default.removeItem(at: attempt.scratch) }
        #expect(attempt.result["status"] as? String == "no-data")
        #expect(attempt.result["summary"] == nil)
        #expect(try CoverageIO.files(bootstrap).isEmpty)
        #expect(!CoverageIO.exists(f.cli))
    }

    @Test func `source revision mismatch and malformed capture provenance fail explicitly`() throws
    {
        let f = try GoCoverageFixture()
        try f.attempt()
        try f.records([])
        var info = try CoverageIO.read(f.run.appendingPathComponent("attempt.json"))
        info["git"] = ["commit": String(repeating: "a", count: 40)]
        try CoverageIO.write(info, to: f.run.appendingPathComponent("attempt.json"))
        #expect(throws: (any Error).self) {
            try AggregateCoverageAttempt(input: f.run, repo: f.repo)
        }
        var source = try f.source
        source.removeValue(forKey: "files")
        try CoverageIO.write(source, to: f.run.appendingPathComponent("coverage/source.json"))
        #expect(throws: (any Error).self) {
            try GoCoverage(repo: f.repo, run: f.run).deriveAttempt()
        }
        #expect(throws: (any Error).self) {
            try CaptureCommand.parse(["--phase", "unknown", "--run-dir", f.run.path])
        }
    }
}

@Suite
struct `coverage source scope` {
    @Test
    func
        `generated marker is a file header and an empty handwritten denominator has no percentage`()
        throws
    {
        #expect(
            try GoCoverage.generated("// Code generated by fixture. DO NOT EDIT.\npackage sample\n")
        )
        #expect(
            try !GoCoverage.generated(
                "package sample\nfunc X() {\n// Code generated by example. DO NOT EDIT.\n}\n"
            )
        )
        let f = try GoCoverageFixture(build: false)
        try f.write("go/cmd/wendy/main.go", "package main\nfunc main() {}\n")
        _ = try f.commit()
        try f.buildBinaries()
        try f.attempt()
        let directory = "observations/suite/empty"
        let native = try f.native(directory + "/coverage/go")
        try f.invoke(native, "ignored")
        try f.records([f.record("empty", directory: directory)])
        #expect(try GoCoverage(repo: f.repo, run: f.run).deriveAttempt() == 0)
        let result = try CoverageIO.read(f.run.appendingPathComponent("coverage/summary.json"))
        #expect(result["status"] as? String == "no-data")
        #expect((result["summary"] as? [String: Any])?["percent"] is NSNull)
    }
}

@Suite
struct `managed coverage participants` {
    @Test func `a managed build cannot silently collect only CLI evidence`() throws {
        let f = try GoCoverageFixture(managed: true)
        try f.attempt()
        let directory = "observations/suite/test"
        let native = try f.native(directory + "/coverage/go")
        try f.invoke(native, "left")
        try f.records([f.record("test", directory: directory)])
        #expect(try GoCoverage(repo: f.repo, run: f.run).deriveAttempt() == 1)
        let result = try CoverageIO.read(f.run.appendingPathComponent("coverage/summary.json"))
        #expect(result["status"] as? String == "incomplete")
        #expect(result["summary"] == nil)
        #expect(
            (result["errors"] as? [String] ?? []).contains {
                $0.contains("Missing managed Agent participant")
            }
        )
    }
}

@Suite
struct `captured invocation receipts` {
    @Test func `removing an entire recorded CLI participant still fails completeness`() throws {
        let f = try GoCoverageFixture()
        try f.attempt()
        let directory = "observations/suite/test"
        let native = try f.native(directory + "/coverage/go")
        try f.invoke(native, "left")
        try f.records([f.record("test", directory: directory)])
        for path in try CoverageIO.files(native) { try FileManager.default.removeItem(at: path) }
        #expect(try GoCoverage(repo: f.repo, run: f.run).deriveAttempt() == 1)
        let result = try CoverageIO.read(f.run.appendingPathComponent("coverage/summary.json"))
        #expect(result["status"] as? String == "incomplete")
        #expect(result["summary"] == nil)
        #expect(
            (result["errors"] as? [String] ?? []).contains {
                $0.contains("CLI invocation evidence changed")
            }
        )
    }
}
