import Foundation
import Testing
import WendyE2ETesting

struct AgentE2ETests {
    @Test("build CLI and agent", .timeLimit(.minutes(10)))
    func buildCLIAndAgent() async throws {
        let rootDirectoryURL = Self.rootDirectoryURL()

        let goDirectoryPath = rootDirectoryURL.appendingPathComponent("go").path
        let swiftDirectoryPath = rootDirectoryURL.appendingPathComponent("go").path

        let cli = Machine(name: "CLI", workingDirectory: goDirectoryPath)
        let agent = Machine(name: "Agent", workingDirectory: swiftDirectoryPath)

        try await cli.run("make build")
        try await agent.run("make build-dev")

        print("All done!")
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
