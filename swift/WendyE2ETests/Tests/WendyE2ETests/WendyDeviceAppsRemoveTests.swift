import Testing
import WendyE2ETesting

@Suite
struct `'wendy device apps remove'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays usage for `wendy device apps remove`. The output includes the
     command synopsis, local flags, inherited global flags, and concise
     descriptions. Help exits successfully, writes to stdout, emits no
     stderr, and leaves configuration, cache, project, cloud, and device
     state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy device apps remove --help") { result in
                #expect(result.status.isSuccess)
                #expect(result.stdout.contains("Remove an application"))
                #expect(result.stdout.contains("wendy device apps remove [app-name] [flags]"))
                #expect(result.stdout.contains("--cleanup"))
                #expect(result.stdout.contains("--delete-volumes"))
                #expect(result.stdout.contains("--force"))
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
            "WDY-1952: explicit-target removal needs seeded managed-agent application state without a physical device."
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
            try await cli.sh("wendy device apps remove example --force --json") { result in
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
     Removes the named application only after confirmation or `--force`.
     Success output identifies the removed app and any optional cleanup
     performed.
     */
    @Test(
        .disabled(
            "WDY-1952: confirmation and successful removal need seeded managed-agent application state."
        )
    )
    func `removes an application after confirmation`() async throws {}

    /**
     `--cleanup` removes the container image and `--delete-volumes` removes
     persistent volumes only for the named app. Omitted cleanup flags leave
     those resources intact.
     */
    @Test(
        .disabled(
            "WDY-1952: cleanup isolation needs seeded application, image, and persistent-volume state."
        )
    )
    func `honors cleanup and volume deletion flags`() async throws {}

    /**
     An app name that is not deployed produces a failure diagnostic and does
     not remove images, volumes, or other apps.
     */
    @Test(
        .disabled(
            "WDY-1952: unknown-application behavior needs seeded neighboring application and resource state."
        )
    )
    func `reports unknown applications without deleting resources`() async throws {}

    /**
     Rejects flags that are not part of the command's documented interface.

     The command reports a usage error on stderr and does not perform the
     requested operation.
     */
    @Test
    func `rejects undocumented flags`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy device apps remove example --bogus") { result in
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
            try await cli.sh("wendy device apps remove one two") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("accepts at most 1 arg(s), received 2"))
            }
        }
    }
}
