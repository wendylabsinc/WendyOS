import Testing
import WendyE2ETesting

@Suite
struct `'wendy device dashboard'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays usage for `wendy device dashboard`. The output includes the
     command synopsis, local flags, inherited global flags, and concise
     descriptions. Help exits successfully, writes to stdout, emits no
     stderr, and leaves configuration, cache, project, cloud, and device
     state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy device dashboard --help") { result in
                #expect(result.status.isSuccess)
                #expect(
                    result.stdout.contains("Live dashboard showing metrics and logs from a device")
                )
                #expect(result.stdout.contains("wendy device dashboard [flags]"))
                #expect(result.stdout.contains("--app"))
                #expect(result.stdout.contains("--device"))
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
            "WDY-1952: explicit-target dashboard startup needs a seeded managed agent with telemetry state."
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
            try await cli.sh("wendy device dashboard --app Example") { result in
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
     Displays live metrics and logs for the selected device, optionally
     filtered to an application. The dashboard is informational and does not
     mutate device state.
     */
    @Test(
        .disabled(
            "WDY-1952: live dashboard rendering needs seeded metrics/log streams and scripted terminal control."
        )
    )
    func `shows a live device dashboard`() async throws {}

    /**
     Without an interactive terminal, reports that the live dashboard requires
     a terminal or suggests machine-readable alternatives.
     */
    @Test(
        .disabled(
            "WDY-1952: terminal capability behavior needs a seeded target plus controlled TTY/non-TTY execution."
        )
    )
    func `reports non-interactive dashboard limitations`() async throws {}

    /**
     Cancelling the dashboard closes log and metric streams and restores
     terminal output.
     */
    @Test(
        .disabled(
            "WDY-1952: dashboard cancellation cleanup needs seeded streams and harness process control."
        )
    )
    func `shuts down cleanly on cancellation`() async throws {}

    /**
     Rejects flags that are not part of the command's documented interface.

     The command reports a usage error on stderr and does not perform the
     requested operation.
     */
    @Test
    func `rejects undocumented flags`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy device dashboard --bogus") { result in
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
            "WDY-1934: 'wendy device dashboard' silently accepts positional arguments because the leaf command has no cobra.NoArgs validator."
        )
    )
    func `rejects undocumented positional arguments`() async throws {}
}
