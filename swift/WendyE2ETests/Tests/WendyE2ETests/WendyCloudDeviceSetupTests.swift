import Testing
import WendyE2ETesting

@Suite
struct `'wendy cloud device setup'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays usage for `wendy cloud device setup`. The output includes the
     command synopsis, local flags, inherited global flags, and concise
     descriptions. Help exits successfully, writes to stdout, emits no
     stderr, and leaves configuration, cache, project, cloud, and device
     state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy cloud device setup --help") { result in
                #expect(result.status.isSuccess)
                #expect(result.stdout.contains("Walks through enrollment"))
                #expect(result.stdout.contains("wendy cloud device setup [flags]"))
                #expect(result.stdout.contains("--device"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     `--device` selects the cloud device and skips local discovery and pickers.
     */
    @Test(
        .disabled(
            "WDY-1949/WDY-1952: explicit cloud-target setup needs seeded provisioning, auth, WiFi, and version state."
        )
    )
    func `uses explicit device selection without prompting`() async throws {}

    /**
     Without an explicit or configured device in a non-interactive context,
     reports that a device selection is required, emits no prompt escape
     sequences, and performs no device operation.
     */
    @Test(
        .disabled(
            "WDY-1949: missing cloud-device selection can only be observed after injecting valid isolated auth."
        )
    )
    func `reports missing device selection in non-interactive mode`() async throws {}

    /**
     Cloud-routed device commands validate the selected Wendy Cloud auth
     session before connecting to the broker. Missing or ambiguous auth fails
     before device state changes.
     */
    @Test
    func `requires cloud authentication before opening a tunnel`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy cloud device setup --device target --json") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("not logged in"))
                #expect(result.stderr.contains("wendy auth login"))
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
     Guides the user through device naming, cloud enrollment, and WiFi
     configuration for a new device, explaining each side effect before
     applying it.
     */
    @Test(
        .disabled(
            "WDY-1952: the setup wizard needs seeded provisioning/auth/WiFi state plus scripted PTY interaction."
        )
    )
    func `walks through new device setup`() async throws {}

    /**
     Existing auth sessions, device names, or WiFi configuration are detected
     and not repeated unless the user chooses to change them.
     */
    @Test(
        .disabled(
            "WDY-1952: setup resume behavior needs seeded partially configured device and auth state."
        )
    )
    func `uses existing state to skip completed setup steps`() async throws {}

    /**
     Cancelling the setup flow stops at the current step and avoids applying
     later enrollment or WiFi changes.
     */
    @Test(
        .disabled(
            "WDY-1952: setup cancellation needs seeded state and harness process control across wizard stages."
        )
    )
    func `cancels without continuing later setup`() async throws {}

    /**
     Rejects flags that are not part of the command's documented interface.

     The command reports a usage error on stderr and does not perform the
     requested operation.
     */
    @Test
    func `rejects undocumented flags`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy cloud device setup --bogus") { result in
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
            "WDY-1934: 'wendy cloud device setup' silently accepts positional arguments because the leaf command has no cobra.NoArgs validator."
        )
    )
    func `rejects undocumented positional arguments`() async throws {}
}
