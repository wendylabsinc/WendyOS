import Foundation
import Testing
import WendyE2ETesting

@Suite
struct `'wendy cloud device apps list'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays usage for `wendy cloud device apps list`. The output includes the
     command synopsis, local flags, inherited global flags, and concise
     descriptions. Help exits successfully, writes to stdout, emits no
     stderr, and leaves configuration, cache, project, cloud, and device
     state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy cloud device apps list --help") { result in
                #expect(result.status.isSuccess)
                #expect(result.stdout.contains("List deployed applications"))
                #expect(result.stdout.contains("wendy cloud device apps list [flags]"))
                #expect(result.stdout.contains("--cloud-grpc"))
                #expect(result.stdout.contains("--broker-url"))
                #expect(result.stdout.contains("--device"))
                #expect(result.stdout.contains("--json"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     Without an explicit or configured device in a non-interactive context,
     reports that a device selection is required, emits no prompt escape
     sequences, and performs no device operation.
     */
    @Test(
        .disabled(
            "Cloud v2 auto-selects a sole online device; refusal needs a controlled multi-device roster."
        )
    )
    func `reports missing device selection in non-interactive mode`() async throws {}

    /**
     Cloud-routed device commands validate the selected Wendy Cloud auth
     session before connecting to the broker. Missing or ambiguous auth fails
     before device state changes.
     */
    @Test
    func `requires cloud authentication before opening a tunnel`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy cloud device apps list --device example --json") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("not logged in"))
                #expect(result.stderr.contains("wendy auth login"))
            }
        }
    }

    /**
     Connection failures, timeouts, and incompatible agent responses produce
     stderr diagnostics and a failure status. Output does not claim that the
     operation succeeded.
     */
    @Test(
        .disabled(
            "WDY-1952: tunnel and incompatible-RPC failures need seeded cloud and managed-agent responses."
        )
    )
    func `reports unreachable devices without partial success`() async throws {}

    /**
     Displays deployed applications with names, images, status, restart policy,
     and relevant ports. An empty device reports an empty successful list.
     */
    @Test(
        .disabled(
            "WDY-1952: human application inventory needs seeded cloud tunnel and managed-agent container state."
        )
    )
    func `lists deployed applications`() async throws {}

    /**
     Lists applications on the explicitly selected Cloud v2 device through its
     Cloud tunnel. Stdout is a JSON array, including an empty array on an empty
     device. Every entry has a name and running state; optional version, port,
     failure and service fields retain machine-readable types.

     The roster first verifies the fixture's tenant, asset UUID, name and PKI
     identity. An unrelated saved default is never selected. No applications
     are created or changed and credentials and target settings are preserved.
     */
    @Test(.enabled(if: CloudReferenceFixture.enabled))
    func `prints JSON application inventory`() async throws {
        let target = try CloudReferenceFixture.target()
        try await CloudReferenceFixture.scenario().run { cli, _ in
            try await CloudReferenceFixture.prepare(cli, target: target)
            let rows = try await CloudReferenceFixture.roster(cli, target: target, all: false)
            try CloudReferenceFixture.selected(rows, target: target)
            let result = try await LocalCLIAssertions.run(
                cli,
                ["cloud", "device", "apps", "list"] + target.flags,
                exit: 0
            )
            CloudReferenceFixture.tunnelNotice(result, target: target)
            let apps = try #require(
                try JSONSerialization.jsonObject(with: Data(result.stdout.utf8)) as? [[String: Any]]
            )
            var names = Set<String>()
            for app in apps {
                let name = try #require(app["name"] as? String)
                #expect(!name.isEmpty)
                #expect(names.insert(name).inserted)
                let state = try #require(app["runningState"] as? String)
                #expect(!state.isEmpty)
                if let version = app["version"] { #expect(version is String) }
                if let count = app["failureCount"] { #expect(count is Int) }
                if let port = app["httpPort"] { #expect(port is Int) }
                if let services = app["services"] {
                    let entries = try #require(services as? [[String: Any]])
                    for service in entries {
                        #expect(service["name"] is String)
                        #expect(service["runningState"] is String)
                    }
                }
            }
            try await CloudReferenceFixture.preserved(cli)
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
            try await cli.sh("wendy cloud device apps list --bogus") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("unknown flag"))
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
            "WDY-1934: 'wendy cloud device apps list' silently accepts positional arguments because the mirrored leaf command has no cobra.NoArgs validator."
        )
    )
    func `rejects undocumented positional arguments`() async throws {}
}
