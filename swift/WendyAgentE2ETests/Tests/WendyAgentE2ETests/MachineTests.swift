import Foundation
import Subprocess
import Testing

@testable import WendyAgentE2E

struct MachineTests {
    @Test("creates SSH machine")
    func createsSSHMachine() throws {
        let machine = try Machine(ssh: "ai@example.local", path: "~/wendy-agent")

        #expect(machine.sshTarget == "ai@example.local")
        #expect(machine.baseDirectory == "~/wendy-agent")
        #expect(machine.description == "ai@example.local:~/wendy-agent")
    }

    @Test("runs commands over separate SSH invocations")
    func runsCommandsOverSeparateSSHInvocations() async throws {
        try await Self.withFixtureMachine { machine, fixture in
            try await machine.run("touch first.txt")
            try await machine.run("touch second.txt")

            #expect(FileManager.default.fileExists(atPath: fixture.remoteRoot.path + "/first.txt"))
            #expect(FileManager.default.fileExists(atPath: fixture.remoteRoot.path + "/second.txt"))
            #expect(try fixture.counter(named: "run-count") == 2)
        }
    }

    @Test("closure API streams stdout and stderr")
    func closureAPIStreamsStdoutAndStderr() async throws {
        try await Self.withFixtureMachine { machine, _ in
            let outcome = try await machine.run(
                "printf 'hello\\n'; printf 'oops\\n' >&2"
            ) { _, _, stdout, stderr in
                async let stdoutLines = Self.collectLines(from: stdout)
                async let stderrLines = Self.collectLines(from: stderr)
                return try await (stdoutLines, stderrLines)
            }

            #expect(outcome.terminationStatus.isSuccess)
            #expect(outcome.value.0 == ["hello"])
            #expect(outcome.value.1 == ["oops"])
        }
    }

    @Test("collected output API matches swift-subprocess style")
    func collectedOutputAPIMatchesSwiftSubprocessStyle() async throws {
        try await Self.withFixtureMachine { machine, _ in
            let record = try await machine.run(
                "printf 'hello'",
                output: .string(limit: .max),
                error: .string(limit: .max)
            )

            #expect(record.terminationStatus.isSuccess)
            #expect(record.standardOutput == "hello")
            #expect(record.standardError == "")
        }
    }

    @Test("simple run throws when the remote command exits non-zero")
    func simpleRunThrowsOnNonZeroExit() async throws {
        try await Self.withFixtureMachine { machine, _ in
            await #expect(throws: MachineError.self) {
                try await machine.run("exit 7")
            }
            return ()
        }
    }

    private static func collectLines(from sequence: AsyncBufferSequence) async throws -> [String] {
        var lines: [String] = []
        for try await line in sequence.lines() {
            lines.append(line.trimmingCharacters(in: .newlines))
        }
        return lines
    }

    private static func withFixtureMachine<Result>(
        _ body: (Machine, SSHFixture) async throws -> Result
    ) async throws -> Result {
        let fixture = try SSHFixture()
        let machine = Machine(
            sshTarget: "ai@example.local",
            baseDirectory: fixture.remoteRoot.path,
            sshExecutable: fixture.sshScript.path
        )

        defer { fixture.remove() }
        return try await body(machine, fixture)
    }
}

private struct SSHFixture {
    let root: URL
    let remoteRoot: URL
    let sshScript: URL

    init() throws {
        self.root = FileManager.default.temporaryDirectory
            .appendingPathComponent("machine-ssh-" + UUID().uuidString, isDirectory: true)
        self.remoteRoot = self.root.appendingPathComponent("remote", isDirectory: true)
        self.sshScript = self.root.appendingPathComponent("fake-ssh.sh")

        try FileManager.default.createDirectory(at: self.root, withIntermediateDirectories: true)
        try FileManager.default.createDirectory(
            at: self.remoteRoot,
            withIntermediateDirectories: true
        )

        try self.writeFakeSSHScript()
    }

    func remove() {
        try? FileManager.default.removeItem(at: self.root)
    }

    func counter(named name: String) throws -> Int {
        let url = self.root.appendingPathComponent(name)
        guard FileManager.default.fileExists(atPath: url.path) else {
            return 0
        }
        let string = try String(contentsOf: url, encoding: .utf8)
        return Int(string.trimmingCharacters(in: .whitespacesAndNewlines)) ?? 0
    }

    private func writeFakeSSHScript() throws {
        let stateDirectory = Self.shellQuote(self.root.path)
        let contents = """
            #!/bin/bash
            set -euo pipefail

            state_dir=\(stateDirectory)
            args=()

            increment() {
              local file="$1"
              local count=0
              if [[ -f "$file" ]]; then
                count=$(<"$file")
              fi
              echo $((count + 1)) > "$file"
            }

            while (($#)); do
              case "$1" in
                -T)
                  shift
                  ;;
                -o)
                  shift 2
                  ;;
                *)
                  args+=("$1")
                  shift
                  ;;
              esac
            done

            command="${args[1]:-}"
            run_count="$state_dir/run-count"

            increment "$run_count"
            printf '%s\n' "$command" >> "$state_dir/commands.log"
            exec /bin/bash -lc "$command"
            """

        try contents.write(to: self.sshScript, atomically: true, encoding: .utf8)
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o755],
            ofItemAtPath: self.sshScript.path
        )
    }

    private static func shellQuote(_ value: String) -> String {
        "'" + value.replacingOccurrences(of: "'", with: "'\\''") + "'"
    }
}
