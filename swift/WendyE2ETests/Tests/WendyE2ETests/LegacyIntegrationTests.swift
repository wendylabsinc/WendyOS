import Foundation
import Testing
import WendyE2ETesting

private let legacyIntegrationTestsEnabled = LegacyIntegrationTestEnvironment.flag(
    "WENDY_E2E_LEGACY_INTEGRATION"
)

/// Legacy integration tests preserve parity with `go/scripts/test-ci.sh`.
///
/// These tests deploy the existing `.github/ci-tests` fixtures against a real
/// WendyOS device. They are intentionally separated from the CLI spec suites so
/// the shell-driven app integration coverage can move into the Swift E2E harness
/// without redefining the formal command contract.
///
/// All scenarios run unauthenticated (`authenticated: false`) for parity with
/// the shell driver, which pre-dates CLI auth; authenticated variants are a
/// migration follow-up.
///
/// Set `WENDY_E2E_LEGACY_INTEGRATION=true` and filter this suite to run it during
/// the migration period.
@Suite(.serialized, .enabled(if: legacyIntegrationTestsEnabled))
struct `legacy integration tests` {
    let scenario = CLIAndAgentScenario()

    // MARK: - Swift Fixtures

    /**
     Builds, deploys, starts, and streams the minimal Swift fixture. This
     preserves the `swift-hello` check from `go/scripts/test-ci.sh`.
     */
    @Test
    func `swift-hello`() async throws {
        try await self.runFixture("swift-hello")
    }

    /**
     Builds and runs the Swift fixture with the host-network entitlement. The
     app verifies DNS resolution and TCP connectivity from inside the container.
     */
    @Test
    func `swift-network`() async throws {
        try await self.runFixture("swift-network")
    }

    /**
     Builds and runs the Swift fixture with the Bluetooth entitlement. The app
     verifies that Bluetooth controller state or raw HCI socket access is
     visible inside the container.
     */
    @Test
    func `swift-bluetooth`() async throws {
        try await self.runFixture("swift-bluetooth")
    }

    /**
     Builds and runs the SwiftPM resource fixture. The app verifies that a
     bundled resource file is synced into the image and loaded with
     `Bundle.module` at runtime.
     */
    @Test
    func `swift-resources`() async throws {
        try await self.runFixture("swift-resources")
    }

    // MARK: - Python Fixtures

    /**
     Builds, deploys, starts, and streams the minimal Python fixture. This
     preserves the `python-hello` smoke check from `go/scripts/test-ci.sh`.
     */
    @Test
    func `python-hello`() async throws {
        try await self.runFixture("python-hello")
    }

    /**
     Builds and runs the Python fixture with the host-network entitlement. The
     app verifies outbound HTTP connectivity from inside the container.
     */
    @Test
    func `python-network`() async throws {
        try await self.runFixture("python-network")
    }

    /**
     Builds and runs the Python GPU fixture on GPU-capable devices. The app
     verifies CUDA availability through PyTorch and runs a CUDA matrix multiply.
     Devices without GPU support are recorded as skipped for shell parity.
     */
    @Test
    func `python-gpu`() async throws {
        try await self.runFixture("python-gpu", requiresGPU: true)
    }

    /**
     Builds and runs the Python ONNX Runtime GPU fixture on GPU-capable devices.
     The app verifies that `CUDAExecutionProvider` is available and performs a
     tiny GPU-backed inference.
     */
    @Test
    func `python-onnx-gpu`() async throws {
        try await self.runFixture("python-onnx-gpu", requiresGPU: true)
    }

    /**
     Builds and runs the Python fixture with the Bluetooth entitlement. The app
     verifies that Bluetooth controller state or raw HCI socket access is
     visible inside the container.
     */
    @Test
    func `python-bluetooth`() async throws {
        try await self.runFixture("python-bluetooth")
    }

    /**
     Builds and runs a Python fixture without the network entitlement. The app
     passes only when outbound network access is denied.
     */
    @Test
    func `python-no-network`() async throws {
        try await self.runFixture("python-no-network")
    }

