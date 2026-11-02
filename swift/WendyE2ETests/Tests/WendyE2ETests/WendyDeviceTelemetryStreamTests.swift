import Testing
import WendyE2ETesting

@Suite
struct `'wendy device telemetry-stream'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays usage for `wendy device telemetry-stream`. The output includes
     the command synopsis, local flags, inherited global flags, and concise
     descriptions. Help exits successfully, writes to stdout, emits no
     stderr, and leaves configuration, cache, project, cloud, and device
     state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy device telemetry-stream --help") { result in
                #expect(result.status.isSuccess)
                #expect(
                    result.stdout.contains("Stream telemetry data (logs, metrics, traces) as JSONL")
                )
                #expect(result.stdout.contains("wendy device telemetry-stream [flags]"))
                #expect(result.stdout.contains("--app"))
                #expect(result.stdout.contains("--service"))
                #expect(result.stdout.contains("--logs"))
                #expect(result.stdout.contains("--metrics"))
                #expect(result.stdout.contains("--traces"))
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
            "WDY-1952: explicit-target telemetry needs a seeded managed agent with logs, metrics, and traces."
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
            try await cli.sh("wendy device telemetry-stream --logs --json") { result in
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
     Streams selected logs, metrics, and traces as JSON lines with stable
     envelope fields and timestamps. The stream continues until cancelled or
     the device closes it.
     */
    @Test(
        .disabled(
            "WDY-1952: JSONL framing needs seeded logs, metrics, and traces plus bounded stream control."
        )
    )
    func `streams telemetry as JSONL`() async throws {}

    /**
     App, service, logs, metrics, and traces filters constrain the stream
     before entries are emitted to stdout.
     */
    @Test(
        .disabled(
            "WDY-1952: telemetry filtering needs seeded neighboring app/service records across all signal types."
        )
    )
    func `applies telemetry filters`() async throws {}

    /**
     Cancelling the stream closes the remote telemetry subscription and emits
     no malformed trailing JSON.
     */
    @Test(
        .disabled(
            "WDY-1952: telemetry cancellation cleanup needs seeded streams and harness process control."
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
            try await cli.sh("wendy device telemetry-stream --bogus") { result in
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
            "WDY-1934: 'wendy device telemetry-stream' silently accepts positional arguments because the leaf command has no cobra.NoArgs validator."
        )
    )
    func `rejects undocumented positional arguments`() async throws {}
}
