import Foundation
import Testing
import WendyE2ETesting

// Shared state for the local validation specs. No saved sign-in or device is used.
enum LocalCLIAssertions {
    static let legacyDefaultDeviceNotice =
        "The saved default device is ignored; use --device <device> or set WENDY_DEVICE for this shell."

    static let entitlements =
        #"[{"type":"gpio","pins":[4,17]},{"type":"i2c","device":"i2c-1"},{"type":"persist","name":"recordings","path":"/data/recordings"}]"#
    static let project =
        #"{"appId":"sh.wendy.e2e","version":"1.0","env":{"KEEP":"unchanged"},"entitlements":\#(entitlements)}"#
        + "\n"
    static let config =
        #"{"analytics":{"enabled":false},"completionPromptDismissed":true,"devicePins":{"keep.local":{"orgId":7,"cloudGRPC":"127.0.0.1:1","assetId":"9"}},"future":{"keep":42}}"#
        + "\n"

    static func prepare(_ cli: WendyE2ESession, config: String? = config) async throws {
        let home = try #require(cli.env["HOME"], "These specs require an isolated scenario HOME")
        // Match CLIAndAgentScenario's path spelling, including C:/ roots on Windows.
        let separator = home.contains("\\") && !home.contains("/") ? "\\" : "/"
        try #require(cli.workingDirectory == home + separator + "work")
        try await cli.sh(
            posix:
                "mkdir -p \"$HOME/.wendy\"; printf '%s' 'unchanged trust state' > \"$HOME/.wendy/keep.txt\"",
            power:
                "New-Item -ItemType Directory -Force -Path '../.wendy' | Out-Null; [IO.File]::WriteAllText((Join-Path $pwd '../.wendy/keep.txt'), 'unchanged trust state')"
        )
        try await write(cli, path: "wendy.json", text: project)
        if let config {
            try await write(cli, path: "../.wendy/config.json", text: config)
        }
        try await cli.sh(
            posix: "cp -R \"$HOME/.wendy\" before-config && cp wendy.json before-project",
            power:
                "Copy-Item -Recurse '../.wendy' 'before-config'; Copy-Item 'wendy.json' 'before-project'"
        )
    }

    static func write(_ cli: WendyE2ESession, path: String, text: String) async throws {
        try await cli.sh(
            posix: "printf '%s' \(quote(text)) > \(quote(path))",
            power:
                "[IO.File]::WriteAllText((Join-Path $pwd \(powerQuote(path))), \(powerQuote(text)))"
        )
    }

    static func read(_ cli: WendyE2ESession, path: String) async throws -> String {
        try await cli.sh(
            posix: "cat \(quote(path))",
            power: "[Console]::Write([IO.File]::ReadAllText((Join-Path $pwd \(powerQuote(path)))))"
        ) { result in
            try result.requireSuccess()
            return result.stdout
        }
    }

    static func unchanged(_ cli: WendyE2ESession, project: Bool = true) async throws {
        try await cli.sh(
            posix: "diff -r before-config \"$HOME/.wendy\""
                + (project ? " && cmp before-project wendy.json" : ""),
            power: """
                $before = @(Get-ChildItem -File -Recurse 'before-config' | ForEach-Object { $_.FullName.Substring((Join-Path $pwd 'before-config').Length) + ':' + (Get-FileHash $_.FullName).Hash })
                $after = @(Get-ChildItem -File -Recurse '../.wendy' | ForEach-Object { $_.FullName.Substring((Resolve-Path '../.wendy').Path.Length) + ':' + (Get-FileHash $_.FullName).Hash })
                if (Compare-Object $before $after) { throw 'configuration changed' }
                \(project ? "if ((Get-FileHash 'before-project').Hash -ne (Get-FileHash 'wendy.json').Hash) { throw 'project changed' }" : "")
                """
        )
    }

