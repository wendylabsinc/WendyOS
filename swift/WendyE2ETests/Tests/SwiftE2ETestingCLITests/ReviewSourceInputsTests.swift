import ArgumentParser
import Foundation
import Testing

@testable import SwiftE2ETestingCLI

@Suite
struct ReviewSourceInputsTests {
    @Test
    func `diff preparation uses trusted siblings and retains source schema pins and generators`()
        throws
    {
        let fixture = try SourceReviewFixture()
        try fixture.write("GENERATED_BODY_SENTINEL\n", to: "go/proto/gen/service.pb.go")
        try fixture.write("message SOURCE_SCHEMA {}\n", to: "Proto/service.proto")
        try fixture.write("SOURCE_PIN\n", to: "go/proto/service-protos.version")
        try fixture.write("SOURCE_GENERATOR\n", to: "go/scripts/generate-proto.sh")
        try fixture.write("HANDWRITTEN_LOOKALIKE\n", to: "Sources/handwritten.pb.go")
        for name in ["review_sources.py", "review_diff.py"] {
            try fixture.write(
                "raise RuntimeError('EVENT_HEAD_MUST_NOT_SUPPLY_POLICY')\n",
                to: ".github/scripts/\(name)"
            )
        }
        try fixture.commit()
        let context = try fixture.prepare(diff: true)
        let patch = try String(contentsOf: context.sources.patchURL, encoding: .utf8)
        #expect(!patch.contains("GENERATED_BODY_SENTINEL"))
        for source in ["SOURCE_SCHEMA", "SOURCE_PIN", "SOURCE_GENERATOR", "HANDWRITTEN_LOOKALIKE"] {
            #expect(patch.contains(source))
        }
        let manifest = try fixture.manifest()
        #expect(manifest["policy_commit"] as? String == fixture.base)
        let head = try fixture.git(["rev-parse", "HEAD"])
        #expect(manifest["head_commit"] as? String == head)
        let coverage = try #require(manifest["coverage"] as? [String: Any])
        #expect((coverage["excluded_generated_files"] as? [[String: Any]])?.count == 1)
        #expect((coverage["non_utf8_files"] as? [String]) == [])
        let names = try String(contentsOf: context.sources.changedFilesURL, encoding: .utf8)
        #expect(!names.contains("go/proto/gen/service.pb.go"))
        #expect(names.contains("Proto/service.proto"))
        let prompt = try fixture.prompt(context)
        #expect(prompt.contains("prepared inputs only"))
        #expect(prompt.contains("upstream schema bodies"))
        #expect(prompt.contains("tool responses are not automatically filtered"))
        #expect(!prompt.contains("git -C"))
    }

    @Test
    func `full preparation scopes committed source without claiming tools are constrained`() throws
    {
        let fixture = try SourceReviewFixture()
        let context = try fixture.prepare(diff: false)
        #expect(context.sources.status == "full_source_index")
        let files = try String(contentsOf: context.sources.sourceFilesURL, encoding: .utf8)
        #expect(!files.contains("go/proto/gen/service.pb.go"))
        #expect(files.contains("Proto/service.proto"))
        #expect(files.contains("service-protos.version"))
        #expect(files.contains("Sources/handwritten.pb.go"))
        let prompt = try fixture.prompt(context)
        #expect(prompt.contains("unrestricted"))
        #expect(prompt.contains("not a filesystem sandbox"))
        #expect(prompt.contains("custom review prompt"))
        #expect(!prompt.contains("git -C"))
    }

    @Test(arguments: [false, true])
    func `generated-only changes retain passing or failed run evidence and AI requests`(
        failed: Bool
    ) throws {
        let fixture = try SourceReviewFixture()
        try fixture.write("GENERATED_BODY_SENTINEL\n", to: "go/proto/gen/service.pb.go")
        try fixture.commit()
        let attempt = fixture.runURL.appendingPathComponent("attempts/local/0001")
        try FileManager.default.createDirectory(at: attempt, withIntermediateDirectories: true)
        try "{\"exitStatus\":\(failed ? 1 : 0)}".write(
            to: attempt.appendingPathComponent("attempt.json"),
            atomically: true,
            encoding: .utf8
        )
        // Only a reused local run directory can already hold a review; CI starts empty.
        let prior = fixture.runURL.appendingPathComponent("review.fixture")
        try FileManager.default.createDirectory(at: prior, withIntermediateDirectories: true)
        let priorURL = prior.appendingPathComponent("prior.md")
        try "PRIOR_ACTIONABLE_FINDING".write(to: priorURL, atomically: true, encoding: .utf8)
        let context = try fixture.prepare(diff: true)
        #expect(context.sources.status == "generated_only")
        #expect(try Data(contentsOf: context.sources.patchURL).isEmpty)
        #expect((try fixture.manifest()["index"] as? [Any])?.isEmpty == true)
        let prompt = try fixture.prompt(context)
        #expect(prompt.contains("exitStatus=`\(failed ? 1 : 0)`"))
        #expect(prompt.contains("Keep qualitative failure diagnosis"))
        #expect(prompt.contains("does not make a failed run pass"))
        #expect(prompt.contains("Findings from earlier CI runs are not supplied"))
        #expect(!prompt.contains("prior actionable findings"))
        let evidenceScope = try #require(try fixture.manifest()["evidence_scope"] as? String)
        #expect(!evidenceScope.contains("prior-finding"))
        #expect(try String(contentsOf: priorURL, encoding: .utf8) == "PRIOR_ACTIONABLE_FINDING")
    }

