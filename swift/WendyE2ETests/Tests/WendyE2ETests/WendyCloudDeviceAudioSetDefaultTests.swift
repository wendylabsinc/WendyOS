import Testing
import WendyE2ETesting

@Suite
struct `'wendy cloud device audio set-default'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays usage for `wendy cloud device audio set-default`. The output
     includes the command synopsis, local flags, inherited global flags,
     and concise descriptions. Help exits successfully, writes to stdout,
     emits no stderr, and leaves configuration, cache, project, cloud, and
     device state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy cloud device audio set-default --help") { result in
                #expect(result.status.isSuccess)
                #expect(result.stdout.contains("Set the default audio device"))
                #expect(result.stdout.contains("wendy cloud device audio set-default [flags]"))
                #expect(result.stdout.contains("--id"))
                #expect(result.stdout.contains("--device"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     `--device` selects the cloud device and skips local discovery and pickers.
     The command does not read or change the saved default device when an
     explicit target is supplied.
     */
    @Test(
        .disabled(
            "WDY-1949/WDY-1952: explicit cloud-target mutation needs seeded managed-agent audio state without a physical device."
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
            try await cli.sh("wendy cloud device audio set-default --device target --id 4 --json") {
                result in
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
     Sets the selected audio device id as the device default and prints a
     concise confirmation with the chosen id or name.
     */
    @Test(
        .disabled(
            "WDY-1952: successful default selection needs seeded managed-agent audio device state."
        )
    )
    func `sets the default audio device`() async throws {}

    /**
     Missing or invalid `--id` values produce a usage diagnostic before
     contacting or mutating audio settings.
     */
    @Test
    func `requires an audio device id`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy cloud device audio set-default") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.readableStderr.contains("required flag(s) \"id\" not set"))
            }
        }
    }

    /**
     If the agent rejects the id or does not support default audio selection,
     the previous default remains active.
     */
    @Test(
        .disabled(
            "WDY-1952: rejection and previous-default preservation need seeded managed-agent audio state."
        )
    )
    func `reports unsupported devices without changing defaults`() async throws {}

    /**
     Rejects flags that are not part of the command's documented interface.

     The command reports a usage error on stderr and does not perform the
     requested operation.
     */
    @Test
    func `rejects undocumented flags`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy cloud device audio set-default --bogus") { result in
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
            "WDY-1934: 'wendy cloud device audio set-default' silently accepts positional arguments because the leaf command has no cobra.NoArgs validator."
        )
    )
    func `rejects undocumented positional arguments`() async throws {}
}
