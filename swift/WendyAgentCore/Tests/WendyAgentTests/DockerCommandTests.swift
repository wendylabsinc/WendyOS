import Foundation
import GRPCCore
import Testing

@testable import WendyAgentCore

/// Exercises the private docker invocation pipeline inside `ContainerService`
/// through its `runDockerForTesting` shim. These used to live in a separate
/// `DockerCLITests` file targeting the now-deleted `DockerCLI` type; the
/// behaviors we care about (timeout, non-zero exit surfacing stderr, streamed
/// output without deadlock) are the same, just exercised at the new layer.
struct DockerCommandTests {
    @Test("docker invocation surfaces a timeout when the command sleeps past the deadline")
    func invocationTimesOut() async throws {
        let scriptURL = try Self.makeExecutableScript(
            name: "fake-docker-timeout.sh",
            contents: """
                #!/bin/sh
                sleep 1
                exit 0
                """
        )
        defer { try? FileManager.default.removeItem(at: scriptURL.deletingLastPathComponent()) }

        let service = Self.makeService(dockerExecutable: scriptURL.path)

        do {
            _ = try await service.runDockerForTesting(
                ["version"],
                timeout: .milliseconds(100)
            )
            Issue.record("expected timeout to throw")
        } catch let error as RPCError {
            #expect(error.code == .deadlineExceeded)
            #expect(error.message.contains("timed out"))
        }
    }

    @Test("docker invocation returns stdout when the command completes successfully")
    func invocationSucceeds() async throws {
        let scriptURL = try Self.makeExecutableScript(
            name: "fake-docker-ok.sh",
            contents: """
                #!/bin/sh
                echo 27.0.1
                exit 0
                """
        )
        defer { try? FileManager.default.removeItem(at: scriptURL.deletingLastPathComponent()) }

        let service = Self.makeService(dockerExecutable: scriptURL.path)

        let output = try await service.runDockerForTesting(
            ["version"],
            timeout: .seconds(2)
        )
        #expect(output == "27.0.1")
    }

    @Test("a non-zero exit surfaces the exit status and stderr in the thrown error")
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

        let service = Self.makeService(dockerExecutable: scriptURL.path)

        do {
            _ = try await service.runDockerForTesting(["pull", "example/image:tag"])
            Issue.record("expected runDockerForTesting to throw")
        } catch let error as RPCError {
            #expect(error.code == .internalError)
            #expect(error.message.contains("status 7"))
            #expect(error.message.contains("boom"))
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

        let service = Self.makeService(dockerExecutable: scriptURL.path)

        let output = try await service.runDockerForTesting(["pull", "example/image:tag"])

        // 1024 bytes per line (1023 'a' plus a newline), times chunkCount,
        // minus the trailing newline that trimming removes.
        #expect(output.count == chunkCount * 1024 - 1)
    }

    // MARK: - Helpers

    private static func makeService(dockerExecutable: String) -> ContainerService {
        let tempDirectory = FileManager.default.temporaryDirectory
            .appendingPathComponent("wendy-docker-command-\(UUID().uuidString)", isDirectory: true)
        return ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            stateDirectory: tempDirectory,
            appsBase: tempDirectory.appendingPathComponent("apps"),
            dockerExecutable: dockerExecutable
        )
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