    @Test(arguments: [false, true])
    func `boundary rename preparation retains both path sides`(toGenerated: Bool) throws {
        let fixture = try SourceReviewFixture()
        let source = toGenerated ? "Sources/handwritten.pb.go" : "go/proto/gen/service.pb.go"
        let target = toGenerated ? "go/proto/gen/renamed.pb.go" : "Sources/renamed.go"
        _ = try fixture.git(["mv", source, target])
        try fixture.commit()
        let context = try fixture.prepare(diff: true)
        let names = try String(contentsOf: context.sources.changedFilesURL, encoding: .utf8)
        #expect(names.contains(source))
        #expect(names.contains(target))
        #expect(context.sources.status == "source_diff")
    }

    @Test
    func `a non-UTF-8 text file is replaced and recorded without failing the range`() throws {
        let fixture = try SourceReviewFixture()
        try fixture.write("SOURCE_CHANGE\n", to: "Proto/service.proto")
        // Git has no NUL byte to see here, so it emits these raw bytes as a text diff.
        try fixture.write(Data([0x63, 0x61, 0x66, 0xE9, 0x0A]), to: "Docs/latin1.txt")
        try fixture.commit()
        let context = try fixture.prepare(diff: true)
        #expect(context.sources.status == "source_diff")
        let patch = try String(contentsOf: context.sources.patchURL, encoding: .utf8)
        #expect(patch.contains("+caf\u{FFFD}\n"))
        #expect(patch.contains("SOURCE_CHANGE"))
        let coverage = try #require(try fixture.manifest()["coverage"] as? [String: Any])
        #expect((coverage["non_utf8_files"] as? [String]) == ["Docs/latin1.txt"])
        let names = try String(contentsOf: context.sources.changedFilesURL, encoding: .utf8)
        #expect(names.contains("Docs/latin1.txt"))
    }

    @Test(arguments: ["review_sources.py", "review_diff.py"])
    func `missing canonical dependency fails without fallback`(missing: String) throws {
        let fixture = try SourceReviewFixture(missing: missing)
        let error = try #require(throws: ReviewSourcePreparationError.self) {
            try fixture.prepare(diff: false)
        }
        #expect(error.description.contains(".github/scripts/\(missing)"))
        #expect(error.description.contains("--source-policy-ref"))
        #expect(error.description.contains("fatal: path"))
        #expect(try fixture.manifest()["status"] as? String == "error")
        #expect(
            !FileManager.default.fileExists(
                atPath: fixture.runURL.appendingPathComponent("git-diff-source.diff").path
            )
        )
    }

    @Test
    func `malformed complete patch fails and removes stale prepared input`() throws {
        let fixture = try SourceReviewFixture()
        try fixture.write("SOURCE_CHANGE\n", to: "Proto/service.proto")
        try fixture.commit()
        let stale = fixture.runURL.appendingPathComponent("git-diff-source.diff")
        try "STALE_PREPARED_INPUT".write(to: stale, atomically: true, encoding: .utf8)
        let mock = fixture.rootURL.appendingPathComponent("malformed.py")
        try """
        import importlib.util
        import sys
        spec = importlib.util.spec_from_file_location('adapter', \(String(reflecting: sourceReviewAdapter.path)))
        adapter = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(adapter)
        original = adapter.git
        def malformed(repo, *args):
            if 'diff' in args:
                return b'diff --git a/source.go b/source.go\\n--- a/source.go\\n+++ b/source.go\\n@@ -0,0 +1,2 @@\\n+INCOMPLETE\\n'
            return original(repo, *args)
        adapter.git = malformed
        sys.exit(adapter.main())
        """.write(to: mock, atomically: true, encoding: .utf8)
        #expect(throws: ReviewSourcePreparationError.self) {
            try fixture.prepare(diff: true, adapter: mock)
        }
        #expect(try fixture.manifest()["status"] as? String == "error")
        #expect(!FileManager.default.fileExists(atPath: stale.path))
    }

    @Test
    func `an unresolvable range reports Git's reason and what to check`() throws {
        let fixture = try SourceReviewFixture()
        let error = try #require(throws: ReviewSourcePreparationError.self) {
            try fixture.prepare(range: "nosuchref...HEAD")
        }
        #expect(error.description.contains("nosuchref...HEAD"))
        #expect(error.description.contains("enough history"))
        #expect(error.description.contains("fatal: Needed a single revision"))
        #expect(!error.description.contains("returned non-zero exit status"))
    }

    @Test
    func `an unresolvable policy ref names its remedy`() throws {
        let fixture = try SourceReviewFixture()
        let error = try #require(throws: ReviewSourcePreparationError.self) {
            try fixture.prepare(range: nil, policyRef: "origin/nosuchref")
        }
        #expect(error.description.contains("origin/nosuchref"))
        #expect(error.description.contains("--source-policy-ref"))
        #expect(error.description.contains("fatal: Needed a single revision"))
    }

    @Test
    func `a preparation failure is not reported as a command-line mistake`() throws {
        let fixture = try SourceReviewFixture(missing: "review_sources.py")
        let error = try #require(throws: (any Error).self) { try fixture.prepare(diff: false) }
        let message = SwiftE2ETestingCLI.fullMessage(for: error)
        #expect(message.contains("--source-policy-ref"))
        #expect(!message.contains("Usage:"))
        #expect(!SwiftE2ETestingCLI.exitCode(for: error).isSuccess)
    }
}

