import Darwin
import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import Logging
import Testing

@testable import WendyAgentCore

/// Audit 1736: the actual CLI watcher, file-sync RPCs, native app runtime and
/// telemetry transport. No packaged app, hardware, Keychain or container engine.
@Suite("CLI watch native transport")
struct WatchSessionTransportTests {
    @Test(
        .enabled(if: ProcessInfo.processInfo.environment["WENDY_WATCH_CLI"] != nil),
        .timeLimit(.minutes(2))
    )
    func fileEditRedeployAndInterrupt() async throws {
        let cli = try #require(ProcessInfo.processInfo.environment["WENDY_WATCH_CLI"])
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent("wendy-watch-session-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        var canRemoveRoot = true
        defer {
            if canRemoveRoot {
                try? FileManager.default.removeItem(at: root)
            } else {
                print("WATCH_CLEANUP_INCOMPLETE retained owned files at \(root.path)")
            }
        }
        let project = root.appendingPathComponent("project")
        let apps = root.appendingPathComponent("apps")
        try FileManager.default.createDirectory(at: project, withIntermediateDirectories: true)
        let appID = "sh.wendy.tests.WatchSession"
        let app = apps.appendingPathComponent(appID)
        let token = UUID().uuidString
        let initial = "initial-\(token)"
        let edited = "edited-\(token)"
        let late = "after-deploy-\(token)"
        let survivor = "after-interrupt-\(token)"
        let script = project.appendingPathComponent("launch.sh")
        func source(_ marker: String) -> String {
            """
            #!/bin/sh
            set -eu
            test -p input.fifo || /usr/bin/mkfifo input.fifo
            exec 3<>input.fifo
            echo "$$" > app.pid
            printf '%s\\n' '\(marker)'
            while IFS= read -r value <&3; do
                printf '%s\\n' "$value"
                printf '%s\\n' "$value" > receipt
            done
            """
        }
        try source(initial).write(to: script, atomically: false, encoding: .utf8)
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: script.path)
        let hook = root.appendingPathComponent("hook.sh")
        let hookFIFO = root.appendingPathComponent("hook.fifo")
        let madeFIFO = unsafe mkfifo(hookFIFO.path, 0o600)
        try #require(madeFIFO == 0)
        let hookSource = """
            #!/bin/sh
            set -eu
            echo "$$" > '\(root.path)/hook.pid'
            echo started >> '\(root.path)/hook.starts'
            exec 3<>'\(hookFIFO.path)'
            while IFS= read -r value <&3; do
                printf '%s\\n' "$value" > '\(root.path)/hook.receipt'
            done
            """
        try hookSource.write(to: hook, atomically: false, encoding: .utf8)
        struct Manifest: Encodable {
            struct Run: Encodable { let command = "launch.sh" }
            struct Hooks: Encodable {
                struct PostStart: Encodable { let cli: String }
                let postStart: PostStart
            }
            let appId: String
            let platform = "darwin"
            let run = Run()
            let hooks: Hooks
        }
        let config = Manifest(
            appId: appID,
            hooks: .init(postStart: .init(cli: "exec /bin/sh '\(hook.path)'"))
        )
        try JSONEncoder().encode(config).write(to: project.appendingPathComponent("wendy.json"))

        let broadcaster = TelemetryBroadcaster()
        let containers = ContainerService(
            broadcaster: broadcaster,
            executablePath: "/usr/bin/false",
            appsBase: apps
        )
        let server = try WendyAgent.makeMainServer(
            services: [
                AgentService(
                    gpuDiscovery: GPUDiscovery(devices: { [] }),
                    reportedVersion: { "watch-test" }
                ),
                FileSyncService(appsBase: apps), containers,
                TelemetryService(broadcaster: broadcaster),
            ],
            certs: nil,
            host: "127.0.0.1",
            port: 0,
            environment: [:],
            logger: Logger(label: "test.watch-transport")
        )
        try await withThrowingTaskGroup(of: Void.self) { group in
            group.addTask { try await server.serve() }
            defer {
                server.beginGracefulShutdown()
                group.cancelAll()
            }
            let port = try #require(try await server.listeningAddress?.ipv4?.port)
            let output = root.appendingPathComponent("cli.log")
            FileManager.default.createFile(atPath: output.path, contents: nil)
            let handle = try FileHandle(forWritingTo: output)
            defer { try? handle.close() }
            let process = Process()
            process.executableURL = URL(fileURLWithPath: cli)
            process.arguments = [
                "--device", "127.0.0.1:\(port)", "--json=false", "run", "--watch",
                "--prefix", project.path, "--debounce", "100", "--no-restart",
                "--skip-cloud-registration",
            ]
            process.environment = [
                "PATH": "/usr/bin:/bin:/usr/sbin:/sbin", "HOME": root.path,
                "WENDY_CONFIG_DIR": root.appendingPathComponent("config").path,
                "WENDY_SECRET_STORE": "file", "CI": "1", "WENDY_ANALYTICS": "false",
                "WENDY_NO_UPDATE_CHECK": "1", "TERM": "dumb",
            ]
            process.standardInput = FileHandle.nullDevice
            process.standardOutput = handle
            process.standardError = handle
            try process.run()
            do {
                try await watchWait("initial native output", output: output) {
                    try watchOutput(output).contains(initial)
                }
                try await watchWait("completed initial deploy", output: output) {
                    try watchOutput(output).contains("redeployed in")
                }
                try #require(await broadcaster.logSubscriberCountForTesting() == 1)
                let firstPID = try String(
                    contentsOf: app.appendingPathComponent("app.pid"),
                    encoding: .utf8
                )
                let firstProcessID = try #require(
                    Int32(firstPID.trimmingCharacters(in: .whitespacesAndNewlines))
                )
                let firstBirth = try #require(
                    NativeProcessConfiguration.birthTime(forPID: firstProcessID)
                )
                try watchSend(late, fifo: app.appendingPathComponent("input.fifo"))
                try await watchWait("live output after deploy RPC closed", output: output) {
                    try watchOutput(output).contains(late)
                }

                try await watchWait("session CLI hook started", output: output) {
                    FileManager.default.fileExists(
                        atPath: root.appendingPathComponent("hook.pid").path
                    )
                }
                let hookPID = try #require(
                    Int32(
                        String(contentsOf: root.appendingPathComponent("hook.pid"), encoding: .utf8)
                            .trimmingCharacters(in: .whitespacesAndNewlines)
                    )
                )
                let hookBirth = try #require(NativeProcessConfiguration.birthTime(forPID: hookPID))

