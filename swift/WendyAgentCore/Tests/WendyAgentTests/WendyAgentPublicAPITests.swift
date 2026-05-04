import Testing
import WendyAgentCore

struct WendyAgentPublicAPITests {
    @Test("WendyAgent.version is exposed as public API")
    func versionIsAccessible() {
        let readVersion: () -> String = { WendyAgent.version }
        _ = readVersion
        #expect(Bool(true))
    }
}
