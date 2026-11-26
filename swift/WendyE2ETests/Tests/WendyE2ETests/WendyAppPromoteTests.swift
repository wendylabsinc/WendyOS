import Foundation
import Testing
import WendyE2ETesting

@Suite
struct `'wendy app promote'` {
    /**
     A signed-in operator chooses a verified Dev target and a new project with
     no cached direct run. Promotion explains how to obtain an image instead
     of rebuilding, registering or publishing the app, or deploying one.
     The isolated app cache stays empty and Cloud has no release or deployment.
     The builder-opted-in case below exercises the cached-image promotion path.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `explains how to proceed when no direct run is cached`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            // Native auth uses its normal credential store; image caches still
            // live in this scenario's isolated HOME. No cached image is seeded.
            let layout = "\(cli.wendyCacheDirectory)/ocilayout/\(app.app)-linux_arm64"
            try await cli.sh("test ! -e \(AppsDeploymentFixture.quote(layout))")
            let before = try await app.catalog(cli)
            try #require(before.appCode == "NotFound" && before.app == nil)
            try #require(before.releases?.isEmpty == true)
            try #require(before.deployments?.isEmpty == true)

            let result = try await app.command(
                cli,
                "wendy app promote --tag 1.0.0 --platform linux/arm64 --device \(app.asset) --name workshop --config E2E_ATTEMPT=\(app.attempt) --run --yes \(app.cloudFlag) </dev/null"
            )
            #expect(result.status.isFailure)
            #expect(result.readableStderr.contains("Nothing to promote"))
            #expect(result.readableStderr.contains("wendy run"))
            #expect(result.readableStderr.contains("wendy app publish"))

            try await cli.sh(
                "test ! -e \(AppsDeploymentFixture.quote(layout + "/index.json"))"
            )
            let after = try await app.catalog(cli)
            #expect(after.appCode == "NotFound" && after.app == nil)
            #expect(after.releases?.isEmpty == true)
            #expect(after.deployments?.isEmpty == true)
            #expect(!(try await app.inventory(cli)).contains { $0.name == app.app })
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
            try await AppsBuilderFixture.withBuilder(cli) {
                let source = URL(fileURLWithPath: AppsDeploymentFixture.observerPath)
                    .deletingLastPathComponent().deletingLastPathComponent().appendingPathComponent(
                        ".github/ci-tests/apps-deployments-small"
                    ).path
                try await cli.sh(
                    "cp \(AppsDeploymentFixture.quote(source))/main.go \(AppsDeploymentFixture.quote(source))/Dockerfile ."
                )
                let configure =
                    "import json; p='wendy.json'; c=json.load(open(p)); c['language']='go'; c['env']['E2E_ATTEMPT']=\(String(reflecting: app.attempt)); json.dump(c,open(p,'w'))"
                try await cli.sh("python3 -c \(AppsDeploymentFixture.quote(configure))")
                let bakeA =
                    #"from pathlib import Path; p=Path('main.go'); s=p.read_text(); assert 'var imageVersion string' in s; p.write_text(s.replace('var imageVersion string', 'var imageVersion = \"1.0.0\"')); Path('version.txt').write_text('1.0.0\n')"#
                try await cli.sh("python3 -c \(AppsDeploymentFixture.quote(bakeA))")
                let compile =
                    "env CGO_ENABLED=0 GOOS=linux GOARCH=arm64 GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go build -trimpath -buildvcs=false -ldflags '-s -w' -o server main.go"
                _ = try await app.succeed(cli, compile)
                try await cli.sh("shasum -a 256 server > cached-A.sha256")
                try await cli.sh("touch direct-owned-app")
                _ = try await app.succeed(
                    cli,
                    "wendy --json --device \(AppsDeploymentFixture.quote(app.device)) run --builder docker --stagefile-backend=dockerfile --build-arg APP_VERSION=1.0.0 --detach"
                )
                try await app.requireResponse(cli, version: "1.0.0", unit: "fahrenheit")
                // Edit source without any subsequent build. Promotion must use A.
                let edit =
                    #"from pathlib import Path; p=Path('main.go'); s=p.read_text(); assert 'var imageVersion = \"1.0.0\"' in s; p.write_text(s.replace('var imageVersion = \"1.0.0\"', 'var imageVersion = \"2.0.0\"')); Path('version.txt').write_text('2.0.0\n')"#
                try await cli.sh("python3 -c \(AppsDeploymentFixture.quote(edit))")
                // Compile B's changed executable in the build context, without any
                // subsequent image build or direct run. Cached image A must win.
                _ = try await app.succeed(cli, compile)
                try await cli.sh(
                    "shasum -a 256 server > edited-B.sha256 && ! cmp -s cached-A.sha256 edited-B.sha256"
                )
                _ = try await app.succeed(
                    cli,
                    "wendy app promote --tag 1.0.0 --device \(app.asset) --name workshop --config E2E_ATTEMPT=\(app.attempt) --config DISPLAY_UNIT=celsius --run --keep-trying 7d --yes \(app.cloudFlag)"
                )
                let catalog = try await app.catalog(cli)
                let row = try #require(catalog.releases?.first { $0.name == "1.0.0" })
                let digest = try #require(row.imageDigest)
                let release = AppsDeploymentFixture.Release(
                    id: row.id,
                    tag: row.name,
                    digest: digest
                )
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
}
