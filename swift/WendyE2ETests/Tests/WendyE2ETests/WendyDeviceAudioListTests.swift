import Testing
import WendyE2ETesting

@Suite
struct `'wendy device audio list'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays usage for `wendy device audio list`. The output includes the
     command synopsis, local flags, inherited global flags, and concise
     descriptions. Help exits successfully, writes to stdout, emits no
     stderr, and leaves configuration, cache, project, cloud, and device
     state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy device audio list --help") { result in
                #expect(result.status.isSuccess)
                #expect(result.stdout.contains("List audio devices"))
                #expect(result.stdout.contains("wendy device audio list [flags]"))
                #expect(result.stdout.contains("--device"))
                #expect(result.stdout.contains("--json"))
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
            "WDY-1952: explicit-target inventory needs seeded managed-agent audio state without a physical device."
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
            try await cli.sh("wendy device audio list --json") { result in
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
     Displays input and output audio devices with ids, names, channel counts,
     sample rates, and default markers when available.
     */
    @Test(
        .disabled(
            "WDY-1952: human audio inventory needs seeded managed-agent audio hardware/capability state."
        )
    )
    func `lists audio devices`() async throws {}

    /**
     With `--json`, emits audio device objects with stable id, direction,
     default, channel, and sample-rate fields.
     */
    @Test(
        .disabled(
            "WDY-1952: JSON audio schema needs seeded managed-agent audio hardware/capability state."
        )
    )
    func `prints JSON audio device inventory`() async throws {}

    /**
     Rejects flags that are not part of the command's documented interface.

     The command reports a usage error on stderr and does not perform the
     requested operation.
     */
    @Test
    func `rejects undocumented flags`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy device audio list --bogus") { result in
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
    @Test
    func `rejects undocumented positional arguments`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await LocalCLIAssertions.prepare(cli)
            let result = try await LocalCLIAssertions.run(
                cli,
                ["device", "audio", "list", "unexpected", "--json", "--device", "127.0.0.1:1"],
                exit: 2,
                monitorNetwork: true
            )
            try LocalCLIAssertions.error(result, code: "cli_usage", exit: 2, message: "unexpected")
            #expect(result.readableStderr.contains("wendy device audio list"))
            try await LocalCLIAssertions.unchanged(cli)
        }
    }
}
