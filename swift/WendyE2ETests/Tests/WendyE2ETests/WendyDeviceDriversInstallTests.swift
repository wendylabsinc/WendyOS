import Testing
import WendyE2ETesting

@Suite
struct `'wendy device drivers install'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays usage for `wendy device drivers install`. The output includes the
     command synopsis, local flags, inherited global flags, and concise
     descriptions. Help exits successfully, writes to stdout, emits no
     stderr, and leaves configuration, cache, project, cloud, and device
     state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy device drivers install --help") { result in
                #expect(result.status.isSuccess)
                #expect(result.stdout.contains("wendy device drivers install <name> [flags]"))
                #expect(result.stdout.contains("--file"))
                #expect(result.stdout.contains("--signature"))
                #expect(result.stdout.contains("--module"))
                #expect(result.stdout.contains("--pr"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     The add-on name is required: it must match the image's embedded
     extension-release, so there is no sensible default to infer.
     */
    @Test
    func `requires the add-on name`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy device drivers install --json") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("accepts 1 arg(s)"))
            }
        }
    }

    /**
     An explicitly empty `--file` must not silently fall through to the
     registry path, where it would install something the operator did not ask
     for.
     */
    @Test
    func `rejects an empty --file`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy device drivers install acme --file '' --json") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("--file requires a path"))
            }
        }
    }

    /**
     `--module` overrides the add-on's own list and is meaningful only with a
     local file; a registry install takes its list from the manifest, where an
     override would be silently discarded.
     */
    @Test
    func `rejects --module without --file`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy device drivers install acme --module snd --json") { result in
                #expect(result.status.isFailure)
                #expect(result.stderr.contains("--module is only valid with --file"))
            }
        }
    }

    /**
     A registry install carries the manifest's signature, so accepting an
     override would let one add-on's bytes be paired with another's signature.
     */
    @Test
    func `rejects --signature without --file`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy device drivers install acme --signature AAAA --json") {
                result in
                #expect(result.status.isFailure)
                #expect(result.stderr.contains("--signature is only valid with --file"))
            }
        }
    }

    /**
     Rejects flags that are not part of the command's documented interface.
     */
    @Test
    func `rejects undocumented flags`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy device drivers install acme --bogus") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("unknown flag"))
            }
        }
    }
}
