import ArgumentParser
import Foundation
import Testing

@testable import SwiftE2ETestingCLI

private func analysisScript(_ product: URL, named name: String) throws -> String {
    let text = try String(
        contentsOf: product.appendingPathComponent(".github/workflows/swift-e2e-tests.yml"),
        encoding: .utf8
    )
    let step = try #require(
        text.components(separatedBy: "      - name: " + name + "\n").dropFirst().first
    )
    .components(separatedBy: "\n      - name:")[0]
    let body = try #require(step.components(separatedBy: "        run: |\n").dropFirst().first)
    return body.components(separatedBy: "\n").map {
        String($0.dropFirst(min(10, $0.prefix(while: { $0 == " " }).count)))
    }.joined(separator: "\n")
}

@Suite(.serialized)
struct `trusted tooling and tested source` {
    @Test
    func `exact preparation pins a synthetic merge and never contaminates trusted Swift targets`()
        throws
    {
        let f = try GoCoverageFixture(build: false)
        let package = "swift/WendyE2ETests"
        let trustedManifest =
            "// swift-tools-version: 6.1\nimport PackageDescription\nlet package = Package(name: \"TrustFixture\", targets: [.testTarget(name: \"Specs\", path: \"Tests/WendyE2ETests\")])\n"
        try f.write(package + "/Package.swift", trustedManifest)
        try f.write(package + "/Tests/WendyE2ETests/BaseTests.swift", "func trustedBase() {}\n")
        let common = try f.commit()
        _ = try f.git(["checkout", "-b", "feature"])
        try f.write(
            package + "/Package.swift",
            "// swift-tools-version: 6.1\nimport PackageDescription\nfatalError(\"TESTED_PACKAGE_MUST_NOT_EXECUTE\")\n"
        )
        try f.write(
            package + "/Tests/WendyE2ETests/inert.c",
            "/* inert mixed-language control */\n"
        )
        try f.write(".gitattributes", "*.go filter=tripwire\n")
        try f.write(
            "go/internal/sample/branch.go",
            GoCoverageFixture.branch.replacingOccurrences(of: "return 2", with: "return 3")
        )
        let feature = try f.commit()
        _ = try f.git(["checkout", "-b", "base", common])
        try f.write("go/internal/sample/base.go", "package sample\nconst BaseOnly = 1\n")
        let base = try f.commit()
        _ = try f.git([
            "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "merge",
            "--no-ff", "-m", "synthetic merge", feature,
        ])
        let tested = try f.git(["rev-parse", "HEAD"])
        #expect(tested != base && tested != feature && base != feature)
        _ = try f.git(["update-ref", "refs/tested/merge", tested])
        try f.buildBinaries()
        _ = try f.git(["checkout", "--detach", base])
        let marker = f.repo.appendingPathComponent("executed-untrusted-code")
        let hooks = f.repo.appendingPathComponent("private-hooks")
        for name in ["post-checkout", "post-index-change"] {
            try f.write("private-hooks/" + name, "#!/bin/sh\ntouch '" + marker.path + "'\n")
            try FileManager.default.setAttributes(
                [.posixPermissions: 0o755],
                ofItemAtPath: hooks.appendingPathComponent(name).path
            )
        }
        _ = try f.git(["config", "core.hooksPath", hooks.path])
        _ = try f.git(["config", "filter.tripwire.smudge", "touch '" + marker.path + "'"])
        _ = try f.git(["config", "filter.tripwire.clean", "touch '" + marker.path + "'"])
        let host = f.repo.appendingPathComponent("host")
        try FileManager.default.createDirectory(at: host, withIntermediateDirectories: true)
        let data = host.appendingPathComponent("e2e-tested-source")
        defer { _ = try? CoverageIO.execute(["chmod", "-R", "u+w", data.path]) }
        let binary = f.product.appendingPathComponent(
            "swift/WendyE2ETests/.build/debug/swift-e2e-testing"
        )
        try f.write(
            "fake-tool/swift",
            "#!/bin/sh\n[ \"$1\" = run ] && [ \"$2\" = --package-path ] && [ \"$3\" = \"$GITHUB_WORKSPACE/swift/WendyE2ETests\" ] && [ \"$4\" = swift-e2e-testing ] || exit 91\nshift 4\nexec '"
                + binary.path + "' \"$@\"\n"
        )
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o755],
            ofItemAtPath: f.repo.appendingPathComponent("fake-tool/swift").path
        )
        var environment = ProcessInfo.processInfo.environment
        environment["TESTED_SHA"] = tested
        environment["PR_BASE_SHA"] = base
        environment["PR_HEAD_SHA"] = feature
        environment["GITHUB_WORKSPACE"] = f.repo.path
        environment["RUNNER_TEMP"] = host.path
        environment["GITHUB_OUTPUT"] = host.appendingPathComponent("outputs").path
        environment["PATH"] =
            f.repo.appendingPathComponent("fake-tool").path + ":" + environment["PATH", default: ""]
        let result = try CoverageIO.execute(
            [
                "/bin/bash", "-c",
                analysisScript(f.product, named: "Prepare tested source as read-only review data"),
            ],
            cwd: f.repo,
            environment: environment
        )
        #expect(result.status == 0, Comment(rawValue: result.error))
        #expect(!CoverageIO.exists(marker))
        #expect(
            try CoverageIO.command(["git", "rev-parse", "HEAD"], cwd: data).trimmingCharacters(
                in: .whitespacesAndNewlines
            ) == tested
        )
        #expect(try CoverageIO.sourceID(repo: data) == f.source["sourceID"] as? String)
        #expect(
            try String(
                contentsOf: f.repo.appendingPathComponent(package + "/Package.swift"),
                encoding: .utf8
            ) == trustedManifest
        )
        #expect(
            !CoverageIO.exists(
                f.repo.appendingPathComponent(package + "/Tests/WendyE2ETests/inert.c")
            )
        )
        #expect(
            CoverageIO.exists(data.appendingPathComponent(package + "/Tests/WendyE2ETests/inert.c"))
        )
        let swift = try CoverageIO.executable(
            "swift",
            environment: ProcessInfo.processInfo.environment
        )
        let described = try CoverageIO.execute([
            swift.path, "package", "--package-path", f.repo.appendingPathComponent(package).path,
            "describe", "--type", "json",
        ])
        #expect(described.status == 0, Comment(rawValue: described.error))
        let workflow = try String(
            contentsOf: f.product.appendingPathComponent(".github/workflows/swift-e2e-tests.yml"),
            encoding: .utf8
        )
        #expect(workflow.contains("TESTED_SHA: ${{ github.sha }}"))
        #expect(!workflow.contains("TESTED_SHA: ${{ github.event.pull_request.head.sha"))
    }

    @Test func `raw materialization ignores CRLF attributes and omits symbolic links`() throws {
        let f = try GoCoverageFixture(build: false)
        try f.write(".gitattributes", "*.go text eol=crlf\n")
        try FileManager.default.createSymbolicLink(
            atPath: f.repo.appendingPathComponent("not-traversed").path,
            withDestinationPath: "/does-not-exist"
        )
        let head = try f.commit()
        let destination = f.repo.appendingPathComponent("run-source")
        defer { _ = try? CoverageIO.execute(["chmod", "-R", "u+w", destination.path]) }
        try CoverageIO.materializeSource(repo: f.repo, revision: head, into: destination)
        let expected = try CoverageIO.execute(
            ["git", "cat-file", "blob", head + ":go/internal/sample/branch.go"],
            cwd: f.repo
        ).data
        #expect(
            try Data(contentsOf: destination.appendingPathComponent("go/internal/sample/branch.go"))
                == expected
        )
        #expect(!expected.contains(13))
        #expect(!CoverageIO.exists(destination.appendingPathComponent("not-traversed")))
        #expect(throws: (any Error).self) {
            try CoverageIO.materializeSource(
                repo: f.repo,
                revision: "HEAD",
                into: f.repo.appendingPathComponent("run-invalid")
            )
        }
    }

    @Test
    func `review validates selected source but starts the offline harness from trusted tooling`()
        throws
    {
        let f = try GoCoverageFixture(build: false)
        try f.write(
            "swift/WendyE2ETests/Package.swift",
            "// swift-tools-version: 6.1\nimport PackageDescription\nfatalError(\"TESTED_PACKAGE_MUST_NOT_EXECUTE\")\n"
        )
        try f.write(
            "swift/WendyE2ETests/Tests/WendyE2ETests/ExampleTests.swift",
            "@Suite\nstruct `source suite` {\n@Test\nfunc `behavior`() {\n// AI: TESTED_SOURCE_SENTINEL\n}\n}\n"
        )
        _ = try f.commit()
        try f.buildBinaries()
        try f.attempt()
        let directory = "observations/suite/test"
        try f.invoke(f.native(directory + "/coverage/go"), "left")
        try f.records([f.record("test", directory: directory)])
        let input = f.repo.appendingPathComponent("run-input/swift.review.local.0001")
        try FileManager.default.createDirectory(
            at: input.deletingLastPathComponent(),
            withIntermediateDirectories: true
        )
        try FileManager.default.copyItem(at: f.run, to: input)
        let output = f.repo.appendingPathComponent("run-output")
        var aggregate = try AggregateCommand.parse([
            "--package-dir", f.product.appendingPathComponent("swift/WendyE2ETests").path, "--repo",
            f.repo.path, "--output-dir", output.path, input.path,
        ])
        try aggregate.run()
        let run = output.appendingPathComponent("swift.review")
        try validateRecordedReviewRevision(runURL: run, repoURL: f.repo)
        #expect(throws: (any Error).self) {
            try validateRecordedReviewRevision(runURL: run, repoURL: f.product)
        }
        let fake = f.repo.appendingPathComponent("fake-provider/claude")
        try f.write(
            "fake-provider/claude",
            """
            #!/bin/sh
            if [ "$1" = auth ]; then exit 1; fi
            printf '%s\\n' "$PWD" "$WENDY_E2E_REVIEW_REPO_DIR" > "$FIXTURE_RECORD"
            cp "$WENDY_E2E_REVIEW_PROMPT" "$FIXTURE_PROMPT"
            """
        )
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: fake.path)
        let home = f.repo.appendingPathComponent("empty-home")
        try FileManager.default.createDirectory(at: home, withIntermediateDirectories: true)
        let record = f.repo.appendingPathComponent("provider-record")
        let prompt = f.repo.appendingPathComponent("provider-prompt")
        let environment = [
            "PATH": fake.deletingLastPathComponent().path + ":/usr/bin:/bin", "HOME": home.path,
            "TMPDIR": home.path, "ANTHROPIC_API_KEY": "synthetic-not-a-secret",
            "FIXTURE_RECORD": record.path, "FIXTURE_PROMPT": prompt.path,
        ]
        let binary = f.product.appendingPathComponent(
            "swift/WendyE2ETests/.build/debug/swift-e2e-testing"
        )
        let reviewed = try CoverageIO.execute(
            [
                binary.path, "review", "--package-dir",
                f.product.appendingPathComponent("swift/WendyE2ETests").path, "--repo", f.repo.path,
                "--run-dir", run.path, "--harness", "claude",
            ],
            environment: environment
        )
        #expect(reviewed.status == 0, Comment(rawValue: reviewed.error + reviewed.text))
        #expect(
            try String(contentsOf: record, encoding: .utf8).components(separatedBy: "\n").prefix(2)
                == [f.product.path, f.repo.path]
        )
        let instructions = try String(contentsOf: prompt, encoding: .utf8)
        #expect(instructions.contains("TESTED_SOURCE_SENTINEL"))
        #expect(instructions.contains("Do not execute scripts, package manifests, hooks"))
    }
}