                // A real filesystem write must cause fsnotify -> debounce ->
                // runCommand -> file sync -> replacement -> native stdout -> RPC.
                try source(edited).write(to: script, atomically: false, encoding: .utf8)
                try await watchWait("edited native output", output: output) {
                    try watchOutput(output).contains(edited)
                }
                let secondPID = try String(
                    contentsOf: app.appendingPathComponent("app.pid"),
                    encoding: .utf8
                )
                try #require(firstPID != secondPID, "edit did not replace the real native process")
                try #require(
                    NativeProcessConfiguration.birthTime(forPID: firstProcessID) != firstBirth,
                    "previous native process survived replacement"
                )
                try #require(await broadcaster.logSubscriberCountForTesting() == 1)
                try #require(
                    try String(contentsOf: app.appendingPathComponent("launch.sh"), encoding: .utf8)
                        == source(edited)
                )
                try #require(process.isRunning, "watch exited after redeploy")

                try watchSend("hook-after-redeploy", fifo: hookFIFO)
                try await watchWait("hook survives redeploy", output: output) {
                    (try? String(
                        contentsOf: root.appendingPathComponent("hook.receipt"),
                        encoding: .utf8
                    )) == "hook-after-redeploy\n"
                }
                try #require(
                    try String(
                        contentsOf: root.appendingPathComponent("hook.starts"),
                        encoding: .utf8
                    ) == "started\n",
                    "postStart CLI hook repeated across redeploy"
                )
                try #require(NativeProcessConfiguration.birthTime(forPID: hookPID) == hookBirth)

                process.interrupt()
                try await watchWait("CLI exit on interrupt", output: output) { !process.isRunning }
                process.waitUntilExit()
                try #require(process.terminationReason == .exit && process.terminationStatus == 0)
                try #require(
                    NativeProcessConfiguration.birthTime(forPID: hookPID) != hookBirth,
                    "session hook outlived watch exit"
                )
                try await watchWait("telemetry subscription cleanup", output: output, seconds: 20) {
                    await broadcaster.logSubscriberCountForTesting() == 0
                }
                // Ctrl-C belongs to watch, not the app: require a new app-side
                // receipt after CLI exit, not merely a stale PID or status field.
                try watchSend(survivor, fifo: app.appendingPathComponent("input.fifo"))
                try await watchWait("app survives watch interruption", output: output) {
                    (try? String(contentsOf: app.appendingPathComponent("receipt"), encoding: .utf8))
                        == survivor + "\n"
                }
                try #require(
                    try String(contentsOf: app.appendingPathComponent("app.pid"), encoding: .utf8)
                        == secondPID
                )
                print(
                    "WATCH_SESSION_VERIFIED file-edit, actual new process/output, interrupt, subscription drained, app survived"
                )
            } catch {
                print("watch CLI output:\n\((try? watchOutput(output)) ?? "unreadable")")
                canRemoveRoot = await watchStop(process)
                await containers.stopAllApps()
                let appsStopped = await containers.currentAppInfosForTesting().allSatisfy {
                    $0.status == .stopped && $0.pid == nil
                }
                canRemoveRoot = canRemoveRoot && appsStopped
                if !canRemoveRoot {
                    Issue.record("watch CLI or native app cleanup did not finish")
                }
                throw error
            }
            canRemoveRoot = await watchStop(process)
            await containers.stopAllApps()
            let appsStopped = await containers.currentAppInfosForTesting().allSatisfy {
                $0.status == .stopped && $0.pid == nil
            }
            canRemoveRoot = canRemoveRoot && appsStopped
            try #require(canRemoveRoot, "watch CLI or native app cleanup did not finish")
        }
    }
}

