// FILE: EnglishCacheClearTests.swift
import Foundation
import Testing
import WendyE2ETesting

@Suite("English spec cache clearing")
struct EnglishCacheClearTests {
    /// Setup: Use the isolated CLI sandbox without authentication or an agent. Create
    /// images/entry.bin in the CLI's Wendy cache directory containing "cached\n".
    /// Action: Run `wendy cache clear`, check its result and the file's absence, then
    /// run the command again.
    /// Expectations: Both invocations succeed, print exactly "Cache cleared.\n" to
    /// stdout, and print nothing to stderr. The seeded file is absent after the first.
    /// Cleanup: Remove the case-owned images directory even after failure, preserving
    /// recordings, the isolated run directory, and the user's real home and cache.
    @Test("English spec removes cached entries and safely repeats")
    func clearsCacheAndRepeats() async throws {
        let scenario = CLIAndAgentScenario(
            after: { cli, _ in
                let directory = "\(cli.wendyCacheDirectory)/images"
                try await cli.sh(
                    posix: "rm -rf \(EnglishCacheClearPaths.posix(directory))",
                    power: """
                        $ErrorActionPreference = 'Stop'
                        if (Test-Path -LiteralPath \(EnglishCacheClearPaths.powerShell(directory))) {
                            Remove-Item -LiteralPath \(EnglishCacheClearPaths.powerShell(directory)) -Recurse -Force -ErrorAction Stop
                        }
                        """
                )
            }
        )

        try await scenario.run(authenticated: false) { cli, _ in
            let directory = "\(cli.wendyCacheDirectory)/images"
            let entry = "\(directory)/entry.bin"

            try await cli.sh(
                posix: """
                    mkdir -p \(EnglishCacheClearPaths.posix(directory)) &&
                    printf 'cached\\n' > \(EnglishCacheClearPaths.posix(entry))
                    """,
                power: """
                    $ErrorActionPreference = 'Stop'
                    [System.IO.Directory]::CreateDirectory(\(EnglishCacheClearPaths.powerShell(directory))) | Out-Null
                    [System.IO.File]::WriteAllText(
                        \(EnglishCacheClearPaths.powerShell(entry)),
                        "cached`n",
                        [System.Text.UTF8Encoding]::new($false)
                    )
                    """
            )

            try await cli.sh("wendy cache clear") { result in
                #expect(result.status.isSuccess)
                #expect(result.stdout == "Cache cleared.\n")
                #expect(result.stderr.isEmpty)
            }

            try await cli.sh(
                posix: "test ! -e \(EnglishCacheClearPaths.posix(entry))",
                power: """
                    $ErrorActionPreference = 'Stop'
                    if (Test-Path -LiteralPath \(EnglishCacheClearPaths.powerShell(entry))) {
                        throw 'The seeded cache file still exists.'
                    }
                    """
            )

            try await cli.sh("wendy cache clear") { result in
                #expect(result.status.isSuccess)
                #expect(result.stdout == "Cache cleared.\n")
                #expect(result.stderr.isEmpty)
            }
        }
    }
}

private enum EnglishCacheClearPaths {
    static func posix(_ path: String) -> String {
        "'" + path.replacingOccurrences(of: "'", with: "'\"'\"'") + "'"
    }

    static func powerShell(_ path: String) -> String {
        "'" + path.replacingOccurrences(of: "'", with: "''") + "'"
    }
}
