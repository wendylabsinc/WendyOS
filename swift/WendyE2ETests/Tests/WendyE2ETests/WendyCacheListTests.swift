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

    /**
     Populated caches report exact file bytes for OS downloads, nested flashpacks,
     per-app build stores and other cache directories. JSON and the noninteractive
     list agree on largest-first order; shared build data belongs only to the
     human summary. Listing leaves every seeded payload unchanged. POSIX hosts only.
     */
    @Test(.enabled(if: WendyE2EMachine.cli.os != .windows, "POSIX cache fixture"))
    func `lists populated categories with exact bytes in JSON and size order`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await CacheCommandFixture.populate(cli)
            let expectedNames = [
                "misc", "ocilayout/com.example.demo-linux_arm64",
                "buildx/com.example.demo-linux_arm64", "os-images/jetson.flashpack",
                "os-images/pi.img",
            ]
            try await cli.sh("\(try CacheCommandFixture.environment(cli))\nwendy cache list --json")
            { result in
                #expect(result.status.isSuccess)
                #expect(result.stderr.isEmpty)
                let rows = try #require(
                    try JSONSerialization.jsonObject(with: Data(result.stdout.utf8))
                        as? [[String: Any]]
                )
                #expect(rows.compactMap { $0["name"] as? String } == expectedNames)
                #expect(rows.compactMap { $0["sizeBytes"] as? Int } == [19, 17, 13, 12, 11])
                #expect(
                    rows.compactMap { $0["size"] as? String } == [
                        "19 B", "17 B", "13 B", "12 B", "11 B",
                    ]
                )
                for row in rows {
                    let name = try #require(row["name"] as? String)
                    let path = try #require(row["path"] as? String)
                    #expect(path == cli.wendyCacheDirectory + "/" + name)
                    if name.hasPrefix("buildx/") || name.hasPrefix("ocilayout/") {
                        #expect(!(row["lastBuilt"] as? String ?? "").isEmpty)
                    } else {
                        #expect(row["lastBuilt"] == nil)
                    }
                }
            }
            try await cli.sh("\(try CacheCommandFixture.environment(cli))\nwendy cache list") {
                result in
                #expect(result.status.isSuccess)
                #expect(result.stderr.isEmpty)
                let lines = result.stdout.split(separator: "\n").map(String.init)
                #expect(lines.count == 6)
                for (index, name) in expectedNames.enumerated() {
                    let line = try #require(lines.indices.contains(index) ? lines[index] : nil)
                    let prefix = "  \(name)  (\([19, 17, 13, 12, 11][index]) B"
                    if name.hasPrefix("buildx/") || name.hasPrefix("ocilayout/") {
                        #expect(line.hasPrefix(prefix + ", built "))
                        #expect(line.hasSuffix(" ago)"))
                    } else {
                        #expect(line == prefix + ")")
                    }
                }
                #expect(result.stdout.contains("Build cache: 33 B on disk (3 B shared)"))
            }
            try await cli.sh(
                """
                \(try CacheCommandFixture.environment(cli))
                test "$(cat "$cache/misc/nested/payload")" = '1234567890123456789'
                test "$(cat "$cache/ocilayout/com.example.demo-linux_arm64/blobs/sha256/97f40b8ae3e4d3118bb4afb623d3e768f04d9bc2f913bd64296ebcd53386c1ac")" = '12345678901234567'
                test "$(cat "$cache/buildx/com.example.demo-linux_arm64/blobs/sha256/bca2b41a2b25e137c83fee346af7bd1e0f52bd560583ca07a1b42f9944c5c50b")" = '1234567890123'
                test "$(cat "$cache/os-images/jetson.flashpack/boot/kernel")" = '1234567'
                test "$(cat "$cache/os-images/jetson.flashpack/rootfs")" = '12345'
                test "$(cat "$cache/os-images/pi.img")" = '12345678901'
                test "$(cat "$cache/ocilayout/blobs/shared")" = '123'
                """
            )
        }
    }

    /**
     An unreadable per-app build cache must fail visibly instead of presenting a
     successful zero-byte or partial inventory. Both output modes identify the
     affected app, emit no successful listing, and preserve its payload. POSIX
     permissions are checked with a real denied read before running the CLI.

     **CURRENT KNOWN DEFECT:** unreadable per-app build directories currently
     return success with zero bytes instead of failing. This guarantee does not
     currently pass. Opt in with `WENDY_E2E_KNOWN_CACHE_DEFECTS=1` and filter
     `fails loudly when a populated build cache cannot be sized` to reproduce it
     using the isolated denied-read fixture below.
     Tracked by [WDY-4297 — wendy cache list reports an unreadable populated build cache as empty](https://linear.app/wendylabsinc/issue/WDY-4297/wendy-cache-list-reports-an-unreadable-populated-build-cache-as-empty).
     */
    @Test(
        .enabled(if: WendyE2EMachine.cli.os != .windows, "POSIX permissions"),
        .disabled(
            if: !CacheCommandFixture.knownDefectsEnabled,
            "DEFECT: [WDY-4297 — wendy cache list reports an unreadable populated build cache as empty](https://linear.app/wendylabsinc/issue/WDY-4297/wendy-cache-list-reports-an-unreadable-populated-build-cache-as-empty). Replay the denied-read fixture with WENDY_E2E_KNOWN_CACHE_DEFECTS=1."
        )
    )
    func `fails loudly when a populated build cache cannot be sized`() async throws {
        let scenario = CacheCommandFixture.restoringPermissions(
            for: "ocilayout/com.example.locked-linux_arm64"
        )
        try await scenario.run(authenticated: false) { cli, _ in
            for mode in ["", "--json"] {
                try await cli.sh(
                    """
                    \(try CacheCommandFixture.environment(cli))
                    test "$(id -u)" -ne 0
                    locked="$cache/ocilayout/com.example.locked-linux_arm64"
                    mkdir -p "$locked"
                    printf 'preserve-this-layer' > "$locked/layer"
                    chmod 000 "$locked"
                    trap 'chmod 700 "$locked"' EXIT
                    if cat "$locked/layer" >/dev/null 2>&1; then
                      echo 'negative control unexpectedly read locked layer' >&2; exit 90
                    fi
                    set +e
                    wendy cache list \(mode)
                    cache_command_exit=$?
                    set -e
                    exit "$cache_command_exit"
                    """
                ) { result in
                    #expect(!result.status.isSuccess)
                    #expect(result.stdout.isEmpty)
                    #expect(result.stderr.contains("com.example.locked-linux_arm64"))
                    #expect(result.stderr.lowercased().contains("permission denied"))
                }
                // Preservation must succeed independently of the expected CLI failure.
                try await cli.sh(
                    """
                    \(try CacheCommandFixture.environment(cli))
                    test "$(cat "$cache/ocilayout/com.example.locked-linux_arm64/layer")" = 'preserve-this-layer'
                    """
                )
            }
        }
    }
}
