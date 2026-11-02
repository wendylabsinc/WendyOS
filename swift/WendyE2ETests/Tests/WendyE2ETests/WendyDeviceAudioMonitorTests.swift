import Testing
import WendyE2ETesting

@Suite
struct `'wendy device audio monitor'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays usage for `wendy device audio monitor`. The output includes the
     command synopsis, local flags, inherited global flags, and concise
     descriptions. Help exits successfully, writes to stdout, emits no
     stderr, and leaves configuration, cache, project, cloud, and device
     state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy device audio monitor --help") { result in
                #expect(result.status.isSuccess)
                #expect(result.stdout.contains("Real-time VU meter for audio levels"))
                #expect(result.stdout.contains("wendy device audio monitor [flags]"))
                #expect(result.stdout.contains("--id"))
                #expect(result.stdout.contains("--rate"))
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
            "WDY-1952: explicit-target monitoring needs a seeded managed agent and simulated audio capability without physical hardware."
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
            try await cli.sh("wendy device audio monitor --json") { result in
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
     Displays a live VU meter for the selected audio input at the requested
     update rate and keeps diagnostics separate from the terminal UI.
     */
    @Test(
        .disabled(
            "WDY-1952: VU rendering needs seeded managed-agent level frames plus controlled terminal output."
        )
    )
    func `renders live audio levels`() async throws {}

    /**
     Invalid device ids or update rates fail before a monitoring stream is
     opened.
     */
    @Test(
        .disabled(
            "WDY-1956: semantic audio parameter ranges are not validated locally before target connection/RPC."
        )
    )
    func `validates monitor parameters before streaming`() async throws {}

    /**
     Cancelling the monitor closes the remote stream and restores terminal
     output without modifying device settings.
     */
    @Test(
        .disabled(
            "WDY-1952: cancellation cleanup needs seeded streaming RPC state and harness process control."
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
            try await cli.sh("wendy device audio monitor --bogus") { result in
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
            "WDY-1934: 'wendy device audio monitor' silently accepts positional arguments because the leaf command has no cobra.NoArgs validator."
        )
    )
    func `rejects undocumented positional arguments`() async throws {}
}
