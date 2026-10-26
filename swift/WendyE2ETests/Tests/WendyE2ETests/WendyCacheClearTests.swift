import Foundation
import Testing
import WendyE2ETesting

@Suite
struct `'wendy cache clear'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays usage for `wendy cache clear`. The output includes the command
     synopsis, local flags, inherited global flags, and concise
     descriptions. Help exits successfully, writes to stdout, emits no
     stderr, and leaves configuration, cache, project, cloud, and device
     state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy cache clear --help") { result in
                let stdout = result.stdout

                #expect(result.status.isSuccess)
                #expect(stdout.contains("Clear the local cache"))
                #expect(stdout.contains("Usage:"))
                #expect(stdout.contains("wendy cache clear [flags]"))
                #expect(stdout.contains("--help"))
                #expect(stdout.contains("--device"))
                #expect(stdout.contains("--json"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     Removes entries from the local CLI cache and reports a concise summary.
     The command also succeeds when the cache directory is already absent,
     reporting the same success without error.
     */
    @Test
    func `clears cached items and prints a summary`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            let cacheDirectory = cli.wendyCacheDirectory

            try await cli.sh(
                posix: """
                    mkdir -p "\(cacheDirectory)/images"
                    printf 'cached\n' > "\(cacheDirectory)/images/entry.bin"
                    wendy cache clear
                    """,
                power: """
                    $cacheDirectory = Join-Path $env:LOCALAPPDATA 'wendy'
                    New-Item -ItemType Directory -Force -Path (Join-Path $cacheDirectory 'images') | Out-Null
                    Set-Content -LiteralPath (Join-Path $cacheDirectory 'images/entry.bin') -Value 'cached'
                    wendy cache clear
                    """
            ) { result in
                #expect(result.status.isSuccess)
                #expect(result.stdout == "Cache cleared.\n")
                #expect(result.stderr == "")
            }

            // Clearing an already-empty cache is still a success, not an error.
            try await cli.sh("wendy cache clear") { result in
                #expect(result.status.isSuccess)
                #expect(result.stdout == "Cache cleared.\n")
                #expect(result.stderr == "")
            }
        }
    }

    /**
     Cache clearing is limited to cache directories. Wendy CLI config,
     authentication credentials, analytics identity, and project files
     outside the cache remain untouched.
     */
    @Test
    func `does not remove configuration or credentials`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            let cacheDirectory = cli.wendyCacheDirectory

            try await cli.sh(
                posix: """
                    mkdir -p "$HOME/.wendy"
                    printf '{"defaultDevice":"keep-me"}\n' > "$HOME/.wendy/config.json"
                    printf 'analytics-identity\n' > "$HOME/.wendy/analytics_id"
                    printf 'project artifact\n' > project-file.txt
                    mkdir -p "\(cacheDirectory)/images"
                    printf 'cached\n' > "\(cacheDirectory)/images/entry.bin"
                    wendy cache clear
                    test -f "$HOME/.wendy/config.json"
                    test -f "$HOME/.wendy/analytics_id"
                    test -f project-file.txt
                    cat "$HOME/.wendy/config.json"
                    """,
                power: """
                    $wendyDirectory = Join-Path $env:HOME '.wendy'
                    New-Item -ItemType Directory -Force -Path $wendyDirectory | Out-Null
                    Set-Content -LiteralPath (Join-Path $wendyDirectory 'config.json') -Value '{"defaultDevice":"keep-me"}'
                    Set-Content -LiteralPath (Join-Path $wendyDirectory 'analytics_id') -Value 'analytics-identity'
                    Set-Content -LiteralPath 'project-file.txt' -Value 'project artifact'
                    $cacheDirectory = Join-Path $env:LOCALAPPDATA 'wendy'
                    New-Item -ItemType Directory -Force -Path (Join-Path $cacheDirectory 'images') | Out-Null
                    Set-Content -LiteralPath (Join-Path $cacheDirectory 'images/entry.bin') -Value 'cached'
                    wendy cache clear
                    if (-not (Test-Path -LiteralPath (Join-Path $wendyDirectory 'config.json'))) { throw 'config removed' }
                    if (-not (Test-Path -LiteralPath (Join-Path $wendyDirectory 'analytics_id'))) { throw 'analytics id removed' }
                    if (-not (Test-Path -LiteralPath 'project-file.txt')) { throw 'project file removed' }
                    Get-Content -LiteralPath (Join-Path $wendyDirectory 'config.json')
                    """
            ) { result in
                #expect(result.status.isSuccess)
                #expect(result.stdout.contains("Cache cleared."))
                #expect(result.stdout.contains("keep-me"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     Permission errors while clearing the cache are reported on stderr with a
     failure status. The command does not print a success summary when it
     could not remove the cache.
     */
    @Test
    func `reports filesystem errors without partial success output`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            let cacheDirectory = cli.wendyCacheDirectory

            // The CLI makes the cache tree writable itself, so block the removal from the parent.
            try await cli.sh(
                posix: """
                    mkdir -p "\(cacheDirectory)"
                    printf 'cached\n' > "\(cacheDirectory)/entry.bin"
                    parent=$(dirname "\(cacheDirectory)")
                    chmod a-w "$parent"
                    trap 'chmod u+w "$parent" 2>/dev/null || true' EXIT
                    wendy cache clear
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
                    $entry = Join-Path $cacheDirectory 'locked'
                    New-Item -ItemType Directory -Force -Path $entry | Out-Null
                    Set-Content -LiteralPath (Join-Path $entry 'entry.bin') -Value 'cached'
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
                        wendy cache clear
                        $status = $LASTEXITCODE
                    } finally {
                        $handle.Dispose()
                    }
                    exit $status
                    """
            ) { result in
                #expect(!result.status.isSuccess)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("clearing cache"))
                #expect(!result.stderr.contains("Cache cleared."))
            }
        }
    }

    /**
     Unknown flags are rejected with a usage diagnostic on stderr, a failure
     status, and no success summary on stdout, leaving the cache untouched.
     */
    @Test
    func `rejects undocumented flags`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy cache clear --bogus") { result in
                let stderr = result.stderr

                #expect(!result.status.isSuccess)
                #expect(result.stdout == "")
                #expect(stderr.contains("unknown flag"))
                #expect(stderr.contains("--bogus"))
                #expect(!stderr.contains("Cache cleared."))
            }
        }
    }

    /**
     With `--json`, emits one JSON object describing removed item counts,
     removed byte totals, skipped entries, and the cache path so automation
     can consume the result.
     */
    @Test(
        .disabled(
            "WDY-1909: 'wendy cache clear' currently ignores global --json and prints the human summary; JSON output is not implemented."
        )
    )
    func `prints JSON clear summary for automation`() async throws {
        // TODO: enable once 'wendy cache clear' emits a JSON summary under --json (WDY-1909).
    }

    /**
     Rejects unexpected positional arguments with a usage diagnostic on
     stderr and a failure status, matching `wendy cache list`, rather than
     silently ignoring them.
     */
    @Test(
        .disabled(
            "WDY-1934: 'wendy cache clear' silently accepts and ignores extra positional arguments instead of rejecting them like 'wendy cache list'."
        )
    )
    func `rejects unexpected positional arguments`() async throws {
        // TODO: enable once 'wendy cache clear' rejects unexpected positional arguments.
    }
}
