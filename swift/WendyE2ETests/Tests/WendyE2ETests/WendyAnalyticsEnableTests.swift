import Foundation
import Testing
import WendyE2ETesting

@Suite
struct `'wendy analytics enable'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays usage for `wendy analytics enable`, including inherited global
     flags, without consulting authentication, project, or device state.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy analytics enable --help") { result in
                let stdout = result.stdout
                #expect(result.status.isSuccess)
                #expect(stdout.contains("Enable anonymous usage analytics"))
                #expect(stdout.contains("Usage:"))
                #expect(stdout.contains("wendy analytics enable [flags]"))
                #expect(stdout.contains("--help"))
                #expect(stdout.contains("--device"))
                #expect(stdout.contains("--json"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     Writes `analytics.enabled=true`, prints `Analytics enabled.` to stdout,
     emits no stderr, and exits successfully.
     */
    @Test(
        .disabled(
            "WDY-1935: 'wendy analytics enable' writes its normal confirmation to stderr instead of stdout."
        )
    )
    func `enables analytics and prints confirmation`() async throws {
        // TODO: enable when successful analytics command output uses stdout (WDY-1935).
    }

    /**
     Running the command repeatedly stores the same preference without
     changing an existing analytics identity.
     */
    @Test
    func `is idempotent when analytics is already enabled`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh(
                posix:
                    "mkdir -p \"$HOME/.wendy\"; printf 'stable-id\\n' > \"$HOME/.wendy/analytics_id\"",
                power: """
                    New-Item -ItemType Directory -Force -Path (Join-Path $env:HOME '.wendy') | Out-Null
                    Set-Content -LiteralPath (Join-Path $env:HOME '.wendy/analytics_id') -Value 'stable-id'
                    """
            )

            try await cli.sh("wendy analytics enable") { #expect($0.status.isSuccess) }
            try await cli.sh("wendy analytics enable") { #expect($0.status.isSuccess) }

            try await cli.sh(
                posix: "cat \"$HOME/.wendy/config.json\"; cat \"$HOME/.wendy/analytics_id\"",
                power: """
                    Get-Content -Raw -LiteralPath (Join-Path $env:HOME '.wendy/config.json')
                    Get-Content -Raw -LiteralPath (Join-Path $env:HOME '.wendy/analytics_id')
                    """
            ) { result in
                #expect(result.stdout.contains("\"enabled\": true"))
                #expect(result.stdout.contains("stable-id"))
            }
        }
    }

    /**
     Updates only the analytics preference while preserving known
     authentication, device, and update-check configuration fields.
     */
    @Test
    func `preserves unrelated known configuration keys`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh(
                posix: """
                    mkdir -p "$HOME/.wendy"
                    printf '%s\n' '{"defaultBuildHost":"keep-device","lastCLIUpdateCheck":"2026-07-20T12:00:00Z","availableCLIUpdate":"2026.07.20","completionPromptDismissed":true,"analytics":{"enabled":false}}' > "$HOME/.wendy/config.json"
                    """,
                power: """
                    New-Item -ItemType Directory -Force -Path (Join-Path $env:HOME '.wendy') | Out-Null
                    Set-Content -LiteralPath (Join-Path $env:HOME '.wendy/config.json') -Value '{"defaultBuildHost":"keep-device","lastCLIUpdateCheck":"2026-07-20T12:00:00Z","availableCLIUpdate":"2026.07.20","completionPromptDismissed":true,"analytics":{"enabled":false}}'
                    """
            )

            try await cli.sh("wendy analytics enable") { #expect($0.status.isSuccess) }
            try await cli.sh(
                posix: "cat \"$HOME/.wendy/config.json\"",
                power: "Get-Content -Raw -LiteralPath (Join-Path $env:HOME '.wendy/config.json')"
            ) { result in
                let json = try #require(
                    try JSONSerialization.jsonObject(with: Data(result.stdout.utf8))
                        as? [String: Any]
                )
                #expect(json["defaultBuildHost"] as? String == "keep-device")
                #expect(json["lastCLIUpdateCheck"] as? String == "2026-07-20T12:00:00Z")
                #expect(json["availableCLIUpdate"] as? String == "2026.07.20")
                #expect(json["completionPromptDismissed"] as? Bool == true)
                let analytics = try #require(json["analytics"] as? [String: Any])
                #expect(analytics["enabled"] as? Bool == true)
            }
        }
    }

    /**
     Preserves unrecognized top-level fields so an older CLI cannot erase
     configuration introduced by a newer Wendy component.
     */
    @Test(
        .disabled(
            "WDY-1940: typed config load/save currently discards unknown top-level keys during analytics mutations."
        )
    )
    func `preserves unknown future configuration keys`() async throws {
        // TODO: enable when config mutations round-trip unknown fields (WDY-1940).
    }

    /**
     Creates the Wendy config directory and file when absent, storing the
     enabled preference with restrictive POSIX permissions.
     */
    @Test
    func `creates missing configuration state`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy analytics enable") { result in
                #expect(result.status.isSuccess)
            }

            try await cli.sh(
                posix: """
                    test -f "$HOME/.wendy/config.json"
                    if stat -f '%Lp' "$HOME/.wendy/config.json" >/dev/null 2>&1; then
                      stat -f '%Lp' "$HOME/.wendy/config.json"
                    else
                      stat -c '%a' "$HOME/.wendy/config.json"
                    fi
                    """,
                power: """
                    $path = Join-Path $env:HOME '.wendy/config.json'
                    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) { throw 'config.json missing' }
                    Write-Output 'present'
                    """
            ) { result in
                if cli.machine.os == .windows {
                    #expect(result.stdout.contains("present"))
                } else {
                    #expect(result.stdout.trimmingCharacters(in: .whitespacesAndNewlines) == "600")
                }
            }
        }
    }

    /**
     Malformed configuration is reported before mutation and remains
     byte-for-byte unchanged.
     */
    @Test
    func `reports invalid CLI configuration before acting`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh(
                posix:
                    "mkdir -p \"$HOME/.wendy\"; printf '{ invalid json\\n' > \"$HOME/.wendy/config.json\"",
                power: """
                    New-Item -ItemType Directory -Force -Path (Join-Path $env:HOME '.wendy') | Out-Null
                    Set-Content -NoNewline -LiteralPath (Join-Path $env:HOME '.wendy/config.json') -Value '{ invalid json'
                    """
            )

            try await cli.sh("wendy analytics enable") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("parsing config"))
                #expect(!result.stderr.contains("Analytics enabled."))
            }
            try await cli.sh(
                posix: "cat \"$HOME/.wendy/config.json\"",
                power: "Get-Content -Raw -LiteralPath (Join-Path $env:HOME '.wendy/config.json')"
            ) { result in
                #expect(result.stdout.contains("{ invalid json"))
            }
        }
    }

    /**
     Unknown flags produce a failure diagnostic and leave existing config
     state unchanged.
     */
    @Test
    func `rejects undocumented flags`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh(
                posix:
                    "mkdir -p \"$HOME/.wendy\"; printf '{\"analytics\":{\"enabled\":false}}\\n' > \"$HOME/.wendy/config.json\"",
                power: """
                    New-Item -ItemType Directory -Force -Path (Join-Path $env:HOME '.wendy') | Out-Null
                    Set-Content -LiteralPath (Join-Path $env:HOME '.wendy/config.json') -Value '{"analytics":{"enabled":false}}'
                    """
            )

            try await cli.sh("wendy analytics enable --bogus") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("unknown flag"))
                #expect(result.stderr.contains("--bogus"))
            }
            try await cli.sh(
                posix: "cat \"$HOME/.wendy/config.json\"",
                power: "Get-Content -Raw -LiteralPath (Join-Path $env:HOME '.wendy/config.json')"
            ) { result in
                #expect(result.stdout.contains("\"enabled\":false"))
            }
        }
    }

    /**
     Extra positional arguments are rejected instead of being silently
     ignored while the preference is changed.
     */
    @Test(
        .disabled(
            "WDY-1934: 'wendy analytics enable' silently accepts extra positional arguments because the leaf command has no cobra.NoArgs validator."
        )
    )
    func `rejects undocumented positional arguments`() async throws {
        // TODO: enable when analytics leaf commands reject positional arguments (WDY-1934).
    }

    /**
     Stores the explicit preference even when runtime environment kill
     switches make analytics ineffective for this process.
     */
    @Test
    func `stores preference regardless of analytics environment overrides`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh(
                posix: "CI=true WENDY_ANALYTICS=false wendy analytics enable",
                power: """
                    $env:CI = 'true'
                    $env:WENDY_ANALYTICS = 'false'
                    wendy analytics enable
                    exit $LASTEXITCODE
                    """
            ) { result in
                #expect(result.status.isSuccess)
            }
            try await cli.sh(
                posix: "cat \"$HOME/.wendy/config.json\"",
                power: "Get-Content -Raw -LiteralPath (Join-Path $env:HOME '.wendy/config.json')"
            ) { result in
                #expect(result.stdout.contains("\"enabled\": true"))
            }
        }
    }
}
