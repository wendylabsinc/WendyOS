import Foundation
import Testing
import WendyE2ETesting

private let usbRoute = ProcessInfo.processInfo.environment["WENDY_E2E_AUTH_MODES_USB_ROUTE"]

/// Externally provisioned USB first-boot proof. No test performs a flash.
/// Routes are independently skipped, never reported as another route's pass.
extension AuthModesLiveAcceptance {
    @Suite(.serialized, .enabled(if: authModesLiveEnabled && authModesLocalTestsSupported))
    struct `'wendy install --close acceptance'` {
        @Test(.enabled(if: usbRoute == "orin"))
        func `A27 Orin first boot is Closed with the installed owner`() async throws {
            try await firstBoot(route: "orin")
        }
        @Test(.enabled(if: usbRoute == "thor"))
        func `A27 Thor first boot is Closed with the installed owner`() async throws {
            try await firstBoot(route: "thor")
        }
        @Test(.enabled(if: usbRoute == "dragonwing"))
        func `A27 Dragonwing first boot is Closed with the installed owner`() async throws {
            try await firstBoot(route: "dragonwing")
        }

        private struct Receipt: Decodable {
            let route: String
            let device: String
            let imageSHA256: String
            let installerExitStatus: Int
            let firstBootID: String
            let fingerprint: String
            let installerConfigDirectory: String
            let accessFile: String
        }

        private func firstBoot(
            route: String,
            filePath: String = #filePath,
            function: String = #function,
            line: Int = #line
        ) async throws {
            let fixture = try AuthModesLiveFixture.load()
            try #require(
                !FileManager.default.fileExists(
                    atPath: fixture.recoveryDirectory + "/journal.json"
                ),
                "Reconcile the preceding lifecycle journal before a USB booking"
            )
            let path = try #require(
                ProcessInfo.processInfo.environment["WENDY_E2E_AUTH_MODES_USB_RECEIPT"]
            )
            let receipt = try JSONDecoder().decode(
                Receipt.self,
                from: Data(contentsOf: URL(fileURLWithPath: path))
            )
            try #require(receipt.route == route && receipt.device == fixture.device)
            try #require(receipt.installerExitStatus == 0 && receipt.imageSHA256.count == 64)
            try #require(receipt.imageSHA256.allSatisfy { $0.isHexDigit })
            try #require(
                receipt.installerConfigDirectory.hasPrefix("/") && receipt.accessFile.hasPrefix("/")
            )
            let scenario = CLIAndAgentScenario(after: { cli, _ in
                try await cli.sh(
                    #"rm -f "$WENDY_CONFIG_DIR/local-operator-key.pem" "$HOME/second-key/local-operator-key.pem""#
                )
            })
            try await scenario.run(
                authenticated: false,
                filePath: filePath,
                function: function,
                line: line
            ) { cli, agent in
                try #require(
                    !agent.machine.isLocal && agent.machine.address == fixture.device,
                    "First-boot file assertions require the board's --agent-address"
                )
                try await cli.sh(
                    """
                    mkdir -p "$WENDY_CONFIG_DIR"
                    cp \(AuthModesFixture.quote(receipt.installerConfigDirectory + "/local-operator-key.pem")) "$WENDY_CONFIG_DIR/local-operator-key.pem"
                    chmod 600 "$WENDY_CONFIG_DIR/local-operator-key.pem"
                    """
                )
                let local = try await cli.sh("wendy auth local-key show --json") { $0 }
                try #require(local.status.isSuccess)
                let key =
                    try JSONSerialization.jsonObject(with: Data(local.stdout.utf8))
                    as? [String: String]
                #expect(key?["fingerprint"] == receipt.fingerprint)
                let command = "wendy --json --device " + AuthModesFixture.quote(fixture.device)
                try await cli.sh("\(command) device access") { result in
                    try #require(result.status.isSuccess)
                    let access =
                        try JSONSerialization.jsonObject(with: Data(result.stdout.utf8))
                        as? [String: Any]
                    #expect(access?["shape"] as? String == "closed")
                    #expect(access?["yourRole"] as? String == "owner")
                    #expect(access?["keylessApps"] as? Bool == false)
                }
                try await cli.sh("\(command) device keys list") { result in
                    try #require(result.status.isSuccess)
                    let object =
                        try JSONSerialization.jsonObject(with: Data(result.stdout.utf8))
                        as? [String: Any]
                    let keys = object?["keys"] as? [[String: Any]]
                    try #require(keys?.count == 1)
                    #expect(keys?[0]["fingerprint"] as? String == receipt.fingerprint)
                    #expect(keys?[0]["role"] as? String == "owner")
                    #expect(keys?[0]["addedBy"] as? String == "install")
                }
                try await agent.sh("stat -c %a \(AuthModesFixture.quote(receipt.accessFile))") {
                    result in
                    try #require(result.status.isSuccess)
                    #expect(result.normalizedStdout == "600")
                }
                try await agent.sh("cat /proc/sys/kernel/random/boot_id") { result in
                    try #require(result.status.isSuccess)
                    #expect(result.normalizedStdout == receipt.firstBootID)
                }
                try await cli.sh(
                    "env WENDY_CONFIG_DIR=\"$HOME/second-key\" \(command) device access"
                ) {
                    result in
                    #expect(result.status.isFailure)
                    #expect(result.stderr.contains("does not trust this computer's key"))
                }
            }
        }
    }

}
