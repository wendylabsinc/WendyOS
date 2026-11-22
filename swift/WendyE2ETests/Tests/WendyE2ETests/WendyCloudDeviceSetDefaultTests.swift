import Testing
import WendyE2ETesting

@Suite
struct `'wendy cloud device set-default'` {
    let scenario = CLIAndAgentScenario()

    /**
     The removed command's help explains that callers must choose a device
     with `--device` or `WENDY_DEVICE`. It does not connect or change settings.
     */
    @Test
    func `prints migration help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await LocalCLIAssertions.prepare(cli)
            let result = try await LocalCLIAssertions.run(
                cli,
                ["cloud", "device"] + ["set-default", "--help"],
                exit: 0
            )
            #expect(
                result.stdout.contains("Removed: select a device with --device or WENDY_DEVICE")
            )
            #expect(result.stderr == "")
            try await LocalCLIAssertions.unchanged(cli)
        }
    }

    /**
     Legacy default-device commands fail with migration guidance instead of
     saving or selecting a device. They preserve configuration, device pins,
     unknown settings and project files, and require no Cloud authentication.
     */
    @Test
    func `rejects removed default commands without changing state`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await LocalCLIAssertions.removedDeviceDefaults(cli, parent: ["cloud", "device"])
        }
    }
}
