import Foundation
import WendyE2ETesting

final class CLIAndAgentScenario: WendyE2EScenario, Sendable {
    // MARK: - Internal

    func run<Result>(
        filePath: String = #filePath,
        function: String = #function,
        line: Int = #line,
        _ body: @Sendable (_ cli: WendyE2ESession, _ agent: WendyE2ESession) async throws -> Result
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
    ) async throws -> (cli: WendyE2ESession, agent: WendyE2ESession) {
        var cliSession: WendyE2ESession?
        var agentSession: WendyE2ESession?

        do {
            let recorder = try WendyE2ERecorder(
                filePath: filePath,
                function: function,
                line: line
            )
            let repositoryRootDirectoryURL = Self.repositoryRootDirectoryURL()
            let testDirectoryURL = URL(
                fileURLWithPath: recorder.testDirectoryPath,
                isDirectory: true
            )
            let testName = testDirectoryURL.lastPathComponent
            let testDirectoryName = Self.path(
                testDirectoryURL.deletingLastPathComponent().lastPathComponent,
                testName
            )
            let fallbackTestDirectory = testDirectoryURL.path
            let isolation = WendyE2EEnvironment.isolation
            let resetDirectoriesOnFirstCommand =
                isolation == .perRun
                && !WendyE2EEnvironment.parallel
            let cliMachine = WendyE2EMachine.cli
            let agentMachine = WendyE2EMachine.agent
            let cliOS = cliMachine.os
            let agentOS = agentMachine.os
            let cliSandbox = Self.roleSandbox(
                role: "cli",
                runDirectory: WendyE2EEnvironment.cliRunDirectory,
                fallbackTestDirectory: fallbackTestDirectory,
                testName: testDirectoryName,
                isolation: isolation
            )
            let cliBinDirectory = Self.roleBinDirectory(
                explicitDirectory: WendyE2EEnvironment.cliBinDirectory,
                runDirectory: WendyE2EEnvironment.cliRunDirectory,
                fallbackDirectory: repositoryRootDirectoryURL.appendingPathComponent("go/bin").path
            )
            let cliEnvironment = Self.roleEnvironment(
                sandbox: cliSandbox,
                binDirectory: cliBinDirectory,
                machineOS: cliOS
            )
            let agentSandbox = Self.roleSandbox(
                role: "agent",
                runDirectory: WendyE2EEnvironment.agentRunDirectory,
                fallbackTestDirectory: fallbackTestDirectory,
                testName: testDirectoryName,
                isolation: isolation
            )
            let agentBinDirectory = Self.roleBinDirectory(
                explicitDirectory: WendyE2EEnvironment.agentBinDirectory,
                runDirectory: WendyE2EEnvironment.agentRunDirectory,
                fallbackDirectory: nil
            )
            let agentEnv = Self.roleEnvironment(
                sandbox: agentSandbox,
                binDirectory: agentBinDirectory,
                machineOS: agentOS
            )
            let cli = try await WendyE2ESession.begin(
                for: cliMachine,
                workingDirectory: cliSandbox.workingDirectory,
                env: cliEnvironment,
                resetDirectoriesOnFirstCommand: resetDirectoriesOnFirstCommand,
                recorder: recorder
            )
            cliSession = cli
            let agent = try await WendyE2ESession.begin(
                for: agentMachine,
                workingDirectory: agentSandbox.workingDirectory,
                env: agentEnv,
                resetDirectoriesOnFirstCommand: resetDirectoriesOnFirstCommand,
                recorder: recorder
            )
            agentSession = agent

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
        cli: WendyE2ESession?,
        agent: WendyE2ESession?
    ) async throws {
        var firstError: (any Error)?

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

    private struct RoleSandbox {
        let homeDirectory: String?
        let temporaryDirectory: String?
        let workingDirectory: String?
    }

    private static func roleSandbox(
        role: String,
        runDirectory: String?,
        fallbackTestDirectory: String,
        testName: String,
        isolation: WendyE2EIsolation
    ) -> RoleSandbox {
        let roleDirectory: String?
        switch isolation {
        case .none:
            roleDirectory = nil
        case .perRun:
            roleDirectory = runDirectory ?? Self.path(Self.parentPath(fallbackTestDirectory), role)
        case .perTest:
            if let runDirectory {
                roleDirectory = Self.path(Self.parentPath(runDirectory), "tests", testName, role)
            } else {
                roleDirectory = Self.path(fallbackTestDirectory, role)
            }
        }

        guard let roleDirectory else {
            return RoleSandbox(
                homeDirectory: nil,
                temporaryDirectory: nil,
                workingDirectory: nil
            )
        }

        let homeDirectory = Self.path(roleDirectory, "home")
        return RoleSandbox(
            homeDirectory: homeDirectory,
            temporaryDirectory: Self.path(roleDirectory, "tmp"),
            workingDirectory: Self.path(homeDirectory, "work")
        )
    }

    private static func parentPath(_ path: String) -> String {
        var trimmed = path
        while trimmed.count > 1, Self.hasTrailingSeparator(trimmed) {
            trimmed.removeLast()
        }
        guard trimmed != "/", trimmed != "\\" else {
            return trimmed
        }
        guard let separatorIndex = trimmed.lastIndex(where: Self.isPathSeparator) else {
            return "."
        }
        if separatorIndex == trimmed.startIndex {
            return String(trimmed[...separatorIndex])
        }

        return String(trimmed[..<separatorIndex])
    }

    private static func roleBinDirectory(
        explicitDirectory: String?,
        runDirectory: String?,
        fallbackDirectory: String?
    ) -> String? {
        if let explicitDirectory {
            return explicitDirectory
        }
        guard let runDirectory else {
            return fallbackDirectory
        }

        return Self.path(runDirectory, "bin")
    }

    private static func path(_ first: String, _ rest: String...) -> String {
        let separator = Self.preferredSeparator(for: first)
        return rest.reduce(first) { path, component in
            let suffix = component.trimmingCharacters(in: CharacterSet(charactersIn: "/\\"))
            return Self.hasTrailingSeparator(path)
                ? "\(path)\(suffix)" : "\(path)\(separator)\(suffix)"
        }
    }

    private static func preferredSeparator(for path: String) -> String {
        path.contains("\\") && !path.contains("/") ? "\\" : "/"
    }

    private static func hasTrailingSeparator(_ path: String) -> Bool {
        path.hasSuffix("/") || path.hasSuffix("\\")
    }

    private static func isPathSeparator(_ character: Character) -> Bool {
        character == "/" || character == "\\"
    }

    private static func roleEnvironment(
        sandbox: RoleSandbox,
        binDirectory: String?,
        machineOS: WendyE2EMachineOS
    ) -> [String: String] {
        var environment = [
            "WENDY_ANALYTICS": "false"
        ]
        if let homeDirectory = sandbox.homeDirectory {
            environment["HOME"] = homeDirectory
            if machineOS == .windows {
                environment["USERPROFILE"] = homeDirectory
                environment["APPDATA"] = Self.path(homeDirectory, "AppData", "Roaming")
                environment["LOCALAPPDATA"] = Self.path(homeDirectory, "AppData", "Local")
            }
        }
        if let temporaryDirectory = sandbox.temporaryDirectory {
            environment["TMPDIR"] = temporaryDirectory
            if machineOS == .windows {
                environment["TMP"] = temporaryDirectory
                environment["TEMP"] = temporaryDirectory
            }
        }
        if let binDirectory {
            let separator = machineOS == .windows ? ";" : ":"
            environment["PATH"] = "\(binDirectory)\(separator)$PATH"
        }
        return environment
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
