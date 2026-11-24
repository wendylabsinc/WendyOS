import Foundation
import Testing

@testable import SwiftE2ETestingCLI

@Suite(.serialized)
struct `native Go coverage` {
    @Test func `real invocations union statements once and omit generated source and normal hooks`()
        throws
    {
        let f = try GoCoverageFixture()
        let left = try f.native("observations/first/test/coverage/go")
        let right = try f.native("observations/different/second/coverage/go")
        #expect(try f.invoke(left, "left") == 0)
        #expect(try f.invoke(left, "left") == 0)
        #expect(try f.invoke(right, "right") == 0)
        #expect(try GoCoverage.checkCLICounters(left) == 2)
        let a = try GoCoverage.profile(
            [left],
            destination: f.run.appendingPathComponent("a"),
            source: f.source
        )
        let b = try GoCoverage.profile(
            [right],
            destination: f.run.appendingPathComponent("b"),
            source: f.source
        )
        let union = try GoCoverage.profile(
            [left, right],
            destination: f.run.appendingPathComponent("union"),
            source: f.source
        )
        let package = "example.test/coverage/go/internal/sample"
        let pa = (a["packages"] as! [String: [String: Int]])[package]!
        let pb = (b["packages"] as! [String: [String: Int]])[package]!
        let combined = (union["packages"] as! [String: [String: Int]])[package]!
        #expect(combined["covered"]! > pa["covered"]!)
        #expect(combined["covered"]! > pb["covered"]!)
        #expect(combined["statements"] == 3)
        #expect(try f.invoke(left, "fail") == 7)
        #expect(try GoCoverage.checkCLICounters(left) == 3)
        #expect(union["statements"] as? Int == a["statements"] as? Int)
        #expect(
            try !String(
                contentsOf: f.run.appendingPathComponent("union/coverage.out"),
                encoding: .utf8
            ).contains("generated.go")
        )
        for role in ["wendy", "wendy-agent"] {
            let files = try CoverageIO.command(
                ["go", "list", "-f={{.GoFiles}}", "./go/cmd/" + role],
                cwd: f.repo
            )
            #expect(!files.contains("coverage_e2e.go"))
        }
        let normal = f.repo.appendingPathComponent("binary-normal")
        _ = try CoverageIO.command(
            ["go", "build", "-o", normal.path, "./go/cmd/wendy"],
            cwd: f.repo
        )
        let untouched = try f.native("ordinary")
        var env = ProcessInfo.processInfo.environment
        env["GOCOVERDIR"] = untouched.path
        #expect(try CoverageIO.execute([normal.path, "left"], environment: env).status == 0)
        #expect(try CoverageIO.children(untouched).isEmpty)
    }

    @Test
    func
        `actual Agent hook resets intervals and merges shared code across binaries without losing metadata`()
        throws
    {
        let f = try GoCoverageFixture(managed: true)
        let first = try f.native("observations/agent/first/coverage/go")
        let mixed = try f.native("observations/mixed/second/coverage/go")
        let startup = try f.native("coverage/bootstrap")
        let controlRoot = URL(fileURLWithPath: "/tmp/e2ecov-" + UUID().uuidString.prefix(8))
        try FileManager.default.createDirectory(at: controlRoot, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: controlRoot) }
        let socket = controlRoot.appendingPathComponent("a.sock")
        let process = Process()
        let stdin = Pipe()
        let stdout = Pipe()
        process.executableURL = f.agent
        process.standardInput = stdin
        process.standardOutput = stdout
        var env = ProcessInfo.processInfo.environment
        env["WENDY_E2E_COVERAGE_SOCKET"] = socket.path
        env["GOCOVERDIR"] = startup.path
        process.environment = env
        try process.run()
        defer {
            try? stdin.fileHandleForWriting.close()
            if process.isRunning { process.terminate() }
            process.waitUntilExit()
        }
        #expect(try GoCoverageFixture.line(stdout.fileHandleForReading) == "ready")
        #expect(
            (try FileManager.default.attributesOfItem(atPath: socket.path)[.posixPermissions]
                as? NSNumber)?.intValue == 0o600
        )
        let pid = try GoCoverage.agentControl(socket.path, operation: "reset")
        try stdin.fileHandleForWriting.write(contentsOf: Data("left\n".utf8))
        #expect(try GoCoverageFixture.line(stdout.fileHandleForReading) == "1")
        #expect(
            try GoCoverage.agentControl(socket.path, operation: "export", directory: first.path)
                == pid
        )
        #expect(try GoCoverage.agentControl(socket.path, operation: "reset") == pid)
        try stdin.fileHandleForWriting.write(contentsOf: Data("right\n".utf8))
        #expect(try GoCoverageFixture.line(stdout.fileHandleForReading) == "2")
        _ = try GoCoverage.agentControl(socket.path, operation: "export", directory: mixed.path)
        let a = try GoCoverage.profile(
            [first],
            destination: f.run.appendingPathComponent("a"),
            source: f.source
        )
        let b = try GoCoverage.profile(
            [mixed],
            destination: f.run.appendingPathComponent("b"),
            source: f.source
        )
        #expect(try f.invoke(mixed, "left") == 0)
        let union = try GoCoverage.profile(
            [mixed],
            destination: f.run.appendingPathComponent("union"),
            source: f.source
        )
        let package = "example.test/coverage/go/internal/sample"
        #expect((a["packages"] as! [String: [String: Int]])[package]!["covered"] == 2)
        #expect((b["packages"] as! [String: [String: Int]])[package]!["covered"] == 2)
        #expect(
            (union["packages"] as! [String: [String: Int]])[package] == [
                "statements": 3, "covered": 3,
            ]
        )
        let metadata = try CoverageIO.files(mixed, prefix: "covmeta.")
        #expect(metadata.count == 2)
        for path in metadata {
            let bytes = try Data(contentsOf: path)
            try FileManager.default.removeItem(at: path)
            #expect(throws: (any Error).self) {
                try GoCoverage.profile(
                    [mixed],
                    destination: f.run.appendingPathComponent("bad"),
                    source: f.source
                )
            }
            try bytes.write(to: path)
        }
        let hash = String(metadata[0].lastPathComponent.dropFirst(8))
        for path in try CoverageIO.files(mixed, prefix: "covcounters." + hash + ".") {
            try FileManager.default.removeItem(at: path)
        }
        #expect(throws: (any Error).self) {
            try GoCoverage.profile(
                [mixed],
                destination: f.run.appendingPathComponent("bad"),
                source: f.source
            )
        }
    }

    @Test
    func
        `runner preserves original failure repeated calls and canonical receipts and interrupted recovery`()
        throws
    {
        let f = try GoCoverageFixture()
        try f.attempt(exit: 9)
        let alias = f.repo.appendingPathComponent("run-alias")
        try FileManager.default.createSymbolicLink(at: alias, withDestinationURL: f.run)
        var env = try f.fakeSwift(
            body: """
                native="$TEST_ROOT/observations/suite/test/coverage/go"
                mkdir -p "$native"
                printf '%s' "$RECEIPT_ROOT/observations/suite/test" > "$WENDY_E2E_COVERAGE_RECEIPT"
                GOCOVERDIR="$native" "$FIXTURE_CLI" left
                GOCOVERDIR="$native" "$FIXTURE_CLI" fail
                printf '<testsuite><testcase name="fixture"><failure/></testcase></testsuite>' > "${!#}"
                exit 9
                """
        )
        env["RECEIPT_ROOT"] = alias.path
        #expect(
            try GoCoverage.runTests(
                run: alias,
                filters: ["fixture"],
                control: nil,
                environment: env
            ) == 9
        )
        #expect(try GoCoverage(repo: f.repo, run: alias).finishCapture(control: nil) == 0)
        #expect(try GoCoverage(repo: f.repo, run: alias).deriveAttempt() == 0)
        var records =
            try JSONSerialization.jsonObject(
                with: Data(contentsOf: f.run.appendingPathComponent("coverage/tests.json"))
            ) as! [[String: Any]]
        #expect(records[0]["directory"] as? String == "observations/suite/test")
        #expect(records[0]["cliInvocations"] as? Int == 2)
        #expect(records[0]["testExitStatus"] as? Int == 9)
        #expect(
            try CoverageIO.read(f.run.appendingPathComponent("attempt.json"))["exitStatus"] as? Int
                == 9
        )
        records[0]["status"] = "incomplete"
        records[0].removeValue(forKey: "directory")
        try f.records(records)
        #expect(try GoCoverage(repo: f.repo, run: alias).finishCapture(control: nil) == 1)
        #expect(try GoCoverage(repo: f.repo, run: alias).deriveAttempt() == 1)
        let partial = try CoverageIO.read(f.run.appendingPathComponent("coverage/summary.json"))
        #expect(partial["status"] as? String == "incomplete")
        #expect((partial["summary"] as? [String: Any])?["statements"] as? Int ?? 0 > 0)
        records[0].removeValue(forKey: "directory")
        try f.records(records)
        try f.repo.appendingPathComponent("outside").path.write(
            to: f.run.appendingPathComponent("coverage/current-test.txt"),
            atomically: true,
            encoding: .utf8
        )
        #expect(try GoCoverage(repo: f.repo, run: f.run).finishCapture(control: nil) == 1)
    }

    @Test func `zero tests skipped tests and no Go observations remain honest no data`() throws {
        let f = try GoCoverageFixture()
        try f.attempt()
        for state in ["empty", "skipped", "no-go"] {
            let xml =
                state == "skipped"
                ? "<testsuite><testcase><skipped/></testcase></testsuite>"
                : "<testsuite><testcase/></testsuite>"
            let env = try f.fakeSwift(
                list: state == "empty" ? "" : "fixture/test()",
                body: "printf '%s' '\(xml)' > \"${!#}\""
            )
            #expect(
                try GoCoverage.runTests(
                    run: f.run,
                    filters: ["fixture"],
                    control: nil,
                    environment: env
                ) == 0
            )
            #expect(try GoCoverage(repo: f.repo, run: f.run).finishCapture(control: nil) == 0)
            #expect(try GoCoverage(repo: f.repo, run: f.run).deriveAttempt() == 0)
            let summary = try CoverageIO.read(f.run.appendingPathComponent("coverage/summary.json"))
            #expect(summary["status"] as? String == "no-data")
            #expect(summary["summary"] == nil)
        }
        let bootstrap = try f.native("coverage/bootstrap")
        try f.invoke(bootstrap, "left")
        try GoCoverage.pack(run: f.run)
        #expect(throws: (any Error).self) {
            try GoCoverage(repo: f.repo, run: f.run).deriveAttempt()
        }
        _ = try CoverageIO.command([
            "tar", "-xzf", f.run.appendingPathComponent("coverage/go-native.tar.gz").path, "-C",
            f.run.path,
        ])
        #expect(try GoCoverage(repo: f.repo, run: f.run).deriveAttempt() == 0)
    }

    @Test func `missing killed counters source changes and timeouts are incomplete`() throws {
        let f = try GoCoverageFixture()
        try f.attempt()
        let native = try f.native("observations/suite/test/coverage/go")
        try f.invoke(native, "left")
        let process = Process()
        let stdout = Pipe()
        process.executableURL = f.cli
        process.arguments = ["hang"]
        process.standardOutput = stdout
        var env = ProcessInfo.processInfo.environment
        env["GOCOVERDIR"] = native.path
        process.environment = env
        try process.run()
        #expect(try GoCoverageFixture.line(stdout.fileHandleForReading) == "2")
        process.terminate()
        process.waitUntilExit()
        #expect(throws: (any Error).self) { try GoCoverage.checkCLICounters(native) }
        try f.records([f.record("test", directory: "observations/suite/test")])
        // Derivation failure is separate from the captured execution outcome.
        #expect(try GoCoverage(repo: f.repo, run: f.run).deriveAttempt() == 1)
        #expect(
            try CoverageIO.read(f.run.appendingPathComponent("attempt.json"))["exitStatus"] as? Int
                == 0
        )
        let before = try CoverageIO.sourceID(repo: f.repo)
        try f.write("deleted.go", "package fixture\n")
        _ = try f.git(["add", "deleted.go"])
        let present = try CoverageIO.sourceID(repo: f.repo)
        try FileManager.default.removeItem(at: f.repo.appendingPathComponent("deleted.go"))
        #expect(try CoverageIO.sourceID(repo: f.repo) != present)
        #expect(try CoverageIO.sourceID(repo: f.repo) != before)
        try f.records([])
        #expect(try GoCoverage(repo: f.repo, run: f.run).deriveAttempt() == 1)
        _ = try f.git(["rm", "--cached", "-f", "deleted.go"])
        try f.write("run/timeout.json", "{}")
        #expect(try GoCoverage(repo: f.repo, run: f.run).deriveAttempt() == 1)
    }

    @Test func `pack retains exact paths bytes totals and partial failure evidence`() throws {
        let f = try GoCoverageFixture()
        try f.attempt()
        var records: [[String: Any]] = []
        for (i, argument) in ["left", "right"].enumerated() {
            let directory = "observations/suite/test-\(i)"
            let native = try f.native(directory + "/coverage/go")
            try f.invoke(native, argument)
            records.append(try f.record(String(i), directory: directory))
        }
        try f.records(records)
        #expect(try GoCoverage(repo: f.repo, run: f.run).deriveAttempt(perTest: true) == 0)
        let paths = try GoCoverage.nativeFiles(run: f.run)
        let original = try Dictionary(
            uniqueKeysWithValues: paths.map {
                (try CoverageIO.relative($0, to: f.run), try Data(contentsOf: $0))
            }
        )
        let profile = try Data(contentsOf: f.run.appendingPathComponent("coverage/coverage.out"))
        let attempt = try Data(contentsOf: f.run.appendingPathComponent("attempt.json"))
        try GoCoverage.pack(run: f.run)
        #expect(!CoverageIO.exists(f.cli))
        #expect(try Data(contentsOf: f.run.appendingPathComponent("attempt.json")) == attempt)
        #expect(
            try Data(contentsOf: f.run.appendingPathComponent("coverage/coverage.out")) == profile
        )
        #expect(throws: (any Error).self) {
            try GoCoverage(repo: f.repo, run: f.run).deriveAttempt()
        }
        try GoCoverage.pack(run: f.run)
        let archive = f.run.appendingPathComponent("coverage/go-native.tar.gz")
        let listing = try CoverageIO.command(["tar", "-tvzf", archive.path])
        #expect(listing.contains("link to") || listing.contains("hrw"))
        _ = try CoverageIO.command(["tar", "-xzf", archive.path, "-C", f.run.path])
        for (path, bytes) in original {
            #expect(try Data(contentsOf: f.run.appendingPathComponent(path)) == bytes)
        }
        #expect(try GoCoverage(repo: f.repo, run: f.run).deriveAttempt(perTest: true) == 0)
        #expect(
            try Data(contentsOf: f.run.appendingPathComponent("coverage/coverage.out")) == profile
        )
        // Counter-only interrupted interval survives packaging and stays incomplete after recovery.
        let partial = f.repo.appendingPathComponent("run-partial")
        try CoverageIO.write(f.source, to: partial.appendingPathComponent("coverage/source.json"))
        try CoverageIO.write(
            [
                [
                    "test": "partial", "status": "incomplete", "sourceID": f.source["sourceID"]!,
                    "directory": "observations/suite/partial",
                ]
            ],
            to: partial.appendingPathComponent("coverage/tests.json")
        )
        try CoverageIO.write(["exitStatus": 9], to: partial.appendingPathComponent("attempt.json"))
        let missing = partial.appendingPathComponent(
            "observations/suite/partial/coverage/go/covcounters.missing.1.0"
        )
        try FileManager.default.createDirectory(
            at: missing.deletingLastPathComponent(),
            withIntermediateDirectories: true
        )
        try Data("partial evidence".utf8).write(to: missing)
        try GoCoverage.pack(run: partial)
        _ = try CoverageIO.command([
            "tar", "-xzf", partial.appendingPathComponent("coverage/go-native.tar.gz").path, "-C",
            partial.path,
        ])
        #expect(try GoCoverage(repo: f.repo, run: partial).deriveAttempt() == 1)
        #expect(
            try CoverageIO.read(partial.appendingPathComponent("attempt.json"))["exitStatus"]
                as? Int == 9
        )
    }
}

