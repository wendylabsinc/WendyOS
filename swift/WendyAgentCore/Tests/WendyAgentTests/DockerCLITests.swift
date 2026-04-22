import Foundation
import Testing

@testable import WendyAgentCore

struct DockerCLITests {
    @Test("checkAvailable returns false when the docker probe times out")
    func checkAvailableReturnsFalseWhenProbeTimesOut() async throws {
        let scriptURL = try Self.makeExecutableScript(
            name: "fake-docker-timeout.sh",
            contents: """
                #!/bin/sh
                sleep 1
                exit 0
                """
        )
        defer { try? FileManager.default.removeItem(at: scriptURL.deletingLastPathComponent()) }

        let docker = DockerCLI(
            executable: scriptURL.path,
            startupCommandTimeout: .milliseconds(100)
        )

        let available = await docker.checkAvailable()

        #expect(available == false)
    }

    @Test("checkAvailable returns true when the docker probe completes")
    func checkAvailableReturnsTrueWhenProbeCompletes() async throws {
        let scriptURL = try Self.makeExecutableScript(
            name: "fake-docker-ok.sh",
            contents: """
                #!/bin/sh
                echo 27.0.1
                exit 0
                """
        )
        defer { try? FileManager.default.removeItem(at: scriptURL.deletingLastPathComponent()) }

        let docker = DockerCLI(
            executable: scriptURL.path,
            startupCommandTimeout: .seconds(2)
        )

        let available = await docker.checkAvailable()

        #expect(available == true)
    }

    @Test("a non-zero exit surfaces the exit status and stderr in DockerError")
    func nonZeroExitSurfacesStderr() async throws {
        let scriptURL = try Self.makeExecutableScript(
            name: "fake-docker-fail.sh",
            contents: """
                #!/bin/sh
                echo "something on stdout"
                echo "boom" 1>&2
                exit 7
                """
        )
        defer { try? FileManager.default.removeItem(at: scriptURL.deletingLastPathComponent()) }

        let docker = DockerCLI(executable: scriptURL.path)

        do {
            _ = try await docker.pull(image: "example/image:tag")
            Issue.record("expected pull to throw")
        } catch let error as DockerError {
            guard case .commandFailed(_, _, let status, let stderr) = error else {
                Issue.record("expected commandFailed, got \(error)")
                return
            }
            #expect(status == 7)
            #expect(stderr == "boom")
        }
    }

    @Test("stdout is drained incrementally and large outputs do not deadlock")
    func largeStdoutIsDrainedWithoutDeadlock() async throws {
        // Write well beyond the OS pipe buffer (typically 64 KiB on macOS) so
        // that a naive "read after termination" implementation would deadlock.
        // We still stay comfortably under the collected-output cap.
        let chunkCount = 2048  // 2048 lines * ~1 KiB ≈ 2 MiB
        let scriptURL = try Self.makeExecutableScript(
            name: "fake-docker-large.sh",
            contents: """
                #!/bin/sh
                i=0
                while [ $i -lt \(chunkCount) ]; do
                    # 1 KiB of 'a' per line
                    awk 'BEGIN{ while(c++ < 1023) printf "a"; print "" }'
                    i=$((i+1))
                done
                exit 0
                """
        )
        defer { try? FileManager.default.removeItem(at: scriptURL.deletingLastPathComponent()) }

        let docker = DockerCLI(executable: scriptURL.path)

        let output = try await docker.pull(image: "example/image:tag")

        // 1024 bytes per line (1023 'a' plus a newline), times chunkCount,
        // minus the trailing newline that trimmingCharacters removes.
        #expect(output.count == chunkCount * 1024 - 1)
    }

    private static func makeExecutableScript(name: String, contents: String) throws -> URL {
        let directoryURL = FileManager.default.temporaryDirectory
            .appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: directoryURL, withIntermediateDirectories: true)

        let scriptURL = directoryURL.appendingPathComponent(name)
        try contents.write(to: scriptURL, atomically: true, encoding: .utf8)
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o755],
            ofItemAtPath: scriptURL.path
        )
        return scriptURL
    }
}
