import Foundation
import Testing
import WendyE2ETesting

@Suite
struct `'wendy os cache clear'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays usage for `wendy os cache clear`. The output includes the command
     synopsis, local flags, inherited global flags, and concise
     descriptions. Help exits successfully, writes to stdout, emits no
     stderr, and leaves configuration, cache, project, cloud, and device
     state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy os cache clear --help") { result in
                let stdout = result.stdout

                #expect(result.status.isSuccess)
                #expect(stdout.contains("Clear all cached OS images"))
                #expect(stdout.contains("Usage:"))
                #expect(stdout.contains("wendy os cache clear [flags]"))
                #expect(stdout.contains("--help"))
                #expect(stdout.contains("--device"))
                #expect(stdout.contains("--json"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     Removes cached OS images and reports a concise summary. The command also
     succeeds when the OS image cache directory is already absent, reporting
     the same success without error.
     */
    @Test
    func `clears cached items and prints a summary`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            let cacheDirectory = cli.wendyCacheDirectory

            try await cli.sh(
                posix: """
                    mkdir -p "\(cacheDirectory)/os-images"
                    printf 'image\n' > "\(cacheDirectory)/os-images/wendyos.img"
                    wendy os cache clear
                    """,
                power: """
                    $osCacheDirectory = Join-Path (Join-Path $env:LOCALAPPDATA 'wendy') 'os-images'
                    New-Item -ItemType Directory -Force -Path $osCacheDirectory | Out-Null
                    Set-Content -LiteralPath (Join-Path $osCacheDirectory 'wendyos.img') -Value 'image'
                    wendy os cache clear
                    """
            ) { result in
                #expect(result.status.isSuccess)
                #expect(result.stdout == "OS image cache cleared.\n")
                #expect(result.stderr == "")
            }

            // Clearing an already-empty OS image cache is still a success.
            try await cli.sh("wendy os cache clear") { result in
                #expect(result.status.isSuccess)
                #expect(result.stdout == "OS image cache cleared.\n")
                #expect(result.stderr == "")
            }
        }
    }

    /**
     Clearing is scoped to the OS image cache. Wendy CLI config,
     authentication credentials, analytics identity, and other cache
     subdirectories outside `os-images` remain untouched.
     */
    @Test
    func `does not remove configuration or credentials`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            let cacheDirectory = cli.wendyCacheDirectory

            try await cli.sh(
                posix: """
                    mkdir -p "$HOME/.wendy"
                    printf '{"defaultBuildHost":"keep-me"}\n' > "$HOME/.wendy/config.json"
                    printf 'analytics-identity\n' > "$HOME/.wendy/analytics_id"
                    mkdir -p "\(cacheDirectory)/os-images"
                    printf 'image\n' > "\(cacheDirectory)/os-images/wendyos.img"
                    mkdir -p "\(cacheDirectory)/other"
                    printf 'keep\n' > "\(cacheDirectory)/other/keep.bin"
                    wendy os cache clear
                    test -f "$HOME/.wendy/config.json"
                    test -f "$HOME/.wendy/analytics_id"
                    test -f "\(cacheDirectory)/other/keep.bin"
                    cat "$HOME/.wendy/config.json"
                    """,
                power: """
                    $wendyDirectory = Join-Path $env:HOME '.wendy'
                    New-Item -ItemType Directory -Force -Path $wendyDirectory | Out-Null
                    Set-Content -LiteralPath (Join-Path $wendyDirectory 'config.json') -Value '{"defaultBuildHost":"keep-me"}'
                    Set-Content -LiteralPath (Join-Path $wendyDirectory 'analytics_id') -Value 'analytics-identity'
                    $cacheDirectory = Join-Path $env:LOCALAPPDATA 'wendy'
                    New-Item -ItemType Directory -Force -Path (Join-Path $cacheDirectory 'os-images') | Out-Null
                    Set-Content -LiteralPath (Join-Path $cacheDirectory 'os-images/wendyos.img') -Value 'image'
                    New-Item -ItemType Directory -Force -Path (Join-Path $cacheDirectory 'other') | Out-Null
                    Set-Content -LiteralPath (Join-Path $cacheDirectory 'other/keep.bin') -Value 'keep'
                    wendy os cache clear
                    if (-not (Test-Path -LiteralPath (Join-Path $wendyDirectory 'config.json'))) { throw 'config removed' }
                    if (-not (Test-Path -LiteralPath (Join-Path $wendyDirectory 'analytics_id'))) { throw 'analytics id removed' }
                    if (-not (Test-Path -LiteralPath (Join-Path $cacheDirectory 'other/keep.bin'))) { throw 'sibling cache removed' }
                    Get-Content -LiteralPath (Join-Path $wendyDirectory 'config.json')
                    """
            ) { result in
                #expect(result.status.isSuccess)
                #expect(result.stdout.contains("OS image cache cleared."))
                #expect(result.stdout.contains("keep-me"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     Permission errors while clearing the OS image cache are reported on
     stderr with a failure status. The command does not print a success
     summary when it could not remove the cache.
     */
    @Test
    func `reports filesystem errors without partial success output`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            let cacheDirectory = cli.wendyCacheDirectory

            try await cli.sh(
                posix: """
                    mkdir -p "\(cacheDirectory)/os-images/locked"
                    printf 'image\n' > "\(cacheDirectory)/os-images/locked/wendyos.img"
                    chmod 000 "\(cacheDirectory)/os-images/locked"
                    trap 'chmod 700 "\(cacheDirectory)/os-images/locked" 2>/dev/null || true' EXIT
                    wendy os cache clear
                    """,
                power: """
                    $source = @'
                    using System;
                    using System.Runtime.InteropServices;
                    using Microsoft.Win32.SafeHandles;
                    public static class WendyE2EOsDirectoryLock {
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

                    $osCacheDirectory = Join-Path (Join-Path $env:LOCALAPPDATA 'wendy') 'os-images'
                    $entry = Join-Path $osCacheDirectory 'locked'
                    New-Item -ItemType Directory -Force -Path $entry | Out-Null
                    Set-Content -LiteralPath (Join-Path $entry 'wendyos.img') -Value 'image'
                    $handle = [WendyE2EOsDirectoryLock]::CreateFile(
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
                        wendy os cache clear
                        $status = $LASTEXITCODE
                    } finally {
                        $handle.Dispose()
                    }
                    exit $status
                    """
            ) { result in
                #expect(!result.status.isSuccess)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("clearing OS image cache"))
                #expect(!result.stderr.contains("OS image cache cleared."))
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
            try await cli.sh("wendy os cache clear --bogus") { result in
                let stderr = result.stderr

                #expect(!result.status.isSuccess)
                #expect(result.stdout == "")
                #expect(stderr.contains("unknown flag"))
                #expect(stderr.contains("--bogus"))
                #expect(!stderr.contains("OS image cache cleared."))
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
            "WDY-1909: 'wendy os cache clear' currently ignores global --json and prints the human summary; JSON output is not implemented."
        )
    )
    func `prints JSON clear summary for automation`() async throws {
        // TODO: enable once 'wendy os cache clear' emits a JSON summary under --json (WDY-1909).
    }

    /**
     Rejects unexpected positional arguments with a usage diagnostic on
     stderr and a failure status, matching `wendy os cache list`, rather than
     silently ignoring them.
     */
    @Test(
        .disabled(
            "WDY-1934: 'wendy os cache clear' silently accepts and ignores extra positional arguments instead of rejecting them like 'wendy os cache list'."
        )
    )
    func `rejects unexpected positional arguments`() async throws {
        // TODO: enable once 'wendy os cache clear' rejects unexpected positional arguments.
    }
}