@Suite
struct `coverage fixture child environments` {
    @Test func `actual four product fixture sanitizers retain coverage only when enabled`() throws {
        let f = try GoCoverageFixture()
        let normal = f.repo.appendingPathComponent("binary-normal")
        _ = try CoverageIO.command(
            ["go", "build", "-o", normal.path, "./go/cmd/wendy"],
            cwd: f.repo
        )
        for filename in [
            "ContextCLIAssertions.swift", "FirstAppFixture.swift",
            "WendyProjectOptimizeTests.swift", "LocalCLIAssertions.swift",
        ] {
            let source = try String(
                contentsOf: f.product.appendingPathComponent(
                    "swift/WendyE2ETests/Tests/WendyE2ETests/" + filename
                ),
                encoding: .utf8
            )
            let pythonLines = CoverageIO.lines(source).map {
                $0.trimmingCharacters(in: .whitespaces)
            }.filter { $0.hasPrefix("keep = ") || $0.hasPrefix("env = {k:") }.joined(
                separator: "\n"
            )
            // This executes the existing product fixture's child command, not a
            // Python coverage implementation. Collection/control stays Swift.
            let script =
                "import os, subprocess, sys\n" + pythonLines
                + "\np = subprocess.run([sys.argv[1], 'left'], env=env, capture_output=True)\nsys.stdout.buffer.write(p.stdout); sys.stderr.buffer.write(p.stderr); sys.exit(p.returncode)\n"
            for covered in [false, true] {
                let native = try f.native("sanitizer/\(filename)/\(covered)/coverage/go")
                var env = ProcessInfo.processInfo.environment
                env["HOME"] = f.repo.path
                env["TMPDIR"] = f.repo.path
                env.removeValue(forKey: "GOCOVERDIR")
                if covered { env["GOCOVERDIR"] = native.path }
                let binary = covered ? f.cli : normal
                let result: CoverageIO.Output
                if filename == "LocalCLIAssertions.swift" {
                    let suffix = try #require(
                        source.range(of: "env -i ").map { String(source[$0.upperBound...]) }
                    )
                    let prefix = "env -i " + suffix.components(separatedBy: "code=$?")[0]
                    let quoted =
                        "'" + binary.path.replacingOccurrences(of: "'", with: "'\\''") + "' left"
                    let shell = prefix.replacingOccurrences(
                        of: #"\(posixCommand) \(posixArgs)"#,
                        with: quoted
                    )
                    result = try CoverageIO.execute(["bash", "-c", shell], environment: env)
                } else {
                    result = try CoverageIO.execute(
                        ["python3", "-c", script, binary.path],
                        environment: env
                    )
                }
                #expect(result.status == 0, Comment(rawValue: result.error))
                #expect(result.text.trimmingCharacters(in: .whitespacesAndNewlines) == "1")
                #expect(result.error.isEmpty)
                #expect(try GoCoverage.checkCLICounters(native) == (covered ? 1 : 0))
                #expect(try CoverageIO.files(native, prefix: "covmeta.").isEmpty == !covered)
            }
        }
    }
}
