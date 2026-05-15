import Subprocess
import Testing
import WendyE2ETesting

@Suite
struct `'wendy completion bash'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays usage for `wendy completion bash`. The output includes the
     command synopsis, local flags, inherited global flags, and concise
     descriptions. Help exits successfully, writes to stdout, emits no
     stderr, and leaves configuration, cache, project, cloud, and device
     state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run { cli, _ in
            try await cli.sh("wendy completion bash --help") {
                terminationStatus,
                standardOutput,
                standardError in

                #expect(terminationStatus.isSuccess)
                #expect(standardOutput.contains("Print bash completion script"))
                #expect(standardOutput.contains("Usage:"))
                #expect(standardOutput.contains("wendy completion bash [flags]"))
                #expect(standardOutput.contains("--help"))
                #expect(standardOutput.contains("--device"))
                #expect(standardOutput.contains("--json"))
                #expect(standardError == "")
            }
        }
    }

    /**
     Writes a valid bash completion script to stdout. The command emits no
     stderr, exits successfully, and does not read or write shell rc files.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `prints the bash completion script`() async throws {
        // TODO: implement.
    }

    /**
     The generated script completes top-level commands, nested commands,
     local flags, inherited global flags, and documented aliases using the
     syntax of the target shell.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `includes commands, flags, and aliases`() async throws {
        // TODO: implement.
    }

    /**
     Repeated invocations for the same CLI version produce identical output
     apart from line endings normalized by the platform shell conventions.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `is deterministic across repeated runs`() async throws {
        // TODO: implement.
    }

    /**
     Unexpected positional arguments produce a usage diagnostic on stderr
     and no completion script on stdout.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `rejects extra arguments without printing a script`() async throws {
        // TODO: implement.
    }
}
