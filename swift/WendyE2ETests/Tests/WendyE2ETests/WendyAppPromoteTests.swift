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
        try await promoteCachedImage(requireRuntimeConvergence: true)
    }

    /**
     Direct-run A, then edit and compile B without rebuilding its image. The
     normal promotion command selects the independently rehashed cached A
     image, preserves its exact layer bytes when adding release metadata, and
     records the intended deployment. A's actual HTTP response is
     checked before promotion; device acknowledgment is not claimed here. The
     original full promotion case retains its terminal version/settings checks.
     */
    @Test(
        .enabled(
            if: appsDeploymentTestsEnabled
                && ProcessInfo.processInfo.environment["WENDY_E2E_APPS_BUILDER"] == "true"
        )
    )
    func `publishes cached direct run content and records promotion intent`() async throws {
        try await promoteCachedImage(requireRuntimeConvergence: false)
    }

    private struct PromotionImage: Decodable {
        struct Layer: Decodable, Equatable {
            let digest: String
            let size: Int
        }
        let digest: String?
        let layers: [Layer]
    }

    private func promoteCachedImage(
        requireRuntimeConvergence: Bool,
        filePath: String = #filePath,
        function: String = #function,
        line: Int = #line
    ) async throws {
        try await AppsDeploymentFixture.withApp(filePath: filePath, function: function, line: line)
        { cli, app in
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
                let cachedLayout = "\(cli.wendyCacheDirectory)/ocilayout/\(app.app)-linux_arm64"
                let rehash = #"""
                    import hashlib, json, pathlib, sys
                    root = pathlib.Path(sys.argv[1])
                    def blob(digest):
                        algorithm, value = digest.split(':', 1)
                        assert algorithm == 'sha256'
                        data = (root / 'blobs' / algorithm / value).read_bytes()
                        assert hashlib.sha256(data).hexdigest() == value
                        return data
                    matches = {}
                    def visit(descriptor):
                        raw = blob(descriptor['digest'])
                        document = json.loads(raw)
                        if 'manifests' in document:
                            for child in document['manifests']: visit(child)
                        elif 'config' in document:
                            config = json.loads(blob(document['config']['digest']))
                            if config.get('architecture') == 'arm64' and config.get('os') == 'linux':
                                digest = 'sha256:' + hashlib.sha256(raw).hexdigest()
                                layers = []
                                for layer in document['layers']:
                                    assert len(blob(layer['digest'])) == layer['size']
                                    layers.append({'digest': layer['digest'], 'size': layer['size']})
                                matches[digest] = {'digest': digest, 'layers': layers}
                    for descriptor in json.loads((root / 'index.json').read_bytes())['manifests']:
                        visit(descriptor)
                    assert len(matches) == 1
                    print(json.dumps(next(iter(matches.values()))))
                    """#
                let cachedJSON = try await cli.sh(
                    "python3 -c \(AppsDeploymentFixture.quote(rehash)) \(AppsDeploymentFixture.quote(cachedLayout))"
                ) { $0.stdout.trimmingCharacters(in: .whitespacesAndNewlines) }
                let cachedA = try JSONDecoder().decode(
                    PromotionImage.self,
                    from: Data(cachedJSON.utf8)
                )
                try #require(
                    (cachedA.digest ?? "").hasPrefix("sha256:")
                        && (cachedA.digest ?? "").count == 71
                )
                try #require(!cachedA.layers.isEmpty)
                let cachedDigest = try #require(cachedA.digest)
                // Edit source without a subsequent image build. Promotion must use A.
                let edit =
                    #"from pathlib import Path; p=Path('main.go'); s=p.read_text(); assert 'var imageVersion = \"1.0.0\"' in s; p.write_text(s.replace('var imageVersion = \"1.0.0\"', 'var imageVersion = \"2.0.0\"')); Path('version.txt').write_text('2.0.0\n')"#
                try await cli.sh("python3 -c \(AppsDeploymentFixture.quote(edit))")
                // Compile B's changed executable in the build context, without any
                // subsequent image build or direct run. Cached image A must win.
                _ = try await app.succeed(cli, compile)
                try await cli.sh(
                    "shasum -a 256 server > edited-B.sha256 && ! cmp -s cached-A.sha256 edited-B.sha256"
                )
                let promoted = try await app.succeed(
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
                let artifact = try await app.requireArtifact(cli, release: release)
                let receipt = promoted.stdout + "\n" + promoted.readableStderr
                #expect(
                    receipt.contains(
                        "Promoting the image wendy run last built for linux/arm64: \(cachedDigest),"
                    )
                )
                // Publishing adds the app-definition config label, so its OCI
                // manifest digest can differ. Compare the actual immutable
                // layer descriptors, not an invented pre-publication digest.
                let manifestData = try #require(Data(base64Encoded: artifact.manifest.ociManifest))
                let publishedImage = try JSONDecoder().decode(
                    PromotionImage.self,
                    from: manifestData
                )
                #expect(publishedImage.layers == cachedA.layers)
                #expect(artifact.computedDigest == digest)
                _ = try await app.requireHistory(cli, release: release, revision: 1)
                if requireRuntimeConvergence {
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
}