private func watchOutput(_ url: URL) throws -> String {
    let handle = try FileHandle(forReadingFrom: url)
    defer { try? handle.close() }
    let data = try handle.read(upToCount: 1_048_577) ?? Data()
    guard data.count <= 1_048_576 else { throw WatchFailure(message: "CLI output exceeded 1 MiB") }
    return String(decoding: data, as: UTF8.self)
}

private func watchWait(
    _ description: String,
    output: URL,
    seconds: Int = 12,
    condition: () async throws -> Bool
) async throws {
    let deadline = ContinuousClock.now + .seconds(seconds)
    while ContinuousClock.now < deadline {
        if try await condition() { return }
        try await Task.sleep(for: .milliseconds(40))
    }
    throw WatchFailure(message: "watch boundary failed: \(description)\n\(try watchOutput(output))")
}

private struct WatchFailure: Error { let message: String }

private func watchSend(_ value: String, fifo: URL) throws {
    let fd = unsafe open(fifo.path, O_WRONLY | O_NONBLOCK)
    guard fd >= 0 else { throw WatchFailure(message: "owned app FIFO has no live reader") }
    defer { close(fd) }
    guard fcntl(fd, F_SETNOSIGPIPE, 1) == 0 else {
        throw WatchFailure(message: "cannot suppress FIFO SIGPIPE")
    }
    let data = Array((value + "\n").utf8)
    let count = unsafe data.withUnsafeBytes { unsafe write(fd, $0.baseAddress, $0.count) }
    guard count == data.count else { throw WatchFailure(message: "short write to owned app FIFO") }
}

private func watchStop(_ process: Process) async -> Bool {
    guard process.isRunning else { return true }
    process.terminate()
    let deadline = ContinuousClock.now + .seconds(3)
    while process.isRunning && ContinuousClock.now < deadline {
        try? await Task.sleep(for: .milliseconds(40))
    }
    // Foundation may reap asynchronously. Never signal a numeric PID after
    // inspecting isRunning: that PID could have been reused in between.
    guard !process.isRunning else { return false }
    process.waitUntilExit()
    return true
}
