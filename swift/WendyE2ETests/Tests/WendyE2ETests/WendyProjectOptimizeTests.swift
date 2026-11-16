import Foundation
import Testing
import WendyE2ETesting

/// Local static optimization checks use POSIX file modes and symlinks, with no
/// image builds, daemon, device or saved sign-in. Fixtures require Python 3.
@Suite
struct `'wendy project optimize'` {
    let scenario = CLIAndAgentScenario()

    /// Inspection is read-only, including when a missing `.dockerignore` is
    /// reported. Non-terminal default output and explicit `--json` describe the
    /// same findings, source lines, severities and available fixes. Project bytes,
    /// permissions, symlinks and unrelated files remain unchanged.
    @Test
    func `reports actionable findings without changing the project`() async throws {
        try await scenario.run(authenticated: false) { cli, _ in
            try await OptimizeFixture.prepare(cli)
            let before = try await FirstAppFixture.snapshot(cli)
            var reports: [OptimizeFixture.Report] = []
            for flags in [[], ["--json"]] {
                let result = try await OptimizeFixture.run(cli, flags, exit: 1)
                #expect(result.stderr == "")
                let report = try await OptimizeFixture.report(cli, result.stdout)
                #expect(report.targets == [.init(name: "app", kind: "dockerfile")])
                #expect(report.findings == OptimizeFixture.initialFindings)
                reports.append(report)
                #expect(try await FirstAppFixture.snapshot(cli) == before)
                try await LocalCLIAssertions.unchanged(cli, project: false)
            }
            #expect(reports[0] == reports[1])
        }
    }

    /// Explicit `--fix` composes cache and release changes on the same build
    /// instruction and creates the default `.dockerignore`. Every other file and
    /// existing mode is preserved. Repeating the fix leaves exact content and
    /// modes unchanged. Contextual CUDA/toolchain findings remain report-only.
    /// This case uses human output; the separate known-defect case retains the
    /// machine-readable fix-output contract.
    @Test
    func `applies only safe edits and leaves a repeat run unchanged`() async throws {
        try await scenario.run(authenticated: false) { cli, _ in
            try await OptimizeFixture.prepare(cli)
            let before = try await OptimizeFixture.tree(cli)
            _ = try await OptimizeFixture.run(cli, ["--fix", "--json=false"], exit: 1)
            try await OptimizeFixture.onlySafeEdits(cli, before: before)
            let fixed = try await FirstAppFixture.snapshot(cli)
            _ = try await OptimizeFixture.run(cli, ["--fix", "--json=false"], exit: 1)
            #expect(try await FirstAppFixture.snapshot(cli) == fixed)
            let report = try await OptimizeFixture.report(
                cli,
                try await OptimizeFixture.run(cli, ["--json"], exit: 1).stdout
            )
            #expect(report.findings == OptimizeFixture.residualFindings)
            #expect(try await FirstAppFixture.snapshot(cli) == fixed)
            // A caller's customized ignore rules and mode also survive --fix.
            try await LocalCLIAssertions.write(
                cli,
                path: ".dockerignore",
                text: "# my rules\nprivate-data/\n"
            )
            try await OptimizeFixture.python(
                cli,
                "from pathlib import Path; Path('.dockerignore').chmod(0o600)"
            )
            let customized = try await FirstAppFixture.snapshot(cli)
            _ = try await OptimizeFixture.run(cli, ["--fix", "--json=false"], exit: 1)
            #expect(try await FirstAppFixture.snapshot(cli) == customized)
            try await LocalCLIAssertions.unchanged(cli, project: false)
        }
    }

