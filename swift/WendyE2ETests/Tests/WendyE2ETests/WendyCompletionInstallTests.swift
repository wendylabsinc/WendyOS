import Subprocess
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
        try await self.scenario.run { cli, _ in
            try await cli.sh("wendy completion install --help") {
                terminationStatus,
                standardOutput,
                standardError in

                #expect(terminationStatus.isSuccess)
                #expect(standardOutput.contains("Detect the current shell"))
                #expect(standardOutput.contains("Usage:"))
                #expect(standardOutput.contains("wendy completion install [flags]"))
                #expect(standardOutput.contains("--shell"))
                #expect(standardOutput.contains("--print-path"))
                #expect(standardOutput.contains("--stdout"))
                #expect(standardOutput.contains("--device"))
                #expect(standardOutput.contains("--json"))
                #expect(standardError == "")
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
            ) {
                terminationStatus,
                standardOutput,
                standardError in

                #expect(terminationStatus.isSuccess)
                #expect(standardOutput == "")
                #expect(standardError.contains("Wrote"))
                #expect(
                    standardError.contains("Updated")
                        || standardError.contains("Already configured")
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
            ) {
                terminationStatus,
                standardOutput,
                standardError in

                #expect(terminationStatus.isSuccess)
                #expect(standardOutput == "")
                #expect(standardError.contains("wendy.fish"))
                #expect(standardError.contains("Fish auto-loads completions"))
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
            ) {
                terminationStatus,
                standardOutput,
                standardError in

                #expect(terminationStatus.isSuccess)
                #expect(standardOutput.contains("/.zfunc/_wendy"))
                #expect(standardOutput.contains("/.zshrc"))
                #expect(standardError == "")
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
            ) {
                terminationStatus,
                standardOutput,
                standardError in

                #expect(terminationStatus.isSuccess)
                #expect(standardOutput.contains("#compdef wendy"))
                #expect(standardOutput.contains("compdef _wendy wendy"))
                #expect(standardError == "")
            }
        }
    }

    /**
     Running installation repeatedly leaves a single managed completion
     script and a single source line in the relevant shell rc file.
     */
    @Test
    func `is idempotent when completion is already installed`() async throws {
        try await self.scenario.run { cli, _ in
            try await cli.sh(
                """
                SHELL=/bin/zsh wendy completion install
                SHELL=/bin/zsh wendy completion install
                test -f "$HOME/.zfunc/_wendy"
                test "$(grep -c '^# wendy-completion$' "$HOME/.zshrc")" = 1
                """
            ) {
                terminationStatus,
                standardOutput,
                standardError in

                #expect(terminationStatus.isSuccess)
                #expect(standardOutput == "")
                #expect(standardError.contains("Already configured"))
                #expect(standardError.contains(".zfunc/_wendy"))
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
            try await cli.sh("wendy completion install extra") {
                terminationStatus,
                standardOutput,
                standardError in

                #expect(!terminationStatus.isSuccess)
                #expect(standardOutput == "")
                #expect(standardError.contains("unknown command"))
                #expect(standardError.contains("extra"))
                #expect(!standardError.contains("Wrote"))
            }
        }
    }
}
