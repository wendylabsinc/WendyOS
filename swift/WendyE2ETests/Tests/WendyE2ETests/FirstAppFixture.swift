import Foundation
import Testing
import WendyE2ETesting

/// Local filesystem and process observations, never a replacement CLI.
/// Python 3 is already required by Scripts/E2ETest.sh.
enum FirstAppFixture {
    static let knownDefectsEnabled =
        ProcessInfo.processInfo.environment["WENDY_E2E_KNOWN_INIT_DEFECTS"] == "1"

    static let pythonWheelhouse = ProcessInfo.processInfo.environment["WENDY_E2E_PYTHON_WHEELHOUSE"]
        .flatMap { $0.isEmpty ? nil : $0 }

    static let manual = [
        "init", "--here", "--app-id", "first-app", "--target", "wendyos",
        "--language", "python", "--no-extra-entitlements", "--assistant", "skip",
        "--git-init", "no",
    ]

    static func requirePythonScaffoldTools(_ cli: WendyE2ESession) async throws {
        let result = try await python(
            cli,
            """
            import sys
            if sys.version_info < (3, 11):
                sys.stderr.write('First-app Python scaffold fixture requires Python 3.11+ (tomllib); '
                                 'found ' + sys.version.split()[0] + ' at ' + sys.executable + '. '
                                 'Put Python 3.11+ first on the CLI machine PATH and rerun this spec.\\n')
                sys.exit(1)
            """
        )
        try #require(result.status.isSuccess, "\(result.stderr)")
    }

    static func run(
        _ cli: WendyE2ESession,
        _ arguments: [String],
        succeeds: Bool = true
    ) async throws -> WendyE2EShellResult {
        let home = try #require(cli.env["HOME"], "Requires per-test sandbox HOME")
        try #require(cli.workingDirectory?.hasPrefix(home) == true)
        // An absent answer must fail rather than read the agent's terminal.
        // A timeout is always an assertion failure, including in negative specs.
        let script = """
            import os, shutil, subprocess, sys
            keep = ['HOME', 'PATH', 'GOCOVERDIR', 'TMPDIR', 'TEMP', 'TMP', 'USERPROFILE', 'APPDATA', 'LOCALAPPDATA', 'SystemRoot']
            env = {k: os.environ[k] for k in keep if k in os.environ}
            env.update(WENDY_CONFIG_DIR=os.path.join(env['HOME'], '.wendy'), WENDY_ANALYTICS='false',
                       WENDY_NO_UPDATE_CHECK='1', WENDY_SECRET_STORE='file', CI='1', NO_COLOR='1')
            try:
                result = subprocess.run([shutil.which('wendy')] + sys.argv[1:], env=env,
                                        stdin=subprocess.DEVNULL, capture_output=True, timeout=15)
            except subprocess.TimeoutExpired:
                sys.stderr.write('FIRST_APP_COMMAND_TIMED_OUT')
                sys.exit(124)
            sys.stdout.buffer.write(result.stdout)
            sys.stderr.buffer.write(result.stderr)
            sys.exit(result.returncode)
            """
        let result = try await python(cli, script, arguments: arguments)
        #expect(!result.stderr.contains("FIRST_APP_COMMAND_TIMED_OUT"))
        #expect(result.status.isSuccess == succeeds, "\(arguments): \(result.readableStderr)")
        #expect(!result.stderr.contains("could not open a new TTY"))
        #expect(!result.stdout.contains("\u{1B}"))
        #expect(!result.stderr.contains("\u{1B}"))
        return result
    }

    static func python(
        _ cli: WendyE2ESession,
        _ script: String,
        arguments: [String] = []
    ) async throws -> WendyE2EShellResult {
        try await cli.sh(
            posix: "python3 -c "
                + ([script] + arguments).map(LocalCLIAssertions.quote).joined(separator: " "),
            power: "python -c "
                + ([script] + arguments).map(LocalCLIAssertions.powerQuote).joined(separator: " ")
        ) { $0 }
    }

    static func object(_ text: String) throws -> [String: Any] {
        try #require(try JSONSerialization.jsonObject(with: Data(text.utf8)) as? [String: Any])
    }

