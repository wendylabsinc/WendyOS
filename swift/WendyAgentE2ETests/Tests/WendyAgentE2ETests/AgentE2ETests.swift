import Foundation
import Testing
import WendyE2ETesting

@Suite(.serialized)
struct AgentE2ETests {

    @Test(.timeLimit(.minutes(10)))
    func `build CLI and agent`() async throws {
        let rootDirectoryURL = Self.rootDirectoryURL()

        let goDirectoryPath = rootDirectoryURL.appendingPathComponent("go").path
        let swiftDirectoryPath = rootDirectoryURL.appendingPathComponent("swift").path

        let cli = Machine(name: "CLI", workingDirectory: goDirectoryPath)
        let agent = Machine(name: "Agent", workingDirectory: swiftDirectoryPath)

        try await cli.run("make build") { standardOutput, standardError in
            #expect(standardOutput.contains(/go build .* bin\/wendy/))
            #expect(standardOutput.contains(/go build .* bin\/wendy-agent/))
        }

        try await agent.run("make build-dev") { standardOutput, standardError in
            #expect(standardOutput.contains(/Created macOS app artifact: .*wendy-agent-macos-arm64-.*\.zip/))
        }
    }

    private static func rootDirectoryURL() -> URL {
        URL(fileURLWithPath: #filePath, isDirectory: false)
            .deletingLastPathComponent()  // Tests/WendyAgentE2ETests
            .deletingLastPathComponent()  // Tests
            .deletingLastPathComponent()  // swift/WendyAgentE2ETests
            .deletingLastPathComponent()  // swift
            .deletingLastPathComponent()  // repository root
    }
}
