import Foundation
import Testing
import WendyE2ETesting

struct AgentE2ETests {
    @Test("build CLI and agent", .timeLimit(.minutes(10)))
    func buildCLIAndAgent() async throws {
        let repository = Self.repositoryDirectory()
        let cli = Machine(name: "CLI", path: repository.appendingPathComponent("go").path)
        let agent = Machine(name: "Agent", path: repository.appendingPathComponent("swift").path)

        try await cli.run("make build")
        try await agent.run("make build-dev")

        print("All done!")
    }

    private static func repositoryDirectory() -> URL {
        URL(fileURLWithPath: #filePath, isDirectory: false)
            .deletingLastPathComponent()  // Tests/WendyAgentE2ETests
            .deletingLastPathComponent()  // Tests
            .deletingLastPathComponent()  // swift/WendyAgentE2ETests
            .deletingLastPathComponent()  // swift
            .deletingLastPathComponent()  // repository root
    }
}