    static func manifest(
        _ cli: WendyE2ESession,
        path: String = "wendy.json"
    ) async throws -> [String: Any] {
        try object(try await LocalCLIAssertions.read(cli, path: path))
    }

    static func snapshot(_ cli: WendyE2ESession) async throws -> String {
        let result = try await python(
            cli,
            """
            import hashlib, json, os, pathlib, stat
            tree = {}
            for root, dirs, files in os.walk('.', followlinks=False):
                for name in dirs + files:
                    p = pathlib.Path(root, name)
                    mode = p.lstat().st_mode
                    value = ['link', os.readlink(p)] if p.is_symlink() else (
                        ['directory'] if p.is_dir() else ['file', hashlib.sha256(p.read_bytes()).hexdigest()])
                    tree[p.as_posix()] = [stat.S_IMODE(mode), value]
            print(json.dumps(tree, sort_keys=True))
            """
        )
        try result.requireSuccess()
        return result.stdout
    }

    static func validate(_ cli: WendyE2ESession, path: String = "wendy.json") async throws {
        let result = try await run(cli, ["project", "validate", path, "--json"])
        let summary = try object(result.stdout)
        #expect(summary["valid"] as? Bool == true)
        #expect(summary["error"] == nil)
        #expect((summary["path"] as? String)?.hasSuffix("wendy.json") == true)
    }

    static let project = #"""
        {
          "appId": "first-app", "version": "0.1.0", "platform": "linux", "language": "python",
          "env": {"KEEP": "original"},
          "entitlements": [{"type":"network","mode":"host"}],
          "readiness": {"tcpSocket":{"port":3001},"timeoutSeconds":10},
          "hooks": {"postStart":{"openURL":"http://${WENDY_HOSTNAME}:3001"}},
          "services": {"worker":{"context":".","env":{"WORKER":"original"},"entitlements":[{"type":"audio"}]}},
          "future": {"counter":9007199254740993,"nested":[true,{"text":"keep exactly"}]}
        }
        """#

    // Keep unbalanced JSON outside the test declaration: the reference reader
    // counts literal braces when locating the end of a test body.
    static let invalidManifests = [
        ("{", "valid JSON"), ("[]", "JSON object"),
        (#"{"appId":""}"#, "appId"),
        (
            #"{"appId":"first-app","entitlements":[{"type":"persist","name":"data","path":"relative"}]}"#,
            "path"
        ),
        (#"{"appId":"first-app","entitlements":[{"type":"http","port":70000}]}"#, "port"),
    ]

    static func prepareProject(_ cli: WendyE2ESession) async throws {
        try await LocalCLIAssertions.write(cli, path: "wendy.json", text: project + "\n")
        try await LocalCLIAssertions.write(
            cli,
            path: "notes.txt",
            text: "Keep my source and instructions.\n"
        )
        try await cli.sh(
            posix: "chmod 640 wendy.json",
            power: "$null = Get-Item -LiteralPath 'wendy.json'"
        )
        try await validate(cli)
    }

    static func editSummary(
        _ result: WendyE2EShellResult,
        saved: Bool,
        dryRun: Bool
    ) throws -> [[String: Any]] {
        let summary = try object(result.stdout)
        #expect(summary["saved"] as? Bool == saved)
        #expect(summary["dryRun"] as? Bool == dryRun)
        #expect((summary["path"] as? String)?.hasSuffix("wendy.json") == true)
        return try #require(summary["changes"] as? [[String: Any]])
    }

    static func sameManifest(_ cli: WendyE2ESession, _ expected: [String: Any]) async throws {
        #expect(try await manifest(cli) as NSDictionary == expected as NSDictionary)
        #expect(
            try await LocalCLIAssertions.read(cli, path: "notes.txt")
                == "Keep my source and instructions.\n"
        )
        if cli.machine.os != .windows {
            let permissions = try await python(
                cli,
                "import os, stat; print(stat.S_IMODE(os.stat('wendy.json').st_mode))"
            )
            try permissions.requireSuccess()
            #expect(permissions.normalizedStdout == "416\n")  // 0640
        }
    }
}
