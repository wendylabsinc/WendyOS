import Testing
import WendyE2ETesting

@Suite
struct `'wendy device hardware list'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays usage for `wendy device hardware list`. The output includes the
     command synopsis, local flags, inherited global flags, and concise
     descriptions. Help exits successfully, writes to stdout, emits no
     stderr, and leaves configuration, cache, project, cloud, and device
     state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy device hardware list --help") { result in
                #expect(result.status.isSuccess)
                #expect(result.stdout.contains("List hardware capabilities"))
                #expect(result.stdout.contains("wendy device hardware list [flags]"))
                #expect(result.stdout.contains("--category"))
                #expect(result.stdout.contains("--device"))
                #expect(result.stdout.contains("--json"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     `--device` selects the target device hostname and skips discovery and
     pickers.
     */
    @Test(
        .disabled(
            "WDY-1952: explicit-target inventory needs seeded managed-agent hardware state without a physical device."
        )
    )
    func `uses explicit device selection without prompting`() async throws {}

    /**
     Without an explicit or configured device in a non-interactive context,
     reports that a device selection is required, emits no prompt escape
     sequences, and performs no device operation.
     */
    @Test
    func `reports missing device selection in non-interactive mode`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy device hardware list --json") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("no device specified"))
                #expect(!result.stderr.contains("Select a device"))
            }
        }
    }

    /**
     Connection failures, timeouts, and incompatible agent responses produce
     stderr diagnostics and a failure status. Output does not claim that the
     operation succeeded.
     */
    @Test(
        .disabled(
            "WDY-1952: connection and incompatible-RPC failures need controllable seeded managed-agent responses."
        )
    )
    func `reports unreachable devices without partial success`() async throws {}

    /**
     Displays hardware capabilities such as GPU, audio, camera, storage, and
     buses with ids and user-facing labels.
     */
    @Test(
        .disabled("WDY-1952: human hardware inventory needs seeded managed-agent capability state.")
    )
    func `lists hardware capabilities`() async throws {}

    /**
     `--category` restricts output to matching capabilities. Unknown categories
     produce an empty successful result or clear validation diagnostic.
     */
    @Test(
        .disabled(
            "WDY-1952: category filtering needs seeded multi-category managed-agent capability state."
        )
    )
    func `filters by category`() async throws {}

    /**
     With `--json`, emits hardware objects grouped or labeled by category with
     stable field names.
     */
    @Test(.disabled("WDY-1952: JSON hardware schema needs seeded managed-agent capability state."))
    func `prints JSON hardware inventory`() async throws {}

    /**
     Rejects flags that are not part of the command's documented interface.

     The command reports a usage error on stderr and does not perform the
     requested operation.
     */
    @Test
    func `rejects undocumented flags`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy device hardware list --bogus") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("unknown flag"))
            }
        }
    }

    /**
     Rejects positional arguments because this command is entirely flag-driven.

     The command reports a usage error instead of treating undocumented input as
     a valid request.
     */
    @Test(
        .disabled(
            "WDY-1934: 'wendy device hardware list' silently accepts extra positional arguments because the leaf command has no cobra.NoArgs validator."
        )
    )
    func `rejects undocumented positional arguments`() async throws {}
}
