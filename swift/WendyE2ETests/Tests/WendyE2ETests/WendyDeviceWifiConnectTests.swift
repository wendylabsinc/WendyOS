import Testing
import WendyE2ETesting

@Suite
struct `'wendy device wifi connect'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays usage for `wendy device wifi connect`. The output includes the
     command synopsis, local flags, inherited global flags, and concise
     descriptions. Help exits successfully, writes to stdout, emits no
     stderr, and leaves configuration, cache, project, cloud, and device
     state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy device wifi connect --help") { result in
                #expect(result.status.isSuccess)
                #expect(result.stdout.contains("Connect to a WiFi network"))
                #expect(result.stdout.contains("wendy device wifi connect [flags]"))
                #expect(result.stdout.contains("--ssid"))
                #expect(result.stdout.contains("--password"))
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
            "WDY-1952: explicit-target connection needs a seeded managed agent and simulated WiFi capability without physical radios."
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
            try await cli.sh("wendy device wifi connect --ssid Example --json") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("no device specified"))
                #expect(!result.stderr.contains("Password"))
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
     Connects the device to the requested SSID using the supplied password when
     needed and reports the resulting connection status.
     */
    @Test(
        .disabled(
            "WDY-1952: successful connection needs simulated managed-agent WiFi association state without physical radios."
        )
    )
    func `connects to a WiFi network`() async throws {}

    /**
     For secured networks without `--password`, prompts in an interactive
     terminal without echoing the secret. Non-interactive mode reports that a
     password is required.
     */
    @Test(
        .disabled(
            "WDY-1952: password prompting and redaction need a scripted PTY plus simulated secured-network state."
        )
    )
    func `prompts for missing password only in interactive mode`() async throws {}

    /**
     Prompts for a WiFi network when its SSID is omitted in an interactive
     session.

     The selected network is passed to the connection flow; cancellation leaves
     device networking unchanged.
     */
    @Test(
        .disabled(
            "WDY-1952: omitted SSID intentionally opens a device-backed network picker and needs simulated scan results."
        )
    )
    func `selects a missing SSID interactively`() async throws {}

    /**
     Authentication or association failures report the error and do not replace
     existing working network credentials.
     */
    @Test(
        .disabled(
            "WDY-1952: failed-credential preservation needs seeded neighboring managed-agent network profiles."
        )
    )
    func `does not save failed credentials`() async throws {}

    /**
     Rejects flags that are not part of the command's documented interface.

     The command reports a usage error on stderr and does not perform the
     requested operation.
     */
    @Test
    func `rejects undocumented flags`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy device wifi connect --bogus") { result in
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
            "WDY-1934: 'wendy device wifi connect' silently accepts positional arguments because the leaf command has no cobra.NoArgs validator."
        )
    )
    func `rejects undocumented positional arguments`() async throws {}
}