    /**
     Builds and runs a Python fixture without the Bluetooth entitlement. The app
     passes only when Bluetooth devices and raw HCI socket access are denied.
     */
    @Test
    func `python-no-bluetooth`() async throws {
        try await self.runFixture("python-no-bluetooth")
    }

    /**
     Builds and runs a Python fixture that calls `ptrace(PTRACE_TRACEME)`. The
     app passes only when the default seccomp profile denies the syscall.
     */
    @Test
    func `python-no-ptrace`() async throws {
        try await self.runFixture("python-no-ptrace")
    }

    /**
     Builds and runs a Python fixture that calls `unshare(CLONE_NEWUSER)`. The
     app passes only when the default seccomp profile denies the syscall.
     */
    @Test
    func `python-no-unshare`() async throws {
        try await self.runFixture("python-no-unshare")
    }

    /**
     Builds and runs a Python fixture that calls the kernel-module and kexec
     syscalls. The app passes only when the default seccomp profile denies
     them with `EPERM` — these are host-escape primitives a normal application
     container never needs.
     */
    @Test
    func `python-no-kexec-module`() async throws {
        try await self.runFixture("python-no-kexec-module")
    }

    /**
     Builds and runs a Python fixture with the `host-admin` network mode. The
     app verifies that `CAP_NET_ADMIN` is present in its effective capability
     set, since `host-admin` is the explicit opt-in for reconfiguring host
     networking.
     */
    @Test
    func `python-network-host-admin`() async throws {
        try await self.runFixture("python-network-host-admin")
    }

    /**
     Builds and runs a Python fixture with plain `host` networking. The app
     passes only when `CAP_NET_ADMIN` is absent, guarding against a regression
     that re-couples the capability to plain host networking.
     */
    @Test
    func `python-no-net-admin`() async throws {
        try await self.runFixture("python-no-net-admin")
    }

    /**
     Builds and runs a Python fixture that declares app-level resource limits
     in `wendy.json`. The app reads its own cgroup and verifies the memory,
     CPU, and pids ceilings were applied end-to-end.
     */
    @Test
    func `python-resources`() async throws {
        try await self.runFixture("python-resources")
    }

    /**
     Builds and runs a Python fixture with a top-level `serviceName` in
     `wendy.json`. The app verifies the `WENDY_HOSTNAME`, `WENDY_APP_GROUP`,
     and `WENDY_APP_ID` environment variables follow the container-naming
     convention.
     */
    @Test
    func `python-servicename`() async throws {
        try await self.runFixture("python-servicename")
    }

    // MARK: - Multi-Service Fixtures

    /**
     Deploys the legacy Python multi-service fixture. The full deployment and
     `--service api` deployment must succeed, while `--service ghost` must fail
     with a diagnostic that mentions the unknown service name.
     */
    @Test
    func `python-multiservice`() async throws {
        try await self.scenario.run(authenticated: false) { cli, agent in
            let device = agent.machine.address
            let fixture = try Self.fixturePath("python-multiservice")

            try await Self.assertWendyRunSucceeds(
                on: cli,
                device: device,
                fixture: fixture,
                extraArguments: ["--deploy"]
            )
            try await Self.assertWendyRunSucceeds(
                on: cli,
                device: device,
                fixture: fixture,
                extraArguments: ["--deploy", "--service", "api"]
            )

            let command = try Self.wendyRunCommand(
                device: device,
                fixture: fixture,
                extraArguments: ["--deploy", "--service", "ghost"]
            )
            try await cli.sh(posix: command.posix, power: command.power) { result in
                let output = result.normalizedStdout + result.normalizedStderr

                #expect(result.status.isFailure)
                #expect(output.localizedCaseInsensitiveContains("ghost"))
            }
        }
    }

