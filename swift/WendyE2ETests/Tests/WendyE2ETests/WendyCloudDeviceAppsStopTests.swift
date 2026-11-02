import Testing
import WendyE2ETesting

@Suite
struct `'wendy cloud device apps stop'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays usage for `wendy cloud device apps stop`. The output includes the
     command synopsis, local flags, inherited global flags, and concise
     descriptions. Help exits successfully, writes to stdout, emits no
     stderr, and leaves configuration, cache, project, cloud, and device
     state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy cloud device apps stop --help") { result in
                #expect(result.status.isSuccess)
                #expect(result.stdout.contains("Stop an application"))
                #expect(
                    result.stdout.contains(
                        "wendy cloud device apps stop [app-name] [flags]"
                    )
                )
                #expect(result.stdout.contains("--cloud-grpc"))
                #expect(result.stdout.contains("--device"))
                #expect(result.stdout.contains("--json"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     `--device` selects the cloud device and skips local discovery and pickers.
     */
    @Test(
        .disabled(
            "WDY-1949/WDY-1952: explicit cloud-target stop needs isolated auth, tunnel, and seeded managed-agent application state."
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
                "wendy cloud device apps stop example --device target --json"
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
            "WDY-1952: tunnel, connection, and incompatible-RPC failures need controllable seeded cloud and managed-agent responses."
        )
    )
    func `reports unreachable devices without partial success`() async throws {}

    /**
     Stops the named application and prints a concise confirmation after the
     agent reports it stopped.
     */
    @Test(
        .disabled(
            "WDY-1952: successful stop needs seeded running cloud-managed application/container state."
        )
    )
    func `stops a running application`() async throws {}

    /**
     An already stopped app produces a clear no-op or not-running result
     without affecting other applications.
     */
    @Test(
        .disabled(
            "WDY-1952: stopped-app idempotence needs seeded stopped cloud-managed application/container state."
        )
    )
    func `handles already stopped applications predictably`() async throws {}

    /**
     Unknown app names fail without stopping containers that have similar
     names.
     */
    @Test(
        .disabled(
            "WDY-1952: unknown-app isolation needs seeded neighboring cloud-managed application/container state."
        )
    )
    func `reports unknown applications without side effects`() async throws {}

    /**
     Rejects flags that are not part of the command's documented interface.

     The command reports a usage error on stderr and does not perform the
     requested operation.
     */
    @Test
    func `rejects undocumented flags`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy cloud device apps stop example --bogus") { result in
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
            try await cli.sh("wendy cloud device apps stop one two") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("accepts at most 1 arg(s), received 2"))
            }
        }
    }
}
