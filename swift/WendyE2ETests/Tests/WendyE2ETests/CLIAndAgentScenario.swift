import Foundation
import WendyE2ETesting

final class CLIAndAgentScenario: Scenario, Sendable {
    // MARK: - Internal

    func run<Result>(
        filePath: String = #filePath,
        function: String = #function,
        line: Int = #line,
        _ body: @Sendable (_ cli: Session, _ agent: Session) async throws -> Result
    ) async throws -> Result {
        let (cli, agent) = try await self.setUp(
            filePath: filePath,
            function: function,
            line: line
        )

        let result: Result
        do {
            result = try await body(cli, agent)
        } catch {
            try? await Self.tearDown(
                cli: cli,
                agent: agent
            )
            throw error
        }

        try await Self.tearDown(
            cli: cli,
            agent: agent
        )
        return result
    }

    // MARK: - Private

    private func setUp(
        filePath: String,
        function: String,
        line: Int
    ) async throws -> (cli: Session, agent: Session) {
        var cliSession: Session?
        var agentSession: Session?

        do {
            let recorder = try Recorder(
                filePath: filePath,
                function: function,
                line: line
            )
            let repositoryRootDirectoryURL = Self.repositoryRootDirectoryURL()
            let cliSourceDirectory = repositoryRootDirectoryURL.appendingPathComponent("go").path
            let testDirectoryURL = URL(
                fileURLWithPath: recorder.testDirectoryPath,
                isDirectory: true
            )
            let cliUsesRunDirectory =
                Environment.runDirectory != nil && Environment.cliAddress == nil
            let cliHomeDirectory =
                cliUsesRunDirectory
                ? testDirectoryURL.appendingPathComponent("home", isDirectory: true).path
                : "/tmp/wendy-e2e-cli-home-\(UUID().uuidString)"
            let cliTemporaryDirectory =
                cliUsesRunDirectory
                ? testDirectoryURL.appendingPathComponent("tmp", isDirectory: true).path
                : "/tmp/wendy-e2e-cli-tmp-\(UUID().uuidString)"
            let defaultCLIWorkingDirectory =
                cliUsesRunDirectory
                ? URL(fileURLWithPath: cliHomeDirectory, isDirectory: true)
                    .appendingPathComponent("work", isDirectory: true).path
                : cliSourceDirectory
            let cliWorkingDirectory =
                Environment.cliWorkingDirectory ?? defaultCLIWorkingDirectory
            let cliBinDirectory =
                cliUsesRunDirectory
                ? Self.runDirectoryPath("cli", "bin") ?? "\(cliSourceDirectory)/bin"
                : "\(cliSourceDirectory)/bin"

            if cliUsesRunDirectory {
                for directory in [cliHomeDirectory, cliTemporaryDirectory, cliWorkingDirectory] {
                    try FileManager.default.createDirectory(
                        atPath: directory,
                        withIntermediateDirectories: true
                    )
                }
            }

            let cliMachine = Machine(
                id: "cli",
                name: "CLI",
                os: Environment.cliOS ?? .current,
                tags: [.cli],
                user: Environment.cliUser,
                address: Environment.cliAddress,
                workingDirectory: cliWorkingDirectory,
                env: [
                    "HOME": cliHomeDirectory,
                    "PATH": "\(cliBinDirectory):$PATH",
                    "TMPDIR": cliTemporaryDirectory,
                    "WENDY_ANALYTICS": "false",
                ]
            )

            var agentEnv: [String: String] = [:]
            if Environment.agentAddress == nil,
                let agentBinDirectory = Self.runDirectoryPath("agent", "bin")
            {
                agentEnv["PATH"] = "\(agentBinDirectory):$PATH"
            }

            let agentMachine = Machine(
                id: "agent",
                name: "Agent",
                os: Environment.agentOS ?? .current,
                tags: [.agent],
                user: Environment.agentUser,
                address: Environment.agentAddress,
                workingDirectory: Environment.agentWorkingDirectory
                    ?? repositoryRootDirectoryURL.appendingPathComponent("swift").path,
                env: agentEnv
            )

            let cli = try await Session.begin(
                for: cliMachine,
                recorder: recorder
            )
            cliSession = cli
            let agent = try await Session.begin(
                for: agentMachine,
                recorder: recorder
            )
            agentSession = agent

            try await cli.sh("mkdir -p \"$HOME\" \"$TMPDIR\"")

            return (cli, agent)
        } catch {
            try? await Self.tearDown(
                cli: cliSession,
                agent: agentSession
            )
            throw error
        }
    }

    private static func tearDown(
        cli: Session?,
        agent: Session?
    ) async throws {
        var firstError: (any Error)?

        if let cli, Environment.runDirectory == nil {
            do {
                try await cli.sh(
                    """
                    for directory in "$HOME" "$TMPDIR"; do
                      if [ -d "$directory" ]; then
                        chmod -R u+w "$directory" 2>/dev/null || true
                        rm -rf "$directory"
                      fi
                    done
                    """
                )
            } catch {
                firstError = firstError ?? error
            }
        }
        if let agent {
            do {
                try await agent.end()
            } catch {
                firstError = firstError ?? error
            }
        }
        if let cli {
            do {
                try await cli.end()
            } catch {
                firstError = firstError ?? error
            }
        }

        if let firstError {
            throw firstError
        }
    }

    private static func runDirectoryPath(_ components: String...) -> String? {
        guard let runDirectory = Environment.runDirectory else {
            return nil
        }

        return components.reduce(
            URL(fileURLWithPath: runDirectory, isDirectory: true)
        ) { url, component in
            url.appendingPathComponent(component, isDirectory: true)
        }.path
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
