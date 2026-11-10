import Foundation
import Testing
import WendyE2ETesting

@Suite
struct `'wendy project entitlements list'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays usage for `wendy project entitlements list`. The output includes
     the command synopsis, local flags, inherited global flags, and concise
     descriptions. Help exits successfully, writes to stdout, emits no
     stderr, and leaves configuration, cache, project, cloud, and device
     state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy project entitlements list --help") { result in
                let stdout = result.stdout

                #expect(result.status.isSuccess)
                #expect(stdout.contains("List project entitlements"))
                #expect(stdout.contains("Usage:"))
                #expect(stdout.contains("wendy project entitlements list [flags]"))
                #expect(stdout.contains("--show-all"))
                #expect(stdout.contains("--help"))
                #expect(stdout.contains("--device"))
                #expect(stdout.contains("--json"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     Outside a Wendy project, or with malformed JSON, reports the project
     problem on stderr, returns a failure status, and leaves files
     unchanged.
     */
    @Test
    func `reports missing or invalid project files without mutation`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            // Missing project.
            try await cli.sh("wendy project entitlements list") { result in
                #expect(!result.status.isSuccess)
                #expect(result.stderr.contains("wendy.json"))
            }

            try await cli.sh(
                posix: "test ! -e wendy.json",
                power: "if (Test-Path -LiteralPath 'wendy.json') { throw 'wendy.json was created' }"
            )

            // Malformed project.
            try await cli.sh(
                posix: "printf 'not json' > wendy.json",
                power: "Set-Content -NoNewline -LiteralPath 'wendy.json' -Value 'not json'"
            )

            try await cli.sh("wendy project entitlements list") { result in
                #expect(!result.status.isSuccess)
                // The diagnostic names the file and says its JSON is invalid.
                #expect(result.readableStderr.contains("wendy.json"))
                #expect(result.readableStderr.contains("JSON"))
            }

            try await cli.sh(
                posix: "grep -q 'not json' wendy.json",
                power:
                    "if (-not (Select-String -LiteralPath 'wendy.json' -Pattern 'not json' -Quiet)) { throw 'file mutated' }"
            )
        }
    }

    /**
     Unknown flags are rejected with a usage diagnostic on stderr and a
     failure status.
     */
    @Test
    func `rejects unknown flags`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh(
                posix: "printf '{\"appId\":\"sh.wendy.demo\"}\\n' > wendy.json",
                power:
                    "Set-Content -LiteralPath 'wendy.json' -Value '{\"appId\":\"sh.wendy.demo\"}'"
            )

            try await cli.sh("wendy project entitlements list --bogus") { result in
                #expect(!result.status.isSuccess)
                #expect(result.stderr.contains("unknown flag"))
                #expect(result.stderr.contains("--bogus"))
            }
        }
    }

    /**
     Reads `wendy.json` from the current directory and displays enabled
     entitlements with their configured fields on stdout. An empty
     entitlement set is a successful empty listing.
     */
    @Test(
        .disabled(
            "WDY-1935: 'wendy project entitlements list' writes the entitlement listing to stderr and leaves stdout empty, unlike 'wendy info' / 'wendy cache list'."
        )
    )
    func `lists entitlements from the current project`() async throws {
        // TODO: enable once the listing is written to stdout.
    }

    /**
     With `--json`, emits machine-readable entitlement objects on stdout,
     preserving configured values such as GPIO pins, I2C devices, and
     persistent volume paths.
     */
    @Test(
        .disabled(
            "WDY-1935: 'wendy project entitlements list --json' writes JSON to stderr instead of stdout."
        )
    )
    func `prints JSON entitlements for automation`() async throws {
        // TODO: enable once JSON entitlements are written to stdout.
    }

    /**
     Rejects unexpected positional arguments with a usage diagnostic on
     stderr and a failure status, rather than silently ignoring them.
     */
    @Test(
        .disabled(
            "WDY-1934: 'wendy project entitlements list' silently accepts and ignores extra positional arguments."
        )
    )
    func `rejects unexpected positional arguments`() async throws {
        // TODO: enable once 'wendy project entitlements list' rejects unexpected positional arguments.
    }
}
