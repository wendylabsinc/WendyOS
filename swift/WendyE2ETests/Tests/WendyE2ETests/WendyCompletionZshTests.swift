import Subprocess
import Testing
import WendyE2ETesting

@Suite
struct `'wendy completion zsh'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays usage for `wendy completion zsh`. The output includes the command
     synopsis, local flags, inherited global flags, and concise
     descriptions. Help exits successfully, writes to stdout, emits no
     stderr, and leaves configuration, cache, project, cloud, and device
     state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run { cli, _ in
            try await cli.sh("wendy completion zsh --help") {
                terminationStatus,
                standardOutput,
                standardError in

                #expect(terminationStatus.isSuccess)
                #expect(standardOutput.contains("Print zsh completion script"))
                #expect(standardOutput.contains("Usage:"))
                #expect(standardOutput.contains("wendy completion zsh [flags]"))
                #expect(standardOutput.contains("--help"))
                #expect(standardOutput.contains("--device"))
                #expect(standardOutput.contains("--json"))
                #expect(standardError == "")
            }
        }
    }

    /**
     Writes a valid zsh completion script to stdout. The command emits no
     stderr, exits successfully, and does not read or write shell rc files.
     */
    @Test
    func `prints the zsh completion script`() async throws {
        try await self.scenario.run { cli, _ in
            try await cli.sh(
                """
                wendy completion zsh
                test ! -e "$HOME/.zshrc"
                """
            ) {
                terminationStatus,
                standardOutput,
                standardError in

                #expect(terminationStatus.isSuccess)
                #expect(standardOutput.contains("#compdef wendy"))
                #expect(standardOutput.contains("compdef _wendy wendy"))
                #expect(standardOutput.contains("_wendy()"))
                #expect(standardError == "")
            }
        }
    }

    /**
     The generated script completes top-level commands, nested commands,
     local flags, inherited global flags, and documented aliases using the
     syntax of the target shell.
     */
    @Test
    func `includes commands, flags, and aliases`() async throws {
        try await self.scenario.run { cli, _ in
            try await cli.sh("wendy completion zsh") {
                terminationStatus,
                standardOutput,
                standardError in

                #expect(terminationStatus.isSuccess)
                #expect(standardOutput.contains("requestComp"))
                #expect(standardOutput.contains("compdef _wendy wendy"))
                #expect(standardError == "")
            }

            try await cli.sh("wendy __complete device ''") {
                terminationStatus,
                standardOutput,
                _ in

                #expect(terminationStatus.isSuccess)
                #expect(standardOutput.contains("wifi"))
                #expect(standardOutput.contains("bluetooth"))
            }

            try await cli.sh("wendy __complete device version --") {
                terminationStatus,
                standardOutput,
                _ in

                #expect(terminationStatus.isSuccess)
                #expect(standardOutput.contains("--device"))
                #expect(standardOutput.contains("--json"))
                #expect(standardOutput.contains("--check-updates"))
            }
        }
    }

    /**
     Repeated invocations for the same CLI version produce identical output
     apart from line endings normalized by the platform shell conventions.
     */
    @Test
    func `is deterministic across repeated runs`() async throws {
        try await self.scenario.run { cli, _ in
            let first = try await cli.sh("wendy completion zsh") {
                terminationStatus,
                standardOutput,
                standardError in

                #expect(terminationStatus.isSuccess)
                #expect(standardError == "")
                return standardOutput
            }

            let second = try await cli.sh("wendy completion zsh") {
                terminationStatus,
                standardOutput,
                standardError in

                #expect(terminationStatus.isSuccess)
                #expect(standardError == "")
                return standardOutput
            }

            #expect(first == second)
            #expect(!first.isEmpty)
        }
    }

    /**
     Unexpected positional arguments produce a usage diagnostic on stderr
     and no completion script on stdout.
     */
    @Test
    func `rejects extra arguments without printing a script`() async throws {
        try await self.scenario.run { cli, _ in
            try await cli.sh("wendy completion zsh extra") {
                terminationStatus,
                standardOutput,
                standardError in

                #expect(!terminationStatus.isSuccess)
                #expect(standardOutput == "")
                #expect(standardError.contains("unknown command"))
                #expect(standardError.contains("extra"))
                #expect(!standardError.contains("#compdef"))
            }
        }
    }
}
