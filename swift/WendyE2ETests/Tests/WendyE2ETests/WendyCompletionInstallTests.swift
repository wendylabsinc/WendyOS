import Testing
import WendyE2ETesting

@Suite
struct `'wendy completion install'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays usage for `wendy completion install`. The output includes the
     command synopsis, local flags, inherited global flags, and concise
     descriptions. Help exits successfully, writes to stdout, emits no
     stderr, and leaves configuration, cache, project, cloud, and device
     state untouched.
     */
    @Test
    func `prints command help`() async throws {
        // AI: Review this help as installer documentation. Check whether users
        // can understand the difference between installing, printing paths,
        // printing scripts to stdout, and choosing a shell explicitly.
        try await self.scenario.run { cli, _ in
            try await cli.sh("wendy completion install --help").run { result in
                let stdout = result.stdout

                #expect(result.status.isSuccess)
                #expect(stdout.contains("Detect the current shell"))
                #expect(stdout.contains("Usage:"))
                #expect(stdout.contains("wendy completion install [flags]"))
                #expect(stdout.contains("--shell"))
                #expect(stdout.contains("--print-path"))
                #expect(stdout.contains("--stdout"))
                #expect(stdout.contains("--device"))
                #expect(stdout.contains("--json"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     Detects the user's shell, writes the generated completion script to
     the conventional location, and adds an idempotent source line to
     the shell rc file when that shell needs one.
     */
    @Test
    func `installs completion for the detected shell`() async throws {
        try await self.scenario.run { cli, _ in
            try await cli.sh(
                """
                SHELL=/bin/zsh wendy completion install
                test -f "$HOME/.zfunc/_wendy"
                test -f "$HOME/.zshrc"
                grep -q '#compdef wendy' "$HOME/.zfunc/_wendy"
                grep -q 'wendy-completion' "$HOME/.zshrc"
                """
            ).run { result in
                let stderr = result.stderr

                #expect(result.status.isSuccess)
                #expect(result.stdout == "")
                #expect(stderr.contains("Wrote"))
                #expect(
                    stderr.contains("Updated")
                        || stderr.contains("Already configured")
                )
            }
        }
    }

    /**
     `--shell` bypasses shell detection and installs the script for the
     selected shell only. Other shell configuration files remain
     unchanged.
     */
    @Test
    func `uses the requested shell override`() async throws {
        try await self.scenario.run { cli, _ in
            try await cli.sh(
                """
                wendy completion install --shell fish
                test -f "$HOME/.config/fish/completions/wendy.fish"
                test ! -e "$HOME/.bashrc"
                test ! -e "$HOME/.zshrc"
                """
            ).run { result in

                #expect(result.status.isSuccess)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("wendy.fish"))
                #expect(result.stderr.contains("Fish auto-loads completions"))
            }
        }
    }

    /**
     `--print-path` reports the target script and rc paths as a dry run.
     The command exits successfully without creating directories or
     editing shell configuration.
     */
    @Test
    func `prints install paths without writing files`() async throws {
        try await self.scenario.run { cli, _ in
            try await cli.sh(
                """
                wendy completion install --print-path --shell zsh
                test ! -e "$HOME/.zfunc/_wendy"
                test ! -e "$HOME/.zshrc"
                """
            ).run { result in

                #expect(result.status.isSuccess)
                #expect(result.stdout.contains("/.zfunc/_wendy"))
                #expect(result.stdout.contains("/.zshrc"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     `--stdout` emits the completion script to stdout and performs no
     installation. Stderr remains empty on success.
     */
    @Test
    func `prints the script to stdout when requested`() async throws {
        try await self.scenario.run { cli, _ in
            try await cli.sh(
                """
                wendy completion install --stdout --shell zsh
                test ! -e "$HOME/.zshrc"
                test ! -d "$HOME/.zsh"
                """
            ).run { result in

                #expect(result.status.isSuccess)
                #expect(result.stdout.contains("#compdef wendy"))
                #expect(result.stdout.contains("compdef _wendy wendy"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     Running installation repeatedly leaves a single managed completion
     script and a single source line in the relevant shell rc file.
     */
    @Test
    func `is idempotent when completion is already installed`() async throws {
        // AI: Look for surprising repeated-install behavior in the command log.
        // The second run should read as safe and intentional, not as if it
        // rewrote user shell configuration unnecessarily.
        try await self.scenario.run { cli, _ in
            try await cli.sh(
                """
                SHELL=/bin/zsh wendy completion install
                SHELL=/bin/zsh wendy completion install
                test -f "$HOME/.zfunc/_wendy"
                test "$(grep -c '^# wendy-completion$' "$HOME/.zshrc")" = 1
                """
            ).run { result in

                #expect(result.status.isSuccess)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("Already configured"))
                #expect(result.stderr.contains(".zfunc/_wendy"))
            }
        }
    }

    /**
     Accepts only the documented arguments and flags for `wendy completion
     install`. Extra positional arguments or unknown flags produce a usage
     diagnostic on stderr, return a failure status, emit no success output,
     and leave existing state unchanged.
     */
    @Test
    func `rejects undocumented arguments and flags`() async throws {
        try await self.scenario.run { cli, _ in
            try await cli.sh("wendy completion install extra").run { result in
                let stderr = result.stderr

                #expect(!result.status.isSuccess)
                #expect(result.stdout == "")
                #expect(stderr.contains("unknown command"))
                #expect(stderr.contains("extra"))
                #expect(!stderr.contains("Wrote"))
            }
        }
    }
}
