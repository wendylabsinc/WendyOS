import Foundation
import Testing

@testable import SwiftE2ETestingCLI

@Suite(.serialized)
struct `scenario coverage lifecycle` {
    @Test func `one real suite attributes CLI and managed Agent operations and cleanup`() throws {
        let f = try GoCoverageFixture(build: false)
        let sockets = URL(fileURLWithPath: "/tmp/e2e-scenario-" + UUID().uuidString.prefix(8))
        try FileManager.default.createDirectory(at: sockets, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: sockets) }
        let request = sockets.appendingPathComponent("request.sock")
        let control = sockets.appendingPathComponent("control.sock")
        try f.write(
            "go/cmd/wendy-agent/main.go",
            """
            package main
            import ("fmt"; "net"; "net/http"; "os"; "example.test/coverage/go/internal/sample")
            func main() {
                listener, err := net.Listen("unix", os.Getenv("REQUEST_SOCKET"))
                if err != nil { panic(err) }
                fmt.Println("ready")
                http.Serve(listener, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                    fmt.Fprintln(w, sample.Branch(r.URL.Path[1:]))
                }))
            }
            """
        )
        _ = try f.commit()
        try f.buildBinaries(managed: true)
        try f.attempt()
        try GoCoverage(repo: f.repo, run: f.run).startCapture(control: control.path)
        let process = Process()
        let stdout = Pipe()
        process.executableURL = f.agent
        process.standardOutput = stdout
        process.standardError = FileHandle.nullDevice
        var agentEnv = ProcessInfo.processInfo.environment
        agentEnv["REQUEST_SOCKET"] = request.path
        agentEnv["WENDY_E2E_COVERAGE_SOCKET"] = control.path
        agentEnv["GOCOVERDIR"] = try f.native("coverage/bootstrap").path
        process.environment = agentEnv
        try process.run()
        defer {
            if process.isRunning { process.terminate() }
            process.waitUntilExit()
        }
        #expect(try GoCoverageFixture.line(stdout.fileHandleForReading) == "ready")

