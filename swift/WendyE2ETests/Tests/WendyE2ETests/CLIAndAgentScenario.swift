import Foundation
import WendyE2ETesting

final class CLIAndAgentScenario: Scenario, Sendable {
    static var shared: CLIAndAgentScenario {
        get async throws {
            try await _shared.value
        }
    }

    let cli: Session
    let agent: Session

    private static let _shared = Task {
        try await CLIAndAgentScenario()
    }

    private init() async throws {
        let repositoryRootDirectoryURL = Self.repositoryRootDirectoryURL()

        let cli = Machine(
            id: "cli",
            name: "CLI",
            os: Environment.cliOS ?? .current,
            tags: [.cli],
            ssh: Environment.cliSSH,
            workingDirectory: Environment.cliWorkingDirectory
                ?? repositoryRootDirectoryURL.appendingPathComponent("go").path
        )

        let agent = Machine(
            id: "agent",
            name: "Agent",
            os: Environment.agentOS ?? .current,
            tags: [.agent],
            ssh: Environment.agentSSH,
            workingDirectory: Environment.agentWorkingDirectory
                ?? repositoryRootDirectoryURL.appendingPathComponent("swift").path
        )

        self.cli = try await Session.begin(for: cli)
        self.agent = try await Session.begin(for: agent)
    }

    private static func repositoryRootDirectoryURL() -> URL {
        URL(fileURLWithPath: #filePath, isDirectory: false)
            .deletingLastPathComponent()  // Tests/WendyE2ETests
            .deletingLastPathComponent()  // Tests
            .deletingLastPathComponent()  // swift/WendyE2ETests
            .deletingLastPathComponent()  // swift
            .deletingLastPathComponent()  // repository root
    }
}
