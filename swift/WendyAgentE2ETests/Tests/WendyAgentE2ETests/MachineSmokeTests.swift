import Foundation
import Testing

@testable import WendyAgentE2E

extension Tag {
    @Tag static var e2e: Self
}

@Suite("Machine smoke tests", .serialized, .tags(.e2e))
struct MachineSmokeTests {
    @Test("build, push, open, and smoke-test the mac agent", .timeLimit(.minutes(10)))
    func buildPushOpenAndSmokeTestMacAgent() async throws {
        let environment = ProcessInfo.processInfo.environment
        guard environment["WENDY_E2E_SMOKE"] == "1" else {
            return
        }

        let workspace = try TemporaryDirectory(prefix: "wendy-e2e-")
        defer { workspace.remove() }

        let runner = try environment["E2E_RUNNER"].map(Machine.parse) ?? .local(Self.repoRoot)
        let cli =
            try environment["E2E_CLI"].map(Machine.parse)
            ?? .local(workspace.url.appendingPathComponent("cli", isDirectory: true).path)
        let agent =
            try environment["E2E_AGENT"].map(Machine.parse)
            ?? .local(workspace.url.appendingPathComponent("agent", isDirectory: true).path)
        let device = environment["E2E_DEVICE"] ?? "127.0.0.1"

        if cli.isLocal {
            try FileManager.default.createDirectory(
                atPath: cli.baseDirectory,
                withIntermediateDirectories: true
            )
        }
        if agent.isLocal {
            try FileManager.default.createDirectory(
                atPath: agent.baseDirectory,
                withIntermediateDirectories: true
            )
        }

        try await runner.run("cd swift && make build-dev")
        try await runner.run("cd go && make build")

        try await runner.push("go/bin/wendy", to: cli)
        try await runner.push("fixtures/HelloMac", to: cli)
        try await runner.push("swift/Build/WendyAgentMac.app", to: agent)

        defer {
            Task {
                try? await agent.run(
                    "osascript -e 'tell application id \"sh.wendy.WendyAgentMac\" to quit' || true"
                )
            }
        }

        try await agent.run("open WendyAgentMac.app")
        try await Self.waitForAgent(cli: cli, device: device)
    }

    private static var repoRoot: String {
        URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .path
    }

    private static func waitForAgent(cli: Machine, device: String) async throws {
        let command = "./wendy --device \(device) apps list"

        for attempt in 1...30 {
            do {
                try await cli.run(command)
                return
            } catch {
                if attempt == 30 {
                    throw error
                }
                try await Task.sleep(for: .seconds(1))
            }
        }
    }
}

private struct TemporaryDirectory {
    let url: URL

    init(prefix: String) throws {
        let root = FileManager.default.temporaryDirectory
        self.url = root.appendingPathComponent(prefix + UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: self.url, withIntermediateDirectories: true)
    }

    func remove() {
        try? FileManager.default.removeItem(at: self.url)
    }
}
