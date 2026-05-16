import Foundation
import Subprocess
import Testing

@testable import WendyE2ETesting

@Suite
struct `machine` {
    @Test
    func `creates remote machine metadata`() {
        let machine = WendyE2EMachine(
            id: "ssh",
            name: "SSH",
            user: "ai",
            address: "example.local"
        )

        #expect(machine.name == "SSH")
        #expect(machine.isLocal == false)
        #expect(machine.user == "ai")
        #expect(machine.address == "example.local")
        #expect(machine.id == "ssh")
        #expect(machine.description == "ssh")
    }

    @Test
    func `creates remote machine without session state`() {
        let machine = WendyE2EMachine(id: "ssh", name: "SSH", user: "ai", address: "example.local")

        #expect(machine.isLocal == false)
        #expect(machine.user == "ai")
        #expect(machine.address == "example.local")
        #expect(machine.id == "ssh")
        #expect(machine.description == "ssh")
    }

    @Test
    func `defaults machine to current host`() {
        let machine = WendyE2EMachine(id: "local", name: "Local")

        #expect(machine.isLocal)
        #expect(machine.user == nil)
        #expect(!machine.address.isEmpty)
        #expect(machine.id == "local")
        #expect(machine.description == "local")
    }

    @Test
    func `declares current runner machine`() {
        #expect(WendyE2EMachine.current.id == "current")
        #expect(WendyE2EMachine.current.name == "Current")
        #expect(WendyE2EMachine.current.tags == [.runner])
        #expect(WendyE2EMachine.current.isLocal)
        #expect(WendyE2EMachine.current.user == nil)
        #expect(!WendyE2EMachine.current.address.isEmpty)
    }

    @Test
    func `stores a routable address`() {
        let machine = WendyE2EMachine(id: "remote", name: "Remote", address: "192.168.64.2")

        #expect(machine.isLocal == false)
        #expect(machine.address == "192.168.64.2")
    }
}

@Suite
struct `session` {
    @Test
    func `runs a simple shell command`() async throws {
        let session = try await WendyE2ESession.begin(
            for: WendyE2EMachine(id: "local", name: "Local")
        )
        let record = try await session.sh(
            "printf 'wendy-machine-smoke'",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )

        #expect(record.terminationStatus.isSuccess)
        #expect(record.standardOutput == "wendy-machine-smoke")
        #expect(record.standardError == "")
    }

    @Test
    func `returns a rich shell result`() async throws {
        let session = try await WendyE2ESession.begin(
            for: WendyE2EMachine(id: "local", name: "Local")
        )
        let result = try await session.sh(.posix, "printf 'wendy-machine-smoke'")

        #expect(result.dialect == .posix)
        #expect(result.isSuccess)
        #expect(!result.isFailure)
        #expect(result.stdout == "wendy-machine-smoke")
        #expect(result.stderr == "")
        #expect(result.normalizedStdout == "wendy-machine-smoke")
    }

