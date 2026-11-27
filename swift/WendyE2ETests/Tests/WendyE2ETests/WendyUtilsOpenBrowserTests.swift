import Foundation
import Testing
import WendyE2ETesting

@Suite
struct `'wendy utils open-browser'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays usage for `wendy utils open-browser`. The output includes the
     command synopsis, local flags, inherited global flags, and concise
     descriptions. Help exits successfully, writes to stdout, emits no
     stderr, and leaves configuration, cache, project, cloud, and device
     state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy utils open-browser --help") { result in
                let stdout = result.stdout

                #expect(result.status.isSuccess)
                #expect(stdout.contains("Open a URL in the default browser"))
                #expect(stdout.contains("Usage:"))
                #expect(stdout.contains("wendy utils open-browser <url> [flags]"))
                #expect(stdout.contains("--help"))
                #expect(stdout.contains("--device"))
                #expect(stdout.contains("--json"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     Missing or malformed URLs produce a usage diagnostic on stderr, return a
     failure status, and do not invoke the platform browser opener. A URL
     without a scheme, and an `http` URL without a host, are both rejected.
     */
    @Test
    func `rejects missing or malformed URLs`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            // Missing URL argument.
            try await cli.sh("wendy utils open-browser") { result in
                #expect(!result.status.isSuccess)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("accepts 1 arg"))
            }

            // Missing scheme.
            try await cli.sh("wendy utils open-browser example.com") { result in
                #expect(!result.status.isSuccess)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("invalid URL"))
                #expect(result.stderr.contains("example.com"))
            }

            // Missing host for an http URL.
            try await cli.sh("wendy utils open-browser http://") { result in
                #expect(!result.status.isSuccess)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("invalid URL"))
                #expect(result.stderr.contains("must include a host"))
            }
        }
    }

    /**
     The utility operates independently of Wendy project files, cloud auth,
     and device configuration: URL validation runs before any of that state
     is consulted, so a malformed URL fails the same way even when a bogus
     build host is configured.
     */
    @Test
    func `does not require project, auth, or device state`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh(
                posix: """
                    mkdir -p "$HOME/.wendy"
                    printf '{"defaultBuildHost":"do-not-contact.invalid"}\n' > "$HOME/.wendy/config.json"
                    """,
                power: """
                    New-Item -ItemType Directory -Force -Path (Join-Path $env:HOME '.wendy') | Out-Null
                    Set-Content -LiteralPath (Join-Path $env:HOME '.wendy/config.json') -Value '{"defaultBuildHost":"do-not-contact.invalid"}'
                    """
            )

            try await cli.sh("wendy utils open-browser example.com") { result in
                #expect(!result.status.isSuccess)
                #expect(result.stderr.contains("invalid URL"))
                #expect(!result.stderr.contains("do-not-contact.invalid"))
                #expect(!result.stderr.lowercased().contains("device"))
                #expect(!result.stderr.lowercased().contains("auth"))
            }
        }
    }

    /**
     Delegates a valid HTTP or HTTPS URL to the platform browser opener and
     exits successfully after handoff.
     */
    @Test(
        .disabled(
            "WDY-1938: a valid URL invokes the real platform opener (open/xdg-open/rundll32) and would launch a browser on the runner; the CLI has no injectable or fake opener to make this deterministic and side-effect free."
        )
    )
    func `opens a valid URL with the system browser`() async throws {
        // TODO: enable once the opener can be redirected to a test double.
    }

    /**
     If the platform browser command is unavailable or returns an error,
     reports the failure on stderr with a non-zero exit status.
     */
    @Test(
        .disabled(
            "WDY-1938: requires a controllable platform opener; the command also exits 0 even when the opener fails (it prints 'Could not open browser' to stderr but returns success), contradicting the non-zero-exit spec."
        )
    )
    func `reports platform opener failures clearly`() async throws {
        // TODO: enable with an injectable opener and a decision on the failure exit status.
    }
}
