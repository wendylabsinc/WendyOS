import Foundation
import Subprocess
import Testing

@testable import WendyE2ETesting

@Suite
struct `machine` {
    @Test
    func `creates remote machine metadata`() {
        let machine = Machine(
            name: "SSH",
            user: "ai",
            address: "example.local",
            workingDirectory: "~/wendy-agent"
        )

        #expect(machine.name == "SSH")
        #expect(machine.isLocal == false)
        #expect(machine.user == "ai")
        #expect(machine.address == "example.local")
        #expect(machine.workingDirectory == "~/wendy-agent")
        #expect(machine.id == "ai@example.local:~/wendy-agent")
        #expect(machine.description == "ai@example.local:~/wendy-agent")
    }

    @Test
    func `defaults remote machine to user home directory`() {
        let machine = Machine(name: "SSH", user: "ai", address: "example.local")

        #expect(machine.isLocal == false)
        #expect(machine.user == "ai")
        #expect(machine.address == "example.local")
        #expect(machine.workingDirectory == nil)
        #expect(machine.id == "ai@example.local:~")
        #expect(machine.description == "ai@example.local:~")
    }

    @Test
    func `defaults machine to current host and directory`() {
        let machine = Machine(name: "Local")

        #expect(machine.isLocal)
        #expect(machine.user == nil)
        #expect(!machine.address.isEmpty)
        #expect(machine.workingDirectory == FileManager.default.currentDirectoryPath)
        #expect(machine.id == "\(machine.address):\(FileManager.default.currentDirectoryPath)")
        #expect(
            machine.description == "\(machine.address):\(FileManager.default.currentDirectoryPath)"
        )
    }

    @Test
    func `declares current runner machine`() {
        #expect(Machine.current.id == "current")
        #expect(Machine.current.name == "Current")
        #expect(Machine.current.tags == [.runner])
        #expect(Machine.current.isLocal)
        #expect(Machine.current.user == nil)
        #expect(!Machine.current.address.isEmpty)
        #expect(Machine.current.workingDirectory == FileManager.default.currentDirectoryPath)
    }

    @Test
    func `stores shell environment variables`() {
        let machine = Machine(
            name: "Local",
            env: [
                "HOME": "/tmp/wendy-e2e-home",
                "PATH": "/tmp/wendy-e2e-bin:$PATH",
                "WENDY_ANALYTICS": "false",
            ]
        )

        #expect(machine.isLocal)
        #expect(machine.env["HOME"] == "/tmp/wendy-e2e-home")
        #expect(machine.env["PATH"] == "/tmp/wendy-e2e-bin:$PATH")
        #expect(machine.env["WENDY_ANALYTICS"] == "false")
    }

    @Test
    func `stores a routable address`() {
        let machine = Machine(name: "Remote", address: "192.168.64.2")

        #expect(machine.isLocal == false)
        #expect(machine.address == "192.168.64.2")
    }
}

@Suite
struct `session` {
    @Test
    func `runs a simple shell command`() async throws {
        let session = try await Session.begin(for: Machine(name: "Local"))
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
    func `runs local commands in working directory`() async throws {
        let directory = FileManager.default.temporaryDirectory
            .appendingPathComponent("machine-local-" + UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }

        let machine = Machine(name: "Local", workingDirectory: directory.path)
        let session = try await Session.begin(for: machine)
        try await session.sh("touch local.txt")

        #expect(!session.machine.address.isEmpty)
        #expect(session.machine.workingDirectory == directory.path)
        #expect(session.description == "\(session.machine.address):\(directory.path)")
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

        let machine = Machine(
            name: "Local",
            workingDirectory: directory.path,
            env: [
                "HOME": homeDirectory.path,
                "PATH": "\(binDirectory.path):$PATH",
                "WENDY_ANALYTICS": "false",
            ]
        )
        let session = try await Session.begin(for: machine)

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
            await #expect(throws: MachineError.self) {
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

        let machine = Machine(name: "Local", workingDirectory: directory.path)
        let session = try await Session.begin(for: machine)
        try await session.command("touch builder.txt").run()

        #expect(FileManager.default.fileExists(atPath: directory.path + "/builder.txt"))
    }

    @Test
    func `command builder callback receives command output`() async throws {
        let session = try await Session.begin(for: Machine(name: "Local"))

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

        let machine = Machine(name: "Local", workingDirectory: directory.path)
        let session = try await Session.begin(for: machine)
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

        let machine = Machine(name: "Local", workingDirectory: directory.path)
        let session = try await Session.begin(for: machine)
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
        let session = try await Session.begin(for: Machine(name: "Local"))

        await #expect(throws: MachineError.self) {
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
            _ = try Recorder(filePath: #filePath, function: "init()", line: #line)
        }
    }

    @Test
    func `dasherizes command record file names`() {
        #expect(Recorder.slug("buildAgent(with:)") == "build-agent-with")
        #expect(Recorder.slug("URLParserTests") == "url-parser-tests")
        #expect(
            Recorder.slug("'--json' reports a missing device") == "json-reports-a-missing-device"
        )
        #expect(
            Recorder.recordingFileName(
                filePath: "/tmp/WendyDeviceInfoTests.swift",
                suite: "'wendy device info'",
                testName: "'--device' selects an explicit device"
            )
                == "wendy-device-info.device-selects-an-explicit-device.md"
        )
    }

    @Test
    func `with begins sessions and ends them after the body`() async throws {
        try await Session.with(Machine(name: "Local"), Machine(name: "Local")) { first, second in
            #expect(first.machine.name == "Local")
            #expect(second.machine.name == "Local")
        }
    }

    private static func withTemporarySession<Result>(
        _ body: (Session, URL) async throws -> Result
    ) async throws -> Result {
        let directory = try Self.makeTemporaryDirectory()
        let session = try await Session.begin(
            for: Machine(name: "Local", workingDirectory: directory.path)
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
