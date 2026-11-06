import Testing

@testable import WendyAgentCore

@Suite("Mac network access")
struct NetworkAccessTests {
    @Test("new configurations are local-only")
    func localOnlyByDefault() {
        let configuration = WendyAgentConfiguration()
        #expect(configuration.host == "127.0.0.1")
        #expect(!configuration.allowsNetworkAccess)
    }

    @Test(
        "all supported loopback overrides suppress remote access",
        arguments: ["localhost", "127.0.0.1", "::1"]
    )
    func loopbackOverrides(host: String) {
        #expect(!WendyAgentConfiguration(host: host).allowsNetworkAccess)
    }

    @Test(
        "explicit remote hosts enable network access",
        arguments: ["::", "0.0.0.0", "192.168.1.20"]
    )
    func remoteHosts(host: String) {
        #expect(WendyAgentConfiguration(host: host).allowsNetworkAccess)
    }

    @Test("a stopped agent rejects access changes")
    func changingStoppedAgent() async {
        let agent = WendyAgent()
        do {
            try await agent.setNetworkAccess(allowed: true)
            Issue.record("An agent must be running before changing network access")
        } catch {
            #expect(await agent.configuration.host == "127.0.0.1")
        }
    }
}
