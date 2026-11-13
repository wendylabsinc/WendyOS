import Testing
import WendyE2ETesting

@Suite
struct `'wendy device apps stop'` {
    let scenario = CLIAndAgentScenario()

    /**
     An explicitly named application that is absent from the device fails
     without inventing success or changing the inventory of deployed apps.
     The diagnostic identifies the requested app so the caller can correct it.
     */
    @Test(.enabled(if: httpAppTestsEnabled))
    func `rejects a missing application and preserves deployed app state`() async throws {
        try await HTTPAppFixture.scenario().run { cli, _ in
            try await HTTPAppFixture.withProject(cli: cli, imageMayBeImported: false) { app in
                try await cli.sh("\(app.command) device apps stop \(app.app)") { result in
                    #expect(result.status.isFailure)
                    #expect(result.stderr.contains(app.app))
                    #expect(result.stdout.isEmpty)
                }
                let after = try await app.inventory(cli)
                #expect(!after.contains { $0.name == app.app })
            }
        }
    }

    /**
     Stopping a running HTTP application identifies the app in its JSON result,
     leaves it listed as stopped with the same version and port, and makes its
     endpoint stop answering. Other application identities remain present.
     */
    @Test(
        .enabled(if: httpAppTestsEnabled),
        .disabled(
            "Fixture blocked: Jetson build host has no BuildKit and the CLI host has no container builder; validate a supported builder path before enabling"
        )
    )
    func `stops the selected HTTP app and its endpoint`() async throws {
        try await HTTPAppFixture.scenario().run { cli, _ in
            try await HTTPAppFixture.withApp(cli: cli) { app in
                _ = try await app.response(cli)
                try await app.stop(cli)
            }
        }
    }

    /**
     Displays usage for `wendy device apps stop`. The output includes the
     command synopsis, local flags, inherited global flags, and concise
     descriptions. Help exits successfully, writes to stdout, emits no
     stderr, and leaves configuration, cache, project, cloud, and device
     state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy device apps stop --help") { result in
                #expect(result.status.isSuccess)
                #expect(result.stdout.contains("Stop an application"))
                #expect(result.stdout.contains("wendy device apps stop [app-name] [flags]"))
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
            "WDY-1952: explicit-target stop needs seeded managed-agent application state without a physical device."
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
            try await cli.sh("wendy device apps stop example --json") { result in
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
     Stops the named application and prints a concise confirmation after the
     agent reports it stopped.
     */
    @Test(
        .disabled(
            "WDY-1952: successful stop needs seeded running managed-agent application/container state."
        )
    )
    func `stops a running application`() async throws {}

    /**
     An already stopped app produces a clear no-op or not-running result
     without affecting other applications.
     */
    @Test(
        .disabled(
            "WDY-1952: stopped-app idempotence needs seeded stopped managed-agent application/container state."
        )
    )
    func `handles already stopped applications predictably`() async throws {}

    /**
     Unknown app names fail without stopping containers that have similar
     names.
     */
    @Test(
        .disabled(
            "WDY-1952: unknown-app isolation needs seeded neighboring managed-agent application/container state."
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
            try await cli.sh("wendy device apps stop example --bogus") { result in
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
            try await cli.sh("wendy device apps stop one two") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("accepts at most 1 arg(s), received 2"))
            }
        }
    }
}
