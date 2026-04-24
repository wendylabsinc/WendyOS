import Foundation
import Testing

@testable import WendyAgentCore

struct DockerCLITests {
    @Test("checkAvailability returns false when the docker probe times out")
    func checkAvailabilityReturnsFalseWhenProbeTimesOut() async throws {
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

        let availability = await docker.checkAvailability()

        #expect(availability.isAvailable == false)
        #expect(availability.failureMessage?.contains("timed out") == true)
    }

    @Test("checkAvailability returns true when the docker probe completes")
    func checkAvailabilityReturnsTrueWhenProbeCompletes() async throws {
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

        let availability = await docker.checkAvailability()

        #expect(availability.isAvailable == true)
        #expect(availability.failureMessage == nil)
    }

    @Test("checkAvailability reports the paths searched when docker is missing")
    func checkAvailabilityReportsSearchedPathsWhenDockerIsMissing() async throws {
        let executable = "missing-docker-\(UUID().uuidString)"
        let docker = DockerCLI(
            executable: executable,
            startupCommandTimeout: .milliseconds(100),
            environment: ["PATH": "/tmp:/bin"]
        )

        let availability = await docker.checkAvailability()
        let resolution = docker.resolveExecutableForTesting()

        #expect(availability.isAvailable == false)
        #expect(availability.failureMessage?.contains("Could not find \(executable) executable") == true)
        #expect(availability.failureMessage?.contains("/tmp/\(executable)") == true)
        #expect(availability.failureMessage?.contains("/Applications/Docker.app/Contents/Resources/bin/\(executable)") == true)
        #expect(resolution.resolvedPath == nil)
        #expect(resolution.searchedPaths.contains("/tmp/\(executable)"))
        #expect(
            resolution.searchedPaths.contains(
                "/Applications/Docker.app/Contents/Resources/bin/\(executable)"
            )
        )
    }

    @Test("docker commands inherit a PATH that includes Docker helper binaries")
    func dockerCommandsInheritAPathThatIncludesDockerHelperBinaries() {
        let docker = DockerCLI(
            executable: "/Applications/Docker.app/Contents/Resources/bin/docker",
            environment: ["PATH": "/usr/bin:/bin"]
        )

        let environment = docker.processEnvironmentForTesting(
            resolvedExecutable: "/Applications/Docker.app/Contents/Resources/bin/docker"
        )
        let path = environment["PATH"] ?? ""

        #expect(path.contains("/Applications/Docker.app/Contents/Resources/bin"))
        #expect(path.contains("/usr/bin"))
        #expect(path.contains("/bin"))
    }

    @Test("DockerContainerBackend rewrites loopback registry hosts to 127.0.0.1")
    func dockerBackendRewritesLoopbackRegistryHosts() {
        #expect(
            DockerContainerBackend.rewriteLoopbackRegistryHostForTesting(
                "localhost:5555/helloworld:latest"
            ) == "127.0.0.1:5555/helloworld:latest"
        )
        #expect(
            DockerContainerBackend.rewriteLoopbackRegistryHostForTesting(
                "[::1]:5555/helloworld:latest"
            ) == "127.0.0.1:5555/helloworld:latest"
        )
        #expect(
            DockerContainerBackend.rewriteLoopbackRegistryHostForTesting(
                "localhost/library/alpine:latest"
            ) == "127.0.0.1/library/alpine:latest"
        )
    }

    @Test("DockerContainerBackend leaves non-loopback registry hosts unchanged")
    func dockerBackendLeavesNonLoopbackRegistryHostsUnchanged() {
        #expect(
            DockerContainerBackend.rewriteLoopbackRegistryHostForTesting(
                "host.docker.internal:5555/helloworld:latest"
            ) == "host.docker.internal:5555/helloworld:latest"
        )
        #expect(
            DockerContainerBackend.rewriteLoopbackRegistryHostForTesting(
                "ghcr.io/wendylabsinc/helloworld:latest"
            ) == "ghcr.io/wendylabsinc/helloworld:latest"
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
