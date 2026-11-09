// FILE: EnglishDeviceDefaultTests.swift
import Foundation
import Testing
import WendyE2ETesting

@Suite("English spec saving a default device")
struct EnglishDeviceDefaultTests {
    /// Setup: Use the isolated CLI sandbox without authentication or an agent. Create
    /// .wendy/config.json in its isolated home with analytics.enabled false,
    /// lastCLIUpdateCheck "2026-07-20T12:00:00Z", and completionPromptDismissed true.
    /// Include no credentials.
    /// Action: Run `wendy device set-default 127.0.0.1:1` and read the saved config.
    /// Expectations: The command succeeds. The saved JSON has defaultDevice
    /// "127.0.0.1:1" and preserves all three seeded values exactly. Saving this
    /// hostname requires no connection to the address.
    /// Cleanup: Remove the case's config file even after failure, preserving recordings
    /// and the isolated run directory without reading or modifying the user's config.
    @Test("English spec preserves known configuration when setting the default device")
    func setsDefaultDevice() async throws {
        let scenario = CLIAndAgentScenario(
            after: { cli, _ in
                try await cli.sh(
                    posix: """
                        rm -f "$HOME/.wendy/config.json"
                        """,
                    power: """
                        $ErrorActionPreference = 'Stop'
                        $configPath = Join-Path $env:HOME '.wendy/config.json'
                        if (Test-Path -LiteralPath $configPath) {
                            Remove-Item -LiteralPath $configPath -Force -ErrorAction Stop
                        }
                        """
                )
            }
        )

        try await scenario.run(authenticated: false) { cli, _ in
            try await cli.sh(
                posix: """
                    mkdir -p "$HOME/.wendy" &&
                    printf '%s\\n' '{"analytics":{"enabled":false},"lastCLIUpdateCheck":"2026-07-20T12:00:00Z","completionPromptDismissed":true}' > "$HOME/.wendy/config.json"
                    """,
                power: """
                    $ErrorActionPreference = 'Stop'
                    $configDirectory = Join-Path $env:HOME '.wendy'
                    [System.IO.Directory]::CreateDirectory($configDirectory) | Out-Null
                    [System.IO.File]::WriteAllText(
                        (Join-Path $configDirectory 'config.json'),
                        ('{"analytics":{"enabled":false},"lastCLIUpdateCheck":"2026-07-20T12:00:00Z","completionPromptDismissed":true}' + "`n"),
                        [System.Text.UTF8Encoding]::new($false)
                    )
                    """
            )

            try await cli.sh("wendy device set-default 127.0.0.1:1") { result in
                #expect(result.status.isSuccess)
            }

            try await cli.sh(
                posix: """
                    cat "$HOME/.wendy/config.json"
                    """,
                power: """
                    $ErrorActionPreference = 'Stop'
                    [Console]::Out.Write(
                        [System.IO.File]::ReadAllText(
                            (Join-Path $env:HOME '.wendy/config.json')
                        )
                    )
                    """
            ) { result in
                try #require(result.status.isSuccess)

                let configuration = try #require(
                    try JSONSerialization.jsonObject(with: Data(result.stdout.utf8))
                        as? [String: Any]
                )
                #expect(configuration["defaultDevice"] as? String == "127.0.0.1:1")
                #expect(
                    configuration["lastCLIUpdateCheck"] as? String == "2026-07-20T12:00:00Z"
                )

                let analytics = try #require(configuration["analytics"] as? [String: Any])
                let analyticsEnabled = try #require(analytics["enabled"] as? Bool)
                #expect(analyticsEnabled == false)

                let completionPromptDismissed = try #require(
                    configuration["completionPromptDismissed"] as? Bool
                )
                #expect(completionPromptDismissed == true)
            }
        }
    }
}
