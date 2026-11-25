import Foundation
import Testing

@testable import SwiftE2ETestingCLI

@Suite(.serialized)
struct `repeatable coverage aggregation` {
    private func cli(_ f: GoCoverageFixture) throws -> URL {
        try #require(
            [".build/out/Products/Debug/swift-e2e-testing", ".build/debug/swift-e2e-testing"]
                .map { f.product.appendingPathComponent("swift/WendyE2ETests/" + $0) }
                .first { CoverageIO.exists($0) }
        )
    }

    @Test(arguments: [0, 7])
    func `failed derivation preserves capture facts and restored source retries successfully`(
        executionExit: Int
    ) throws {
        let f = try GoCoverageFixture()
        try f.attempt(exit: executionExit)
        let directory = "observations/suite/test"
        let native = try f.native(directory + "/coverage/go")
        try f.invoke(native, "left")
        try f.records([f.record("test", directory: directory)])
        let expected = try GoCoverage.profile(
            [native],
            destination: f.repo.appendingPathComponent("expected"),
            source: f.source
        )
        let input = f.repo.appendingPathComponent("run-input/swift.retry.local.0001")
        try FileManager.default.createDirectory(
            at: input.deletingLastPathComponent(),
            withIntermediateDirectories: true
        )
        try FileManager.default.copyItem(at: f.run, to: input)
        let records = try Data(contentsOf: input.appendingPathComponent("coverage/tests.json"))
        let output = f.repo.appendingPathComponent("run-output")
        let arguments = [
            try cli(f).path, "aggregate", "--repo", f.repo.path, "--package-dir", f.repo.path,
            "--output-dir", output.path, input.path,
        ]
        try f.write("go/internal/sample/branch.go", GoCoverageFixture.branch + "// dirty source\n")
        let failed = try CoverageIO.execute(arguments)
        #expect(failed.status == 1)
        #expect(failed.error.contains("Source changed since coverage build"))
        #expect(
            try CoverageIO.read(input.appendingPathComponent("attempt.json"))["exitStatus"] as? Int
                == executionExit
        )
        #expect(
            try Data(contentsOf: input.appendingPathComponent("coverage/tests.json")) == records
        )
        let archive = try Data(
            contentsOf: input.appendingPathComponent("coverage/go-native.tar.gz")
        )
        try f.write("go/internal/sample/branch.go", GoCoverageFixture.branch)
        let retry = try CoverageIO.execute(arguments)
        #expect(retry.status == 0, Comment(rawValue: retry.error))
        let summary = try CoverageIO.read(
            output.appendingPathComponent("swift.retry/coverage/summary.json")
        )
        #expect(summary["status"] as? String == "collected")
        #expect((summary["errors"] as? [String])?.isEmpty == true)
        #expect(summary["summary"] as? NSDictionary == expected as NSDictionary)
        #expect(
            try Data(contentsOf: output.appendingPathComponent("swift.retry/coverage/coverage.out"))
                == Data(contentsOf: f.repo.appendingPathComponent("expected/coverage.out"))
        )
        #expect(
            try Data(contentsOf: input.appendingPathComponent("coverage/tests.json")) == records
        )
        #expect(
            try Data(contentsOf: input.appendingPathComponent("coverage/go-native.tar.gz"))
                == archive
        )
        #expect(
            try CoverageIO.read(input.appendingPathComponent("attempt.json"))["exitStatus"] as? Int
                == executionExit
        )
    }

    @Test func `collected participant without directory is incomplete`() throws {
        let f = try GoCoverageFixture()
        try f.attempt()
        try f.records([
            [
                "test": "fixture/test", "status": "collected", "sourceID": f.source["sourceID"]!,
                "cliInvocations": 1,
            ]
        ])
        #expect(try GoCoverage(repo: f.repo, run: f.run).deriveAttempt() == 1)
        let capture = try AggregateCoverageAttempt(input: f.run, repo: f.repo)
        defer { try? FileManager.default.removeItem(at: capture.scratch) }
        #expect(capture.result["status"] as? String == "incomplete")
        #expect(capture.result["summary"] == nil)
        let derived = try #require(capture.result["tests"] as? [[String: Any]])
        #expect(derived.first?["status"] as? String == "incomplete")
        #expect(derived.first?["nativeValidated"] as? Bool == false)
        #expect(
            (capture.result["errors"] as? [String] ?? []).contains {
                $0.contains("Missing collected test directory")
            }
        )
    }

    @Test
    func `requested unavailable diff warns without changing test or overall coverage outcome`()
        throws
    {
        let f = try GoCoverageFixture()
        try f.attempt()
        let directory = "observations/suite/test"
        try f.invoke(f.native(directory + "/coverage/go"), "left")
        try f.records([f.record("test", directory: directory)])
        let input = f.repo.appendingPathComponent("run-input/swift.diff.local.0001")
        try FileManager.default.createDirectory(
            at: input.deletingLastPathComponent(),
            withIntermediateDirectories: true
        )
        try FileManager.default.copyItem(at: f.run, to: input)
        let output = f.repo.appendingPathComponent("run-output")
        let result = try CoverageIO.execute([
            try cli(f).path, "aggregate", "--repo", f.repo.path, "--package-dir", f.repo.path,
            "--output-dir", output.path, "--diff", "does-not-exist...HEAD", input.path,
        ])
        #expect(result.status == 0)
        #expect(result.error.contains("WARNING: E2E aggregate comparison unavailable:"))
        #expect(result.error.contains("does-not-exist"))
        let summary = try CoverageIO.read(
            output.appendingPathComponent("swift.diff/coverage/summary.json")
        )
        #expect(summary["status"] as? String == "collected")
        #expect((summary["diff"] as? [String: Any])?["status"] as? String == "unavailable")
        #expect(
            try CoverageIO.read(input.appendingPathComponent("attempt.json"))["exitStatus"] as? Int
                == 0
        )
    }
}
