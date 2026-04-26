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
        #expect(
            availability.failureMessage?.contains("Could not find \(executable) executable") == true
        )
        #expect(availability.failureMessage?.contains("/tmp/\(executable)") == true)
        #expect(
            availability.failureMessage?.contains(
                "/Applications/Docker.app/Contents/Resources/bin/\(executable)"
            ) == true
        )
        #expect(resolution.resolvedPath == nil)
        #expect(resolution.searchedPaths.contains("/tmp/\(executable)"))
        #expect(
            resolution.searchedPaths.contains(
                "/Applications/Docker.app/Contents/Resources/bin/\(executable)"
            )
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
