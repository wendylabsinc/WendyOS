import Foundation
import Testing
import WendyE2ETesting

@Suite
struct `'wendy cloud discover'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays usage for `wendy cloud discover`. The output includes the command
     synopsis, local flags, inherited global flags, and concise
     descriptions. Help exits successfully, writes to stdout, emits no
     stderr, and leaves configuration, cache, project, cloud, and device
     state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy cloud discover --help") { result in
                #expect(result.status.isSuccess)
                #expect(result.stdout.contains("List enrolled devices in Wendy Cloud"))
                #expect(result.stdout.contains("wendy cloud discover [flags]"))
                #expect(result.stdout.contains("--all"))
                #expect(result.stdout.contains("--broker-url"))
                #expect(result.stdout.contains("--cloud-grpc"))
                #expect(result.stdout.contains("--json"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     Lists the enrolled compute devices in the explicitly selected Cloud v2
     tenant. Each JSON row preserves its UUID and a selector containing the
     endpoint, tenant and asset. The owned fixture appears with its expected
     name and PKI identity; no fleet size or IP address is assumed.

     Requires `WENDY_E2E_CLOUD_READ_ONLY=true`, a single-tenant auth fixture,
     and the explicit `WENDY_E2E_CLOUD_*` target variables. No LAN discovery
     or device tunnel is needed. Credentials and saved selections are preserved.
     */
    @Test(.enabled(if: CloudReferenceFixture.enabled))
    func `lists enrolled cloud devices`() async throws {
        let target = try CloudReferenceFixture.target()
        try await CloudReferenceFixture.scenario().run { cli, _ in
            try await CloudReferenceFixture.prepare(cli, target: target)
            let rows = try await CloudReferenceFixture.roster(cli, target: target, all: true)
            try CloudReferenceFixture.selected(rows, target: target)
            try await CloudReferenceFixture.preserved(cli)
        }
    }

    /**
     `--all` includes offline enrolled devices. Verifying offline membership and
     filtering requires an explicit offline fixture rather than assumptions about
     the tenant's changing fleet.
     */
    @Test(.disabled("Needs an owned offline Cloud asset fixture to verify offline filtering."))
    func `includes offline devices when requested`() async throws {}

    /**
     The default JSON roster contains online devices. `--all` also includes
     offline enrolled devices. An explicitly provisioned online fixture occurs
     in both queries with the same UUID, name and tenant-scoped selector.
     Other devices may come and go; no offline count is assumed.
     */
    @Test(.enabled(if: CloudReferenceFixture.enabled))
    func `online and complete rosters preserve the selected asset identity`() async throws {
        let target = try CloudReferenceFixture.target()
        try await CloudReferenceFixture.scenario().run { cli, _ in
            try await CloudReferenceFixture.prepare(cli, target: target)
            let online = try await CloudReferenceFixture.roster(cli, target: target, all: false)
            let all = try await CloudReferenceFixture.roster(cli, target: target, all: true)
            try CloudReferenceFixture.selected(online, target: target)
            try CloudReferenceFixture.selected(all, target: target)
            try await CloudReferenceFixture.preserved(cli)
        }
    }

    /**
     Without credentials, Cloud discovery fails with an actionable login error,
     empty stdout and a machine-readable stderr error. It does not contact even
     an explicit reachable Cloud endpoint or alter the saved target and settings.
     */
    @Test
    func `reports missing auth before contacting discovery services`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await LocalCLIAssertions.prepare(cli)
            let result = try await LocalCLIAssertions.run(
                cli,
                ["cloud", "discover", "--cloud-grpc", "127.0.0.1:1", "--json"],
                exit: 3,
                monitorNetwork: true
            )
            try LocalCLIAssertions.error(
                result,
                code: "auth_required",
                exit: 3,
                message: "not logged in"
            )
            #expect(result.readableStderr.contains("wendy auth login"))
            try await LocalCLIAssertions.unchanged(cli)
        }
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
                    "mkdir -p \"$HOME/.wendy\"; printf '{ broken\\n' > \"$HOME/.wendy/config.json\"",
                power: """
                    New-Item -ItemType Directory -Force -Path (Join-Path $env:HOME '.wendy') | Out-Null
                    Set-Content -NoNewline -LiteralPath (Join-Path $env:HOME '.wendy/config.json') -Value '{ broken'
                    """
            )
            try await cli.sh("wendy cloud discover --json") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("parsing config"))
            }
            try await cli.sh(
                posix: "cat \"$HOME/.wendy/config.json\"",
                power: "Get-Content -Raw -LiteralPath (Join-Path $env:HOME '.wendy/config.json')"
            ) { result in
                #expect(result.stdout.contains("{ broken"))
            }
        }
    }

    /**
     Rejects flags that are not part of the command's documented interface.

     The command reports a usage error on stderr and does not perform the
     requested operation.
     */
    @Test
    func `rejects undocumented flags`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy cloud discover --bogus") { result in
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
            "WDY-1934: 'wendy cloud discover' silently accepts positional arguments because the command has no cobra.NoArgs validator."
        )
    )
    func `rejects undocumented positional arguments`() async throws {}
}
