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
            try await cli.sh("wendy completion bash --help").run { result in
                let stdout = result.stdout

                #expect(result.status.isSuccess)
                #expect(stdout.contains("Print bash completion script"))
                #expect(stdout.contains("Usage:"))
                #expect(stdout.contains("wendy completion bash [flags]"))
                #expect(stdout.contains("--help"))
                #expect(stdout.contains("--device"))
                #expect(stdout.contains("--json"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     Writes a valid bash completion script to stdout. The command emits no
     stderr, exits successfully, and does not read or write shell rc files.
     */
    @Test
    func `prints the bash completion script`() async throws {
        try await self.scenario.run { cli, _ in
            try await cli.sh(
                """
                wendy completion bash
                test ! -e "$HOME/.bashrc"
                """
            ).run { result in

                #expect(result.status.isSuccess)
                #expect(result.stdout.contains("# bash completion V2 for wendy"))
                #expect(result.stdout.contains("__start_wendy"))
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
            try await cli.sh("wendy completion bash").run { result in

                #expect(result.status.isSuccess)
                #expect(result.stdout.contains("requestComp"))
                #expect(result.stdout.contains("allows handling aliases"))
                #expect(result.stderr == "")
            }

            try await cli.sh("wendy __complete device ''").run { result in

                #expect(result.status.isSuccess)
                #expect(result.stdout.contains("wifi"))
                #expect(result.stdout.contains("bluetooth"))
            }

            try await cli.sh("wendy __complete device version --").run { result in
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
            let first = try await cli.sh("wendy completion bash").run { result in

                #expect(result.status.isSuccess)
                #expect(result.stderr == "")
                return result.stdout
            }

            let second = try await cli.sh("wendy completion bash").run { result in

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
            try await cli.sh("wendy completion bash extra").run { result in
                let stderr = result.stderr

                #expect(!result.status.isSuccess)
                #expect(result.stdout == "")
                #expect(stderr.contains("unknown command"))
                #expect(stderr.contains("extra"))
                #expect(!stderr.contains("# bash completion"))
            }
        }
    }
}
