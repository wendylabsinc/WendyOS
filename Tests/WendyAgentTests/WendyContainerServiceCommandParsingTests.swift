import Foundation
import Testing

@testable import wendy_agent

@Suite("WendyContainerService Command Parsing")
struct WendyContainerServiceCommandParsingTests {
    @Test("Parses JSON-encoded command arrays")
    func parsesJSONEncodedCommandArrays() throws {
        let original = ["python", "app.py", "--host", "0.0.0.0", "--name", "hello world"]
        let encoded = try #require(String(data: JSONEncoder().encode(original), encoding: .utf8))

        let parsed = WendyContainerService.parseContainerCommand(encoded)

        #expect(parsed == original)
    }

    @Test("Falls back to whitespace split for shell-style commands")
    func fallsBackToWhitespaceSplit() {
        let parsed = WendyContainerService.parseContainerCommand("python app.py --port 8000")

        #expect(parsed == ["python", "app.py", "--port", "8000"])
    }

    @Test("Returns empty command for blank input")
    func returnsEmptyForBlankInput() {
        #expect(WendyContainerService.parseContainerCommand("").isEmpty)
        #expect(WendyContainerService.parseContainerCommand("   ").isEmpty)
    }
}
