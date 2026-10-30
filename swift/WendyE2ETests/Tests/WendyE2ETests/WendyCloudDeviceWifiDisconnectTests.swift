import Testing
import WendyE2ETesting

@Suite
struct `'wendy cloud device wifi disconnect'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays usage for `wendy cloud device wifi disconnect`. The output
     includes the command synopsis, local flags, inherited global flags,
     and concise descriptions. Help exits successfully, writes to stdout,
     emits no stderr, and leaves configuration, cache, project, cloud, and
     device state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy cloud device wifi disconnect --help") { result in
                #expect(result.status.isSuccess)
                #expect(result.stdout.contains("Disconnect from the current WiFi network"))
                #expect(result.stdout.contains("wendy cloud device wifi disconnect [flags]"))
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
            "WDY-1949/WDY-1952: explicit cloud-target disconnection needs a seeded managed agent and simulated WiFi capability without physical radios."
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
            try await cli.sh("wendy cloud device wifi disconnect --device target --json") {
                result in
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
     Disconnects the active WiFi connection and reports the resulting
     disconnected state.
     */
    @Test(
        .disabled(
            "WDY-1952: successful disconnection needs simulated managed-agent WiFi connection state."
        )
    )
    func `disconnects from the current WiFi network`() async throws {}

    /**
     A device with no active WiFi connection reports a no-op or already-
     disconnected result without changing saved networks.
     */
    @Test(
        .disabled(
            "WDY-1952: already-disconnected semantics need simulated managed-agent WiFi state."
        )
    )
    func `handles already disconnected devices predictably`() async throws {}

    /**
     Rejects flags that are not part of the command's documented interface.

     The command reports a usage error on stderr and does not perform the
     requested operation.
     */
    @Test
    func `rejects undocumented flags`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy cloud device wifi disconnect --bogus") { result in
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
            "WDY-1934: 'wendy cloud device wifi disconnect' silently accepts positional arguments because the leaf command has no cobra.NoArgs validator."
        )
    )
    func `rejects undocumented positional arguments`() async throws {}
}
