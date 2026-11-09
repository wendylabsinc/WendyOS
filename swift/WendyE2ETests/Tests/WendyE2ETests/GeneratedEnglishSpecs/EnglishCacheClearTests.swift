// FILE: EnglishCacheClearTests.swift

import Foundation
import Testing
import WendyE2ETesting

@Suite("English spec cache clear protects external targets.")
struct EnglishCacheClearReadOnlyDescendantsSuite {
    /// Setup: Seed the isolated CLI cache with a writable cached file. On POSIX,
    /// also seed a read-only descendant and a symlink to a read-only external
    /// fixture, and verify their contents, permissions and existence.
    ///
    /// Action: Run the real `wendy cache clear` command without an interactive
    /// terminal, check its result, then run and check it again.
    ///
    /// Expectations: Both invocations succeed with exactly the expected stdout
    /// and empty stderr. The cached file disappears after the first invocation.
    /// On POSIX, the entire cache disappears and the external directory, file,
    /// exact file bytes and permission modes survive both invocations.
    ///
    /// Cleanup: Use the scenario's cleanup-safe hook to restore owner write and
    /// search permissions on the known seeded directories without following
    /// cached symlinks, then remove leftover fixtures. Propagate cleanup errors
    /// and preserve the isolated run directory and command recordings.
    @Test("English spec cache clear removes read-only descendants and succeeds twice.")
    func cacheClearRemovesReadOnlyDescendants() async throws {
        let scenario = CLIAndAgentScenario(
            before: { cli, _ in
                let cache = cli.wendyCacheDirectory

                try await cli.sh(
                    posix: """
                        set -eu
                        cache=\(Self.posixQuote(cache))
                        mkdir -p "$cache/images"
                        printf 'cached\\n' > "$cache/images/entry.bin"
                        test -f "$cache/images/entry.bin"
                        """,
                    power: """
                        $ErrorActionPreference = 'Stop'
                        $cache = \(Self.powerShellQuote(cache))
                        $images = Join-Path $cache 'images'
                        $null = [System.IO.Directory]::CreateDirectory($images)
                        $entry = Join-Path $images 'entry.bin'
                        [System.IO.File]::WriteAllText(
                            $entry,
                            "cached`n",
                            [System.Text.UTF8Encoding]::new($false)
                        )
                        if (-not (Test-Path -LiteralPath $entry -PathType Leaf)) {
                            throw 'The cached file fixture was not created.'
                        }
                        """
                )

                guard let modeCommand = Self.modeCommand(for: cli) else {
                    return
                }

                try await cli.sh(
                    """
                    set -eu
                    cache=\(Self.posixQuote(cache))
                    external="$PWD/external-cache-target"
                    mkdir -p "$cache/simulator/g1_sim"
                    printf 'cached\\n' > "$cache/simulator/g1_sim/simulation.py"
                    chmod 0444 "$cache/simulator/g1_sim/simulation.py"
                    chmod 0555 "$cache/simulator/g1_sim"
                    mkdir "$external"
                    printf 'keep\\n' > "$external/keep"
                    chmod 0444 "$external/keep"
                    chmod 0555 "$external"
                    ln -s "$external" "$cache/external-link"
                    test -d "$cache/simulator/g1_sim"
                    test -f "$cache/simulator/g1_sim/simulation.py"
                    test "$(\(modeCommand) "$cache/simulator/g1_sim")" = '555'
                    test "$(\(modeCommand) "$cache/simulator/g1_sim/simulation.py")" = '444'
                    test -L "$cache/external-link"
                    test "$(readlink "$cache/external-link")" = "$external"
                    cat "$cache/simulator/g1_sim/simulation.py"
                    """
                ) { result in
                    try #require(result.status.isSuccess)
                    try #require(result.stdout == "cached\n")
                    try #require(result.stderr.isEmpty)
                }

                try await Self.verifyExternalTarget(in: cli)
            },
            after: { cli, _ in
                let cache = cli.wendyCacheDirectory

                try await cli.sh(
                    posix: """
                        set -eu
                        cache=\(Self.posixQuote(cache))
                        external="$PWD/external-cache-target"
                        if [ ! -L "$cache" ] &&
                           [ ! -L "$cache/simulator" ] &&
                           [ ! -L "$cache/simulator/g1_sim" ] &&
                           [ -d "$cache/simulator/g1_sim" ]; then
                            chmod u+wx "$cache/simulator/g1_sim"
                        fi
                        if [ ! -L "$external" ] && [ -d "$external" ]; then
                            chmod u+wx "$external"
                        fi
                        rm -rf "$cache"
                        rm -rf "$external"
                        test ! -e "$cache"
                        test ! -L "$cache"
                        test ! -e "$external"
                        test ! -L "$external"
                        """,
                    power: """
                        $ErrorActionPreference = 'Stop'
                        $cache = \(Self.powerShellQuote(cache))
                        if (Test-Path -LiteralPath $cache) {
                            Remove-Item -LiteralPath $cache -Recurse -Force -ErrorAction Stop
                        }
                        if (Test-Path -LiteralPath $cache) {
                            throw 'The isolated cache remains after cleanup.'
                        }
                        """
                )
            }
        )

        try await scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy cache clear") { result in
                #expect(result.status.isSuccess)
                #expect(result.stdout == "Cache cleared.\n")
                #expect(result.stderr.isEmpty)
            }

            let cache = cli.wendyCacheDirectory
            try await cli.sh(
                posix: """
                    set -eu
                    cache=\(Self.posixQuote(cache))
                    test ! -e "$cache/images/entry.bin"
                    test ! -L "$cache/images/entry.bin"
                    test ! -e "$cache"
                    test ! -L "$cache"
                    """,
                power: """
                    $ErrorActionPreference = 'Stop'
                    $cache = \(Self.powerShellQuote(cache))
                    $entry = Join-Path (Join-Path $cache 'images') 'entry.bin'
                    if (Test-Path -LiteralPath $entry) {
                        throw 'The seeded cached file remains after cache clear.'
                    }
                    """
            )

            try await Self.verifyExternalTarget(in: cli)

            try await cli.sh("wendy cache clear") { result in
                #expect(result.status.isSuccess)
                #expect(result.stdout == "Cache cleared.\n")
                #expect(result.stderr.isEmpty)
            }

            try await Self.verifyExternalTarget(in: cli)
        }
    }

    private static func verifyExternalTarget(in cli: WendyE2ESession) async throws {
        guard let modeCommand = modeCommand(for: cli) else {
            return
        }

        try await cli.sh(
            """
            set -eu
            external="$PWD/external-cache-target"
            test -d "$external"
            test ! -L "$external"
            test -f "$external/keep"
            test ! -L "$external/keep"
            test "$(\(modeCommand) "$external")" = '555'
            test "$(\(modeCommand) "$external/keep")" = '444'
            cat "$external/keep"
            """
        ) { result in
            try #require(result.status.isSuccess)
            #expect(result.stdout == "keep\n")
            #expect(result.stderr.isEmpty)
        }
    }

    private static func modeCommand(for cli: WendyE2ESession) -> String? {
        switch cli.machine.os {
        case .macOS:
            return "stat -f %Lp"
        case .linux, .wendyOS:
            return "stat -c %a"
        case .windows:
            return nil
        }
    }

    private static func posixQuote(_ value: String) -> String {
        "'" + value.replacingOccurrences(of: "'", with: "'\"'\"'") + "'"
    }

    private static func powerShellQuote(_ value: String) -> String {
        "'" + value.replacingOccurrences(of: "'", with: "''") + "'"
    }
}
