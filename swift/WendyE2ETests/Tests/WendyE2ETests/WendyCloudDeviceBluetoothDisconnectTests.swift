import Testing
import WendyE2ETesting

@Suite
struct `'wendy cloud device bluetooth disconnect'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays usage for `wendy cloud device bluetooth disconnect`. The output
     includes the command synopsis, local flags, inherited global flags,
     and concise descriptions. Help exits successfully, writes to stdout,
     emits no stderr, and leaves configuration, cache, project, cloud, and
     device state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy cloud device bluetooth disconnect --help") { result in
                #expect(result.status.isSuccess)
                #expect(result.stdout.contains("Disconnect a Bluetooth peripheral"))
                #expect(
                    result.stdout.contains(
                        "wendy cloud device bluetooth disconnect <address> [flags]"
                    )
                )
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
            "WDY-1949/WDY-1952: explicit cloud-target disconnection needs a seeded managed agent and simulated Bluetooth capability without physical hardware."
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
                "wendy cloud device bluetooth disconnect AA:BB:CC:DD:EE:FF --device target --json"
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
     Disconnects the requested peripheral address and prints a concise
     confirmation after the agent reports it disconnected.
     */
    @Test(
        .disabled(
            "WDY-1952: successful disconnection needs simulated managed-agent Bluetooth connection state."
        )
    )
    func `disconnects a Bluetooth peripheral`() async throws {}

    /**
     Missing or malformed addresses produce a usage diagnostic and no Bluetooth
     operation.
     */
    @Test
    func `requires a peripheral address`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy cloud device bluetooth disconnect") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("accepts 1 arg(s), received 0"))
            }
        }
    }

    /**
     Rejects peripheral addresses that do not use the documented Bluetooth
     address format.

     Validation fails before connecting to a device or changing Bluetooth state.
     */
    @Test(
        .disabled(
            "WDY-1957: malformed Bluetooth addresses are forwarded to target resolution/RPC without local validation."
        )
    )
    func `rejects malformed peripheral addresses`() async throws {}

    /**
     A peripheral that is not connected produces a clear no-op or not-connected
     result without affecting pairing state.
     */
    @Test(
        .disabled(
            "WDY-1952: already-disconnected semantics need simulated managed-agent Bluetooth state."
        )
    )
    func `handles already disconnected peripherals predictably`() async throws {}

    /**
     Rejects flags that are not part of the command's documented interface.

     The command reports a usage error on stderr and does not perform the
     requested operation.
     */
    @Test
    func `rejects undocumented flags`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy cloud device bluetooth disconnect AA:BB:CC:DD:EE:FF --bogus") {
                result in
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
            try await cli.sh("wendy cloud device bluetooth disconnect one two") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("accepts 1 arg(s), received 2"))
            }
        }
    }
}
