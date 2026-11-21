import Testing
import WendyE2ETesting

@Suite
struct `'wendy cloud device volumes remove'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays usage for `wendy cloud device volumes remove`. The output
     includes the command synopsis, local flags, inherited global flags,
     and concise descriptions. Help exits successfully, writes to stdout,
     emits no stderr, and leaves configuration, cache, project, cloud, and
     device state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy cloud device volumes remove --help") { result in
                #expect(result.status.isSuccess)
                #expect(result.stdout.contains("Remove a persistent volume"))
                #expect(result.stdout.contains("wendy cloud device volumes remove [name] [flags]"))
                #expect(result.stdout.contains("--force"))
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
            "WDY-1949/WDY-1952: explicit cloud-target removal needs seeded managed-agent persistent-volume state without a physical device."
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
            try await cli.sh(
                "wendy cloud device volumes remove example --force --device target --json"
            ) { result in
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
     Removes the named volume only after confirmation or `--force`. Success
     output identifies the removed volume.
     */
    @Test(
        .disabled(
            "WDY-1952: confirmation and successful removal need seeded managed-agent persistent-volume state."
        )
    )
    func `removes a persistent volume after confirmation`() async throws {}

    /**
     Prompts for a volume when its name is omitted in an interactive session.

     The selected volume is passed to the removal flow; cancellation leaves
     device storage unchanged.
     */
    @Test(
        .disabled(
            "WDY-1952: omitted names intentionally open a device-backed volume picker and need seeded list results."
        )
    )
    func `selects a missing volume name interactively`() async throws {}

    /**
     An unknown volume name fails without deleting similarly named volumes or
     application containers.
     */
    @Test(
        .disabled(
            "WDY-1952: unknown-volume isolation needs seeded neighboring volume and application state."
        )
    )
    func `reports unknown volumes without deleting data`() async throws {}

    /**
     Rejects flags that are not part of the command's documented interface.

     The command reports a usage error on stderr and does not perform the
     requested operation.
     */
    @Test
    func `rejects undocumented flags`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy cloud device volumes remove example --bogus") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("unknown flag"))
            }
        }
    }

    /**
     Rejects more positional arguments than the command's documented interface
     accepts.

     Validation fails before the requested cloud or device operation begins.
     */
    @Test
    func `rejects extra positional arguments`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy cloud device volumes remove one two") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("accepts at most 1 arg(s), received 2"))
            }
        }
    }
}
