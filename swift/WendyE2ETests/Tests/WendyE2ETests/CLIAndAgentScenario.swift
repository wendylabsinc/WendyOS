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
            let reporter = try Reporter(
                filePath: filePath,
                function: function,
                line: line
            )
            let repositoryRootDirectoryURL = Self.repositoryRootDirectoryURL()
            let cliWorkingDirectory =
                Environment.cliWorkingDirectory
                ?? repositoryRootDirectoryURL.appendingPathComponent("go").path
            let cliHomeDirectory = "/tmp/wendy-e2e-cli-home-\(UUID().uuidString)"

            let cliMachine = Machine(
                id: "cli",
                name: "CLI",
                os: Environment.cliOS ?? .current,
                tags: [.cli],
                ssh: Environment.cliSSH,
                workingDirectory: cliWorkingDirectory,
                env: [
                    "HOME": cliHomeDirectory,
                    "PATH": "\(cliWorkingDirectory)/bin:$PATH",
                    "WENDY_ANALYTICS": "false",
                ]
            )

            let agentMachine = Machine(
                id: "agent",
                name: "Agent",
                os: Environment.agentOS ?? .current,
                tags: [.agent],
                ssh: Environment.agentSSH,
                workingDirectory: Environment.agentWorkingDirectory
                    ?? repositoryRootDirectoryURL.appendingPathComponent("swift").path
            )

            let cli = try await Session.begin(
                for: cliMachine,
                reporter: reporter
            )
            cliSession = cli
            let agent = try await Session.begin(
                for: agentMachine,
                reporter: reporter
            )
            agentSession = agent

            try await cli.sh("mkdir -p \"$HOME\"")
            try await self.buildCLIIfNeeded(with: cli)
            try await self.buildAgentIfNeeded(with: agent)
            try await Self.startAgent(with: agent, verifiedBy: cli)

            return (cli, agent)
        } catch {
            try? await Self.tearDown(
                cli: cliSession,
                agent: agentSession
            )
            throw error
        }
    }

    private func buildCLIIfNeeded(with session: Session) async throws {
        switch session.machine.os {
        case .macOS, .linux:
            try await session.sh(
                """
                set -e
                stamp=/tmp/wendy-e2e-\(Environment.runID)-cli-built
                if [ ! -f "$stamp" ]; then
                  make build-cli
                  touch "$stamp"
                fi
                """
            )
        case .windows, .wendyOS:
            fatalError("Building the CLI is not supported on \(session.machine.os) yet.")
        }
    }

    private func buildAgentIfNeeded(with session: Session) async throws {
        switch session.machine.os {
        case .macOS:
            try await session.sh(
                """
                set -e
                stamp=/tmp/wendy-e2e-\(Environment.runID)-agent-built
                if [ ! -f "$stamp" ]; then
                  make build-dev
                  touch "$stamp"
                fi
                """
            )
        case .linux:
            try await session.sh(
                """
                set -e
                stamp=/tmp/wendy-e2e-\(Environment.runID)-agent-built
                if [ ! -f "$stamp" ]; then
                  cd ../go
                  make build-agent
                  touch "$stamp"
                fi
                """
            )
        case .windows, .wendyOS:
            fatalError("Building the agent is not supported on \(session.machine.os) yet.")
        }
    }

    private static func startAgent(with session: Session, verifiedBy cli: Session) async throws {
        try await Self.stopAgent(with: session)

        switch session.machine.os {
        case .macOS:
            try await session.sh("open Build/WendyAgentMac.app")
        case .linux:
            try await session.sh(
                """
                set -e
                pidfile=/tmp/wendy-agent-e2e.pid
                logfile=/tmp/wendy-agent-e2e.log

                cd ../go
                nohup ./bin/wendy-agent > "$logfile" 2>&1 &
                echo $! > "$pidfile"
                """
            )
        case .windows, .wendyOS:
            fatalError("Starting the agent is not supported on \(session.machine.os) yet.")
        }

        try await cli
            .command("wendy --device ::1 device info --json >/dev/null")
            .poll(
                until: .success,
                step: .seconds(1),
                timeout: .seconds(30),
                timeoutMessage: "Wendy agent did not become ready"
            )
            .run()
    }

    private static func stopAgent(with session: Session) async throws {
        switch session.machine.os {
        case .macOS:
            try await session.sh("make quit")
        case .linux:
            try await session.sh(
                """
                set -e
                pidfile=/tmp/wendy-agent-e2e.pid

                if [ -f "$pidfile" ]; then
                  pid="$(cat "$pidfile")"
                  if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
                    kill "$pid"
                    sleep 1
                    if kill -0 "$pid" 2>/dev/null; then
                      kill -9 "$pid"
                    fi
                  fi
                  rm -f "$pidfile"
                fi
                """
            )
        case .windows, .wendyOS:
            fatalError("Stopping the agent is not supported on \(session.machine.os) yet.")
        }
    }

    private static func tearDown(
        cli: Session?,
        agent: Session?
    ) async throws {
        var firstError: (any Error)?

        if let agent {
            do {
                try await Self.stopAgent(with: agent)
            } catch {
                firstError = firstError ?? error
            }
        }
        if let cli {
            do {
                try await cli.sh(
                    """
                    if [ -d "$HOME" ]; then
                      chmod -R u+w "$HOME" 2>/dev/null || true
                      rm -rf "$HOME"
                    fi
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

    private static func repositoryRootDirectoryURL() -> URL {
        URL(fileURLWithPath: #filePath, isDirectory: false)
            .deletingLastPathComponent()  // Tests/WendyE2ETests
            .deletingLastPathComponent()  // Tests
            .deletingLastPathComponent()  // swift/WendyE2ETests
            .deletingLastPathComponent()  // swift
            .deletingLastPathComponent()  // repository root
    }
}
