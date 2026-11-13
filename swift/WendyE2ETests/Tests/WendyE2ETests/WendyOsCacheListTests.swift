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
                    printf '{"defaultDevice":"do-not-list"}\n' > "$HOME/.wendy/config.json"
                    printf 'project artifact\n' > unrelated-project-file.txt
                    wendy os cache list
                    """,
                power: """
                    New-Item -ItemType Directory -Force -Path (Join-Path $env:HOME '.wendy') | Out-Null
                    Set-Content -LiteralPath (Join-Path $env:HOME '.wendy/config.json') -Value '{"defaultDevice":"do-not-list"}'
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

    /**
     The OS inventory includes regular images and recursively sized extracted
     flashpacks. Checksum stamps, other directories, and build caches are excluded.
     JSON reports exact bytes; the plain list reports the same downloads in MiB.
     Listing never edits the files. POSIX hosts only.
     */
    @Test(.enabled(if: WendyE2EMachine.cli.os != .windows, "POSIX cache fixture"))
    func `lists populated images and flashpacks with exact bytes and OS-only scope`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await CacheCommandFixture.populate(cli)
            try await cli.sh(
                """
                \(try CacheCommandFixture.environment(cli))
                printf 'checksum-stamp' > "$cache/os-images/jetson.flashpack.sha256"
                mkdir -p "$cache/os-images/unrelated-directory"
                printf 'not-a-download' > "$cache/os-images/unrelated-directory/keep"
                """
            )
            try await cli.sh(
                "\(try CacheCommandFixture.environment(cli))\nwendy os cache list --json"
            ) { result in
                #expect(result.status.isSuccess)
                #expect(result.stderr.isEmpty)
                let rows = try #require(
                    try JSONSerialization.jsonObject(with: Data(result.stdout.utf8))
                        as? [[String: Any]]
                )
                #expect(
                    rows.compactMap { $0["name"] as? String }.sorted() == [
                        "jetson.flashpack", "pi.img",
                    ]
                )
                for row in rows {
                    let name = try #require(row["name"] as? String)
                    #expect(row["sizeBytes"] as? Int == (name == "pi.img" ? 11 : 12))
                    #expect(row["size"] as? String == (name == "pi.img" ? "11 B" : "12 B"))
                    #expect(row["path"] == nil)
                }
            }
            try await cli.sh("\(try CacheCommandFixture.environment(cli))\nwendy os cache list") {
                result in
                #expect(result.status.isSuccess)
                #expect(result.stderr.isEmpty)
                let lines = result.stdout.split(separator: "\n").map(String.init)
                #expect(lines.count == 3)
                #expect(
                    Set(lines.prefix(2))
                        == Set(["  jetson.flashpack  (0.0 MB)", "  pi.img  (0.0 MB)"])
                )
                #expect(lines.last?.hasPrefix("Cache directory: /") == true)
                #expect(lines.last?.hasSuffix("/wendy/os-images") == true)
            }
            try await cli.sh(
                """
                \(try CacheCommandFixture.environment(cli))
                test "$(cat "$cache/os-images/jetson.flashpack/boot/kernel")" = '1234567'
                test "$(cat "$cache/os-images/jetson.flashpack/rootfs")" = '12345'
                test "$(cat "$cache/os-images/pi.img")" = '12345678901'
                test "$(cat "$cache/os-images/jetson.flashpack.sha256")" = 'checksum-stamp'
                test "$(cat "$cache/os-images/unrelated-directory/keep")" = 'not-a-download'
                """
            )
        }
    }

    /**
     An unreadable extracted flashpack fails the entire inventory with a diagnostic
     naming the download. Neither human nor JSON output may silently omit bytes or
     return a partial success. The denied-read control and payload check use POSIX
     permissions and run without root privileges.
     */
    @Test(.enabled(if: WendyE2EMachine.cli.os != .windows, "POSIX permissions"))
    func `fails loudly when a populated flashpack cannot be sized`() async throws {
        let scenario = CacheCommandFixture.restoringPermissions(
            for: "os-images/locked.flashpack/boot"
        )
        try await scenario.run(authenticated: false) { cli, _ in
            for mode in ["", "--json"] {
                try await cli.sh(
                    """
                    \(try CacheCommandFixture.environment(cli))
                    test "$(id -u)" -ne 0
                    locked="$cache/os-images/locked.flashpack/boot"
                    mkdir -p "$locked"
                    printf 'preserve-this-kernel' > "$locked/kernel"
                    chmod 000 "$locked"
                    trap 'chmod 700 "$locked"' EXIT
                    if cat "$locked/kernel" >/dev/null 2>&1; then
                      echo 'negative control unexpectedly read locked kernel' >&2; exit 90
                    fi
                    set +e
                    wendy os cache list \(mode)
                    cache_command_exit=$?
                    set -e
                    exit "$cache_command_exit"
                    """
                ) { result in
                    #expect(!result.status.isSuccess)
                    #expect(result.stdout.isEmpty)
                    #expect(result.stderr.contains("locked.flashpack"))
                    #expect(result.stderr.lowercased().contains("permission denied"))
                }
                // Preservation must succeed independently of the expected CLI failure.
                try await cli.sh(
                    """
                    \(try CacheCommandFixture.environment(cli))
                    test "$(cat "$cache/os-images/locked.flashpack/boot/kernel")" = 'preserve-this-kernel'
                    """
                )
            }
        }
    }
}
