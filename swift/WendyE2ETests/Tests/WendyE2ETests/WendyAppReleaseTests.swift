import Testing
import WendyE2ETesting

@Suite(.serialized)
struct `'wendy app release'` {
    /**
     Records exactly the supplied tag and digest in the catalog. Unlike publish,
     recording the reference uploads no artifact and creates no deployment.
     The registry returns a missing-artifact refusal for that release.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `records an explicit image reference without running it`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            try await app.create(cli)
            let before = try await app.inventory(cli)
            let digest = "sha256:" + String(repeating: "a", count: 64)
            let result = try await app.succeed(
                cli,
                "wendy app release --tag reference-only --digest \(digest) \(app.cloudFlag)"
            )
            let receipt = result.stdout + result.readableStderr
            #expect(receipt.contains("of \(app.app): reference-only = \(digest)"))
            let after = try await app.inventory(cli)
            #expect(after == before)
            let catalog = try await app.catalog(cli)
            let release = try #require(catalog.releases?.first { $0.name == "reference-only" })
            #expect(release.imageDigest == digest)
            #expect(catalog.releases?.count == 1)
            #expect(catalog.deployments?.isEmpty == true)
            try await app.requireArtifactUnavailable(cli, releaseID: release.id)
        }
    }

    /** A catalog reference requires an explicit tag before authentication. */
    @Test(.enabled(if: appsDeploymentLocalTestsEnabled))
    func `requires a tag for a catalog release`() async throws {
        try await refuseReleaseInput("", guidance: "--tag")
    }

    /** A malformed digest cannot fall back to an implicit latest image. */
    @Test(.enabled(if: appsDeploymentLocalTestsEnabled))
    func `requires a valid digest for a catalog release`() async throws {
        try await refuseReleaseInput("--tag example --digest bad", guidance: "--digest")
    }

    private func refuseReleaseInput(
        _ flags: String,
        guidance: String,
        filePath: String = #filePath,
        function: String = #function,
        line: Int = #line
    ) async throws {
        try await CLIAndAgentScenario().run(
            authenticated: false,
            filePath: filePath,
            function: function,
            line: line
        ) { cli, _ in
            try await cli.sh("wendy app release \(flags) </dev/null") { result in
                #expect(result.status.isFailure)
                #expect(result.readableStderr.contains(guidance))
            }
        }
    }
}
