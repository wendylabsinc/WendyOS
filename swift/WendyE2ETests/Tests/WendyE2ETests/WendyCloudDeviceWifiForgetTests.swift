import Testing
import WendyE2ETesting

@Suite
struct `'wendy cloud device wifi forget'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays usage for `wendy cloud device wifi forget`. The output includes
     the command synopsis, local flags, inherited global flags, and concise
     descriptions. Help exits successfully, writes to stdout, emits no
     stderr, and leaves configuration, cache, project, cloud, and device
     state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy cloud device wifi forget --help") { result in
                #expect(result.status.isSuccess)
                #expect(result.stdout.contains("Remove a known WiFi network"))
                #expect(result.stdout.contains("wendy cloud device wifi forget [flags]"))
                #expect(result.stdout.contains("--ssid"))
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
            "WDY-1949/WDY-1952: explicit cloud-target forget needs a seeded managed agent and simulated WiFi capability without physical radios."
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
            try await cli.sh("wendy cloud device wifi forget --ssid Example --device target --json")
            { result in
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
     Removes saved credentials for the requested SSID without affecting other
     known networks or current nonmatching connections.
     */
    @Test(
        .disabled("WDY-1952: successful forget needs simulated managed-agent saved-network state.")
    )
    func `forgets a known WiFi network`() async throws {}

    /**
     Missing `--ssid` produces a usage diagnostic and no WiFi operation.
     */
    @Test
    func `requires an SSID`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy cloud device wifi forget") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("--ssid is required"))
            }
        }
    }

    /**
     Forgetting an SSID that is not saved reports a clear result and leaves all
     known networks unchanged.
     */
    @Test(
        .disabled(
            "WDY-1952: unknown-network isolation needs seeded neighboring managed-agent network profiles."
        )
    )
    func `reports unknown networks without changing saved credentials`() async throws {}

    /**
     Rejects flags that are not part of the command's documented interface.

     The command reports a usage error on stderr and does not perform the
     requested operation.
     */
    @Test
    func `rejects undocumented flags`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy cloud device wifi forget --bogus") { result in
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
            "WDY-1934: 'wendy cloud device wifi forget' silently accepts positional arguments because the leaf command has no cobra.NoArgs validator."
        )
    )
    func `rejects undocumented positional arguments`() async throws {}
}
