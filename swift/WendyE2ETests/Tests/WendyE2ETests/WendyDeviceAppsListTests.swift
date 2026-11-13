import Testing
import WendyE2ETesting

@Suite
struct `'wendy device apps list'` {
    let scenario = CLIAndAgentScenario()

    /**
     JSON inventory includes exactly one entry for a deployed HTTP app, with
     its configured identity, version, running state, and port. The listed
     endpoint serves that app's own identity in a real HTTP response.
     */
    @Test(
        .enabled(if: httpAppTestsEnabled),
        .disabled(
            "Fixture blocked: Jetson build host has no BuildKit and the CLI host has no container builder; validate a supported builder path before enabling"
        )
    )
    func `identifies the deployed and responding HTTP application`() async throws {
        try await HTTPAppFixture.scenario().run { cli, _ in
            try await HTTPAppFixture.withApp(cli: cli) { app in
                try await app.requireState(cli, "RUNNING")
                _ = try await app.response(cli)
            }
        }
    }

    /**
     Displays usage for `wendy device apps list`. The output includes the
     command synopsis, local flags, inherited global flags, and concise
     descriptions. Help exits successfully, writes to stdout, emits no
     stderr, and leaves configuration, cache, project, cloud, and device
     state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy device apps list --help") { result in
                #expect(result.status.isSuccess)
                #expect(result.stdout.contains("List deployed applications"))
                #expect(result.stdout.contains("wendy device apps list [flags]"))
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
            "WDY-1952: explicit-target inventory needs seeded managed-agent application state without a physical device."
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
            try await cli.sh("wendy device apps list --json") { result in
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
     Displays deployed applications with names, images, status, restart policy,
     and relevant ports. An empty device reports an empty successful list.
     */
    @Test(
        .disabled(
            "WDY-1952: human application inventory needs seeded empty/populated managed-agent container state."
        )
    )
    func `lists deployed applications`() async throws {}

    /**
     With `--json`, emits application objects with stable field names and value
     types. JSON mode emits no table formatting and no stderr on success.
     */
    @Test(
        .disabled(
            "WDY-1952: JSON application schema needs seeded empty/populated managed-agent container state."
        )
    )
    func `prints JSON application inventory`() async throws {}

    /**
     Rejects flags that are not part of the command's documented interface.

     The command reports a usage error on stderr and does not perform the
     requested operation.
     */
    @Test
    func `rejects undocumented flags`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy device apps list --bogus") { result in
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
            "WDY-1934: 'wendy device apps list' silently accepts extra positional arguments because the leaf command has no cobra.NoArgs validator."
        )
    )
    func `rejects undocumented positional arguments`() async throws {}
}
