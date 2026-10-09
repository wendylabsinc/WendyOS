import Testing
import WendyE2ETesting

@Suite
struct `'wendy device logs'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays usage for `wendy device logs`. The output includes the command
     synopsis, local flags, inherited global flags, and concise
     descriptions. Help exits successfully, writes to stdout, emits no
     stderr, and leaves configuration, cache, project, cloud, and device
     state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy device logs --help") { result in
                #expect(result.status.isSuccess)
                #expect(
                    result.stdout.contains(
                        "Stream logs from containers on WendyOS or the firmware console on Wendy Lite"
                    )
                )
                #expect(result.stdout.contains("wendy device logs [app] [flags]"))
                #expect(result.stdout.contains("--app"))
                #expect(result.stdout.contains("--service"))
                #expect(result.stdout.contains("--level"))
                #expect(result.stdout.contains("--tail"))
                #expect(result.stdout.contains("--device"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     `--device` selects the target device hostname and skips discovery and
     pickers. The command does not read or change the saved default device when
     an explicit target is supplied.
     */
    @Test(
        .disabled(
            "WDY-1952: explicit-target log streaming needs a seeded managed agent with telemetry state."
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
            try await cli.sh("wendy device logs Example --json") { result in
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
     Streams logs from the selected device and applies app, service, level, and
     severity filters before presenting entries.
     */
    @Test(
        .disabled(
            "WDY-1952: filtered log streaming needs seeded managed-agent container and telemetry records."
        )
    )
    func `streams device logs`() async throws {}

    /**
     With `--json`, emits newline-delimited or array-wrapped structured log
     entries suitable for automation, without table formatting.
     */
    @Test(
        .disabled(
            "WDY-1952: JSON log framing needs seeded managed-agent telemetry records and bounded stream control."
        )
    )
    func `prints structured log entries in JSON mode`() async throws {}

    /**
     Cancelling log streaming closes the remote stream without changing app or
     device state.
     */
    @Test(
        .disabled(
            "WDY-1952: log cancellation cleanup needs a seeded stream and harness process control."
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
            try await cli.sh("wendy device logs --bogus") { result in
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
            try await cli.sh("wendy device logs one two") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("accepts at most 1 arg(s), received 2"))
            }
        }
    }
}
