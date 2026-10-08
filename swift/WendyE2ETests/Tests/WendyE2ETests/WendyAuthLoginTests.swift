import Testing
import WendyE2ETesting

@Suite
struct `'wendy auth login'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays usage for `wendy auth login`. The output includes the command
     synopsis, local flags, inherited global flags, and concise
     descriptions. Help exits successfully, writes to stdout, emits no
     stderr, and leaves configuration, cache, project, cloud, and device
     state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy auth login --help") { result in
                let stdout = result.stdout
                #expect(result.status.isSuccess)
                #expect(stdout.contains("dashboard flow by default"))
                #expect(stdout.contains("wendy auth login [flags]"))
                #expect(stdout.contains("--api-key"))
                #expect(stdout.contains("--cloud"))
                #expect(stdout.contains("--cloud-grpc"))
                #expect(stdout.contains("--org"))
                #expect(!stdout.contains("--production"))
                #expect(!stdout.contains("--development"))
                #expect(!stdout.contains("--legacy"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     Production and development Cloud targets start at the realm-less Wendy Auth
     page. The browser collects email and organization, then returns the selected
     realm to the CLI. This needs the protected browser/Auth fixture tracked by
     WDY-1949; an E2E must never open a real browser or use personal credentials.
     */
    @Test(
        .disabled(
            "WDY-1949: realm-less Cloud login needs a protected browser/Auth fixture and injectable browser."
        )
    )
    func `starts realm-less login for the selected Cloud target`() async throws {
        // Unit coverage verifies target routing without network or browser access.
    }

    /**
     A login can select only one built-in Cloud target. Conflicting target flags
     fail locally before browser, credential, configuration, or network work.
     */
    @Test
    func `rejects conflicting Cloud targets`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            for flags in [
                "--production --development",
                "--production --legacy",
                "--development --legacy",
            ] {
                try await cli.sh("wendy auth login \(flags)") { result in
                    #expect(result.status.isFailure)
                    #expect(result.stdout == "")
                    #expect(
                        result.stderr.contains(
                            "--production, --development, and --legacy are mutually exclusive"
                        )
                    )
                }
            }
        }
    }

    /**
     Without `--api-key`, starts the Wendy Cloud browser login flow,
     receives the callback token, generates client credentials, and
     stores the resulting session in the CLI configuration. Success
     output gives concise next-step guidance and stderr stays empty.
     */
    @Test(
        .disabled(
            "WDY-1949: browser login needs a protected callback/token/PKI fixture and injectable browser; real browsers and personal cloud auth are prohibited."
        )
    )
    func `starts browser-based login and stores the auth session`() async throws {
        // TODO: enable with protected browser and auth fixtures (WDY-1949).
    }

    /**
     With `--api-key`, authenticates against the selected cloud gRPC or
     local pki-core endpoint using the provided bearer token. The token
     is not echoed to stdout, stderr, command records, or saved config.
     */
    @Test(
        .disabled(
            "WDY-1949: API-key login needs an ephemeral protected pki-core endpoint with recorder redaction; production or personal keys cannot be used."
        )
    )
    func `logs in with an API key without opening a browser`() async throws {
        // TODO: enable with protected PKI and secret-redaction fixtures (WDY-1949).
    }

    /**
     `--cloud` and `--cloud-grpc` bind the login to a specific Wendy
     Cloud environment. The stored session records enough endpoint
     identity for later commands to choose the same environment.
     */
    @Test(
        .disabled(
            "WDY-1949: endpoint selection needs multiple protected cloud/PKI fixtures so stored identity can be verified without production auth."
        )
    )
    func `selects the intended cloud endpoint explicitly`() async throws {
        // TODO: enable with protected multi-cloud fixtures (WDY-1949).
    }

    /**
     Cancelling or timing out the browser callback reports a login
     failure, exits non-zero, and leaves prior authentication state
     unchanged.
     */
    @Test(
        .disabled(
            "WDY-1949: cancellation and timeout require controllable browser callback process state rather than opening a real browser."
        )
    )
    func `reports cancelled browser login without storing credentials`() async throws {
        // TODO: enable with the protected browser callback fixture (WDY-1949).
    }

    /**
     A successful login for a cloud that already has stored credentials
     replaces the old credentials atomically while preserving sessions
     for other clouds.
     */
    @Test(
        .disabled(
            "WDY-1949: atomic session replacement needs protected certificate issuance for two cloud identities without personal auth state."
        )
    )
    func `replaces an existing session for the same cloud`() async throws {
        // TODO: enable with protected multi-session auth fixtures (WDY-1949).
    }

    /**
     Reads the Wendy CLI configuration before performing work that depends on
     user state. Malformed configuration is reported as a configuration error,
     no prompts open, no network connection is attempted, and the original file
     remains byte-for-byte unchanged.
     */
    @Test
    func `reports invalid CLI configuration before acting`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh(
                posix:
                    "mkdir -p \"$HOME/.wendy\"; printf '{ invalid json\\n' > \"$HOME/.wendy/config.json\"",
                power: """
                    New-Item -ItemType Directory -Force -Path (Join-Path $env:HOME '.wendy') | Out-Null
                    Set-Content -NoNewline -LiteralPath (Join-Path $env:HOME '.wendy/config.json') -Value '{ invalid json'
                    """
            )
            try await cli.sh("wendy auth login --api-key fixture-value --cloud-grpc localhost:1") {
                result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("parsing config"))
                #expect(!result.stderr.contains("fixture-value"))
            }
            try await cli.sh(
                posix: "cat \"$HOME/.wendy/config.json\"",
                power: "Get-Content -Raw -LiteralPath (Join-Path $env:HOME '.wendy/config.json')"
            ) { result in
                #expect(result.stdout.contains("{ invalid json"))
            }
        }
    }

    /**
     Requires the API endpoint needed to exchange an API key for a Wendy auth
     session.

     Missing endpoint configuration fails locally without writing partial
     credentials.
     */
    @Test
    func `requires an endpoint for API key login`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy auth login --api-key fixture-value") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("--cloud-grpc is required"))
                #expect(!result.stderr.contains("fixture-value"))
            }
        }
    }

    /**
     Rejects flags that are not part of the command's documented interface.

     The command reports a usage error on stderr and does not perform the
     requested operation.
     */
    @Test
    func `rejects unknown flags`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy auth login --bogus") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("unknown flag"))
                #expect(result.stderr.contains("--bogus"))
            }
        }
    }

    /**
     Rejects positional arguments because this command is entirely flag-driven.

     The command reports a usage error instead of treating undocumented input as
     a valid request.
     */
    @Test(
        .disabled(
            "WDY-1934: 'wendy auth login' silently accepts extra positional arguments because the leaf command has no cobra.NoArgs validator."
        )
    )
    func `rejects undocumented positional arguments`() async throws {
        // TODO: enable when auth login rejects positional arguments (WDY-1934).
    }
}
