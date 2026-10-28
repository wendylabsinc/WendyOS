import Testing
import WendyE2ETesting

@Suite
struct `'wendy device bluetooth connect'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays usage for `wendy device bluetooth connect`. The output includes
     the command synopsis, local flags, inherited global flags, and concise
     descriptions. Help exits successfully, writes to stdout, emits no
     stderr, and leaves configuration, cache, project, cloud, and device
     state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy device bluetooth connect --help") { result in
                #expect(result.status.isSuccess)
                #expect(result.stdout.contains("Connect to a Bluetooth peripheral"))
                #expect(result.stdout.contains("wendy device bluetooth connect <address> [flags]"))
                #expect(result.stdout.contains("--pair"))
                #expect(result.stdout.contains("--trust"))
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
            "WDY-1952: explicit-target connection needs a seeded managed agent and simulated Bluetooth capability without physical hardware."
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
            try await cli.sh("wendy device bluetooth connect AA:BB:CC:DD:EE:FF --json") { result in
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
     Connects to the requested peripheral address and applies pair and trust
     options. Success output identifies the connected address.
     */
    @Test(
        .disabled(
            "WDY-1952: pair/trust/connect behavior needs simulated managed-agent Bluetooth state without physical radios."
        )
    )
    func `connects to a Bluetooth peripheral`() async throws {}

    /**
     Missing or malformed addresses produce a usage diagnostic before a
     Bluetooth operation starts.
     */
    @Test
    func `requires a peripheral address`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy device bluetooth connect") { result in
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
     Pairing, trust, or connection failures report the failed stage and do not
     claim the peripheral is connected.
     */
    @Test(
        .disabled(
            "WDY-1952: staged pair/trust/connect failures need controllable simulated managed-agent Bluetooth responses."
        )
    )
    func `reports pairing failures without trusting the device`() async throws {}

    /**
     Rejects flags that are not part of the command's documented interface.

     The command reports a usage error on stderr and does not perform the
     requested operation.
     */
    @Test
    func `rejects undocumented flags`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy device bluetooth connect AA:BB:CC:DD:EE:FF --bogus") { result in
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
            try await cli.sh("wendy device bluetooth connect one two") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("accepts 1 arg(s), received 2"))
            }
        }
    }
}
