import Foundation
import Testing
import WendyE2ETesting

@Suite
struct `'wendy info'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays usage for `wendy info`. The output includes the command synopsis,
     local flags, inherited global flags, and concise descriptions. Help exits
     successfully, writes to stdout, emits no stderr, and leaves configuration,
     cache, project, cloud, and device state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy info --help") { result in
                let stdout = result.stdout

                #expect(result.status.isSuccess)
                #expect(stdout.contains("Display CLI version and system information"))
                #expect(stdout.contains("Usage:"))
                #expect(stdout.contains("wendy info [flags]"))
                #expect(stdout.contains("--help"))
                #expect(stdout.contains("--device"))
                #expect(stdout.contains("--json"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     Reports the Wendy CLI version and local system details useful for
     support, including operating system and architecture. The command does
     not contact devices, cloud services, or update endpoints.
     */
    @Test
    func `prints CLI and system information`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy --json=false info") { result in
                let stdout = result.stdout

                #expect(result.status.isSuccess)
                #expect(stdout.contains("Wendy CLI"))
                #expect(stdout.contains("Version:"))
                #expect(stdout.contains("OS:"))
                #expect(stdout.contains("Arch:"))
                #expect(stdout.contains("Go Version:"))
                #expect(!stdout.contains("{"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     With `--json`, emits one JSON object containing version and system
     fields with stable names and value types. JSON mode emits no stderr on
     success.
     */
    @Test
    func `prints JSON info for automation`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy --json info") { result in
                let stdout = result.stdout

                #expect(result.status.isSuccess)
                #expect(result.stderr == "")
                #expect(!stdout.contains("Wendy CLI"))

                let json = try #require(
                    try JSONSerialization.jsonObject(with: Data(stdout.utf8))
                        as? [String: Any]
                )

                #expect(!(json["version"] as? String ?? "").isEmpty)
                #expect(!(json["os"] as? String ?? "").isEmpty)
                #expect(!(json["arch"] as? String ?? "").isEmpty)
                #expect(!(json["goVersion"] as? String ?? "").isEmpty)
            }
        }
    }

    /**
     Runs successfully outside a Wendy project, with no device, and with no
     auth session. Device configuration is not contacted by this local
     diagnostic command.
     */
    @Test
    func `does not require project, device, or auth state`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh(
                posix: """
                    mkdir -p "$HOME/.wendy"
                    printf '{"defaultBuildHost":"do-not-contact.invalid"}\n' > "$HOME/.wendy/config.json"
                    wendy --json=false info
                    """,
                power: """
                    New-Item -ItemType Directory -Force -Path (Join-Path $env:HOME '.wendy') | Out-Null
                    Set-Content -LiteralPath (Join-Path $env:HOME '.wendy/config.json') -Value '{"defaultBuildHost":"do-not-contact.invalid"}'
                    wendy --json=false info
                    """
            ) { result in
                let stdout = result.stdout

                #expect(result.status.isSuccess)
                #expect(stdout.contains("Wendy CLI"))
                #expect(!stdout.contains("do-not-contact.invalid"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     Accepts only the documented arguments and flags for `wendy info`. Extra
     positional arguments or unknown flags produce a usage diagnostic on
     stderr, return a failure status, emit no success output, and leave
     existing state unchanged.
     */
    @Test
    func `rejects undocumented arguments and flags`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy info extra") { result in
                let stderr = result.stderr

                #expect(!result.status.isSuccess)
                #expect(result.stdout == "")
                #expect(stderr.contains("unknown command"))
                #expect(stderr.contains("extra"))
            }

            try await cli.sh("wendy info --bogus") { result in
                let stderr = result.stderr

                #expect(!result.status.isSuccess)
                #expect(result.stdout == "")
                #expect(stderr.contains("unknown flag"))
                #expect(stderr.contains("--bogus"))
            }
        }
    }
}
