import Testing
import WendyE2ETesting

@Suite
struct `'wendy cloud tunnel'` {
    let scenario = CLIAndAgentScenario()
    /**
     Displays usage for `wendy cloud tunnel`. The output includes the command
     synopsis, local flags, inherited global flags, and concise
     descriptions. Help exits successfully, writes to stdout, emits no
     stderr, and leaves configuration, cache, project, cloud, and device
     state untouched.
     */
    @Test
    func `prints command help`() async throws {
        // AI: Review the full help output as CLI documentation for a networking
        // command. Flag confusing port-mapping wording, missing safety cues,
        // duplicated global flags, or formatting that would make setup hard.
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy cloud tunnel --help") { result in
                let stdout = result.stdout

                #expect(result.status.isSuccess)
                #expect(
                    stdout.contains(
                        "forwards each connection through the Wendy Cloud tunnel broker"
                    )
                )
                #expect(stdout.contains("Usage:"))
                #expect(
                    stdout.contains(
                        "wendy cloud tunnel [<local-port>:<remote-port>[/udp]] [flags]"
                    )
                )
                #expect(stdout.contains("--broker-url"))
                #expect(stdout.contains("--cloud-grpc"))
                #expect(stdout.contains("--device"))
                #expect(stdout.contains("--help"))
                #expect(stdout.contains("--json"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     Listens on the requested local port and forwards each connection to
     the requested remote port on the selected device through the Wendy
     Cloud tunnel broker.
     */
    @Test(
        .disabled(
            "WDY-1949: forwarding needs an isolated authenticated broker, disposable device endpoint, and observable bidirectional connection fixture."
        )
    )
    func `forwards local connections through the cloud broker`() async throws {}

    /**
     `--device`, `--broker-url`, and `--cloud-grpc` bypass interactive
     selection and bind the tunnel to a specific cloud route.
     */
    @Test(
        .disabled(
            "WDY-1949: explicit route selection needs isolated auth plus seeded broker and cloud-device inventory."
        )
    )
    func `selects device and broker explicitly`() async throws {}

    /**
     Malformed mappings, privileged local ports without permission, or
     out-of-range ports fail before opening a listener or contacting the
     broker.
     */
    @Test
    func `rejects invalid port mappings before listening`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy cloud tunnel notaport") { result in
                let stderr = result.stderr

                #expect(!result.status.isSuccess)
                #expect(result.stdout == "")
                #expect(stderr.contains("invalid port"))
                #expect(stderr.contains("notaport"))
                #expect(!stderr.contains("Fetching device list"))
                #expect(!stderr.contains("Forwarding"))
            }
        }
    }

    /**
     Missing auth, unreachable brokers, or rejected tunnels close any
     local listener and return a clear diagnostic.
     */
    @Test
    func `reports auth and broker failures without leaving listeners open`() async throws {
        // AI: Judge whether the failure is actionable for a user who expected a
        // tunnel to start. The output should make the auth problem obvious and
        // should not imply that a listener or forwarding session remains active.
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy cloud tunnel 65535:80") { result in
                let stderr = result.stderr

                #expect(!result.status.isSuccess)
                #expect(result.stdout == "")
                #expect(stderr.contains("not logged in"))
                #expect(stderr.contains("wendy auth login"))
                #expect(!stderr.contains("Forwarding"))
                #expect(!stderr.contains("Press Ctrl+C"))
            }
        }
    }

    /**
     Cancelling the tunnel closes active connections and the local
     listener without modifying configuration.
     */
    @Test(
        .disabled(
            "WDY-1949: cancellation cleanup needs an authenticated disposable tunnel plus harness process control and listener observability."
        )
    )
    func `shuts down cleanly on cancellation`() async throws {}
}
