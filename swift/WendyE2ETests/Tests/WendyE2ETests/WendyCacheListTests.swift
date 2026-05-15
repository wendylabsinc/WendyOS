import Foundation
import Subprocess
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
            try await cli.sh("wendy cache list --help") {
                terminationStatus,
                standardOutput,
                standardError in

                #expect(terminationStatus.isSuccess)
                #expect(standardOutput.contains("List cached items"))
                #expect(standardOutput.contains("Usage:"))
                #expect(standardOutput.contains("wendy cache list [flags]"))
                #expect(standardOutput.contains("--help"))
                #expect(standardOutput.contains("--device"))
                #expect(standardOutput.contains("--json"))
                #expect(standardError == "")
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
            try await cli.sh("wendy cache list") {
                terminationStatus,
                standardOutput,
                standardError in

                #expect(terminationStatus.isSuccess)
                #expect(standardOutput == "Cache is empty.\n")
                #expect(standardError == "")
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
                """
                mkdir -p "$HOME/.wendy"
                printf '{"defaultDevice":"do-not-list"}\n' > "$HOME/.wendy/config.json"
                printf 'project artifact\n' > unrelated-project-file.txt
                wendy cache list
                """
            ) {
                terminationStatus,
                standardOutput,
                standardError in

                #expect(terminationStatus.isSuccess)
                #expect(standardOutput == "Cache is empty.\n")
                #expect(!standardOutput.contains("do-not-list"))
                #expect(!standardOutput.contains("unrelated-project-file"))
                #expect(standardError == "")
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
            try await cli.sh(
                """
                case "$(uname -s)" in
                  Darwin) cache_root="$HOME/Library/Caches/wendy" ;;
                  *) cache_root="${XDG_CACHE_HOME:-$HOME/.cache}/wendy" ;;
                esac
                mkdir -p "$cache_root/unreadable"
                chmod 000 "$cache_root/unreadable"
                trap 'chmod 700 "$cache_root/unreadable" 2>/dev/null || true' EXIT
                wendy cache list
                """
            ) {
                terminationStatus,
                standardOutput,
                standardError in

                #expect(!terminationStatus.isSuccess)
                #expect(standardOutput == "")
                #expect(standardError.contains("determining cache entry size"))
                #expect(standardError.contains("unreadable"))
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
            try await cli.sh("wendy --json cache list") {
                terminationStatus,
                standardOutput,
                standardError in

                #expect(terminationStatus.isSuccess)
                #expect(standardError == "")
                #expect(!standardOutput.contains("Cache is empty"))

                let json = try #require(
                    try JSONSerialization.jsonObject(with: Data(standardOutput.utf8))
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
            try await cli.sh("wendy cache list extra") {
                terminationStatus,
                standardOutput,
                standardError in

                #expect(!terminationStatus.isSuccess)
                #expect(standardOutput == "")
                #expect(standardError.contains("unknown command"))
                #expect(standardError.contains("extra"))
                #expect(!standardError.contains("Cache is empty"))
            }
        }
    }
}
