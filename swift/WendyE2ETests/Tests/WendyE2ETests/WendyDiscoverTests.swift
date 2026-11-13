import Foundation
import Testing
import WendyE2ETesting

@Suite
struct `'wendy discover'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays bounded and continuous discovery usage, transport choices, and
     inherited flags without starting a scan.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy discover --help") { result in
                let stdout = result.stdout
                #expect(result.status.isSuccess)
                #expect(stdout.contains("Continuously discover WendyOS devices"))
                #expect(stdout.contains("Usage:"))
                #expect(stdout.contains("wendy discover [flags]"))
                #expect(stdout.contains("--timeout"))
                #expect(stdout.contains("--type"))
                #expect(stdout.contains("usb, lan, bluetooth, external, mesh, all"))
                #expect(stdout.contains("--json"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     `--timeout` bounds every selected transport, including local runtime
     providers that may invoke Docker or Apple Container subprocesses.
     */
    @Test(
        .disabled(
            "WDY-1954: --timeout is not applied to external providers, so a 1 ms scan still waits seconds for ambient Docker/Apple Container probes."
        )
    )
    func `discovers local devices for a bounded timeout`() async throws {
        // TODO: enable when external providers respect the discovery deadline (WDY-1954).
    }

    /**
     Takes one external-provider snapshot without invoking or populating USB,
     LAN, Bluetooth, cloud, or physical-device routes.
     */
    @Test
    func `takes an external-provider snapshot`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy discover --type external --json") { result in
                #expect(result.status.isSuccess)
                #expect(result.stderr == "")

                let json = try #require(
                    try JSONSerialization.jsonObject(with: Data(result.stdout.utf8))
                        as? [String: Any]
                )
                let external = try #require(json["externalDevices"] as? [[String: Any]])
                #expect(external.contains { $0["providerKey"] as? String == "local" })
            }
        }
    }

    /**
     `--type external` limits results to local runtime providers and does not
     invoke or populate physical discovery transports.
     */
    @Test
    func `filters by discovery transport`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy discover --type external --json") { result in
                #expect(result.status.isSuccess)
                let json = try #require(
                    try JSONSerialization.jsonObject(with: Data(result.stdout.utf8))
                        as? [String: Any]
                )
                #expect(json["usbDevices"] is NSNull)
                #expect(json["lanDevices"] is NSNull)
                #expect(json["bluetoothDevices"] is NSNull)
                #expect(json["ethernetDevices"] is NSNull)
                #expect(json["externalDevices"] is [[String: Any]])
            }
        }
    }

    /**
     A completed physical scan with no matching devices prints a concise
     no-devices result and succeeds.
     */
    @Test(
        .disabled(
            "WDY-1943: deterministic empty USB/LAN/Bluetooth results require an injectable discovery fixture; runner hardware cannot safely define this contract."
        )
    )
    func `reports no devices as an empty successful scan`() async throws {
        // TODO: enable with a deterministic empty discovery fixture (WDY-1943).
    }

    /**
     JSON mode emits one object grouped by transport, with external devices
     carrying stable provider, display-name, OS, and architecture fields.
     */
    @Test
    func `prints JSON discovery results for automation`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy discover --type external --json") { result in
                #expect(result.status.isSuccess)
                #expect(result.stderr == "")
                let json = try #require(
                    try JSONSerialization.jsonObject(with: Data(result.stdout.utf8))
                        as? [String: Any]
                )
                let external = try #require(json["externalDevices"] as? [[String: Any]])
                let local = try #require(
                    external.first { $0["providerKey"] as? String == "local" }
                )
                #expect((local["displayName"] as? String)?.isEmpty == false)
                #expect((local["id"] as? String)?.isEmpty == false)
                #expect((local["os"] as? String)?.isEmpty == false)
                #expect((local["cpuArchitecture"] as? String)?.isEmpty == false)
                #expect(local["isWendyDevice"] as? Bool == false)
            }
        }
    }

    /**
     Invalid transport names fail before any discovery work starts and list
     every accepted value.
     */
    @Test
    func `rejects unknown discovery transports`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy discover --type nonsense --timeout 1ms --json") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("unknown discovery type: nonsense"))
                #expect(result.stderr.contains("usb, lan, bluetooth, external, mesh, all"))
            }
        }
    }

    /**
     Unknown flags fail before discovery or config mutation.
     */
    @Test
    func `rejects unknown flags`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy discover --bogus") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("unknown flag"))
                #expect(result.stderr.contains("--bogus"))
            }
        }
    }

    /**
     Unexpected positional arguments are rejected instead of silently
     starting discovery.
     */
    @Test(
        .disabled(
            "WDY-1934: 'wendy discover' silently accepts extra positional arguments because the command has no cobra.NoArgs validator."
        )
    )
    func `rejects undocumented positional arguments`() async throws {
        // TODO: enable when discover rejects positional arguments (WDY-1934).
    }

    /**
     Cancelling a continuous scan closes discovery resources and exits without
     a partial error or leaked process.
     */
    @Test(
        .disabled(
            "WDY-1943: continuous cancellation needs harness process control that can start the TUI, send Ctrl-C, and assert cleanup without enabling physical discovery in CI."
        )
    )
    func `stops cleanly on cancellation`() async throws {
        // TODO: enable with deterministic discovery fixtures and cancellation control (WDY-1943).
    }
}
