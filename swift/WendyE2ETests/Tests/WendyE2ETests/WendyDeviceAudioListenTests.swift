import Testing
import WendyE2ETesting

@Suite
struct `'wendy device audio listen'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays usage for `wendy device audio listen`. The output includes the
     command synopsis, local flags, inherited global flags, and concise
     descriptions. Help exits successfully, writes to stdout, emits no
     stderr, and leaves configuration, cache, project, cloud, and device
     state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy device audio listen --help") { result in
                #expect(result.status.isSuccess)
                #expect(result.stdout.contains("Stream raw audio from a device microphone"))
                #expect(result.stdout.contains("wendy device audio listen [flags]"))
                #expect(result.stdout.contains("--sample-rate"))
                #expect(result.stdout.contains("--channels"))
                #expect(result.stdout.contains("--stdout"))
                #expect(result.stdout.contains("--buffer-ms"))
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
            "WDY-1952: explicit-target listening needs a seeded managed agent and simulated audio capability without physical hardware."
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
            try await cli.sh("wendy device audio listen --stdout --json") { result in
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
     Streams audio from the selected microphone using the requested device id,
     channel count, and sample rate. The stream continues until cancelled or
     the device ends it.
     */
    @Test(
        .disabled(
            "WDY-1952: microphone streaming needs seeded managed-agent audio frames without physical hardware."
        )
    )
    func `streams microphone audio`() async throws {}

    /**
     `--stdout` writes raw PCM bytes to stdout and sends diagnostics to stderr
     so piping to another process is safe.
     */
    @Test(
        .disabled(
            "WDY-1952: raw PCM routing needs seeded managed-agent audio frames and stream process control."
        )
    )
    func `writes raw PCM to stdout when requested`() async throws {}

    /**
     Invalid ids, channel counts, or sample rates fail before a stream is
     opened.
     */
    @Test(
        .disabled(
            "WDY-1956: semantic audio parameter ranges are not validated locally before target connection/RPC."
        )
    )
    func `validates audio parameters before streaming`() async throws {}

    /**
     Rejects flags that are not part of the command's documented interface.

     The command reports a usage error on stderr and does not perform the
     requested operation.
     */
    @Test
    func `rejects undocumented flags`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy device audio listen --bogus") { result in
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
            "WDY-1934: 'wendy device audio listen' silently accepts positional arguments because the leaf command has no cobra.NoArgs validator."
        )
    )
    func `rejects undocumented positional arguments`() async throws {}
}