        // Compile the actual scenario and framework sources in a tiny ordinary
        // Swift Testing package. No product specs or shared-host operations run.
        let package = try scenarioPackage(f, request: request)
        var env = ProcessInfo.processInfo.environment
        for key in ["WENDY_E2E_CLI_ADDRESS", "WENDY_E2E_AGENT_ADDRESS", "WENDY_AGENT_SOCKET"] {
            env.removeValue(forKey: key)
        }
        env["WENDY_E2E_COVERAGE"] = "true"
        env["WENDY_E2E_PARALLEL"] = "false"
        env["WENDY_E2E_RUN_DIR"] = f.run.path
        env["WENDY_E2E_COVERAGE_CONTROL"] = control.path
        env["WENDY_E2E_CLI_BIN_DIR"] = f.cli.deletingLastPathComponent().path
        env["WENDY_E2E_CLI_AUTH_CONFIG_PATH"] =
            f.repo.appendingPathComponent("missing-auth.json").path
        let xml = f.run.appendingPathComponent("test-results.xml")
        let result = try CoverageIO.execute(
            [
                "swift", "test", "--package-path", package.path, "--no-parallel", "--filter",
                "scenario fixtures", "--xunit-output", xml.path,
            ],
            environment: env
        )
        try #require(result.status == 0, Comment(rawValue: result.error + result.text))
        // SwiftPM may write Swift Testing's output beside its XCTest document.
        let swiftXML = f.run.appendingPathComponent("test-results-swift-testing.xml")
        if CoverageIO.exists(swiftXML) {
            try CoverageIO.remove(xml)
            try FileManager.default.copyItem(at: swiftXML, to: xml)
        }
        #expect(try GoCoverage(repo: f.repo, run: f.run).finishCapture(control: control.path) == 0)
        #expect(try GoCoverage(repo: f.repo, run: f.run).deriveAttempt() == 0)
        let records = try CoverageIO.records(f.run.appendingPathComponent("coverage/tests.json"))
        #expect(records.count == 9)
        #expect(records.filter { $0["status"] as? String == "collected" }.count == 7)
        #expect(records.filter { $0["status"] as? String == "skipped" }.count == 1)
        #expect(records.filter { $0["status"] as? String == "no-data" }.count == 1)
        #expect(
            Set(records.compactMap { $0["agentPID"] as? Int }) == [Int(process.processIdentifier)]
        )
        for (name, expected) in [("left", 2), ("right", 2), ("cleanup", 3), ("body failure", 3)] {
            let record = try #require(
                records.first { ($0["test"] as? String)?.contains("/" + name + "()") == true }
            )
            let directory = try #require(record["directory"] as? String)
            let total = try GoCoverage.profile(
                [f.run.appendingPathComponent(directory + "/coverage/go")],
                destination: f.run.appendingPathComponent("profile-" + name),
                source: f.source
            )
            #expect(
                (total["packages"] as! [String: [String: Int]])[
                    "example.test/coverage/go/internal/sample"
                ]?["covered"] == expected
            )
            if name == "left" || name == "right" {
                // Inspect the Agent alone, so CLI hits cannot conceal a missing
                // Agent request or carryover from the other scenario.
                let native = f.run.appendingPathComponent(directory + "/coverage/go")
                let isolated = try f.native("agent-only/" + name)
                for counter in try CoverageIO.files(native, prefix: "covcounters.")
                where counter.lastPathComponent.components(separatedBy: ".")[2]
                    == String(process.processIdentifier)
                {
                    let hash = counter.lastPathComponent.components(separatedBy: ".")[1]
                    try FileManager.default.copyItem(
                        at: counter,
                        to: isolated.appendingPathComponent(counter.lastPathComponent)
                    )
                    let meta = native.appendingPathComponent("covmeta." + hash)
                    let destination = isolated.appendingPathComponent(meta.lastPathComponent)
                    if !CoverageIO.exists(destination) {
                        try FileManager.default.copyItem(at: meta, to: destination)
                    }
                }
                let agentOnly = try GoCoverage.profile(
                    [isolated],
                    destination: f.run.appendingPathComponent("profile-agent-" + name),
                    source: f.source
                )
                #expect(
                    (agentOnly["packages"] as! [String: [String: Int]])[
                        "example.test/coverage/go/internal/sample"
                    ]?["covered"] == expected
                )
                #expect(record["cliInvocations"] as? Int == (name == "left" ? 2 : 1))
            }
        }
        // The separate failure control uses the already-built package and real
        // scenario. Its caught primary error must not hide corrupt capture data.
        try GoCoverage(repo: f.repo, run: f.run).startCapture(control: control.path)
        let failed = try CoverageIO.execute(
            [
                "swift", "test", "--package-path", package.path, "--skip-build", "--no-parallel",
                "--filter", "capture controls", "--xunit-output", xml.path,
            ],
            environment: env
        )
        try #require(failed.status == 0, Comment(rawValue: failed.error + failed.text))
        #expect((failed.text + failed.error).contains("ERROR: E2E coverage:"))
        if CoverageIO.exists(swiftXML) {
            try CoverageIO.remove(xml)
            try FileManager.default.copyItem(at: swiftXML, to: xml)
        }
        #expect(try GoCoverage(repo: f.repo, run: f.run).finishCapture(control: control.path) == 1)
        #expect(try GoCoverage(repo: f.repo, run: f.run).deriveAttempt() == 1)
        let broken = try CoverageIO.records(f.run.appendingPathComponent("coverage/tests.json"))
        #expect(broken.count == 1)
        #expect(broken[0]["status"] as? String == "incomplete")
        #expect((broken[0]["error"] as? String)?.contains("metadata") == true)

        let runner = try String(
            contentsOf: f.product.appendingPathComponent("swift/Scripts/E2ETest.sh"),
            encoding: .utf8
        )
        let timeoutFunction =
            "run_swift_test_with_timeout() {"
            + runner.components(separatedBy: "run_swift_test_with_timeout() {")[1].components(
                separatedBy: "\nwrite_attempt_info()"
            )[0]
        for timeout in [false, true] {
            // These bounded controls reuse the fixture build, not production attempts.
            for path in [xml, swiftXML, f.run.appendingPathComponent("coverage/current-test.txt")] {
                try CoverageIO.remove(path)
            }
            try f.attempt()
            try GoCoverage(repo: f.repo, run: f.run).startCapture(control: control.path)
            env["FIXTURE_TIMEOUT"] = timeout ? "true" : "false"
            let arguments = [
                "swift", "test", "--package-path", package.path, "--skip-build", "--no-parallel",
                "--filter", "suite interrupt controls", "--xunit-output", xml.path,
            ]
            let exited: CoverageIO.Output
            if timeout {
                exited = try CoverageIO.execute(
                    [
                        "/bin/bash", "-c",
                        timeoutFunction + "\nrun_swift_test_with_timeout 5 \"$@\"", "bash",
                        f.run.appendingPathComponent("timeout.json").path,
                    ] + arguments,
                    environment: env
                )
                #expect(exited.status == 124)
            } else {
                exited = try CoverageIO.execute(arguments, environment: env)
                #expect(exited.status != 0)
            }
            try f.attempt(exit: Int(exited.status))
            #expect(
                try GoCoverage(repo: f.repo, run: f.run).finishCapture(control: control.path) == 1
            )
            #expect(try GoCoverage(repo: f.repo, run: f.run).deriveAttempt() == 1)
            let capture = try #require(f.source["capture"] as? [String: Any])
            #expect(capture["status"] as? String == "incomplete")
            #expect(capture["unexecuted"] != nil)
            let interrupted = try CoverageIO.records(
                f.run.appendingPathComponent("coverage/tests.json")
            )
            try #require(interrupted.count == 1)
            #expect(interrupted[0]["agentExported"] as? Bool == true)
            #expect(interrupted[0]["status"] as? String == "incomplete")
            #expect(
                try CoverageIO.read(f.run.appendingPathComponent("attempt.json"))["exitStatus"]
                    as? Int == Int(exited.status)
            )
        }
    }
}

