import Foundation
import Testing
import WendyE2ETesting

@Suite
struct `device cache fixtures` {
    @Test(.enabled(if: WendyE2EEnvironment.cliDeviceCachePath != nil))
    func `copies a separate cache into opted-in device fixtures`() async throws {
        try await CLIAndAgentScenario().run(useDeviceCache: true) { cli, _ in
            try await cli.sh(
                posix:
                    "cat \"$HOME/.wendy/devices.json\" && test ! -e \"$HOME/.wendy/tls-sessions\" && test ! -e \"$HOME/.wendy/known_devices.json\"",
                power:
                    "Get-Content -Raw -LiteralPath (Join-Path $env:HOME '.wendy/devices.json'); if ((Test-Path (Join-Path $env:HOME '.wendy/tls-sessions')) -or (Test-Path (Join-Path $env:HOME '.wendy/known_devices.json'))) { exit 1 }"
            ) { result in
                #expect(result.status.isSuccess)
                let cache = try #require(
                    try JSONSerialization.jsonObject(with: Data(result.stdout.utf8))
                        as? [String: Any]
                )
                #expect((cache["devices"] as? [[String: Any]])?.count == 1)
            }
            // Mutating this HOME must never change the runner's source snapshot.
            try await cli.sh(
                posix: "printf 'modified' > \"$HOME/.wendy/devices.json\"",
                power:
                    "Set-Content -LiteralPath (Join-Path $env:HOME '.wendy/devices.json') -Value 'modified'"
            )
            let source = try #require(WendyE2EEnvironment.cliDeviceCachePath)
            if cli.machine.isLocal {
                let cache = try JSONSerialization.jsonObject(
                    with: Data(contentsOf: URL(fileURLWithPath: source))
                )
                #expect(cache is [String: Any])
            }
        }
    }

    @Test
    func `keeps ordinary and discovery scenarios cold by default`() async throws {
        try await CLIAndAgentScenario().run { cli, _ in
            try await cli.sh(
                posix: "test ! -e \"$HOME/.wendy/devices.json\"",
                power:
                    "if (Test-Path -LiteralPath (Join-Path $env:HOME '.wendy/devices.json')) { exit 1 }"
            ) { result in
                #expect(result.status.isSuccess)
            }
        }
    }
}
