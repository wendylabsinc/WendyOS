import Testing
import WendyE2ETesting

@Suite
struct `'wendy device apps start'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays usage for `wendy device apps start`. The output includes the
     command synopsis, local flags, inherited global flags, and concise
     descriptions. Help exits successfully, writes to stdout, emits no
     stderr, and leaves configuration, cache, project, cloud, and device
     state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy device apps start --help") { result in
                #expect(result.status.isSuccess)
                #expect(result.stdout.contains("Start an application"))
                #expect(result.stdout.contains("wendy device apps start [app-name] [flags]"))
                #expect(result.stdout.contains("--detach"))
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
            "WDY-1952: explicit-target startup needs seeded managed-agent application state without a physical device."
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
            try await cli.sh("wendy device apps start example --detach --json") { result in
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
     Starts the named deployed application and streams container output until
     the app exits or the user cancels. Startup failure is reported as command
     failure.
     */
    @Test(
        .disabled(
            "WDY-1952: startup and streamed output need seeded managed-agent application/container state plus process control."
        )
    )
    func `starts an application and streams output`() async throws {}

    /**
     `--detach` starts the app and returns after start-up status is known
     without streaming logs.
     */
    @Test(
        .disabled(
            "WDY-1952: detached startup needs seeded managed-agent application/container state."
        )
    )
    func `starts detached when requested`() async throws {}

    /**
     A missing app name or unknown deployed app produces a usage or not-found
     diagnostic and no new container is created.
     */
    @Test(
        .disabled(
            "WDY-1952: unknown-application behavior needs seeded managed-agent application/container state."
        )
    )
    func `reports unknown applications without creating containers`() async throws {}

    /**
     Rejects flags that are not part of the command's documented interface.

     The command reports a usage error on stderr and does not perform the
     requested operation.
     */
    @Test
    func `rejects undocumented flags`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy device apps start example --bogus") { result in
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
            try await cli.sh("wendy device apps start one two") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("accepts at most 1 arg(s), received 2"))
            }
        }
    }
}
