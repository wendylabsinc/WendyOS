import Foundation
import Testing
import WendyE2ETesting

@Suite
struct `'wendy device enroll'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays usage for `wendy device enroll`. The output includes the command
     synopsis, local flags, inherited global flags, and concise
     descriptions. Help exits successfully, writes to stdout, emits no
     stderr, and leaves configuration, cache, project, cloud, and device
     state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy device enroll --help") { result in
                #expect(result.status.isSuccess)
                #expect(
                    result.stdout.contains(
                        "Enrolls the connected device using your stored auth session"
                    )
                )
                #expect(result.stdout.contains("OIDC accounts"))
                #expect(result.stdout.contains("wendy device enroll [flags]"))
                #expect(result.stdout.contains("--name"))
                #expect(result.stdout.contains("--org"))
                #expect(result.stdout.contains("--cloud-grpc"))
                #expect(result.stdout.contains("--device"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     `--device` selects the target device hostname and skips discovery and
     pickers. The command does not read or change the saved default device when
     an explicit target is supplied.
     */
    @Test(
        .disabled(
            "WDY-1949: explicit-target enrollment needs isolated auth, PKI/cloud, and disposable provisioning fixtures."
        )
    )
    func `uses explicit device selection without prompting`() async throws {}

    /**
     With a usable auth session but no explicit or configured device in a
     non-interactive context, reports that a device selection is required,
     emits no prompt escape sequences, and performs no device operation.
     */
    @Test
    func `reports missing device selection in non-interactive mode`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            // A synthetic public certificate passes local expiry validation.
            // It has no private key and cannot authenticate to a real service.
            let configJSON = String(
                decoding: try JSONSerialization.data(withJSONObject: [
                    "auth": [
                        [
                            "cloudGRPC": "127.0.0.1:1",
                            "certificates": [["pemCertificate": Self.enrollmentCertificate]],
                        ]
                    ]
                ]),
                as: UTF8.self
            )
            try await cli.sh(
                posix: """
                    mkdir -p "$HOME/.wendy"
                    printf '%s' '\(configJSON)' > "$HOME/.wendy/config.json"
                    """,
                power: """
                    New-Item -ItemType Directory -Force -Path (Join-Path $env:HOME '.wendy') | Out-Null
                    Set-Content -NoNewline -LiteralPath (Join-Path $env:HOME '.wendy/config.json') -Value '\(configJSON)'
                    """
            )
            try await cli.sh("wendy device enroll --name Example --org 1 --json") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("no device specified"))
                #expect(!result.stderr.contains("Select a device"))
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
     Without a usable auth session, reports that login is required and performs
     no device provisioning.
     */
    @Test
    func `reports missing auth before touching the device`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh(
                "wendy device enroll --device 127.0.0.1:1 --name Example --org 1 --json"
            ) { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("not logged in"))
                #expect(result.stderr.contains("wendy auth login"))
                #expect(!result.stderr.contains("connecting"))
            }
        }
    }

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
            try await cli.sh("wendy device enroll --bogus") { result in
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
            "WDY-1934: 'wendy device enroll' silently accepts positional arguments because the leaf command has no cobra.NoArgs validator."
        )
    )
    func `rejects undocumented positional arguments`() async throws {}

    // Self-signed test certificate, valid until 2126. Only local validation
    // reads it; the missing-device test must never open a connection.
    private static let enrollmentCertificate = """
        -----BEGIN CERTIFICATE-----
        MIIBpTCCAUugAwIBAgIUATCLgMOyyhlsDZIMtH9vfSedZrAwCgYIKoZIzj0EAwIw
        JzElMCMGA1UEAwwcd2VuZHktZTJlLWVucm9sbG1lbnQtZml4dHVyZTAgFw0yNjA5
        MTExNDUyMjBaGA8yMTI2MDgxODE0NTIyMFowJzElMCMGA1UEAwwcd2VuZHktZTJl
        LWVucm9sbG1lbnQtZml4dHVyZTBZMBMGByqGSM49AgEGCCqGSM49AwEHA0IABBH8
        ywsez8b1erKR+4KxWiVRcwI3S2riPUP/0nezDUcGHyF9tovWQW/cSocCKd9uKH1r
        MJqM9znLH2QJvOuRwcujUzBRMB0GA1UdDgQWBBTjVer+3dkaplly/DRGC+M3xDxf
        4DAfBgNVHSMEGDAWgBTjVer+3dkaplly/DRGC+M3xDxf4DAPBgNVHRMBAf8EBTAD
        AQH/MAoGCCqGSM49BAMCA0gAMEUCIEaXvy0EmVWHcjkE19UNT2raDTGTsnVLn/Md
        6c1KxbnZAiEAm6p88NdgpobBIEs2EaMxL/jpWYo/FXz/W8m8oCb5RYU=
        -----END CERTIFICATE-----
        """
}
