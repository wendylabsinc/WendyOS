import Subprocess
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
        try await self.scenario.run { cli, _ in
            try await cli.sh("wendy cloud tunnel --help") {
                terminationStatus,
                standardOutput,
                standardError in

                #expect(terminationStatus.isSuccess)
                #expect(
                    standardOutput.contains(
                        "forwards each connection through the Wendy Cloud tunnel broker"
                    )
                )
                #expect(standardOutput.contains("Usage:"))
                #expect(
                    standardOutput.contains(
                        "wendy cloud tunnel <local-port>:<remote-port> [flags]"
                    )
                )
                #expect(standardOutput.contains("--broker-url"))
                #expect(standardOutput.contains("--cloud-grpc"))
                #expect(standardOutput.contains("--device"))
                #expect(standardOutput.contains("--help"))
                #expect(standardOutput.contains("--json"))
                #expect(standardError == "")
            }
        }
    }

    /**
     Listens on the requested local port and forwards each connection to
     the requested remote port on the selected device through the Wendy
     Cloud tunnel broker.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `forwards local connections through the cloud broker`() async throws {
        // TODO: implement.
    }

    /**
     `--device`, `--broker-url`, and `--cloud-grpc` bypass interactive
     selection and bind the tunnel to a specific cloud route.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `selects device and broker explicitly`() async throws {
        // TODO: implement.
    }

    /**
     Malformed mappings, privileged local ports without permission, or
     out-of-range ports fail before opening a listener or contacting the
     broker.
     */
    @Test
    func `rejects invalid port mappings before listening`() async throws {
        try await self.scenario.run { cli, _ in
            try await cli.sh("wendy cloud tunnel notaport") {
                terminationStatus,
                standardOutput,
                standardError in

                #expect(!terminationStatus.isSuccess)
                #expect(standardOutput == "")
                #expect(standardError.contains("invalid port"))
                #expect(standardError.contains("notaport"))
                #expect(!standardError.contains("Fetching device list"))
                #expect(!standardError.contains("Forwarding"))
            }
        }
    }

    /**
     Missing auth, unreachable brokers, or rejected tunnels close any
     local listener and return a clear diagnostic.
     */
    @Test
    func `reports auth and broker failures without leaving listeners open`() async throws {
        try await self.scenario.run { cli, _ in
            try await cli.sh("wendy cloud tunnel 65535:80") {
                terminationStatus,
                standardOutput,
                standardError in

                #expect(!terminationStatus.isSuccess)
                #expect(standardOutput == "")
                #expect(standardError.contains("not logged in"))
                #expect(standardError.contains("wendy auth login"))
                #expect(!standardError.contains("Forwarding"))
                #expect(!standardError.contains("Press Ctrl+C"))
            }
        }
    }

    /**
     Cancelling the tunnel closes active connections and the local
     listener without modifying configuration.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `shuts down cleanly on cancellation`() async throws {
        // TODO: implement.
    }
}