    /**
     Deploys the legacy multi-service resource-limit fixture. The `db` service
     inherits the app-level memory limit while `api` overrides it; each service
     asserts its own cgroup limits and logs `<svc>: PASS`. The test reads the
     device logs for both PASS lines, mirroring `go/scripts/test-ci.sh`, and
     removes the app afterwards.
     */
    @Test
    func `python-multiservice-resources`() async throws {
        try await self.scenario.run(authenticated: false) { cli, agent in
            let device = agent.machine.address
            let fixture = try Self.fixturePath("python-multiservice-resources")
            let appID = "sh.wendy.ci.python-multiservice-resources"

            try await Self.assertWendyRunSucceeds(
                on: cli,
                device: device,
                fixture: fixture,
                extraArguments: ["--deploy"]
            )

            let logs = try Self.deviceLogsCommand(device: device, appID: appID)
            try await cli.sh(posix: logs.posix, power: logs.power) { result in
                let output = result.normalizedStdout

                #expect(!output.localizedCaseInsensitiveContains("FAIL"))
                #expect(output.contains("db: PASS"))
                #expect(output.contains("api: PASS"))
            }

            try await Self.removeApp(on: cli, device: device, appID: appID)
        }
    }

    // MARK: - Compose Fixtures

    /**
     Deploys the legacy Compose fixture whose services are built from local
     Dockerfiles. The command runs detached, matching `go/scripts/test-ci.sh`.
     */
    @Test
    func `compose-hello`() async throws {
        try await self.runFixture("compose-hello", extraArguments: ["--detach"])
    }

    /**
     Deploys the legacy Compose fixture whose services use public images. The
     command runs detached, matching `go/scripts/test-ci.sh`.
     */
    @Test
    func `compose-images`() async throws {
        try await self.runFixture("compose-images", extraArguments: ["--detach"])
    }

    /**
     Deploys the legacy Compose fixture that pairs a locally built service
     with a public-image companion. The command runs detached, matching
     `go/scripts/test-ci.sh`.
     */
    @Test
    func `compose-companion`() async throws {
        try await self.runFixture("compose-companion", extraArguments: ["--detach"])
    }

    // MARK: - Device Monitoring

    /**
     Deploys a long-running fixture, then verifies `wendy device top --json`
     reports a host snapshot with non-zero CPU and memory totals and lists the
     deployed container. The app is removed afterwards.
     */
    @Test
    func `python-device-top`() async throws {
        try await self.scenario.run(authenticated: false) { cli, agent in
            let device = agent.machine.address
            let fixture = try Self.fixturePath("python-device-top")
            let appID = "sh.wendy.ci.python-device-top"

            try await Self.assertWendyRunSucceeds(
                on: cli,
                device: device,
                fixture: fixture,
                extraArguments: ["--detach"]
            )

            let top = try Self.wendyCommand([
                "device", "top",
                "--device", Self.validatedHost(device),
                "--json",
            ])
            try await cli.sh(posix: top.posix, power: top.power) { result in
                #expect(result.status.isSuccess)

                let stdout = result.stdout.trimmingCharacters(in: .whitespacesAndNewlines)
                guard let data = stdout.data(using: .utf8),
                    let json = try? JSONSerialization.jsonObject(with: data) as? [String: Any]
                else {
                    Issue.record("'wendy device top --json' did not print a JSON object")
                    return
                }

                let host = json["host"] as? [String: Any]
                #expect((host?["cpuCount"] as? Int ?? 0) > 0)
                #expect((host?["memTotalBytes"] as? Int ?? 0) > 0)

                let containers = json["containers"] as? [[String: Any]] ?? []
                #expect(containers.contains { ($0["name"] as? String) == appID })
            }

            try await Self.removeApp(on: cli, device: device, appID: appID)
        }
    }

    // MARK: - Agent Exposure Policy

    /**
     Verifies that the agent OTEL receivers are bound to localhost only. Ports
     4317 and 4318 must not be reachable from the CLI side over the network.
     */
    @Test
    func `otel-localhost-only`() async throws {
        try await self.scenario.run(authenticated: false) { cli, agent in
            let device = agent.machine.address

            try await Self.assertPortIsNotReachable(on: cli, host: device, port: 4317)
            try await Self.assertPortIsNotReachable(on: cli, host: device, port: 4318)
        }
    }

    // MARK: - Private

