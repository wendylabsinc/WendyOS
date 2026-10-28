import Foundation
import Testing
import WendyE2ETesting

@Suite
struct `'wendy auth logout'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays usage for `wendy auth logout`. The output includes the command
     synopsis, local flags, inherited global flags, and concise
     descriptions. Help exits successfully, writes to stdout, emits no
     stderr, and leaves configuration, cache, project, cloud, and device
     state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy auth logout --help") { result in
                #expect(result.status.isSuccess)
                #expect(result.stdout.contains("Log out from Wendy Cloud"))
                #expect(result.stdout.contains("wendy auth logout [flags]"))
                #expect(result.stdout.contains("--json"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     Deletes stored Wendy Cloud credentials for the selected session and
     prints a concise confirmation. Other configuration keys and cached
     project files remain intact.
     */
    @Test
    func `removes the active auth session`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh(
                posix: """
                    mkdir -p "$HOME/.wendy"
                    printf '%s\n' '{"auth":[{"cloudDashboard":"https://fixture.invalid","cloudGRPC":"fixture.invalid:443","certificates":[{"organizationId":42,"userId":"fixture-user"}]}],"defaultBuildHost":"keep-device","lastCLIUpdateCheck":"2026-07-20T12:00:00Z"}' > "$HOME/.wendy/config.json"
                    """,
                power: """
                    New-Item -ItemType Directory -Force -Path (Join-Path $env:HOME '.wendy') | Out-Null
                    Set-Content -LiteralPath (Join-Path $env:HOME '.wendy/config.json') -Value '{"auth":[{"cloudDashboard":"https://fixture.invalid","cloudGRPC":"fixture.invalid:443","certificates":[{"organizationId":42,"userId":"fixture-user"}]}],"defaultBuildHost":"keep-device","lastCLIUpdateCheck":"2026-07-20T12:00:00Z"}'
                    """
            )
            try await cli.sh("wendy auth logout") { result in
                #expect(result.status.isSuccess)
                #expect(result.stdout.contains("Logged out"))
                #expect(result.stderr == "")
            }
            try await cli.sh(
                posix: "cat \"$HOME/.wendy/config.json\"",
                power: "Get-Content -Raw -LiteralPath (Join-Path $env:HOME '.wendy/config.json')"
            ) { result in
                let json = try #require(
                    try JSONSerialization.jsonObject(with: Data(result.stdout.utf8))
                        as? [String: Any]
                )
                #expect(json["auth"] == nil)
                #expect(json["defaultBuildHost"] as? String == "keep-device")
                #expect(json["lastCLIUpdateCheck"] as? String == "2026-07-20T12:00:00Z")
            }
        }
    }

    /**
     With no stored auth session, reports that the user is logged out and
     exits successfully without creating configuration files.
     */
    @Test(
        .disabled(
            "WDY-1947: already-logged-out logout creates/rewrites config and claims credentials were removed instead of remaining a non-mutating no-op."
        )
    )
    func `is idempotent when already logged out`() async throws {
        // TODO: enable when logged-out logout does not create state (WDY-1947).
    }

    /**
     When more than one auth session exists, targets the active or
     explicitly selected cloud and leaves unrelated sessions available.
     */
    @Test(
        .disabled(
            "WDY-1947: logout always clears every auth entry and has no active/explicit session selector, so unrelated cloud sessions cannot be preserved."
        )
    )
    func `selects one session when multiple clouds are configured`() async throws {
        // TODO: enable when logout is session-aware (WDY-1947).
    }

    /**
     With `--json`, emits one JSON object describing whether credentials
     were removed. JSON mode emits no human confirmation text and no
     stderr on success.
     */
    @Test(
        .disabled(
            "WDY-1909: 'wendy auth logout --json' ignores JSON mode and prints a human confirmation."
        )
    )
    func `prints JSON logout result for automation`() async throws {
        // TODO: enable when auth logout implements global --json (WDY-1909).
    }

    /**
     Reads the Wendy CLI configuration before performing work that depends on
     user state. Malformed configuration is reported as a configuration error,
     no prompts open, no network connection is attempted, and the original file
     remains byte-for-byte unchanged.
     */
    @Test
    func `reports invalid CLI configuration before acting`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh(
                posix:
                    "mkdir -p \"$HOME/.wendy\"; printf '{ broken\\n' > \"$HOME/.wendy/config.json\"",
                power: """
                    New-Item -ItemType Directory -Force -Path (Join-Path $env:HOME '.wendy') | Out-Null
                    Set-Content -NoNewline -LiteralPath (Join-Path $env:HOME '.wendy/config.json') -Value '{ broken'
                    """
            )
            try await cli.sh("wendy auth logout") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("parsing config"))
            }
            try await cli.sh(
                posix: "cat \"$HOME/.wendy/config.json\"",
                power: "Get-Content -Raw -LiteralPath (Join-Path $env:HOME '.wendy/config.json')"
            ) { result in
                #expect(result.stdout.contains("{ broken"))
            }
        }
    }

    /**
     Rejects flags that are not part of the command's documented interface.

     The command reports a usage error on stderr and does not perform the
     requested operation.
     */
    @Test
    func `rejects undocumented flags`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy auth logout --bogus") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("unknown flag"))
                #expect(result.stderr.contains("--bogus"))
            }
            try await cli.sh(
                posix: "test ! -f \"$HOME/.wendy/config.json\"",
                power:
                    "if (Test-Path -LiteralPath (Join-Path $env:HOME '.wendy/config.json')) { throw 'config created' }"
            )
        }
    }

    /**
     Rejects positional arguments because this command is entirely flag-driven.

     The command reports a usage error instead of treating undocumented input as
     a valid request.
     */
    @Test(
        .disabled(
            "WDY-1934: 'wendy auth logout' silently accepts extra positional arguments because the leaf command has no cobra.NoArgs validator."
        )
    )
    func `rejects undocumented positional arguments`() async throws {
        // TODO: enable when auth logout rejects positional arguments (WDY-1934).
    }
}
