import ArgumentParser
import Foundation
import Testing

@testable import SwiftE2ETestingCLI

@Suite(.serialized)
struct `reviewed coverage corrections` {
    @Test func `uncollected attempts preserve a compatible collected source union`() throws {
        let f = try GoCoverageFixture()
        try f.attempt()
        let directory = "observations/suite/test"
        try f.invoke(f.native(directory + "/coverage/go"), "left")
        try f.records([f.record("test", directory: directory)])
        let capture = try AggregateCoverageAttempt(input: f.run, repo: f.repo)
        defer { try? FileManager.default.removeItem(at: capture.scratch) }
        for name in ["setup-failed", "other-target"] {
            let missing = f.repo.appendingPathComponent(name)
            try CoverageIO.write(
                ["exitStatus": name == "setup-failed" ? 1 : 0],
                to: missing.appendingPathComponent("attempt.json")
            )
            let run = f.repo.appendingPathComponent("run-" + name)
            try FileManager.default.createDirectory(at: run, withIntermediateDirectories: true)
            #expect(
                try !aggregateGoCoverage(
                    inputs: [missing, f.run],
                    captured: [capture],
                    errors: [],
                    run: run,
                    repo: f.repo,
                    diff: nil
                )
            )
            let result = try CoverageIO.read(run.appendingPathComponent("coverage/summary.json"))
            #expect(result["status"] as? String == "collected")
            #expect((result["errors"] as? [String])?.isEmpty == true)
            let attempts = try #require(result["attempts"] as? [[String: Any]])
            #expect(
                attempts.contains {
                    $0["attempt"] as? String == name && $0["status"] as? String == "not-collected"
                }
            )
            #expect(
                try Data(contentsOf: run.appendingPathComponent("coverage/coverage.out"))
                    == Data(
                        contentsOf: capture.scratch.appendingPathComponent("coverage/coverage.out")
                    )
            )
        }
    }

    @Test func `a requested capture without provenance stays incomplete and non-success`() throws {
        let f = try GoCoverageFixture(build: false)
        let input = f.repo.appendingPathComponent("run-input/swift.requested.local.0001")
        try CoverageIO.write(
            [
                "exitStatus": 9, "coverageRequested": true,
                "github": ["sha": try f.git(["rev-parse", "HEAD"])],
            ],
            to: input.appendingPathComponent("attempt.json")
        )
        let output = f.repo.appendingPathComponent("run-output")
        var command = try AggregateCommand.parse([
            "--package-dir", f.repo.path, "--repo", f.repo.path, "--output-dir", output.path,
            input.path,
        ])
        #expect(throws: (any Error).self) { try command.run() }
        let result = try CoverageIO.read(
            output.appendingPathComponent("swift.requested/coverage/summary.json")
        )
        #expect(result["status"] as? String == "incomplete")
        #expect(
            (result["errors"] as? [String] ?? []).contains {
                $0.contains("source provenance is missing")
            }
        )
        #expect(
            (result["attempts"] as? [[String: Any]])?.first?["status"] as? String == "incomplete"
        )
        #expect(
            try CoverageIO.read(input.appendingPathComponent("attempt.json"))["exitStatus"] as? Int
                == 9
        )
    }

    @Test func `unpacked native data reaggregates without overwriting its verified archive`() throws
    {
        let f = try GoCoverageFixture()
        try f.attempt()
        let directory = "observations/suite/test"
        let native = try f.native(directory + "/coverage/go")
        try f.invoke(native, "left")
        try f.records([f.record("test", directory: directory)])
        try GoCoverage.pack(run: f.run)
        let archive = f.run.appendingPathComponent("coverage/go-native.tar.gz")
        let original = try Data(contentsOf: archive)
        _ = try CoverageIO.command(["tar", "-xzf", archive.path, "-C", f.run.path])
        #expect(throws: Never.self) { try GoCoverage.pack(run: f.run) }
        #expect(try Data(contentsOf: archive) == original)
        _ = try CoverageIO.command(["tar", "-xzf", archive.path, "-C", f.run.path])
        let counter = try #require(CoverageIO.files(native, prefix: "covcounters.").first)
        try Data("corrupt counter".utf8).write(to: counter)
        #expect(throws: (any Error).self) { try GoCoverage.pack(run: f.run) }
        #expect(try Data(contentsOf: archive) == original)
        #expect(try Data(contentsOf: counter) == Data("corrupt counter".utf8))
    }

    @Test func `a remote only branch resolves as an ordinary comparison base`() throws {
        let f = try GoCoverageFixture(build: false)
        let base = try f.git(["rev-parse", "HEAD"])
        _ = try f.git(["branch", "-M", "topic"])
        _ = try f.git(["update-ref", "refs/remotes/origin/main", base])
        try f.write("notes.txt", "changed\n")
        let head = try f.commit()
        for range in ["main...HEAD", "refs/heads/main..HEAD"] {
            let comparison = try E2EGitComparison(repo: f.repo, range: range, testedRevision: head)
            #expect(comparison.base == base)
            #expect(comparison.head == head)
        }
    }
    @Test(arguments: [true, false])
    func `spec extraction and Go comparison failures stay independent`(brokenSpecs: Bool) throws {
        let f = try GoCoverageFixture(build: false)
        let base = try f.git(["rev-parse", "HEAD"])
        let spec =
            "@Suite\nstruct `quoted suite` {\n    @Test\n    func `example behavior`() {}\n}\n"
        try f.write(
            "swift/WendyE2ETests/Tests/WendyE2ETests/ExampleTests.swift",
            brokenSpecs ? spec + spec : spec
        )
        try f.write(
            "go/internal/sample/branch.go",
            GoCoverageFixture.branch.replacingOccurrences(of: "return 2", with: "return 3")
        )
        let head = try f.commit()
        try f.buildBinaries()
        try f.attempt()
        let directory = "observations/suite/test"
        try f.invoke(f.native(directory + "/coverage/go"), "left")
        try f.records([f.record("test", directory: directory)])
        let capture = try AggregateCoverageAttempt(input: f.run, repo: f.repo)
        defer { try? FileManager.default.removeItem(at: capture.scratch) }
        if !brokenSpecs {
            // The comparison detects a checkout change after native derivation.
            try f.write(
                "go/internal/sample/branch.go",
                GoCoverageFixture.branch + "// checkout changed\n"
            )
        }
        let run = f.repo.appendingPathComponent("run-comparison")
        try FileManager.default.createDirectory(at: run, withIntermediateDirectories: true)
        #expect(
            try !aggregateGoCoverage(
                inputs: [f.run],
                captured: [capture],
                errors: [],
                run: run,
                repo: f.repo,
                diff: base + "..." + head
            )
        )
        let summary = try CoverageIO.read(run.appendingPathComponent("coverage/summary.json"))
        let comparison = try CoverageIO.read(run.appendingPathComponent("comparison.json"))
        let diff = try #require(summary["diff"] as? [String: Any])
        #expect(comparison["status"] as? String == "compared")
        if brokenSpecs {
            #expect(comparison["specsStatus"] as? String == "unavailable")
            #expect(!(comparison["specsReason"] as? String ?? "").isEmpty)
            #expect(diff["status"] as? String == "collected")
            #expect(diff["covered"] as? Int == 0)
            #expect(diff["statements"] as? Int == 1)
        } else {
            #expect(diff["status"] as? String == "unavailable")
            #expect(comparison["specsStatus"] as? String == "compared")
            #expect(
                (comparison["specs"] as? [[String: Any]])?.first?["change"] as? String == "Added"
            )
        }
    }

}

