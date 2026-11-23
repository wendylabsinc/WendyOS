import Foundation
import Testing
import WendyE2ETesting

@Suite
struct `'wendy app promote'` {
    /**
     A new project with no cached direct run explains how to obtain an image
     instead of rebuilding, publishing an unrelated image or deploying one.
     The builder-opted-in case below exercises the cached-image promotion path.
     */
    @Test(.enabled(if: appsDeploymentLocalTestsEnabled))
    func `explains how to proceed when no direct run is cached`() async throws {
        try await CLIAndAgentScenario().run(authenticated: false) { cli, _ in
            try await cli.sh(
                #"printf '%s' '{"appId":"sh.wendy.e2e.uncached","env":{}}' > wendy.json"#
            )
            try await cli.sh(
                "wendy app promote --tag 1.0.0 --device 11111111-1111-4111-8111-111111111111 --name workshop --run --yes </dev/null"
            ) { result in
                #expect(result.status.isFailure)
                #expect(result.readableStderr.contains("Nothing to promote"))
                #expect(result.readableStderr.contains("wendy run"))
                #expect(result.readableStderr.contains("wendy app publish"))
            }
        }
    }

    /**
     Direct-run A with a builder that caches its OCI layout; edit the source
     to B without building again. Promote --tag 1.0.0 --device UUID --name
     workshop --config E2E_ATTEMPT=ATTEMPT --run --keep-trying 7d --yes publishes
     and deploys cached A. Its real endpoint still serves A and the confirmed
     settings, and Cloud now records its deployment. No rebuild may substitute B.
     */
    @Test(
        .enabled(
            if: appsDeploymentTestsEnabled
                && ProcessInfo.processInfo.environment["WENDY_E2E_APPS_BUILDER"] == "true"
        )
    )
    func `publishes the last direct-run image rather than rebuilding edited source`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            try await app.create(cli)
            let source = URL(fileURLWithPath: AppsDeploymentFixture.observerPath)
                .deletingLastPathComponent().deletingLastPathComponent().appendingPathComponent(
                    ".github/ci-tests/apps-deployments"
                ).path
            try await cli.sh(
                "cp \(AppsDeploymentFixture.quote(source))/main.py \(AppsDeploymentFixture.quote(source))/Dockerfile ."
            )
            let configure =
                "import json; p='wendy.json'; c=json.load(open(p)); c['env']['E2E_ATTEMPT']=\(String(reflecting: app.attempt)); json.dump(c,open(p,'w'))"
            try await cli.sh("python3 -c \(AppsDeploymentFixture.quote(configure))")
            try await cli.sh("touch direct-owned-app")
            _ = try await app.succeed(
                cli,
                "wendy --json --device \(AppsDeploymentFixture.quote(app.device)) run --builder docker --stagefile-backend=dockerfile --build-arg APP_VERSION=1.0.0 --detach"
            )
            try await app.requireResponse(cli, version: "1.0.0", unit: "fahrenheit")
            // Edit source without any subsequent build. Promotion must use A.
            let edit =
                #"from pathlib import Path; p=Path('main.py'); p.write_text(p.read_text().replace('VERSION = Path("/app/version.txt").read_text().strip()', 'VERSION = "2.0.0"'))"#
            try await cli.sh("python3 -c \(AppsDeploymentFixture.quote(edit))")
            _ = try await app.succeed(
                cli,
                "wendy app promote --tag 1.0.0 --device \(app.asset) --name workshop --config E2E_ATTEMPT=\(app.attempt) --config DISPLAY_UNIT=celsius --run --keep-trying 7d --yes \(app.cloudFlag)"
            )
            let catalog = try await app.catalog(cli)
            let row = try #require(catalog.releases?.first { $0.name == "1.0.0" })
            let digest = try #require(row.imageDigest)
            let release = AppsDeploymentFixture.Release(id: row.id, tag: row.name, digest: digest)
            try await app.requireArtifact(cli, release: release)
            try await app.requireRunning(
                cli,
                release: release,
                revision: 1,
                version: "1.0.0",
                unit: "celsius"
            )
        }
    }
}
