import Testing
import WendyE2ETesting

@Suite
struct `'wendy cloud enroll-device'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays usage for `wendy cloud enroll-device`. The output includes the
     command synopsis, local flags, inherited global flags, and concise
     descriptions. Help exits successfully, writes to stdout, emits no
     stderr, and leaves configuration, cache, project, cloud, and device
     state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy cloud enroll-device --help") { result in
                #expect(result.status.isSuccess)
                #expect(result.stdout.contains("Alias for 'wendy device enroll'"))
                #expect(result.stdout.contains("wendy cloud enroll-device [flags]"))
                #expect(result.stdout.contains("--cloud-grpc"))
                #expect(result.stdout.contains("--name"))
                #expect(result.stdout.contains("--org"))
                #expect(result.stdout.contains("--device"))
                #expect(result.stdout.contains("--json"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     Creates an enrollment token with the stored cloud auth session and
     provisions the selected device with mTLS credentials. Output
     matches the device enrollment flow apart from the command name.
     */
    @Test(
        .disabled(
            "WDY-1949/WDY-1952: alias parity needs isolated PKI auth and a disposable seeded device provisioning target."
        )
    )
    func `acts as the cloud alias for device enrollment`() async throws {}

    /**
     `--cloud-grpc` selects the cloud or pki-core endpoint when more
     than one auth session exists. Sessions for other endpoints remain
     untouched.
     */
    @Test(
        .disabled(
            "WDY-1949: endpoint selection needs multiple isolated cloud/PKI auth sessions and token issuance fixtures."
        )
    )
    func `uses the requested cloud endpoint`() async throws {}

    /**
     Authentication failures, token creation failures, and device
     connection failures leave the device and local credential store in
     their previous state.
     */
    @Test(
        .disabled(
            "WDY-1959: cloud enroll-device connects to and may inspect WiFi on the local device before resolving auth; WDY-1949 tracks isolated failure fixtures."
        )
    )
    func `reports missing auth or unreachable devices without partial enrollment`() async throws {}

    /**
     With `--json`, emits cloud, device, certificate, and enrollment
     status fields without printing token or private key material.
     */
    @Test(
        .disabled(
            "WDY-1949: JSON enrollment output needs isolated successful PKI/cloud and disposable device fixtures."
        )
    )
    func `prints JSON enrollment result for automation`() async throws {}

    /**
     Reads the Wendy CLI configuration before performing work that depends on
     user state. Malformed configuration is reported as a configuration error,
     no prompts open, no network connection is attempted, and the original file
     remains byte-for-byte unchanged.
     */
    @Test
    func `reports invalid CLI configuration before acting`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await LocalCLIAssertions.prepare(cli, config: "{ malformed config\n")
            let result = try await LocalCLIAssertions.run(
                cli,
                [
                    "cloud", "enroll-device", "--json", "--device", "127.0.0.1:1", "--cloud-grpc",
                    "127.0.0.1:1",
                ],
                exit: 1,
                monitorNetwork: true
            )
            try LocalCLIAssertions.error(
                result,
                code: "error",
                exit: 1,
                message: "parsing config: invalid character"
            )
            #expect(!result.stderr.contains("\u{1B}"))
            try await LocalCLIAssertions.unchanged(cli)
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
            try await cli.sh("wendy cloud enroll-device --bogus") { result in
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
            "WDY-1934: 'wendy cloud enroll-device' silently accepts positional arguments because the command has no cobra.NoArgs validator."
        )
    )
    func `rejects undocumented positional arguments`() async throws {}
}
