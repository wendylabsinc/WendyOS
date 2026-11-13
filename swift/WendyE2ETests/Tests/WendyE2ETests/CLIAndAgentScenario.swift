import Foundation
import WendyE2ETesting

final class CLIAndAgentScenario: WendyE2EScenario, Sendable {
    // MARK: - Internal

    typealias Hook =
        @Sendable (_ cli: WendyE2ESession, _ agent: WendyE2ESession) async throws -> Void

    init(
        before: Hook? = nil,
        after: Hook? = nil
    ) {
        self.before = before
        self.after = after
    }

    func run<Result>(
        authenticated: Bool = true,
        useDeviceCache: Bool = false,
        filePath: String = #filePath,
        function: String = #function,
        line: Int = #line,
        _ body: @Sendable (_ cli: WendyE2ESession, _ agent: WendyE2ESession) async throws -> Result
    ) async throws -> Result {
        let (cli, agent) = try await self.setUp(
            authenticated: authenticated,
            useDeviceCache: useDeviceCache,
            filePath: filePath,
            function: function,
            line: line
        )

        var result: Result?
        var primaryError: (any Error)?

        do {
            if let before {
                try await before(cli, agent)
            }
            result = try await body(cli, agent)
        } catch {
            primaryError = error
        }

        do {
            if let after {
                try await after(cli, agent)
            }
        } catch {
            primaryError = primaryError ?? error
        }

        do {
            try await Self.tearDown(
                cli: cli,
                agent: agent
            )
        } catch {
            primaryError = primaryError ?? error
        }

        if let primaryError {
            throw primaryError
        }

        return result!
    }

    // MARK: - Private

    private let before: Hook?
    private let after: Hook?

    private func setUp(
        authenticated: Bool,
        useDeviceCache: Bool,
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
            if authenticated {
                try await Self.installCLIAuthFixture(into: cli)
            }
            if useDeviceCache, let source = WendyE2EEnvironment.cliDeviceCachePath {
                guard authenticated, isolation == .perTest else {
                    throw NSError(
                        domain: "CLIAndAgentScenario",
                        code: 1,
                        userInfo: [
                            NSLocalizedDescriptionKey:
                                "Device cache fixtures require authenticated per-test isolation."
                        ]
                    )
                }
                try await cli.sh(
                    posix: """
                        if [ -n "${WENDY_AGENT_SOCKET:-}" ]; then
                          echo "ERROR: unset WENDY_AGENT_SOCKET when using an explicit device cache fixture." >&2
                          exit 1
                        fi
                        cp \(Self.shellQuote(source)) "$HOME/.wendy/devices.json"
                        """,
                    power: """
                        if ($env:WENDY_AGENT_SOCKET) {
                            Write-Error "unset WENDY_AGENT_SOCKET when using an explicit device cache fixture."
                            exit 1
                        }
                        Copy-Item -LiteralPath \(Self.powerShellQuote(source)) -Destination (Join-Path $env:HOME '.wendy/devices.json') -ErrorAction Stop
                        """
                )
            }
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

    private static func installCLIAuthFixture(into cli: WendyE2ESession) async throws {
        let source = WendyE2EEnvironment.cliAuthConfigPath ?? ""
        try await cli.sh(
            posix: """
                auth_config_source=
                auth_config_source=\(Self.shellQuote(source))
                if [ -z "$auth_config_source" ] || [ ! -f "$auth_config_source" ]; then
                  echo "ERROR: authenticated Wendy E2E scenario requires a CLI auth fixture config. Run 'wendy auth login' or set WENDY_E2E_CLI_AUTH_CONFIG_PATH." >&2
                  exit 1
                fi
                mkdir -p "$HOME/.wendy"
                auth_config_target="$HOME/.wendy/config.json"
                if [ "$auth_config_source" != "$auth_config_target" ]; then
                  cp "$auth_config_source" "$auth_config_target"
                fi
                chmod 600 "$auth_config_target" 2>/dev/null || true
                """,
            power: """
                $authConfigSource = \(Self.powerShellQuote(source))
                if (-not $authConfigSource -or -not (Test-Path -LiteralPath $authConfigSource -PathType Leaf)) {
                    Write-Error "authenticated Wendy E2E scenario requires a CLI auth fixture config. Run 'wendy auth login' or set WENDY_E2E_CLI_AUTH_CONFIG_PATH."
                    exit 1
                }
                $authConfigDirectory = Join-Path $env:HOME '.wendy'
                New-Item -ItemType Directory -Force -Path $authConfigDirectory | Out-Null
                $authConfigTarget = Join-Path $authConfigDirectory 'config.json'
                if ($authConfigSource -ne $authConfigTarget) {
                    Copy-Item -LiteralPath $authConfigSource -Destination $authConfigTarget -Force
                }
                """
        )
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

    private static func shellQuote(_ value: String) -> String {
        "'" + value.replacingOccurrences(of: "'", with: "'\\''") + "'"
    }

    private static func powerShellQuote(_ value: String) -> String {
        "'" + value.replacingOccurrences(of: "'", with: "''") + "'"
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
            environment["WENDY_CONFIG_DIR"] = Self.path(homeDirectory, ".wendy")
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
