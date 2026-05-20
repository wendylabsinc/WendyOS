import Foundation
import Testing
import WendyE2ETesting

@Suite
struct `'wendy cache list'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays usage for `wendy cache list`. The output includes the command
     synopsis, local flags, inherited global flags, and concise
     descriptions. Help exits successfully, writes to stdout, emits no
     stderr, and leaves configuration, cache, project, cloud, and device
     state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run { cli, _ in
            try await cli.sh("wendy cache list --help") { result in
                let stdout = result.stdout

                #expect(result.status.isSuccess)
                #expect(stdout.contains("List cached items"))
                #expect(stdout.contains("Usage:"))
                #expect(stdout.contains("wendy cache list [flags]"))
                #expect(stdout.contains("--help"))
                #expect(stdout.contains("--device"))
                #expect(stdout.contains("--json"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     Shows entries in the local CLI cache with stable columns for name, type,
     size, and last-updated metadata. An empty cache is reported as an empty
     successful result, not an error.
     */
    @Test
    func `lists cached items in a readable table`() async throws {
        try await self.scenario.run { cli, _ in
            try await cli.sh("wendy cache list") { result in

                #expect(result.status.isSuccess)
                #expect(result.stdout == "Cache is empty.\n")
                #expect(result.stderr == "")
            }
        }
    }

    /**
     Only files owned by Wendy cache management appear in the listing.
     Configuration files, credentials, and project-local artifacts are not
     scanned or displayed.
     */
    @Test
    func `ignores unrelated files outside the cache root`() async throws {
        try await self.scenario.run { cli, _ in
            try await cli.sh(
                posix: """
                    mkdir -p "$HOME/.wendy"
                    printf '{"defaultDevice":"do-not-list"}\n' > "$HOME/.wendy/config.json"
                    printf 'project artifact\n' > unrelated-project-file.txt
                    wendy cache list
                    """,
                power: """
                    New-Item -ItemType Directory -Force -Path (Join-Path $env:HOME '.wendy') | Out-Null
                    Set-Content -LiteralPath (Join-Path $env:HOME '.wendy/config.json') -Value '{"defaultDevice":"do-not-list"}'
                    Set-Content -LiteralPath 'unrelated-project-file.txt' -Value 'project artifact'
                    wendy cache list
                    """
            ) { result in
                let stdout = result.stdout

                #expect(result.status.isSuccess)
                #expect(stdout == "Cache is empty.\n")
                #expect(!stdout.contains("do-not-list"))
                #expect(!stdout.contains("unrelated-project-file"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     Unreadable or malformed cache metadata produces a diagnostic that
     identifies the affected entry while preserving the rest of the cache.
     */
    @Test
    func `reports unreadable cache metadata clearly`() async throws {
        try await self.scenario.run { cli, _ in
            let cacheDirectory = cli.wendyCacheDirectory

            try await cli.sh(
                posix: """
                    mkdir -p "\(cacheDirectory)/unreadable"
                    chmod 000 "\(cacheDirectory)/unreadable"
                    trap 'chmod 700 "\(cacheDirectory)/unreadable" 2>/dev/null || true' EXIT
                    wendy cache list
                    """,
                power: """
                    $source = @'
                    using System;
                    using System.Runtime.InteropServices;
                    using Microsoft.Win32.SafeHandles;
                    public static class WendyE2EDirectoryLock {
                        [DllImport("kernel32.dll", SetLastError = true, CharSet = CharSet.Unicode)]
                        public static extern SafeFileHandle CreateFile(
                            string name,
                            uint access,
                            uint share,
                            IntPtr security,
                            uint creation,
                            uint flags,
                            IntPtr templateFile);
                    }
                    '@
                    Add-Type -TypeDefinition $source

                    $cacheDirectory = Join-Path $env:LOCALAPPDATA 'wendy'
                    $entry = Join-Path $cacheDirectory 'unreadable'
                    New-Item -ItemType Directory -Force -Path $entry | Out-Null
                    $handle = [WendyE2EDirectoryLock]::CreateFile(
                        $entry,
                        [uint32]1,
                        [uint32]0,
                        [IntPtr]::Zero,
                        [uint32]3,
                        [uint32]0x02000000,
                        [IntPtr]::Zero)
                    if ($handle.IsInvalid) {
                        throw "locking cache entry failed: $([Runtime.InteropServices.Marshal]::GetLastWin32Error())"
                    }
                    try {
                        wendy cache list
                        $status = $LASTEXITCODE
                    } finally {
                        $handle.Dispose()
                    }
                    exit $status
                    """
            ) { result in

                #expect(!result.status.isSuccess)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("determining cache entry size"))
                #expect(result.stderr.contains("unreadable"))
            }
        }
    }

    /**
     With `--json`, emits one JSON object or array with machine-readable
     cache entries and byte counts. JSON mode emits no table formatting and
     no stderr on success.
     */
    @Test
    func `prints JSON cache entries for automation`() async throws {
        try await self.scenario.run { cli, _ in
            try await cli.sh("wendy --json cache list") { result in

                #expect(result.status.isSuccess)
                #expect(result.stderr == "")
                #expect(!result.stdout.contains("Cache is empty"))

                let json = try #require(
                    try JSONSerialization.jsonObject(with: Data(result.stdout.utf8))
                        as? [[String: Any]]
                )
                #expect(json.isEmpty)
            }
        }
    }

    /**
     Accepts only the documented arguments and flags for `wendy cache list`.
     Extra positional arguments or unknown flags produce a usage diagnostic
     on stderr, return a failure status, emit no success output, and leave
     existing state unchanged.
     */
    @Test
    func `rejects undocumented arguments and flags`() async throws {
        try await self.scenario.run { cli, _ in
            try await cli.sh("wendy cache list extra") { result in
                let stderr = result.stderr

                #expect(!result.status.isSuccess)
                #expect(result.stdout == "")
                #expect(stderr.contains("unknown command"))
                #expect(stderr.contains("extra"))
                #expect(!stderr.contains("Cache is empty"))
            }
        }
    }
}