    private func runFixture(
        _ name: String,
        extraArguments: [String] = [],
        requiresGPU: Bool = false,
        filePath: String = #filePath,
        function: String = #function,
        line: Int = #line
    ) async throws {
        try await self.scenario.run(
            authenticated: false,
            filePath: filePath,
            function: function,
            line: line
        ) { cli, agent in
            let device = agent.machine.address
            if requiresGPU {
                let hasGPU = try await Self.deviceHasGPU(on: cli, device: device)
                guard hasGPU else {
                    try await Self.recordSkip(on: cli, "\(name): no GPU")
                    return
                }
            }

            try await Self.assertWendyRunSucceeds(
                on: cli,
                device: device,
                fixture: Self.fixturePath(name),
                extraArguments: extraArguments
            )
        }
    }

    private static func assertWendyRunSucceeds(
        on cli: WendyE2ESession,
        device: String,
        fixture: String,
        extraArguments: [String] = []
    ) async throws {
        let command = try Self.wendyRunCommand(
            device: device,
            fixture: fixture,
            extraArguments: extraArguments
        )
        try await cli.sh(posix: command.posix, power: command.power) { result in
            #expect(result.status.isSuccess)
        }
    }

    private static func assertPortIsNotReachable(
        on cli: WendyE2ESession,
        host: String,
        port: Int
    ) async throws {
        let host = try Self.validatedHost(host)

        try await cli.sh(
            posix: "! nc -z -w 3 \(Self.shellQuote(host)) \(port) 2>/dev/null",
            power: """
                $reachable = Test-NetConnection -ComputerName \(Self.powerShellQuote(host)) -Port \(port) -InformationLevel Quiet
                if ($reachable) { exit 1 } else { exit 0 }
                """
        ) { result in
            #expect(result.status.isSuccess)
        }
    }

    private static func deviceHasGPU(
        on cli: WendyE2ESession,
        device: String
    ) async throws -> Bool {
        let command = try Self.wendyCommand([
            "--device", Self.validatedHost(device),
            "device", "info",
            "--json",
        ])
        return try await cli.sh(posix: command.posix, power: command.power) { result in
            guard result.status.isSuccess,
                let data = result.stdout.data(using: .utf8),
                let json = try? JSONSerialization.jsonObject(with: data) as? [String: Any]
            else {
                return false
            }

            return json["hasGpu"] as? Bool ?? false
        }
    }

    /**
     Reads recent device logs for an app with a bounded wall-clock wait,
     because `wendy device logs` streams and never exits on its own. This
     mirrors the background-reader pattern in `go/scripts/test-ci.sh`.
     */
    private static func deviceLogsCommand(device: String, appID: String) throws -> ShellCommand {
        let logs = try Self.wendyCommand([
            "device", "logs",
            "--device", Self.validatedHost(device),
            "--app", appID,
            "--tail", "50",
        ])
        return ShellCommand(
            posix: """
                logfile=$(mktemp)
                \(logs.posix) >"$logfile" 2>&1 &
                logs_pid=$!
                sleep 8
                kill "$logs_pid" 2>/dev/null || true
                wait "$logs_pid" 2>/dev/null || true
                cat "$logfile"
                rm -f "$logfile"
                """,
            power: """
                $job = Start-Job -ScriptBlock { \(logs.power) 2>&1 | Out-String }
                $null = Wait-Job -Job $job -Timeout 8
                Stop-Job -Job $job
                Receive-Job -Job $job
                Remove-Job -Job $job -Force
                """
        )
    }

    private static func removeApp(
        on cli: WendyE2ESession,
        device: String,
        appID: String
    ) async throws {
        let command = try Self.wendyCommand([
            "device", "apps", "remove", appID,
            "--device", Self.validatedHost(device),
            "--force", "--cleanup",
        ])
        // Cleanup is best-effort, mirroring `|| true` in the shell driver.
        try await cli.sh(posix: command.posix, power: command.power) { _ in }
    }

    private static func recordSkip(on cli: WendyE2ESession, _ reason: String) async throws {
        try await cli.sh(
            posix: "printf '%s\\n' \(Self.shellQuote("SKIP: \(reason)"))",
            power: "Write-Output \(Self.powerShellQuote("SKIP: \(reason)"))"
        )
    }

