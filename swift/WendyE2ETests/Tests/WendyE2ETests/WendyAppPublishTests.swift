import Foundation
import Testing
import WendyE2ETesting

@Suite(.serialized)
struct `'wendy app publish'` {
    /**
     Publishes a tagged ARM64 release and reads its actual catalog and encrypted
     artifact metadata. The returned digest covers the embedded app definition.
     No deployment or owned device application is created.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `publishes a release without deploying it`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            try await app.create(cli)
            let before = try await app.inventory(cli)
            let release = try await app.publish(cli)
            #expect(!release.id.isEmpty)
            #expect(release.digest.hasPrefix("sha256:"))
            #expect(release.digest.count == 71)
            let after = try await app.inventory(cli)
            #expect(after == before)
            try await app.refuse(
                cli,
                "wendy app deployment show \(app.deployment) \(app.cloudFlag)",
                guidance: "no deployment"
            )
        }
    }

    /**
     A local project cannot publish under a different app id. Refusal occurs
     before Cloud authentication, image building or upload.
     */
    @Test(.enabled(if: appsDeploymentLocalTestsEnabled))
    func `refuses an app id that differs from the project`() async throws {
        try await CLIAndAgentScenario().run(authenticated: false) { cli, _ in
            try await cli.sh(
                #"printf '%s' '{"appId":"sh.wendy.e2e.original","env":{}}' > wendy.json"#
            )
            try await cli.sh("wendy app publish --app sh.wendy.e2e.other --tag 1.0.0 </dev/null") {
                result in
                #expect(result.status.isFailure)
                #expect(result.readableStderr.contains("--app must match appId"))
            }
        }
    }

    /**
     An invalid requiredEnv definition is rejected before upload. Required
     names must exist in env; an invalid project must never become a release.
     */
    @Test(.enabled(if: appsDeploymentLocalTestsEnabled))
    func `validates required settings before publishing`() async throws {
        try await CLIAndAgentScenario().run(authenticated: false) { cli, _ in
            try await cli.sh(
                #"printf '%s' '{"appId":"sh.wendy.e2e.invalid","env":{},"requiredEnv":["MISSING"]}' > wendy.json"#
            )
            try await cli.sh("wendy app publish --tag 1.0.0 </dev/null") { result in
                #expect(result.status.isFailure)
                #expect(result.readableStderr.contains("requiredEnv"))
            }
        }
    }

    /**
     The current release path accepts one container app. A project declaring
     several services refuses before authentication, building or upload and
     explains how to split it into separately deployable apps.
     */
    @Test(.enabled(if: appsDeploymentLocalTestsEnabled))
    func `refuses unsupported multi-service projects before uploading`() async throws {
        try await CLIAndAgentScenario().run(authenticated: false) { cli, _ in
            try await cli.sh(
                #"printf '%s' '{"appId":"sh.wendy.e2e.multi","services":{"api":{"context":"."},"worker":{"context":"."}}}' > wendy.json"#
            )
            try await cli.sh("wendy app publish --tag 1.0.0 </dev/null") { result in
                #expect(result.status.isFailure)
                #expect(result.readableStderr.contains("several services cannot be deployed yet"))
            }
        }
    }

    /**
     A scripted publish cannot silently register a missing Cloud app. After
     explicit registration, the same project can publish normally.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `requires explicit registration when the Cloud app is missing`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            let layout = try AppsDeploymentFixture.setting("WENDY_E2E_APPS_LAYOUT_A")
            try await app.refuse(
                cli,
                "wendy app publish --tag 1.0.0 --oci-layout \(AppsDeploymentFixture.quote(layout)) \(app.cloudFlag)",
                guidance: "wendy app create"
            )
            try await app.create(cli)
            _ = try await app.publish(cli)
        }
    }

    /**
     Publish --tag 1.0.0 --oci-layout A --platform linux/arm64; read the release,
     GetArtifact and app-filtered ListArtifacts, following all pages. The receipt's
     digest matches the returned OCI manifest with embedded app definition,
     encrypted descriptors and complete upload. Defaults and required names
     match wendy.json. No deployment exists. Allowed pulls need no audit row.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `records the encrypted artifact and release metadata`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            try await app.create(cli)
            let release = try await app.publish(cli)
            let artifact = try await app.requireArtifact(cli, release: release)
            try await app.requireArtifactListing(
                cli,
                release: release,
                blobCount: artifact.manifest.blobs.count
            )
            let catalog = try await app.catalog(cli)
            #expect(catalog.deployments?.isEmpty == true)
        }
    }

    /**
     An unavailable registry produces an actionable command failure and never
     deploys an app. A wrapper timeout is not accepted as that command result.
     The owned app and any partially created release remain in the cleanup ledger.
     */
    @Test(
        .enabled(
            if: appsDeploymentTestsEnabled
                && ProcessInfo.processInfo.environment["WENDY_E2E_APPS_FAILURES"] == "true"
        )
    )
    func `reports an unavailable registry without deploying anything`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            try await app.create(cli)
            let layout = try AppsDeploymentFixture.setting("WENDY_E2E_APPS_LAYOUT_A")
            // Loopback discard port is a benign unavailable-service fixture;
            // no real service is stopped or sent malformed requests.
            let result = try await app.command(
                cli,
                "env WENDY_REGISTRY=127.0.0.1:1 wendy app publish --tag 1.0.0 --oci-layout \(AppsDeploymentFixture.quote(layout)) \(app.cloudFlag)"
            )
            try #require(!result.status.description.contains("124"))
            try #require(!result.stderr.contains("E2E command timed out"))
            #expect(result.status.isFailure)
            #expect(
                result.readableStderr.lowercased().contains("registry")
                    || result.readableStderr.lowercased().contains("connect")
            )
            let catalog = try await app.catalog(cli)
            #expect(catalog.deployments?.isEmpty == true)
            let inventory = try await app.inventory(cli)
            #expect(!inventory.contains { $0.name == app.app })
        }
    }

}
