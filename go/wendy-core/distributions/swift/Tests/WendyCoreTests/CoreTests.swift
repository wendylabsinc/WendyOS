import Testing
import WendyCore

@Test
func `calls the linked Go engine`() {
    #expect(WendyHelloWorld() == 42)
}

@Test
func `reports the linked engine version`() {
    let version = WendyCoreVersion()
    #expect(version.hasPrefix("0.1.0"))
    #expect(WendyCoreVersion() == version)
}