private func scenarioPackage(_ f: GoCoverageFixture, request: URL) throws -> URL {
    let original = f.product.appendingPathComponent("swift/WendyE2ETests")
    let package = f.repo.appendingPathComponent("fixture/swift/WendyE2ETests")
    let tests = package.appendingPathComponent("Tests/ScenarioFixtures")
    try FileManager.default.createDirectory(at: tests, withIntermediateDirectories: true)
    try FileManager.default.createDirectory(
        at: package.appendingPathComponent("Sources"),
        withIntermediateDirectories: true
    )
    try FileManager.default.copyItem(
        at: original.appendingPathComponent("Sources/WendyE2ETesting"),
        to: package.appendingPathComponent("Sources/WendyE2ETesting")
    )
    try FileManager.default.copyItem(
        at: original.appendingPathComponent("Tests/WendyE2ETests/CLIAndAgentScenario.swift"),
        to: tests.appendingPathComponent("CLIAndAgentScenario.swift")
    )
    try """
    // swift-tools-version: 6.1
    import PackageDescription
    let settings: [SwiftSetting] = [.enableUpcomingFeature("ExistentialAny"), .enableUpcomingFeature("MemberImportVisibility"), .enableUpcomingFeature("InternalImportsByDefault")]
    let package = Package(name: "ScenarioCaptureFixture", platforms: [.macOS(.v15)], dependencies: [
        .package(path: \(String(reflecting: original.appendingPathComponent(".build/checkouts/swift-subprocess").path))),
        .package(path: \(String(reflecting: original.appendingPathComponent(".build/checkouts/swift-system").path)))
    ], targets: [
        .target(name: "WendyE2ETesting", dependencies: [.product(name: "Subprocess", package: "swift-subprocess"), .product(name: "SystemPackage", package: "swift-system")], swiftSettings: settings),
        .testTarget(name: "ScenarioFixtures", dependencies: ["WendyE2ETesting"], swiftSettings: settings)
    ])
    """.write(
        to: package.appendingPathComponent("Package.swift"),
        atomically: true,
        encoding: .utf8
    )
    try #"""
    import Foundation
    import Testing
    import WendyE2ETesting
    enum Expected: Error { case body, cleanup, before }
    @Suite(.serialized)
    struct `scenario fixtures` {
        @Test func `left`() async throws {
            try await CLIAndAgentScenario().run(authenticated: false) { cli, agent in
                let recorder = try WendyE2ERecorder(filePath: #filePath, function: "left()", line: #line)
                var parallel = ProcessInfo.processInfo.environment
                parallel["WENDY_E2E_PARALLEL"] = "true"
                #expect(throws: (any Error).self) { try WendyE2ECoverage.begin(recorder: recorder, environment: parallel) }
                #expect(throws: (any Error).self) { try WendyE2ECoverage.begin(recorder: recorder) }
                try await cli.sh("wendy left")
                try await cli.sh("wendy left")
                try await agent.sh("curl --fail --silent --unix-socket '__SOCKET__' http://localhost/left")
            }
        }
        @Test func `right`() async throws {
            try await CLIAndAgentScenario().run(authenticated: false) { cli, agent in
                try await cli.sh("wendy right")
                try await agent.sh("curl --fail --silent --unix-socket '__SOCKET__' http://localhost/right")
            }
        }
        @Test func `cleanup`() async throws {
            do {
                try await CLIAndAgentScenario(after: { cli, _ in
                    try await cli.sh("wendy right")
                    throw Expected.cleanup
                }).run(authenticated: false) { cli, _ in try await cli.sh("wendy left") }
                Issue.record("cleanup error disappeared")
            } catch Expected.cleanup {}
        }
        @Test func `body failure`() async throws {
            do {
                try await CLIAndAgentScenario(after: { cli, _ in
                    try await cli.sh("wendy right")
                    throw Expected.cleanup
                }).run(authenticated: false) { cli, _ in
                    try await cli.sh("wendy left")
                    throw Expected.body
                }
                Issue.record("body error disappeared")
            } catch Expected.body {}
        }
        @Test func `before failure`() async throws {
            do {
                try await CLIAndAgentScenario(before: { _, _ in throw Expected.before }, after: { cli, _ in
                    try await cli.sh("wendy left")
                }).run(authenticated: false) { _, _ in Issue.record("body ran") }
                Issue.record("before error disappeared")
            } catch Expected.before {}
        }
        @Test func `partial setup`() async throws {
            do {
                try await CLIAndAgentScenario().run { _, _ in Issue.record("body ran") }
                Issue.record("missing auth fixture accepted")
            } catch let error as WendyE2EMachineError {
                if case .commandFailed = error {} else { throw error }
            }
        }
        @Test func `indirect`() async throws { try await runFixture() }
        private func runFixture(filePath: String = #filePath, function: String = #function, line: Int = #line) async throws {
            try await CLIAndAgentScenario().run(authenticated: false, filePath: filePath, function: function, line: line) { cli, _ in
                try await cli.sh("wendy left")
            }
        }
        @Test func `Swift only`() { #expect(1 + 1 == 2) }
        @Test(.disabled("stub")) func `disabled`() {}
    }
    """#.replacingOccurrences(of: "__SOCKET__", with: request.path).write(
        to: tests.appendingPathComponent("ScenarioTests.swift"),
        atomically: true,
        encoding: .utf8
    )
    try #"""
    import Foundation
    import Testing
    import WendyE2ETesting
    @Suite
    struct `capture controls` {
        @Test func `capture error preserves primary body error`() async throws {
            do {
                try await CLIAndAgentScenario(after: { cli, _ in
                    try await cli.sh("rm -f \"$GOCOVERDIR\"/covmeta.*")
                }).run(authenticated: false) { cli, _ in
                    try await cli.sh("wendy left")
                    throw Expected.body
                }
                Issue.record("primary error disappeared")
            } catch Expected.body {}
        }
    }
    """#.write(
        to: tests.appendingPathComponent("CaptureControlTests.swift"),
        atomically: true,
        encoding: .utf8
    )
    try #"""
    import Foundation
    import Testing
    import WendyE2ETesting
    #if canImport(Darwin)
    import Darwin
    #else
    import Glibc
    #endif
    @Suite
    struct `suite interrupt controls` {
        @Test func `interrupted interval`() async throws {
            try await CLIAndAgentScenario().run(authenticated: false) { cli, _ in
                try await cli.sh("wendy left")
                if ProcessInfo.processInfo.environment["FIXTURE_TIMEOUT"] == "true" {
                    try await Task.sleep(for: .seconds(3600))
                } else { kill(getpid(), SIGKILL) }
            }
        }
    }
    """#.write(
        to: tests.appendingPathComponent("InterruptControlTests.swift"),
        atomically: true,
        encoding: .utf8
    )
    return package
}