    @Test
    func `runs a simple PowerShell command when PowerShell is available`() async throws {
        guard Self.hasPowerShell else {
            return
        }

        let session = try await WendyE2ESession.begin(
            for: WendyE2EMachine(id: "local", name: "Local")
        )
        let record = try await session.ps(
            "Write-Output 'wendy-machine-smoke'",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )

        #expect(record.terminationStatus.isSuccess)
        #expect(
            record.standardOutput?.replacingOccurrences(of: "\r\n", with: "\n")
                == "wendy-machine-smoke\n"
        )
        #expect(record.standardError == "")
    }

    @Test
    func `runs local commands in working directory`() async throws {
        let directory = FileManager.default.temporaryDirectory
            .appendingPathComponent("machine-local-" + UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }

        let machine = WendyE2EMachine(id: "local", name: "Local")
        let session = try await WendyE2ESession.begin(
            for: machine,
            workingDirectory: directory.path
        )
        try await session.sh("touch local.txt")

        #expect(!session.machine.address.isEmpty)
        #expect(session.workingDirectory == directory.path)
        #expect(session.description == "local")
        #expect(FileManager.default.fileExists(atPath: directory.path + "/local.txt"))
    }

    @Test
    func `sets environment variables before running local commands`() async throws {
        let directory = FileManager.default.temporaryDirectory
            .appendingPathComponent("machine-env-" + UUID().uuidString, isDirectory: true)
        let binDirectory = directory.appendingPathComponent("bin", isDirectory: true)
        let homeDirectory = directory.appendingPathComponent("home", isDirectory: true)
        try FileManager.default.createDirectory(at: binDirectory, withIntermediateDirectories: true)
        try FileManager.default.createDirectory(
            at: homeDirectory,
            withIntermediateDirectories: true
        )
        defer { try? FileManager.default.removeItem(at: directory) }

        let wendy = binDirectory.appendingPathComponent("wendy")
        try """
        #!/bin/sh
        printf 'HOME=%s\n' "$HOME"
        printf 'WENDY_ANALYTICS=%s\n' "$WENDY_ANALYTICS"
        """.write(to: wendy, atomically: true, encoding: .utf8)
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o755],
            ofItemAtPath: wendy.path
        )

        let machine = WendyE2EMachine(id: "local", name: "Local")
        let session = try await WendyE2ESession.begin(
            for: machine,
            workingDirectory: directory.path,
            env: [
                "HOME": homeDirectory.path,
                "PATH": "\(binDirectory.path):$PATH",
                "WENDY_ANALYTICS": "false",
            ]
        )

        let record = try await session.sh(
            "wendy",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )

        #expect(record.terminationStatus.isSuccess)
        #expect(record.standardOutput == "HOME=\(homeDirectory.path)\nWENDY_ANALYTICS=false\n")
        #expect(record.standardError == "")
    }

    @Test
    func `computes macOS wendy cache directory`() async throws {
        let session = try await WendyE2ESession.begin(
            for: WendyE2EMachine(id: "mac", name: "Mac", os: .macOS),
            env: ["HOME": "/tmp/e2e-home"]
        )

        #expect(session.wendyCacheDirectory == "/tmp/e2e-home/Library/Caches/wendy")
    }

    @Test
    func `computes Linux wendy cache directory`() async throws {
        let session = try await WendyE2ESession.begin(
            for: WendyE2EMachine(id: "linux", name: "Linux", os: .linux),
            env: ["HOME": "/tmp/e2e-home"]
        )

        #expect(session.wendyCacheDirectory == "/tmp/e2e-home/.cache/wendy")
    }

    @Test
    func `uses XDG cache home for Linux wendy cache directory`() async throws {
        let session = try await WendyE2ESession.begin(
            for: WendyE2EMachine(id: "linux", name: "Linux", os: .linux),
            env: [
                "HOME": "/tmp/e2e-home",
                "XDG_CACHE_HOME": "/tmp/e2e-cache",
            ]
        )

        #expect(session.wendyCacheDirectory == "/tmp/e2e-cache/wendy")
    }

    @Test
    func `computes WendyOS wendy cache directory`() async throws {
        let session = try await WendyE2ESession.begin(
            for: WendyE2EMachine(id: "wendyos", name: "WendyOS", os: .wendyOS),
            env: ["HOME": "/tmp/e2e-home"]
        )

        #expect(session.wendyCacheDirectory == "/tmp/e2e-home/.cache/wendy")
    }

    @Test
    func `creates session directories lazily before running commands`() async throws {
        let directory = FileManager.default.temporaryDirectory
            .appendingPathComponent("machine-lazy-" + UUID().uuidString, isDirectory: true)
        let homeDirectory = directory.appendingPathComponent("home", isDirectory: true)
        let temporaryDirectory = directory.appendingPathComponent("tmp", isDirectory: true)
        let workingDirectory = homeDirectory.appendingPathComponent("work", isDirectory: true)
        defer { try? FileManager.default.removeItem(at: directory) }

        let session = try await WendyE2ESession.begin(
            for: WendyE2EMachine(id: "local", name: "Local"),
            workingDirectory: workingDirectory.path,
            env: [
                "HOME": homeDirectory.path,
                "TMPDIR": temporaryDirectory.path,
            ]
        )

        try await session.sh("printf '%s' \"$PWD\"") { standardOutput, _ in
            #expect(standardOutput == workingDirectory.path)
        }

        #expect(FileManager.default.fileExists(atPath: homeDirectory.path))
        #expect(FileManager.default.fileExists(atPath: temporaryDirectory.path))
        #expect(FileManager.default.fileExists(atPath: workingDirectory.path))
    }

    @Test
    func `resets session directories only before the first command`() async throws {
        let directory = FileManager.default.temporaryDirectory
            .appendingPathComponent("machine-reset-" + UUID().uuidString, isDirectory: true)
        let homeDirectory = directory.appendingPathComponent("home", isDirectory: true)
        let temporaryDirectory = directory.appendingPathComponent("tmp", isDirectory: true)
        let workingDirectory = homeDirectory.appendingPathComponent("work", isDirectory: true)
        try FileManager.default.createDirectory(
            at: homeDirectory,
            withIntermediateDirectories: true
        )
        try "stale".write(
            to: homeDirectory.appendingPathComponent("stale"),
            atomically: true,
            encoding: .utf8
        )
        defer { try? FileManager.default.removeItem(at: directory) }

        let session = try await WendyE2ESession.begin(
            for: WendyE2EMachine(id: "local", name: "Local"),
            workingDirectory: workingDirectory.path,
            env: [
                "HOME": homeDirectory.path,
                "TMPDIR": temporaryDirectory.path,
            ],
            resetDirectoriesOnFirstCommand: true
        )

        try await session.sh("test ! -e \"$HOME/stale\" && touch \"$HOME/fresh\"")
        try await session.sh("test -e \"$HOME/fresh\"")
    }

    @Test
    func `runs commands locally by default`() async throws {
        try await Self.withTemporarySession { session, directory in
            try await session.sh("touch first.txt")
            try await session.sh("touch second.txt")

            #expect(FileManager.default.fileExists(atPath: directory.path + "/first.txt"))
            #expect(FileManager.default.fileExists(atPath: directory.path + "/second.txt"))
        }
    }

    @Test
    func `collected output API matches swift-subprocess style`() async throws {
        try await Self.withTemporarySession { session, _ in
            let record = try await session.sh(
                "printf 'hello'",
                output: .string(limit: .max),
                error: .string(limit: .max)
            )

            #expect(record.terminationStatus.isSuccess)
            #expect(record.standardOutput == "hello")
            #expect(record.standardError == "")
        }
    }

    @Test
    func `collected output callback receives command output`() async throws {
        try await Self.withTemporarySession { session, _ in
            try await session.sh("printf 'hello'; printf 'oops' >&2") {
                standardOutput,
                standardError in
                #expect(standardOutput == "hello")
                #expect(standardError == "oops")
                #expect(standardOutput.contains(/he.*o/))
            }
        }
    }

    @Test
    func `simple shell command throws when the command exits non-zero`() async throws {
        try await Self.withTemporarySession { session, _ in
            await #expect(throws: WendyE2EMachineError.self) {
                try await session.sh("exit 7")
            }
            return ()
        }
    }

    @Test
    func `command builder runs command`() async throws {
        let directory = FileManager.default.temporaryDirectory
            .appendingPathComponent("machine-command-" + UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }

        let machine = WendyE2EMachine(id: "local", name: "Local")
        let session = try await WendyE2ESession.begin(
            for: machine,
            workingDirectory: directory.path
        )
        try await session.command("touch builder.txt").run()

        #expect(FileManager.default.fileExists(atPath: directory.path + "/builder.txt"))
    }

    @Test
    func `command builder callback receives command output`() async throws {
        let session = try await WendyE2ESession.begin(
            for: WendyE2EMachine(id: "local", name: "Local")
        )

        try await session.command("printf 'hello'; printf 'oops' >&2").run {
            standardOutput,
            standardError in
            #expect(standardOutput == "hello")
            #expect(standardError == "oops")
        }
    }

    @Test
    func `poll retries command until it succeeds`() async throws {
        let directory = FileManager.default.temporaryDirectory
            .appendingPathComponent("machine-poll-" + UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }

        let machine = WendyE2EMachine(id: "local", name: "Local")
        let session = try await WendyE2ESession.begin(
            for: machine,
            workingDirectory: directory.path
        )
        try await session
            .command(
                """
                count=$(cat counter.txt 2>/dev/null || echo 0)
                count=$((count + 1))
                echo "$count" > counter.txt
                test "$count" -ge 3
                """
            )
            .poll(until: .success, step: .milliseconds(10), timeout: .seconds(2))
            .run()

        let count = try String(
            contentsOf: directory.appendingPathComponent("counter.txt"),
            encoding: .utf8
        ).trimmingCharacters(in: .whitespacesAndNewlines)
        #expect(count == "3")
    }

    @Test
    func `poll callback receives output from successful attempt`() async throws {
        let directory = FileManager.default.temporaryDirectory
            .appendingPathComponent("machine-poll-output-" + UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }

        let machine = WendyE2EMachine(id: "local", name: "Local")
        let session = try await WendyE2ESession.begin(
            for: machine,
            workingDirectory: directory.path
        )
        try await session
            .command(
                """
                count=$(cat counter.txt 2>/dev/null || echo 0)
                count=$((count + 1))
                echo "$count" > counter.txt
                echo "stdout:$count"
                echo "stderr:$count" >&2
                test "$count" -ge 3
                """
            )
            .poll(until: .success, step: .milliseconds(10), timeout: .seconds(2))
            .run { standardOutput, standardError in
                #expect(standardOutput == "stdout:3\n")
                #expect(standardError == "stderr:3\n")
            }
    }

    @Test
    func `poll throws timeout error with timeout message`() async throws {
        let session = try await WendyE2ESession.begin(
            for: WendyE2EMachine(id: "local", name: "Local")
        )

        await #expect(throws: WendyE2EMachineError.self) {
            try await session
                .command("exit 1")
                .poll(
                    until: .success,
                    step: .milliseconds(10),
                    timeout: .milliseconds(25),
                    timeoutMessage: "command never succeeded"
                )
                .run()
        }
    }

    @Test
    func `requires command recordings to start from test bodies`() {
        #expect(throws: (any Error).self) {
            _ = try WendyE2ERecorder(filePath: #filePath, function: "init()", line: #line)
        }
    }

    @Test
    func `dasherizes command record file names`() {
        #expect(WendyE2ERecorder.slug("buildAgent(with:)") == "build-agent-with")
        #expect(WendyE2ERecorder.slug("URLParserTests") == "url-parser-tests")
        #expect(
            WendyE2ERecorder.slug("'--json' reports a missing device")
                == "json-reports-a-missing-device"
        )
        #expect(
            WendyE2ERecorder.recordingFileName(
                filePath: "/tmp/WendyDeviceInfoTests.swift",
                suite: "'wendy device info'",
                testName: "'--device' selects an explicit device"
            )
                == "wendy-device-info.device-selects-an-explicit-device.md"
        )
    }

    @Test
    func `with begins sessions and ends them after the body`() async throws {
        try await WendyE2ESession.with(
            WendyE2EMachine(id: "first", name: "Local"),
            WendyE2EMachine(id: "second", name: "Local")
        ) { first, second in
            #expect(first.machine.name == "Local")
            #expect(second.machine.name == "Local")
        }
    }

    private static var hasPowerShell: Bool {
        let candidates = ["pwsh", "pwsh.exe", "powershell", "powershell.exe"]
        let environment = ProcessInfo.processInfo.environment
        let pathValue = environment["PATH"] ?? environment["Path"] ?? environment["path"] ?? ""
        let pathSeparator: Character
        #if os(Windows)
            pathSeparator = ";"
        #else
            pathSeparator = ":"
        #endif

        for directory in pathValue.split(separator: pathSeparator, omittingEmptySubsequences: false)
        {
            let directoryPath = directory.isEmpty ? "." : String(directory)
            for candidate in candidates {
                let path = Self.executablePath(directory: directoryPath, candidate: candidate)
                if FileManager.default.isExecutableFile(atPath: path) {
                    return true
                }
            }
        }

        return false
    }

    private static func executablePath(directory: String, candidate: String) -> String {
        if directory.hasSuffix("/") || directory.hasSuffix("\\") {
            return "\(directory)\(candidate)"
        }

        #if os(Windows)
            return "\(directory)\\\(candidate)"
        #else
            return "\(directory)/\(candidate)"
        #endif
    }

    private static func withTemporarySession<Result>(
        _ body: (WendyE2ESession, URL) async throws -> Result
    ) async throws -> Result {
        let directory = try Self.makeTemporaryDirectory()
        let session = try await WendyE2ESession.begin(
            for: WendyE2EMachine(id: "local", name: "Local"),
            workingDirectory: directory.path
        )

        defer { try? FileManager.default.removeItem(at: directory) }
        return try await body(session, directory)
    }

    private static func makeTemporaryDirectory() throws -> URL {
        let directory = FileManager.default.temporaryDirectory
            .appendingPathComponent("machine-local-" + UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        return directory
    }
}