@Suite(.serialized)
struct `coverage control reexecution` {
    @Test func `an instrumented Agent child cannot inherit the parent control socket`() throws {
        let f = try GoCoverageFixture(build: false)
        try f.write(
            "go/cmd/wendy-agent/main.go",
            """
            package main
            import ("bufio"; "fmt"; "os"; "os/exec")
            func main() {
                if len(os.Args) > 1 { fmt.Printf("child socket=%q\\n", os.Getenv("WENDY_E2E_COVERAGE_SOCKET")); return }
                fmt.Println("ready")
                scanner := bufio.NewScanner(os.Stdin)
                for scanner.Scan() {
                    child := exec.Command(os.Args[0], "child")
                    child.Env = os.Environ()
                    output, err := child.CombinedOutput()
                    fmt.Printf("result=%v %s", err, output)
                }
            }
            """
        )
        _ = try f.commit()
        try f.buildBinaries(managed: true)
        let control = URL(fileURLWithPath: "/tmp/cov-" + UUID().uuidString.prefix(8))
        try FileManager.default.createDirectory(
            at: control,
            withIntermediateDirectories: false,
            attributes: [.posixPermissions: 0o700]
        )
        defer { try? FileManager.default.removeItem(at: control) }
        let socket = control.appendingPathComponent("socket")
        let startup = try f.native("coverage/bootstrap")
        var environment = ProcessInfo.processInfo.environment
        environment["WENDY_E2E_COVERAGE_SOCKET"] = socket.path
        environment["GOCOVERDIR"] = startup.path
        let process = Process()
        let input = Pipe()
        let output = Pipe()
        process.executableURL = f.agent
        process.environment = environment
        process.standardInput = input
        process.standardOutput = output
        process.standardError = FileHandle.nullDevice
        try process.run()
        defer {
            try? input.fileHandleForWriting.close()
            if process.isRunning { process.terminate() }
            process.waitUntilExit()
        }
        #expect(try GoCoverageFixture.line(output.fileHandleForReading) == "ready")
        let pid = try GoCoverage.agentControl(socket.path, operation: "reset")
        for _ in 0..<2 {
            try input.fileHandleForWriting.write(contentsOf: Data("spawn\n".utf8))
            let result = try GoCoverageFixture.line(output.fileHandleForReading)
            #expect(result == "result=<nil> child socket=\"\"")
        }
        let native = try f.native("observations/suite/reexec/coverage/go")
        #expect(
            try GoCoverage.agentControl(socket.path, operation: "export", directory: native.path)
                == pid
        )
        try GoCoverage.validateNative([native])
    }
}