    /// Multi-service projects analyze each service's own context rather than the
    /// root Dockerfile. The offline architecture defaults to arm64; `--arch amd64`
    /// removes the forced-amd64 mismatch. `--severity` changes the exit threshold,
    /// never filters findings out of the report. Invalid severity is an execution
    /// error (exit 2), without edits.
    @Test
    func `selects service contexts and gates on severity and architecture`() async throws {
        try await scenario.run(authenticated: false) { cli, _ in
            try await OptimizeFixture.prepare(cli)
            let setup = try await FirstAppFixture.python(
                cli,
                """
                from pathlib import Path
                Path('wendy.json').write_text('{"appId":"optimizer-spec","services":{"beta":{"context":"beta"},"alpha":{"context":"alpha"}}}\\n')
                for name in ['alpha', 'beta']:
                    Path(name).mkdir()
                    Path(name, '.dockerignore').write_text('.git\\n')
                Path('alpha/Dockerfile').write_text('FROM --platform=linux/amd64 alpine:3.20\\nCMD ["true"]\\n')
                Path('beta/Containerfile').write_text('FROM alpine:3.20\\nCMD echo signals\\n')
                """
            )
            try setup.requireSuccess()
            let before = try await FirstAppFixture.snapshot(cli)
            let expected = [
                OptimizeFixture.Finding(
                    analyzer: "arch-image",
                    target: "alpha",
                    severity: "error",
                    title: "amd64 base image on arm64 target",
                    file: "alpha/Dockerfile",
                    line: 1
                ),
                OptimizeFixture.Finding(
                    analyzer: "image-hygiene",
                    target: "beta",
                    severity: "warning",
                    title: "CMD uses shell form",
                    file: "beta/Containerfile",
                    line: 2
                ),
            ]
            for (flags, code, findings) in [
                (["--severity", "error"], 1, expected),
                (["--arch", "amd64", "--severity", "error"], 0, Array(expected.suffix(1))),
                (["--arch", "amd64", "--severity", "warning"], 1, Array(expected.suffix(1))),
                (["--arch", "amd64", "--severity", "info"], 1, Array(expected.suffix(1))),
            ] {
                let result = try await OptimizeFixture.run(cli, flags + ["--json"], exit: code)
                #expect(result.stderr == "")
                let report = try await OptimizeFixture.report(cli, result.stdout)
                #expect(
                    report.targets == [
                        .init(name: "alpha", kind: "compose-service"),
                        .init(name: "beta", kind: "compose-service"),
                    ]
                )
                #expect(report.findings == findings)
                #expect(try await FirstAppFixture.snapshot(cli) == before)
            }
            let invalid = try await OptimizeFixture.run(
                cli,
                ["--severity", "fatal", "--json"],
                exit: 2
            )
            #expect(invalid.stdout == "")
            #expect(invalid.stderr.contains("invalid severity"))
            #expect(try await FirstAppFixture.snapshot(cli) == before)
            try await LocalCLIAssertions.unchanged(cli, project: false)
        }
    }

    /// Preservation checks detect newly created hidden files, changed bytes,
    /// changed permissions and retargeted symlinks. A deliberately altered finding
    /// also differs from the expected report. Null, missing and scalar values
    /// cannot replace available fix objects. These controls exercise the same
    /// observations used above without changing the CLI implementation.
    @Test
    func `rejects unwanted writes and altered findings in its observations`() async throws {
        try await scenario.run(authenticated: false) { cli, _ in
            try await OptimizeFixture.prepare(cli)
            let before = try await FirstAppFixture.snapshot(cli)
            for (change, restore) in [
                (
                    "Path('.dockerignore').write_text('unexpected')",
                    "Path('.dockerignore').unlink()"
                ),
                (
                    "Path('notes.bin').write_bytes(b'changed')",
                    "Path('notes.bin').write_bytes(bytes([0, 255, 10]))"
                ),
                ("Path('Dockerfile').chmod(0o600)", "Path('Dockerfile').chmod(0o640)"),
                (
                    "Path('notes-link').unlink(); Path('notes-link').symlink_to('Dockerfile')",
                    "Path('notes-link').unlink(); Path('notes-link').symlink_to('notes.bin')"
                ),
            ] {
                try await OptimizeFixture.python(cli, "from pathlib import Path\n" + change)
                #expect(try await FirstAppFixture.snapshot(cli) != before)
                try await OptimizeFixture.python(cli, "from pathlib import Path\n" + restore)
                #expect(try await FirstAppFixture.snapshot(cli) == before)
            }
            let result = try await OptimizeFixture.run(cli, ["--json"], exit: 1)
            let altered = result.stdout.replacingOccurrences(of: "\"warning\"", with: "\"info\"")
            #expect(
                try await OptimizeFixture.report(cli, altered).findings
                    != OptimizeFixture.initialFindings
            )
            var object = try FirstAppFixture.object(result.stdout)
            let original = try #require(object["findings"] as? [[String: Any]])
            try #require(original.count == OptimizeFixture.initialFindings.count)
            let replacements: [Any?] = [nil, NSNull(), "not a fix", 0, false]
            for (index, expected) in OptimizeFixture.initialFindings.enumerated()
            where expected.fixable {
                try #require(original[index]["fix"] is [String: Any])
                for replacement in replacements {
                    var findings = original
                    findings[index]["fix"] = replacement
                    object["findings"] = findings
                    let altered = String(
                        decoding: try JSONSerialization.data(withJSONObject: object),
                        as: UTF8.self
                    )
                    let report = try await OptimizeFixture.report(cli, altered)
                    #expect(report.findings != OptimizeFixture.initialFindings)
                    #expect(!report.findings[index].fixable)
                }
            }
            #expect(try await FirstAppFixture.snapshot(cli) == before)
        }
    }

    /// `--fix --json` must emit one parseable JSON report of the residual findings,
    /// with applied-fix commentary kept off stdout. Safe edits still obey the
    /// filesystem guarantees. Known defect: applied-fix lines currently precede
    /// JSON on stdout. Disabled by default; replay the full expected behavior with
    /// `WENDY_E2E_KNOWN_OPTIMIZE_DEFECTS=1`.
    @Test(
        .disabled(
            if: !OptimizeFixture.knownDefectsEnabled,
            "DEFECT: --fix --json mixes applied-fix text into stdout. Replay with WENDY_E2E_KNOWN_OPTIMIZE_DEFECTS=1."
        )
    )
    func `emits a machine readable residual report when applying fixes`() async throws {
        try await scenario.run(authenticated: false) { cli, _ in
            try await OptimizeFixture.prepare(cli)
            let before = try await OptimizeFixture.tree(cli)
            let result = try await OptimizeFixture.run(cli, ["--fix", "--json"], exit: 1)
            try await OptimizeFixture.onlySafeEdits(cli, before: before)
            try await LocalCLIAssertions.unchanged(cli, project: false)
            let report = try await OptimizeFixture.report(cli, result.stdout)
            #expect(report.findings == OptimizeFixture.residualFindings)
        }
    }

    /// A directory without a project is an execution error (documented exit 2),
    /// rather than a successful clean optimization report. It must remain empty.
    /// Known defect: the command currently exits 0 with empty targets/findings.
    /// Disabled by default; replay with `WENDY_E2E_KNOWN_OPTIMIZE_DEFECTS=1`.
    @Test(
        .disabled(
            if: !OptimizeFixture.knownDefectsEnabled,
            "DEFECT: no project returns success instead of documented exit 2. Replay with WENDY_E2E_KNOWN_OPTIMIZE_DEFECTS=1."
        )
    )
    func `fails with execution error when no project exists`() async throws {
        try await scenario.run(authenticated: false) { cli, _ in
            let before = try await FirstAppFixture.snapshot(cli)
            let result = try await OptimizeFixture.run(cli, ["--json"], exit: 2)
            #expect(result.stdout == "")
            #expect(!result.stderr.isEmpty)
            #expect(try await FirstAppFixture.snapshot(cli) == before)
        }
    }
}