    private static func wendyRunCommand(
        device: String,
        fixture: String,
        extraArguments: [String] = []
    ) throws -> ShellCommand {
        try Self.wendyCommand(
            [
                "run",
                "--device", Self.validatedHost(device),
                "--prefix", fixture,
            ] + extraArguments
        )
    }

    private static func wendyCommand(_ arguments: [String]) -> ShellCommand {
        ShellCommand(
            posix: (["wendy"] + arguments.map(Self.shellQuote)).joined(separator: " "),
            power: (["wendy"] + arguments.map(Self.powerShellQuote)).joined(separator: " ")
        )
    }

    private static func fixturePath(_ name: String) throws -> String {
        try Self.path(
            Self.validatedRepositoryRoot(Self.repositoryRootPathOnCLIMachine),
            ".github",
            "ci-tests",
            name
        )
    }

    private static var repositoryRootPathOnCLIMachine: String {
        WendyE2EEnvironment.cliRepoDirectory
            ?? URL(fileURLWithPath: #filePath, isDirectory: false)
            .deletingLastPathComponent()  // Tests/WendyE2ETests
            .deletingLastPathComponent()  // Tests
            .deletingLastPathComponent()  // swift/WendyE2ETests
            .deletingLastPathComponent()  // swift
            .deletingLastPathComponent()  // repository root
            .path
    }

    private static func path(_ first: String, _ rest: String...) -> String {
        let separator = first.contains("\\") && !first.contains("/") ? "\\" : "/"
        return rest.reduce(first) { path, component in
            path.hasSuffix("/") || path.hasSuffix("\\")
                ? "\(path)\(component)" : "\(path)\(separator)\(component)"
        }
    }

    // MARK: - Shell Safety

    /**
     All command arguments in this suite are string literals except two: the
     device address (harness metadata) and the repository root (environment
     variable or compile-time fallback). Both are sanity-checked so a
     misconfigured environment fails fast with a readable message instead of
     a cryptic remote shell error. Quoting handles everything else.
     */

    /// Accepts hostnames, IPv4, and IPv6 addresses (including bracketed
    /// forms and zone indices).
    private static func validatedHost(_ value: String) throws -> String {
        let allowed = CharacterSet(
            charactersIn: "abcdefghijklmnopqrstuvwxyz"
                + "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
                + "0123456789.:%-[]"
        )
        guard !value.isEmpty, value.unicodeScalars.allSatisfy(allowed.contains) else {
            throw ShellSafetyError("device address contains unsupported characters")
        }
        return value
    }

    /// Requires an absolute path without traversal components, so fixture
    /// paths always point inside the checkout the caller intended.
    private static func validatedRepositoryRoot(_ path: String) throws -> String {
        let isPosixAbsolute = path.hasPrefix("/")
        let isWindowsAbsolute: Bool = {
            guard path.count >= 3, let drive = path.unicodeScalars.first,
                CharacterSet.letters.contains(drive)
            else {
                return false
            }
            let afterDrive = path.dropFirst()
            return afterDrive.hasPrefix(":\\") || afterDrive.hasPrefix(":/")
        }()
        guard isPosixAbsolute || isWindowsAbsolute else {
            throw ShellSafetyError("repository root must be an absolute path")
        }

        let components = path.split(whereSeparator: { $0 == "/" || $0 == "\\" })
        guard !components.contains("..") else {
            throw ShellSafetyError("repository root must not contain traversal components")
        }
        return path
    }

    private static func shellQuote(_ value: String) -> String {
        "'" + value.replacingOccurrences(of: "'", with: "'\\''") + "'"
    }

    private static func powerShellQuote(_ value: String) -> String {
        "'" + value.replacingOccurrences(of: "'", with: "''") + "'"
    }

    private struct ShellSafetyError: Error, CustomStringConvertible {
        let description: String

        init(_ description: String) {
            self.description = description
        }
    }

    private struct ShellCommand: Sendable {
        let posix: String
        let power: String
    }
}

private enum LegacyIntegrationTestEnvironment {
    static func flag(_ name: String) -> Bool {
        guard let value = ProcessInfo.processInfo.environment[name]?.lowercased() else {
            return false
        }
        return ["1", "true", "yes", "on", "enabled"].contains(value)
    }
}
