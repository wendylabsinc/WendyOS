import Foundation
import Testing
import WendyE2ETesting

@Suite
struct `HTTP app fixture guards` {
    @Test
    func `accepts only supported plain LAN hosts`() throws {
        for host in ["wendyos-kb-jetson-orin-nano-004.local", "127.0.0.1", "192.168.50.251"] {
            #expect(try HTTPAppFixture.requireLANDevice(host) == host)
        }
        for selector in [
            "cloud://127.0.0.1:443/tenant/test/asset/test", "vm:test", "docker", "apple-container",
            "wendy-lite:/dev/test", "unix:/tmp/agent.sock", "user@device.local",
            "device.local:50051",
            "[::1]", "::1", "device.local/path", "device..local", "256.1.1.1", "device.local\n",
        ] {
            #expect(throws: HTTPAppFixture.InvalidLANDevice.self) {
                _ = try HTTPAppFixture(device: selector, mode: .prebuiltCompose)
            }
        }
    }

    @Test(
        .enabled(
            if: ProcessInfo.processInfo.environment["WENDY_E2E_LAN_REJECTION_CONTROL"] == "true"
        )
    )
    func `rejects an explicit Cloud selector before setup or inventory`() async throws {
        let selector = try #require(WendyE2EEnvironment.deviceAddress)
        try #require(selector.hasPrefix("cloud://127.0.0.1:"))
        #expect(throws: HTTPAppFixture.InvalidLANDevice.self) {
            _ = try HTTPAppFixture.scenario()
        }
        try await WendyE2ESession.with(.current) { cli in
            await #expect(throws: HTTPAppFixture.InvalidLANDevice.self) {
                try await HTTPAppFixture.withProject(cli: cli) { _ in
                    Issue.record("Unsupported transport reached the fixture body")
                }
            }
        }
    }
}