private let sourceReviewRepo = URL(fileURLWithPath: #filePath)
    .deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
    .deletingLastPathComponent().deletingLastPathComponent()
private let sourceReviewAdapter = sourceReviewRepo.appendingPathComponent(
    ".github/scripts/e2e_review_sources.py"
)

private final class SourceReviewFixture {
    let rootURL: URL
    let repoURL: URL
    let runURL: URL
    var base = ""

    init(missing: String? = nil) throws {
        rootURL = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        repoURL = rootURL.appendingPathComponent("repo")
        runURL = rootURL.appendingPathComponent("run")
        try FileManager.default.createDirectory(at: repoURL, withIntermediateDirectories: true)
        try FileManager.default.createDirectory(at: runURL, withIntermediateDirectories: true)
        _ = try git(["init", "-q"])
        for name in ["review_sources.py", "review_diff.py"] where name != missing {
            try write(
                String(
                    contentsOf: sourceReviewRepo.appendingPathComponent(".github/scripts/\(name)"),
                    encoding: .utf8
                ),
                to: ".github/scripts/\(name)"
            )
        }
        for path in [
            "go/proto/gen/service.pb.go", "Proto/service.proto", "go/proto/service-protos.version",
            "go/scripts/generate-proto.sh", "Sources/handwritten.pb.go",
        ] {
            try write("original\n", to: path)
        }
        try write(
            "// AI: Keep qualitative failure diagnosis\nfunc test() {}\n",
            to: "Tests/SpecTests.swift"
        )
        try commit()
        base = try git(["rev-parse", "HEAD"])
    }

    deinit { try? FileManager.default.removeItem(at: rootURL) }

    func write(_ text: String, to path: String) throws {
        let url = repoURL.appendingPathComponent(path)
        try FileManager.default.createDirectory(
            at: url.deletingLastPathComponent(),
            withIntermediateDirectories: true
        )
        try text.write(to: url, atomically: true, encoding: .utf8)
    }

    func commit() throws {
        _ = try git(["add", "-A"])
        _ = try git([
            "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm",
            "fixture",
        ])
    }

    func write(_ data: Data, to path: String) throws {
        let url = repoURL.appendingPathComponent(path)
        try FileManager.default.createDirectory(
            at: url.deletingLastPathComponent(),
            withIntermediateDirectories: true
        )
        try data.write(to: url)
    }

    func git(_ arguments: [String]) throws -> String {
        let p = Process()
        let output = Pipe()
        p.executableURL = URL(fileURLWithPath: "/usr/bin/env")
        p.arguments = ["git", "-C", repoURL.path] + arguments
        p.standardOutput = output
        p.standardError = FileHandle.nullDevice
        try p.run()
        let data = output.fileHandleForReading.readDataToEndOfFile()
        p.waitUntilExit()
        guard p.terminationStatus == 0 else {
            throw ValidationError("Fixture Git failed: \(arguments)")
        }
        return String(decoding: data, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
    }

    func prepare(diff: Bool, adapter: URL = sourceReviewAdapter) throws -> ReviewContext {
        try prepare(range: diff ? "\(base)...HEAD" : nil, adapter: adapter)
    }

    func prepare(
        range: String?,
        policyRef: String? = nil,
        adapter: URL = sourceReviewAdapter
    ) throws -> ReviewContext {
        try ReviewMode(diff: range).prepareContext(
            runURL: runURL,
            repoURL: repoURL,
            adapterURL: adapter,
            policyRef: policyRef ?? base
        )
    }

    func manifest() throws -> [String: Any] {
        try #require(
            JSONSerialization.jsonObject(
                with: Data(contentsOf: runURL.appendingPathComponent("git-diff-source-input.json"))
            ) as? [String: Any]
        )
    }

    func prompt(_ context: ReviewContext) throws -> String {
        try runReviewPrompt(
            basePrompt: "custom review prompt",
            repoURL: repoURL,
            packageURL: repoURL.appendingPathComponent("swift/WendyE2ETests"),
            testsURL: repoURL.appendingPathComponent("Tests"),
            runURL: runURL,
            context: context,
            overview: writeRunOverview(in: runURL),
            reviewer: "fixture",
            reviewDirectoryName: "review.fixture",
            overwrite: false
        )
    }
}
