import Testing
import WendyE2ETesting

@Suite
struct `'wendy device drivers'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays usage for the `wendy device drivers` group. The output includes
     the interactive selection description, its subcommands, and inherited global
     flags. Help exits successfully, writes to stdout, emits no stderr, and leaves configuration,
     cache, project, cloud, and device state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy device drivers --help") { result in
                #expect(result.status.isSuccess)
                #expect(
                    result.stdout.contains(
                        "Interactively select a driver to install, reinstall, or remove."
                    )
                )
                #expect(result.stdout.contains("wendy device drivers [command]"))
                #expect(result.stdout.contains("list"))
                #expect(result.stdout.contains("install"))
                #expect(result.stdout.contains("remove"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     The `driver` alias resolves to the same group, so existing scripts and
     muscle memory keep working.
     */
    @Test
    func `accepts the singular alias`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy device driver --help") { result in
                #expect(result.status.isSuccess)
                #expect(
                    result.stdout.contains(
                        "Interactively select a driver to install, reinstall, or remove."
                    )
                )
            }
        }
    }

    /**
     Rejects subcommands that are not part of the documented interface instead
     of treating them as a device or add-on name.
     */
    @Test
    func `rejects undocumented subcommands`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy device drivers bogus") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("unknown command"))
            }
        }
    }
}
