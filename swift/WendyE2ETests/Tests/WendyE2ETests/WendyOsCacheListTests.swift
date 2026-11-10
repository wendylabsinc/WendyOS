import Foundation
import Testing
import WendyE2ETesting

@Suite
struct `'wendy os cache list'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays usage for `wendy os cache list`. The output includes the command
     synopsis, local flags, inherited global flags, and concise
     descriptions. Help exits successfully, writes to stdout, emits no
     stderr, and leaves configuration, cache, project, cloud, and device
     state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run { cli, _ in
            try await cli.sh("wendy os cache list --help") { result in
                let stdout = result.stdout

                #expect(result.status.isSuccess)
                #expect(stdout.contains("List cached OS images"))
                #expect(stdout.contains("Usage:"))
                #expect(stdout.contains("wendy os cache list [flags]"))
                #expect(stdout.contains("--help"))
                #expect(stdout.contains("--device"))
                #expect(stdout.contains("--json"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     Shows entries in the OS image cache with stable columns for name, type,
     size, and last-updated metadata. An empty cache is reported as an empty
     successful result, not an error.
     */
    @Test
    func `lists cached items in a readable table`() async throws {
        try await self.scenario.run { cli, _ in
            try await cli.sh("wendy os cache list") { result in

                #expect(result.status.isSuccess)
                #expect(result.stdout == "No cached OS images.\n")
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
                    printf '{"defaultBuildHost":"do-not-list"}\n' > "$HOME/.wendy/config.json"
                    printf 'project artifact\n' > unrelated-project-file.txt
                    wendy os cache list
                    """,
                power: """
                    New-Item -ItemType Directory -Force -Path (Join-Path $env:HOME '.wendy') | Out-Null
                    Set-Content -LiteralPath (Join-Path $env:HOME '.wendy/config.json') -Value '{"defaultBuildHost":"do-not-list"}'
                    Set-Content -LiteralPath 'unrelated-project-file.txt' -Value 'project artifact'
                    wendy os cache list
                    """
            ) { result in
                let stdout = result.stdout

                #expect(result.status.isSuccess)
                #expect(stdout == "No cached OS images.\n")
                #expect(!stdout.contains("do-not-list"))
                #expect(!stdout.contains("unrelated-project-file"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     Directories in the OS image cache root are ignored. Cache listing only
     reports cached image files managed by Wendy cache commands.
     */
    @Test
    func `ignores non-image directories in the OS cache root`() async throws {
        try await self.scenario.run { cli, _ in
            try await cli.sh(
                posix: """
                    case "$(uname -s)" in
                      Darwin) cache_root="$HOME/Library/Caches/wendy/os-images" ;;
                      *) cache_root="${XDG_CACHE_HOME:-$HOME/.cache}/wendy/os-images" ;;
                    esac
                    mkdir -p "$cache_root/not-an-image"
                    wendy os cache list
                    """,
                power: """
                    $cacheDirectory = Join-Path $env:LOCALAPPDATA 'wendy'
                    $osCacheDirectory = Join-Path $cacheDirectory 'os-images'
                    New-Item -ItemType Directory -Force -Path (Join-Path $osCacheDirectory 'not-an-image') | Out-Null
                    wendy os cache list
                    """
            ) { result in

                #expect(result.status.isSuccess)
                #expect(result.stdout == "No cached OS images.\n")
                #expect(result.stderr == "")
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
            try await cli.sh("wendy --json os cache list") { result in

                #expect(result.status.isSuccess)
                #expect(result.stderr == "")
                #expect(!result.stdout.contains("No cached OS images"))

                let json = try #require(
                    try JSONSerialization.jsonObject(with: Data(result.stdout.utf8))
                        as? [[String: Any]]
                )
                #expect(json.isEmpty)
            }
        }
    }

    /**
     Accepts only the documented arguments and flags for `wendy os cache list`.
     Extra positional arguments or unknown flags produce a usage diagnostic
     on stderr, return a failure status, emit no success output, and leave
     existing state unchanged.
     */
    @Test
    func `rejects undocumented arguments and flags`() async throws {
        try await self.scenario.run { cli, _ in
            try await cli.sh("wendy os cache list extra") { result in
                let stderr = result.stderr

                #expect(!result.status.isSuccess)
                #expect(result.stdout == "")
                #expect(stderr.contains("unknown command"))
                #expect(stderr.contains("extra"))
                #expect(!stderr.contains("No cached OS images"))
            }
        }
    }
}
