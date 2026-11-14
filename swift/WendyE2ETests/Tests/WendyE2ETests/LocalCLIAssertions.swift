import Foundation
import Testing
import WendyE2ETesting

// Shared state for the local validation specs. No saved sign-in or device is used.
enum LocalCLIAssertions {
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
        monitorNetwork: Bool = false
    ) async throws -> WendyE2EShellResult {
        let posixArgs = args.map(quote).joined(separator: " ")
        let powerArgs = args.map(powerQuote).joined(separator: ", ")
        let posixCommand = monitorNetwork ? "python3 -c \(quote(networkCounter)) wendy" : "wendy"
        let powerCommand =
            monitorNetwork ? "python -c \(powerQuote(networkCounter)) wendy" : "wendy"
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
            #expect(counts == ["positiveControl": 1, "connections": 0])
        }
        return result
    }

    // A TCP handshake needs no service implementation. The positive control
    // proves the listener observes connections before the real CLI is launched.
    // Requires Python 3 on the CLI machine.
    private static let networkCounter = """
        import json, shutil, socket, subprocess, sys
        with socket.socket() as listener:
            listener.bind(('127.0.0.1', 0))
            listener.listen()
            listener.settimeout(2)
            address = listener.getsockname()
            with socket.create_connection(address, timeout=2):
                connection, _ = listener.accept()
                connection.close()
            endpoint = '%s:%d' % address
            command = [shutil.which(sys.argv[1])] + [value.replace('127.0.0.1:1', endpoint) for value in sys.argv[2:]]
            result = subprocess.run(command, stdin=subprocess.DEVNULL, capture_output=True, timeout=10)
            listener.setblocking(False)
            count = 0
            while True:
                try:
                    connection, _ = listener.accept()
                except BlockingIOError:
                    break
                count += 1
                connection.close()
            with open('network-observation.json', 'w') as output:
                json.dump({'positiveControl': 1, 'connections': count}, output)
            sys.stdout.buffer.write(result.stdout)
            sys.stderr.buffer.write(result.stderr)
            sys.exit(result.returncode)
        """

    static func error(
        _ result: WendyE2EShellResult,
        code: String,
        exit: Int,
        message: String
    ) throws {
        #expect(result.stdout == "")
        let object = try #require(
            try JSONSerialization.jsonObject(with: Data(result.stderr.utf8)) as? [String: Any]
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

    static func defaultSelection(
        _ cli: WendyE2ESession,
        command: [String],
        selectors: [[String]]
    ) async throws {
        try await prepare(cli, config: nil)
        // A fresh HOME, unrelated saved settings, and a saved default must all
        // reject the same missing/blank selection without creating or editing state.
        for state in [
            nil, config,
            config.replacingOccurrences(
                of: "{\"analytics\"",
                with: "{\"defaultDevice\":\"127.0.0.1:1\",\"analytics\""
            ),
        ] {
            if let state {
                try await write(cli, path: "../.wendy/config.json", text: state)
                try await write(cli, path: "before-config/config.json", text: state)
            }
            for selector in selectors {
                for format in ["--json", "--json=false"] {
                    let result = try await run(cli, command + selector + [format], exit: 4)
                    if format == "--json" {
                        try error(result, code: "no_device", exit: 4, message: "no device given")
                    }
                    #expect(result.stdout == "")
                    #expect(result.readableStderr.contains("wendy device set-default <device>"))
                    #expect(!result.stderr.contains("could not open a new TTY"))
                    #expect(!result.stderr.contains("\u{1B}"))
                    try await unchanged(cli)
                }
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
