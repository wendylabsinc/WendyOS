import Testing
import WendyE2ETesting

/// Deprecated compatibility alias for `wendy device info`.
@Suite
struct `'wendy device version'` {
    let scenario = CLIAndAgentScenario()

    /**
     The hidden command remains directly invocable for older scripts, but
     `wendy device --help` does not advertise it. Direct help preserves the
     `wendy device info` option surface for users who still discover the legacy
     command explicitly.
     */
    @Test
    func `is hidden from parent help while direct help mirrors '... device info'`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy device --help") { result in
                #expect(result.status.isSuccess)
                #expect(!result.stdout.contains("  version"))
                #expect(result.stderr == "")
            }
            try await cli.sh("wendy device version --help") { result in
                let stdout = result.stdout
                #expect(result.status.isSuccess)
                #expect(stdout.contains("agent version, OS, architecture"))
                #expect(stdout.contains("wendy device version [flags]"))
                #expect(stdout.contains("--check-updates"))
                #expect(stdout.contains("--prerelease"))
                #expect(stdout.contains("--device"))
                #expect(stdout.contains("--json"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     In human-readable mode, the deprecated command reports the same device information as `wendy device info` and directs users to the replacement command.
     */
    @Test(
        .disabled(
            "WDY-1952: human alias equivalence and deprecation output need a seeded managed-agent info response without a physical device."
        )
    )
    func `aliases '... device info' with a deprecation notice`() async throws {
        // TODO: enable with the seeded managed-agent fixture (WDY-1952).
    }

    /**
     With `--json`, deprecation guidance stays out of stdout and stderr so existing scripts can continue parsing the response.
     */
    @Test
    func `'--json' keeps JSON output clean`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy device version --json") { result in
                let stderr = result.stderr
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(stderr.contains("no device specified; use --device flag"))
                #expect(!stderr.lowercased().contains("deprecated"))
                #expect(!stderr.contains("Select a device"))
            }
        }
    }
}