private enum OptimizeFixture {
    static let knownDefectsEnabled =
        ProcessInfo.processInfo.environment["WENDY_E2E_KNOWN_OPTIMIZE_DEFECTS"] == "1"

    static let dockerfile = "FROM swift:6.0\nRUN swift build\nCMD [\"/app/server\"]\n"
    static let fixedDockerfile =
        "FROM swift:6.0\nRUN --mount=type=cache,target=/root/.swiftpm swift build -c release\nCMD [\"/app/server\"]\n"
    static let dockerignore =
        ".git\n**/.build\n**/.swiftpm\nnode_modules\ntarget\n__pycache__\n*.pyc\n.venv\ndist\nbuild\n"

    struct Target: Decodable, Equatable {
        let name: String
        let kind: String
    }

    struct Finding: Equatable {
        let analyzer: String
        var target = "app"
        let severity: String
        let title: String
        var file: String? = nil
        var line: Int? = nil
        var fixable = false
    }

    struct Report: Equatable {
        let targets: [Target]
        let findings: [Finding]
    }

    static let residualFindings = [
        Finding(
            analyzer: "cuda-ml",
            severity: "warning",
            title: "GPU entitlement set but a CPU-only ML wheel is pinned",
            file: "requirements.txt",
            line: 2
        ),
        Finding(
            analyzer: "arch-image",
            severity: "info",
            title: "Single-stage build ships its build toolchain"
        ),
    ]
    static let initialFindings = [
        Finding(
            analyzer: "build-cache",
            severity: "warning",
            title: "\"swift build\" runs without a build cache mount",
            file: "Dockerfile",
            line: 2,
            fixable: true
        ),
        Finding(
            analyzer: "release-debug",
            severity: "warning",
            title: "swift build is a debug build",
            file: "Dockerfile",
            line: 2,
            fixable: true
        ),
        residualFindings[0],
        Finding(
            analyzer: "arch-image",
            severity: "warning",
            title: "No .dockerignore",
            fixable: true
        ),
        residualFindings[1],
    ]

