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
            try await cli.sh("wendy completion zsh --help") { result in
                let stdout = result.stdout

                #expect(result.status.isSuccess)
                #expect(stdout.contains("Print zsh completion script"))
                #expect(stdout.contains("Usage:"))
                #expect(stdout.contains("wendy completion zsh [flags]"))
                #expect(stdout.contains("--help"))
                #expect(stdout.contains("--device"))
                #expect(stdout.contains("--json"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     Writes a valid zsh completion script to stdout. The command emits no
     stderr, exits successfully, and does not read or write shell rc files.
     */
    @Test
    func `prints the zsh completion script`() async throws {
        // AI: Skim the generated script for obvious zsh-completion quality
        // issues that substring assertions miss, such as broken function shape,
        // shell-mismatched syntax, noisy comments, or truncated output.
        try await self.scenario.run { cli, _ in
            try await cli.sh(
                posix: """
                    wendy completion zsh
                    test ! -e "$HOME/.zshrc"
                    """,
                power: """
                    wendy completion zsh
                    if (Test-Path -LiteralPath (Join-Path $env:HOME '.zshrc')) {
                        throw '.zshrc should not exist'
                    }
                    """
            ) { result in
                let stdout = result.stdout

                #expect(result.status.isSuccess)
                #expect(stdout.contains("#compdef wendy"))
                #expect(stdout.contains("compdef _wendy wendy"))
                #expect(stdout.contains("_wendy()"))
                #expect(result.stderr == "")
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
            try await cli.sh("wendy completion zsh") { result in

                #expect(result.status.isSuccess)
                #expect(result.stdout.contains("requestComp"))
                #expect(result.stdout.contains("compdef _wendy wendy"))
                #expect(result.stderr == "")
            }

            try await cli.sh("wendy __complete device ''") { result in

                #expect(result.status.isSuccess)
                #expect(result.stdout.contains("wifi"))
                #expect(result.stdout.contains("bluetooth"))
            }

            try await cli.sh("wendy __complete device version --") { result in
                let stdout = result.stdout

                #expect(result.status.isSuccess)
                #expect(stdout.contains("--device"))
                #expect(stdout.contains("--json"))
                #expect(stdout.contains("--check-updates"))
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
            let first = try await cli.sh("wendy completion zsh") { result in

                #expect(result.status.isSuccess)
                #expect(result.stderr == "")
                return result.stdout
            }

            let second = try await cli.sh("wendy completion zsh") { result in

                #expect(result.status.isSuccess)
                #expect(result.stderr == "")
                return result.stdout
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
            try await cli.sh("wendy completion zsh extra") { result in
                let stderr = result.stderr

                #expect(!result.status.isSuccess)
                #expect(result.stdout == "")
                #expect(stderr.contains("unknown command"))
                #expect(stderr.contains("extra"))
                #expect(!stderr.contains("#compdef"))
            }
        }
    }
}
