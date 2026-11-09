// FILE: EnglishEntitlementInvalidTests.swift
import Foundation
import Testing
import WendyE2ETesting

@Suite("English spec rejecting an unknown entitlement")
struct EnglishEntitlementInvalidTests {
    /// Setup: Use the isolated CLI sandbox without authentication or an agent. Create
    /// wendy.json containing exactly {"appId":"sh.wendy.demo"} followed by a newline,
    /// and preserve those original bytes for comparison.
    /// Action: Run `wendy project entitlements add nonsense` and read the file back.
    /// Expectations: The command fails with empty stdout. Its readable stderr names
    /// nonsense and includes network among the valid choices. The file's bytes remain
    /// identical, its JSON retains the original app ID, and it has no entitlements key.
    /// Cleanup: Remove the case's wendy.json even after failure, preserving recordings
    /// and the isolated run directory without changing another project.
    @Test("English spec rejects nonsense without changing the project bytes")
    func rejectsUnknownEntitlement() async throws {
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
            let originalBytes = Data("{\"appId\":\"sh.wendy.demo\"}\n".utf8)

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

            try await cli.sh("wendy project entitlements add nonsense") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout.isEmpty)
                #expect(result.readableStderr.contains("nonsense"))
                #expect(result.readableStderr.contains("network"))
            }

            try await cli.sh(
                posix: "base64 < wendy.json",
                power: """
                    $ErrorActionPreference = 'Stop'
                    [Console]::Out.Write(
                        [Convert]::ToBase64String(
                            [System.IO.File]::ReadAllBytes(
                                (Join-Path (Get-Location).Path 'wendy.json')
                            )
                        )
                    )
                    """
            ) { result in
                try #require(result.status.isSuccess)

                let encodedBytes = result.stdout.filter { !$0.isWhitespace }
                let savedBytes = try #require(Data(base64Encoded: encodedBytes))
                #expect(savedBytes == originalBytes)

                let project = try #require(
                    try JSONSerialization.jsonObject(with: savedBytes) as? [String: Any]
                )
                #expect(project["appId"] as? String == "sh.wendy.demo")
                #expect(project["entitlements"] == nil)
            }
        }
    }
}