    static func prepare(_ cli: WendyE2ESession) async throws {
        try #require(
            cli.machine.os != .windows,
            "Optimizer preservation fixtures require POSIX modes and symlinks"
        )
        try await LocalCLIAssertions.prepare(cli)
        try await LocalCLIAssertions.write(
            cli,
            path: "wendy.json",
            text:
                "{\"appId\":\"optimizer-spec\",\"entitlements\":[{\"type\":\"gpu\"}]}\n"
        )
        try await LocalCLIAssertions.write(cli, path: "Dockerfile", text: dockerfile)
        try await LocalCLIAssertions.write(
            cli,
            path: "requirements.txt",
            text:
                "# CPU-only wheel deliberately paired with GPU\ntorch==2.5.1+cpu\n"
        )
        try await python(
            cli,
            """
            from pathlib import Path
            Path('Dockerfile').chmod(0o640)
            Path('wendy.json').chmod(0o600)
            Path('requirements.txt').chmod(0o444)
            Path('notes.bin').write_bytes(bytes([0, 255, 10]))
            Path('notes.bin').chmod(0o751)
            Path('notes-link').symlink_to('notes.bin')
            Path('unrelated').mkdir()
            Path('unrelated/.keep').write_text('leave hidden files alone\\n')
            """
        )
    }

    static func python(_ cli: WendyE2ESession, _ script: String) async throws {
        let result = try await FirstAppFixture.python(cli, script)
        try result.requireSuccess()
    }

    static func tree(_ cli: WendyE2ESession) async throws -> [String: Any] {
        try FirstAppFixture.object(try await FirstAppFixture.snapshot(cli))
    }

    static func onlySafeEdits(_ cli: WendyE2ESession, before: [String: Any]) async throws {
        try await fixedFiles(cli)
        let after = try await tree(cli)
        #expect(Set(after.keys) == Set(before.keys).union([".dockerignore"]))
        var untouched = after
        untouched.removeValue(forKey: ".dockerignore")
        untouched["Dockerfile"] = before["Dockerfile"]
        #expect(untouched as NSDictionary == before as NSDictionary)
    }

    static func fixedFiles(_ cli: WendyE2ESession) async throws {
        #expect(try await LocalCLIAssertions.read(cli, path: "Dockerfile") == fixedDockerfile)
        #expect(try await LocalCLIAssertions.read(cli, path: ".dockerignore") == dockerignore)
        try await python(
            cli,
            """
            import pathlib, stat
            for name, mode in [('Dockerfile', 0o640), ('.dockerignore', 0o644)]:
                actual = pathlib.Path(name).lstat().st_mode
                assert stat.S_ISREG(actual), name + ' must remain a regular file'
                assert stat.S_IMODE(actual) == mode, name + ' mode changed'
            """
        )
    }

    static func run(
        _ cli: WendyE2ESession,
        _ flags: [String],
        exit: Int
    ) async throws -> WendyE2EShellResult {
        let home = try #require(cli.env["HOME"])
        try #require(cli.workingDirectory == home + "/work", "Requires the per-test sandbox")
        // Record the exact exit outside the project snapshot. Timeout is never
        // accepted as an expected nonzero CLI result. No inherited routes/auth.
        let script = """
            import os, pathlib, shutil, subprocess, sys
            env = {k: os.environ[k] for k in ['HOME', 'PATH', 'TMPDIR'] if k in os.environ}
            env.update(WENDY_CONFIG_DIR=os.path.join(env['HOME'], '.wendy'), WENDY_ANALYTICS='false',
                       WENDY_NO_UPDATE_CHECK='1', WENDY_SECRET_STORE='file', CI='1', NO_COLOR='1')
            os.umask(0o022)
            exit_path = pathlib.Path('../optimizer-exit')
            exit_path.unlink(missing_ok=True)
            pathlib.Path('../optimizer-directory').write_text(os.getcwd())
            result = subprocess.run([shutil.which('wendy'), 'project', 'optimize'] + sys.argv[1:],
                                    env=env, stdin=subprocess.DEVNULL, capture_output=True, timeout=10)
            exit_path.write_text(str(result.returncode))
            sys.stdout.buffer.write(result.stdout)
            sys.stderr.buffer.write(result.stderr)
            sys.exit(result.returncode)
            """
        let result = try await FirstAppFixture.python(cli, script, arguments: flags)
        let code = try await LocalCLIAssertions.read(cli, path: "../optimizer-exit")
        #expect(
            code == String(exit),
            "\(flags): expected exit \(exit), got \(code); \(result.stderr)"
        )
        #expect(result.status.isSuccess == (exit == 0))
        #expect(!result.stdout.contains("\u{1B}"))
        #expect(!result.stderr.contains("\u{1B}"))
        return result
    }

    static func report(_ cli: WendyE2ESession, _ text: String) async throws -> Report {
        let root = try await LocalCLIAssertions.read(cli, path: "../optimizer-directory")
        let object = try FirstAppFixture.object(text)
        let targetData = try JSONSerialization.data(withJSONObject: try #require(object["targets"]))
        let targets = try JSONDecoder().decode([Target].self, from: targetData)
        let findings = try #require(object["findings"] as? [[String: Any]]).map { f in
            let location = f["location"] as? [String: Any]
            let path = location?["file"] as? String
            var relative: String?
            if let path {
                try #require(
                    path.hasPrefix(root + "/"),
                    "Source location must be inside the exact project directory"
                )
                relative = String(path.dropFirst(root.count + 1))
            }
            return Finding(
                analyzer: try #require(f["analyzer"] as? String),
                target: try #require(f["target"] as? String),
                severity: try #require(f["severity"] as? String),
                title: try #require(f["title"] as? String),
                file: relative,
                line: location?["line"] as? Int,
                fixable: (f["fix"] as? [String: Any]) != nil
            )
        }
        return Report(targets: targets, findings: findings)
    }
}