    static func run(
        _ cli: WendyE2ESession,
        _ args: [String],
        exit: Int,
        monitorNetwork: Bool = false,
        monitorSavedDefault: Bool = false
    ) async throws -> WendyE2EShellResult {
        let posixArgs = args.map(quote).joined(separator: " ")
        let powerArgs = args.map(powerQuote).joined(separator: ", ")
        try #require(!monitorSavedDefault || monitorNetwork)
        let monitorFlag = monitorSavedDefault ? "--saved-default " : ""
        let posixCommand =
            monitorNetwork ? "python3 -I -c \(quote(networkCounter)) \(monitorFlag)wendy" : "wendy"
        let powerCommand =
            monitorNetwork
            ? "python -I -c \(powerQuote(networkCounter)) \(monitorFlag)wendy" : "wendy"
        let result = try await cli.sh(
            posix: """
                env -i HOME="$HOME" PATH="$PATH" TMPDIR="$TMPDIR" WENDY_CONFIG_DIR="$HOME/.wendy" \
                  WENDY_ANALYTICS=false WENDY_NO_UPDATE_CHECK=1 WENDY_SECRET_STORE=file CI=1 NO_COLOR=1 \
                  \(posixCommand) \(posixArgs)
                code=$?; printf '%s' "$code" > cli-exit; exit "$code"
                """,
            power: """
                Get-ChildItem Env:WENDY* | Remove-Item
                $env:WENDY_CONFIG_DIR = (Resolve-Path '../.wendy').Path
                $env:WENDY_ANALYTICS = 'false'; $env:WENDY_NO_UPDATE_CHECK = '1'; $env:WENDY_SECRET_STORE = 'file'
                $env:CI = '1'; $env:NO_COLOR = '1'
                & \(powerCommand) @(\(powerArgs))
                $code = $LASTEXITCODE; [IO.File]::WriteAllText((Join-Path $pwd 'cli-exit'), [string]$code); exit $code
                """
        ) { $0 }
        #expect(result.status.isSuccess == (exit == 0))
        try await cli.sh(
            posix: "test \"$(cat cli-exit)\" = \(exit)",
            power:
                "if ([IO.File]::ReadAllText((Join-Path $pwd 'cli-exit')) -ne '\(exit)') { throw 'wrong CLI exit status' }"
        )
        if monitorNetwork {
            let observation = try await read(cli, path: "network-observation.json")
            let counts = try #require(
                try JSONSerialization.jsonObject(with: Data(observation.utf8)) as? [String: Int]
            )
            var expected = ["positiveControl": 1, "connections": 0]
            if monitorSavedDefault {
                expected["defaultPositiveControl"] = 1
                expected["defaultConnections"] = 0
            }
            #expect(counts == expected)
        }
        return result
    }

    // Each listener gets a real handshake positive control. The optional LAN
    // default is bound in both config and its preservation snapshot BEFORE the
    // command starts, so fallback is observed rather than sent to a dead port.
    // Requires Python 3 on the CLI machine; saved-default mode is unauthed only.
    private static let networkCounter = """
        import contextlib, json, os, pathlib, shutil, socket, subprocess, sys
        def listen(stack):
            listener = stack.enter_context(socket.socket())
            listener.bind(('127.0.0.1', 0))
            listener.listen()
            listener.settimeout(2)
            address = listener.getsockname()
            with socket.create_connection(address, timeout=2):
                connection, _ = listener.accept()
                connection.close()
            listener.setblocking(False)
            return listener, '%s:%d' % address
        def connections(listener):
            count = 0
            while True:
                try:
                    connection, _ = listener.accept()
                except BlockingIOError:
                    return count
                count += 1
                connection.close()
        argv = sys.argv[1:]
        saved_default = argv[0] == '--saved-default'
        if saved_default:
            argv = argv[1:]
        with contextlib.ExitStack() as stack:
            listener, endpoint = listen(stack)
            default_listener = None
            if saved_default:
                default_listener, default_endpoint = listen(stack)
                config_path = pathlib.Path(os.environ['WENDY_CONFIG_DIR']) / 'config.json'
                snapshot = pathlib.Path('before-config/config.json')
                original = config_path.read_bytes()
                assert snapshot.read_bytes() == original, 'configuration changed before observation'
                config = json.loads(original)
                assert not config.get('auth'), 'saved-default observer requires an unauthenticated fixture'
                config['defaultDevice'] = default_endpoint
                observed_config = json.dumps(config).encode()
                config_path.write_bytes(observed_config)
                snapshot.write_bytes(observed_config)
            command = [shutil.which(argv[0])] + [value.replace('127.0.0.1:1', endpoint) for value in argv[1:]]
            try:
                result = subprocess.run(command, stdin=subprocess.DEVNULL, capture_output=True, timeout=10)
            finally:
                observation = {'positiveControl': 1, 'connections': connections(listener)}
                if saved_default:
                    observation.update(defaultPositiveControl=1, defaultConnections=connections(default_listener))
                with open('network-observation.json', 'w') as output:
                    json.dump(observation, output)
            sys.stdout.buffer.write(result.stdout)
            sys.stderr.buffer.write(result.stderr)
            sys.exit(result.returncode)
        """

    static func error(
        _ result: WendyE2EShellResult,
        code: String,
        exit: Int,
        message: String,
        precedingLines: [String]? = nil
    ) throws {
        #expect(result.stdout == "")
        // global-flags.md: only the final stderr line is the JSON error record.
        // Remove one line terminator, never skip blank or malformed final lines.
        var lines = result.normalizedStderr.components(separatedBy: "\n")
        if lines.last == "" {
            lines.removeLast()
        }
        let lastLine = try #require(lines.last)
        if let precedingLines {
            #expect(Array(lines.dropLast()) == precedingLines)
        }
        let object = try #require(
            try JSONSerialization.jsonObject(with: Data(lastLine.utf8)) as? [String: Any]
        )
        #expect(Set(object.keys) == ["error"])
        let error = try #require(object["error"] as? [String: Any])
        #expect(Set(error.keys) == ["code", "exit", "message", "retryable", "next_steps"])
        #expect(error["code"] as? String == code)
        #expect(error["exit"] as? Int == exit)
        #expect(error["retryable"] as? Bool == false)
        #expect((error["message"] as? String)?.contains(message) == true)
        let steps = try #require(error["next_steps"] as? [String])
        #expect(code == "cli_usage" ? steps.count == 1 : steps.isEmpty)
        #expect(!result.stderr.contains("\u{1B}"))
    }

    static func removedDeviceDefaults(
        _ cli: WendyE2ESession,
        parent: [String]
    ) async throws {
        let legacy = config.replacingOccurrences(
            of: "{\"analytics\"",
            with: "{\"defaultDevice\":\"127.0.0.1:1\",\"analytics\""
        )
        try await prepare(cli, config: legacy)
        for arguments in [
            ["set-default", "127.0.0.1:1"], ["set-default"],
            ["set-default", ""], ["set-default", "first", "second"],
            ["get-default"], ["unset-default"],
        ] {
            for format in ["--json", "--json=false"] {
                let result = try await run(cli, parent + arguments + [format], exit: 2)
                if format == "--json" {
                    try error(result, code: "cli_usage", exit: 2, message: "was removed")
                }
                #expect(result.stdout == "")
                #expect(result.readableStderr.contains("use --device <device> or set WENDY_DEVICE"))
                #expect(!result.stderr.contains("could not open a new TTY"))
                #expect(!result.stderr.contains("\u{1B}"))
                try await unchanged(cli)
            }
        }
    }

    static func entitlementChange(_ cli: WendyE2ESession, operation: String) async throws {
        try await prepare(cli)
        let base = try #require(
            try JSONSerialization.jsonObject(with: Data(project.utf8)) as? [String: Any]
        )
        let original = try #require(base["entitlements"] as? [[String: Any]])
        let network: [String: Any] = ["type": "network", "mode": "host"]
        var before = base
        if operation == "remove" {
            before["entitlements"] = original + [network]
            let text = String(
                decoding: try JSONSerialization.data(withJSONObject: before),
                as: UTF8.self
            )
            try await write(cli, path: "wendy.json", text: text)
        }
        let directory = try await cli.sh(posix: "pwd -P", power: "(Get-Location).Path") { result in
            try result.requireSuccess()
            return result.stdout.trimmingCharacters(in: .whitespacesAndNewlines)
        }
        let flags = operation == "add" ? ["--mode", "host"] : []
        let result = try await run(
            cli,
            ["project", "entitlements", operation, "network", "--json"] + flags,
            exit: 0
        )
        #expect(result.stderr == "")
        let output = try #require(
            try JSONSerialization.jsonObject(with: Data(result.stdout.utf8)) as? NSDictionary
        )
        let adding = operation == "add"
        let expected: [String: Any] = [
            "path": directory + (cli.machine.os == .windows ? "\\wendy.json" : "/wendy.json"),
            "saved": true, "dryRun": false, "warnings": NSNull(),
            "changes": [
                [
                    "path": "/entitlements/3/mode", "before": adding ? NSNull() as Any : "host",
                    "after": adding ? "host" as Any : NSNull(),
                ],
                [
                    "path": "/entitlements/3/type", "before": adding ? NSNull() as Any : "network",
                    "after": adding ? "network" as Any : NSNull(),
                ],
            ],
        ]
        #expect(output == expected as NSDictionary)
        let saved = try await read(cli, path: "wendy.json")
        let manifest = try #require(
            try JSONSerialization.jsonObject(with: Data(saved.utf8)) as? NSDictionary
        )
        var expectedManifest = base
        expectedManifest["entitlements"] = adding ? original + [network] : original
        #expect(manifest == expectedManifest as NSDictionary)
        try await unchanged(cli, project: false)
    }

    static func quote(_ value: String) -> String {
        "'" + value.replacingOccurrences(of: "'", with: "'\\''") + "'"
    }

    static func powerQuote(_ value: String) -> String {
        "'" + value.replacingOccurrences(of: "'", with: "''") + "'"
    }
}
