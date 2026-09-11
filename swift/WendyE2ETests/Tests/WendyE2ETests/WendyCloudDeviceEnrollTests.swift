import Testing
import WendyE2ETesting

@Suite
struct `'wendy cloud device enroll'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays usage for `wendy cloud device enroll`. The output includes the
     command synopsis, local flags, inherited global flags, and concise
     descriptions. Help exits successfully, writes to stdout, emits no
     stderr, and leaves configuration, cache, project, cloud, and device
     state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy cloud device enroll --help") { result in
                #expect(result.status.isSuccess)
                #expect(
                    result.stdout.contains(
                        "Enrolls the connected device using your stored auth session"
                    )
                )
                #expect(result.stdout.contains("OIDC accounts"))
                #expect(result.stdout.contains("wendy cloud device enroll [flags]"))
                #expect(result.stdout.contains("--name"))
                #expect(result.stdout.contains("--org"))
                #expect(result.stdout.contains("--cloud-grpc"))
                #expect(result.stdout.contains("--device"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     `--device` selects the cloud device and skips local discovery and pickers.
     The command does not read or change the saved default device when an
     explicit target is supplied.
     */
    @Test(
        .disabled(
            "WDY-1949: explicit-target enrollment needs isolated auth, PKI/cloud, and disposable provisioning fixtures."
        )
    )
    func `uses explicit device selection without prompting`() async throws {}

    /**
     Without an explicit or configured device in a non-interactive context,
     reports that a device selection is required, emits no prompt escape
     sequences, and performs no device operation.
     */
    @Test(
        .disabled(
            "WDY-1949: missing cloud-device selection can only be observed after injecting valid isolated auth."
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
            try await cli.sh(
                "wendy cloud device enroll --device target --name Example --org 1 --json"
            ) { result in
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
            "WDY-1949: connection and incompatible-RPC failures need disposable provisioning and auth fixtures."
        )
    )
    func `reports unreachable devices without partial success`() async throws {}

    /**
     Uses the stored auth session to create an enrollment token and provisions
     the device with mTLS credentials and an optional name.
     */
    @Test(
        .disabled(
            "WDY-1949: successful enrollment needs isolated auth, token issuance, certificate, and disposable device fixtures."
        )
    )
    func `enrolls the selected device with Wendy Cloud`() async throws {}

    /**
     Token creation, certificate issuance, or device provisioning failures
     leave previous device credentials usable and report the failing stage.
     */
    @Test(
        .disabled(
            "WDY-1949: partial-failure rollback needs controllable token, certificate, and provisioning failure fixtures."
        )
    )
    func `does not leave partial credentials on enrollment failure`() async throws {}

    /**
     With `--json`, emits enrollment status and certificate metadata without
     printing tokens or private keys.
     */
    @Test(
        .disabled(
            "WDY-1949: JSON enrollment metadata needs isolated successful PKI/cloud and device fixtures."
        )
    )
    func `prints JSON enrollment metadata`() async throws {}

    /**
     Rejects flags that are not part of the command's documented interface.

     The command reports a usage error on stderr and does not perform the
     requested operation.
     */
    @Test
    func `rejects undocumented flags`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy cloud device enroll --bogus") { result in
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
            "WDY-1934: 'wendy cloud device enroll' silently accepts positional arguments because the leaf command has no cobra.NoArgs validator."
        )
    )
    func `rejects undocumented positional arguments`() async throws {}
}
