// FILE: EnglishEntitlementAddTests.swift
import Foundation
import Testing
import WendyE2ETesting

@Suite("English spec adding a network entitlement")
struct EnglishEntitlementAddTests {
    /// Setup: Use the isolated CLI sandbox without authentication or an agent. Create
    /// wendy.json in its working directory with appId "sh.wendy.demo" and no entitlements.
    /// Action: Run `wendy project entitlements add network` and read the saved file.
    /// Expectations: The command succeeds and its JSON stdout has saved equal to true.
    /// The saved file is valid JSON, retains the app ID, and contains a network entitlement.
    /// Cleanup: Remove the case's wendy.json even after failure, preserving recordings
    /// and the isolated run directory without changing another project.
    @Test("English spec saves a network entitlement while preserving the app identifier")
    func addsNetworkEntitlement() async throws {
        let scenario = CLIAndAgentScenario(
            after: { cli, _ in
                try await cli.sh(
                    posix: "rm -f wendy.json",
                    power: """
                        $ErrorActionPreference = 'Stop'
                        if (Test-Path -LiteralPath 'wendy.json') {
                            Remove-Item -LiteralPath 'wendy.json' -Force -ErrorAction Stop
                        }
                        """
                )
            }
        )

        try await scenario.run(authenticated: false) { cli, _ in
            try await cli.sh(
                posix: """
                    printf '%s\\n' '{"appId":"sh.wendy.demo"}' > wendy.json
                    """,
                power: """
                    $ErrorActionPreference = 'Stop'
                    [System.IO.File]::WriteAllText(
                        (Join-Path (Get-Location).Path 'wendy.json'),
                        ('{"appId":"sh.wendy.demo"}' + "`n"),
                        [System.Text.UTF8Encoding]::new($false)
                    )
                    """
            )

            try await cli.sh("wendy project entitlements add network") { result in
                #expect(result.status.isSuccess)

                let summary = try #require(
                    try JSONSerialization.jsonObject(with: Data(result.stdout.utf8))
                        as? [String: Any]
                )
                let saved = try #require(summary["saved"] as? Bool)
                #expect(saved == true)
            }

            try await cli.sh(
                posix: "cat wendy.json",
                power: """
                    $ErrorActionPreference = 'Stop'
                    [Console]::Out.Write(
                        [System.IO.File]::ReadAllText(
                            (Join-Path (Get-Location).Path 'wendy.json')
                        )
                    )
                    """
            ) { result in
                try #require(result.status.isSuccess)

                let project = try #require(
                    try JSONSerialization.jsonObject(with: Data(result.stdout.utf8))
                        as? [String: Any]
                )
                #expect(project["appId"] as? String == "sh.wendy.demo")

                let entitlements = try #require(project["entitlements"] as? [[String: Any]])
                #expect(entitlements.contains { $0["type"] as? String == "network" })
            }
        }
    }
}
