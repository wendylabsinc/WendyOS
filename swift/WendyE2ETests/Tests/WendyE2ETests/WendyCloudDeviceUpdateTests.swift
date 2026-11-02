import Testing
import WendyE2ETesting

@Suite
struct `'wendy cloud device update'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays usage for `wendy cloud device update`. The output includes the
     command synopsis, local flags, inherited global flags, and concise
     descriptions. Help exits successfully, writes to stdout, emits no
     stderr, and leaves configuration, cache, project, cloud, and device
     state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy cloud device update --help") { result in
                #expect(result.status.isSuccess)
                #expect(result.stdout.contains("Updates the agent binary on the device"))
                #expect(result.stdout.contains("wendy cloud device update [flags]"))
                #expect(result.stdout.contains("--binary"))
                #expect(result.stdout.contains("--nightly"))
                #expect(result.stdout.contains("--artifact-url"))
                #expect(result.stdout.contains("--pr"))
                #expect(result.stdout.contains("--yes"))
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
            "WDY-1949/WDY-1952: explicit cloud-target update needs a disposable seeded agent and isolated release/artifact fixtures."
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
            try await cli.sh("wendy cloud device update --device target --json") { result in
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
            "WDY-1952: connection and incompatible-RPC failures need a disposable seeded agent with controllable responses."
        )
    )
    func `reports unreachable devices without partial success`() async throws {}

    /**
     Downloads the selected agent release, verifies it, uploads it to the
     device, and reports the installed version after update.
     */
    @Test(
        .disabled(
            "WDY-1952: release update needs isolated downloads and a disposable agent restart/reconnect target."
        )
    )
    func `updates the device agent from the latest release`() async throws {}

    /**
     `--binary` skips release download and uploads the provided local agent
     binary after validating that it is readable.
     */
    @Test(
        .disabled(
            "WDY-1952: local binary upload needs an architecture fixture and disposable agent restart/reconnect target."
        )
    )
    func `uploads a local binary when requested`() async throws {}

    /**
     `--nightly` selects a prerelease agent build and reports that prerelease
     channel in output.
     */
    @Test(.disabled("WDY-1952: nightly selection needs isolated release and OS manifest fixtures."))
    func `uses nightly releases when requested`() async throws {}

    /**
     Download, verification, upload, or restart failures report the failing
     stage and do not claim the update completed.
     */
    @Test(
        .disabled(
            "WDY-1952: failure preservation needs controllable download/upload/restart stages on a disposable agent."
        )
    )
    func `preserves the running agent on failed update`() async throws {}

    /**
     Rejects flags that are not part of the command's documented interface.

     The command reports a usage error on stderr and does not perform the
     requested operation.
     */
    @Test
    func `rejects undocumented flags`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy cloud device update --bogus") { result in
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
            "WDY-1934: 'wendy cloud device update' silently accepts positional arguments because the leaf command has no cobra.NoArgs validator."
        )
    )
    func `rejects undocumented positional arguments`() async throws {}
}
