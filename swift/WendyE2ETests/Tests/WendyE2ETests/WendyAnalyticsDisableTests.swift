import Foundation
import Testing
import WendyE2ETesting

@Suite
struct `'wendy analytics disable'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays usage for `wendy analytics disable`, including inherited global
     flags, without consulting authentication, project, or device state.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy analytics disable --help") { result in
                let stdout = result.stdout
                #expect(result.status.isSuccess)
                #expect(stdout.contains("Disable anonymous usage analytics"))
                #expect(stdout.contains("Usage:"))
                #expect(stdout.contains("wendy analytics disable [flags]"))
                #expect(stdout.contains("--help"))
                #expect(stdout.contains("--device"))
                #expect(stdout.contains("--json"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     Disabling analytics stores `analytics.enabled=false`, prints a concise
     confirmation to stdout, and emits nothing to stderr.
     */
    @Test(
        .disabled(
            "WDY-1935: 'wendy analytics disable' writes its normal confirmation to stderr instead of stdout."
        )
    )
    func `disables analytics and prints confirmation`() async throws {
        // TODO: enable when successful analytics command output uses stdout (WDY-1935).
    }

    /**
     Running the command repeatedly stores the same disabled preference.
     */
    @Test
    func `is idempotent when analytics is already disabled`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy analytics disable") { #expect($0.status.isSuccess) }
            try await cli.sh("wendy analytics disable") { #expect($0.status.isSuccess) }
            try await cli.sh(
                posix: "cat \"$HOME/.wendy/config.json\"",
                power: "Get-Content -Raw -LiteralPath (Join-Path $env:HOME '.wendy/config.json')"
            ) { result in
                #expect(result.stdout.contains("\"enabled\": false"))
            }
        }
    }

    /**
     Disabling analytics preserves known authentication-independent config
     fields and changes only the analytics preference.
     */
    @Test
    func `preserves unrelated configuration keys`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh(
                posix: """
                    mkdir -p "$HOME/.wendy"
                    printf '%s\n' '{"defaultBuildHost":"keep-device","lastCLIUpdateCheck":"2026-07-20T12:00:00Z","availableCLIUpdate":"2026.07.20","completionPromptDismissed":true,"analytics":{"enabled":true}}' > "$HOME/.wendy/config.json"
                    """,
                power: """
                    New-Item -ItemType Directory -Force -Path (Join-Path $env:HOME '.wendy') | Out-Null
                    Set-Content -LiteralPath (Join-Path $env:HOME '.wendy/config.json') -Value '{"defaultBuildHost":"keep-device","lastCLIUpdateCheck":"2026-07-20T12:00:00Z","availableCLIUpdate":"2026.07.20","completionPromptDismissed":true,"analytics":{"enabled":true}}'
                    """
            )

            try await cli.sh("wendy analytics disable") { #expect($0.status.isSuccess) }
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
                #expect(analytics["enabled"] as? Bool == false)
            }
        }
    }

    /**
     Disabling analytics retains the anonymous analytics identifier so a
     later enable can resume with the same identity.
     */
    @Test
    func `retains the analytics identifier file`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh(
                posix:
                    "mkdir -p \"$HOME/.wendy\"; printf 'stable-id\\n' > \"$HOME/.wendy/analytics_id\"",
                power: """
                    New-Item -ItemType Directory -Force -Path (Join-Path $env:HOME '.wendy') | Out-Null
                    Set-Content -LiteralPath (Join-Path $env:HOME '.wendy/analytics_id') -Value 'stable-id'
                    """
            )
            try await cli.sh("wendy analytics disable") { #expect($0.status.isSuccess) }
            try await cli.sh(
                posix: "cat \"$HOME/.wendy/analytics_id\"",
                power: "Get-Content -Raw -LiteralPath (Join-Path $env:HOME '.wendy/analytics_id')"
            ) { result in
                #expect(
                    result.stdout.trimmingCharacters(in: .whitespacesAndNewlines) == "stable-id"
                )
            }
        }
    }

    /**
     Extra positional arguments are rejected without changing config state.
     */
    @Test(
        .disabled(
            "WDY-1934: 'wendy analytics disable' silently accepts extra positional arguments because the leaf command has no cobra.NoArgs validator."
        )
    )
    func `rejects positional arguments`() async throws {
        // TODO: enable when analytics leaf commands reject positional arguments (WDY-1934).
    }

    /**
     Unknown flags fail before config mutation.
     */
    @Test
    func `rejects unknown flags`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh(
                posix:
                    "mkdir -p \"$HOME/.wendy\"; printf '{\"analytics\":{\"enabled\":true}}\\n' > \"$HOME/.wendy/config.json\"",
                power: """
                    New-Item -ItemType Directory -Force -Path (Join-Path $env:HOME '.wendy') | Out-Null
                    Set-Content -LiteralPath (Join-Path $env:HOME '.wendy/config.json') -Value '{"analytics":{"enabled":true}}'
                    """
            )
            try await cli.sh("wendy analytics disable --bogus") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("unknown flag"))
                #expect(result.stderr.contains("--bogus"))
            }
            try await cli.sh(
                posix: "cat \"$HOME/.wendy/config.json\"",
                power: "Get-Content -Raw -LiteralPath (Join-Path $env:HOME '.wendy/config.json')"
            ) { result in
                #expect(result.stdout.contains("\"enabled\":true"))
            }
        }
    }

    /**
     Creates the Wendy config directory when absent.
     */
    @Test
    func `creates the config directory when absent`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy analytics disable") { #expect($0.status.isSuccess) }
            try await cli.sh(
                posix: """
                    test -d "$HOME/.wendy"
                    if stat -f '%Lp' "$HOME/.wendy" >/dev/null 2>&1; then
                      stat -f '%Lp' "$HOME/.wendy"
                    else
                      stat -c '%a' "$HOME/.wendy"
                    fi
                    """,
                power: """
                    if (-not (Test-Path -LiteralPath (Join-Path $env:HOME '.wendy') -PathType Container)) { throw '.wendy missing' }
                    Write-Output 'present'
                    """
            ) { result in
                if cli.machine.os == .windows {
                    #expect(result.stdout.contains("present"))
                } else {
                    #expect(result.stdout.trimmingCharacters(in: .whitespacesAndNewlines) == "700")
                }
            }
        }
    }

    /**
     Treats a missing config file as empty and creates it with the disabled
     preference.
     */
    @Test
    func `creates the config file when absent`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy analytics disable") { #expect($0.status.isSuccess) }
            try await cli.sh(
                posix: "cat \"$HOME/.wendy/config.json\"",
                power: "Get-Content -Raw -LiteralPath (Join-Path $env:HOME '.wendy/config.json')"
            ) { result in
                #expect(result.stdout.contains("\"enabled\": false"))
            }
        }
    }

    /**
     Reports malformed config and leaves its bytes unchanged.
     */
    @Test
    func `reports invalid configuration without mutating the file`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh(
                posix:
                    "mkdir -p \"$HOME/.wendy\"; printf '{ invalid json\\n' > \"$HOME/.wendy/config.json\"",
                power: """
                    New-Item -ItemType Directory -Force -Path (Join-Path $env:HOME '.wendy') | Out-Null
                    Set-Content -NoNewline -LiteralPath (Join-Path $env:HOME '.wendy/config.json') -Value '{ invalid json'
                    """
            )
            try await cli.sh("wendy analytics disable") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("parsing config"))
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
     Writes config with owner-only POSIX permissions because the same file may
     contain authentication material.
     */
    @Test
    func `writes config with restricted permissions`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy analytics disable") { #expect($0.status.isSuccess) }
            try await cli.sh(
                posix: """
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
     Reports a read-only config write failure and leaves the preference
     unchanged.
     */
    @Test
    func `reports a read-only config file without mutating it`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh(
                posix: """
                    mkdir -p "$HOME/.wendy"
                    printf '{"analytics":{"enabled":true}}\n' > "$HOME/.wendy/config.json"
                    chmod 400 "$HOME/.wendy/config.json"
                    """,
                power: """
                    New-Item -ItemType Directory -Force -Path (Join-Path $env:HOME '.wendy') | Out-Null
                    $path = Join-Path $env:HOME '.wendy/config.json'
                    Set-Content -LiteralPath $path -Value '{"analytics":{"enabled":true}}'
                    (Get-Item -LiteralPath $path).IsReadOnly = $true
                    """
            )
            try await cli.sh(
                posix: """
                    wendy analytics disable
                    status=$?
                    chmod 600 "$HOME/.wendy/config.json"
                    exit $status
                    """,
                power: """
                    $path = Join-Path $env:HOME '.wendy/config.json'
                    try {
                        wendy analytics disable
                        $status = $LASTEXITCODE
                    } finally {
                        (Get-Item -LiteralPath $path).IsReadOnly = $false
                    }
                    exit $status
                    """
            ) { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("writing config"))
                #expect(!result.stderr.contains("Analytics disabled."))
            }
            try await cli.sh(
                posix: "cat \"$HOME/.wendy/config.json\"",
                power: "Get-Content -Raw -LiteralPath (Join-Path $env:HOME '.wendy/config.json')"
            ) { result in
                #expect(result.stdout.contains("\"enabled\":true"))
            }
        }
    }

    /**
     Stores the preference despite the `WENDY_ANALYTICS` runtime override.
     */
    @Test
    func `stores preference regardless of WENDY_ANALYTICS env var`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh(
                posix: "WENDY_ANALYTICS=true wendy analytics disable",
                power: """
                    $env:WENDY_ANALYTICS = 'true'
                    wendy analytics disable
                    exit $LASTEXITCODE
                    """
            ) { #expect($0.status.isSuccess) }
            try await cli.sh(
                posix: "cat \"$HOME/.wendy/config.json\"",
                power: "Get-Content -Raw -LiteralPath (Join-Path $env:HOME '.wendy/config.json')"
            ) { result in
                #expect(result.stdout.contains("\"enabled\": false"))
            }
        }
    }

    /**
     Stores the preference when CI detection disables runtime tracking.
     */
    @Test
    func `stores preference regardless of CI environment detection`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh(
                posix: "CI=true WENDY_ANALYTICS=true wendy analytics disable",
                power: """
                    $env:CI = 'true'
                    $env:WENDY_ANALYTICS = 'true'
                    wendy analytics disable
                    exit $LASTEXITCODE
                    """
            ) { #expect($0.status.isSuccess) }
            try await cli.sh(
                posix: "cat \"$HOME/.wendy/config.json\"",
                power: "Get-Content -Raw -LiteralPath (Join-Path $env:HOME '.wendy/config.json')"
            ) { result in
                #expect(result.stdout.contains("\"enabled\": false"))
            }
        }
    }

    /**
     On failure, emits a diagnostic only on stderr and returns non-zero.
     */
    @Test
    func `prints diagnostics to stderr on failure`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh(
                posix:
                    "mkdir -p \"$HOME/.wendy\"; printf '{ broken\\n' > \"$HOME/.wendy/config.json\"",
                power: """
                    New-Item -ItemType Directory -Force -Path (Join-Path $env:HOME '.wendy') | Out-Null
                    Set-Content -LiteralPath (Join-Path $env:HOME '.wendy/config.json') -Value '{ broken'
                    """
            )
            try await cli.sh("wendy analytics disable") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("parsing config"))
            }
        }
    }

    /**
     A later enable command reverses the stored preference while preserving
     the rest of config state.
     */
    @Test
    func `is reversible with analytics enable`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy analytics disable") { #expect($0.status.isSuccess) }
            try await cli.sh("wendy analytics enable") { #expect($0.status.isSuccess) }
            try await cli.sh(
                posix: "cat \"$HOME/.wendy/config.json\"",
                power: "Get-Content -Raw -LiteralPath (Join-Path $env:HOME '.wendy/config.json')"
            ) { result in
                #expect(result.stdout.contains("\"enabled\": true"))
            }
        }
    }

    /**
     Emits structured output describing the stored preference when `--json`
     is requested.
     */
    @Test(
        .disabled(
            "WDY-1909: 'wendy analytics disable --json' ignores JSON mode and prints the human confirmation."
        )
    )
    func `prints JSON disable result for automation`() async throws {
        // TODO: enable when analytics mutations implement global --json (WDY-1909).
    }
}
