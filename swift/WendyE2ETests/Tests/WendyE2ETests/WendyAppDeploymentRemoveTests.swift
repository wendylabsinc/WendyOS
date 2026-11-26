import Foundation
import Testing
import WendyE2ETesting

@Suite(.serialized)
struct `'wendy app deployment remove'` {
    /**
     Deploy a published release, then remove the named deployment with a note.
     Cloud records a removed revision with no targets and preserves the earlier
     target history. The catalog app, release and encrypted artifact remain
     readable with their original manifest digest. Device acknowledgment and
     eventual runtime removal are verified by the separate device-response spec.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `records removal while retaining the published release in Cloud`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            try await app.create(cli)
            let release = try await app.publish(cli)
            let artifact = try await app.requireArtifact(cli, release: release)
            _ = try await app.succeed(cli, app.deployCommand(release))
            let first = try await app.requireHistory(cli, release: release, revision: 1)
            _ = try await app.succeed(
                cli,
                "wendy app deployment remove \(app.deployment) --note 'Remove workshop' --yes \(app.cloudFlag)"
            )
            let removed = try await app.requireHistory(
                cli,
                release: release,
                revision: 2,
                targets: [],
                sameDeployment: first.deployment.id
            )
            #expect(removed.deployment.removedAt != nil)
            #expect(removed.history.count == 2)
            let removal = try #require(removed.history.first { $0.revision.revision == 2 })
            #expect(removal.revision.removed)
            #expect(removal.revision.note == "Remove workshop")
            #expect(removal.devices.isEmpty)
            #expect(removal.moves.isEmpty)
            #expect(removed.history.first { $0.revision.revision == 1 }?.devices == [app.asset])
            let catalog = try await app.catalog(cli)
            #expect(catalog.app?.id == app.app)
            let retained = try #require(catalog.releases?.first { $0.id == release.id })
            #expect(retained.name == release.tag)
            #expect(retained.imageDigest == release.digest)
            let retainedArtifact = try await app.requireArtifact(cli, release: release)
            #expect(retainedArtifact.computedDigest == artifact.computedDigest)
            #expect(retainedArtifact.manifest.manifestDigest == artifact.manifest.manifestDigest)
            #expect(retainedArtifact.manifest.contentKeyId == artifact.manifest.contentKeyId)
        }
    }

    /**
     Removal ends the owned runtime and endpoint, records removed history and
     keeps the catalog app and release. A second named deployment can reuse the
     retained release and respond with its original content.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `removes the application while retaining the published release`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            let release = try await app.deployed(cli)
            _ = try await app.succeed(
                cli,
                "wendy app deployment remove \(app.deployment) --note 'Remove workshop' --yes \(app.cloudFlag)"
            )
            try await app.requireAbsent(cli)
            let removed = try await app.snapshot(cli)
            #expect(removed.deployment.removedAt != nil)
            #expect(removed.history.contains { $0.revision.removed })
            _ = try await app.succeed(cli, "wendy app lifecycle \(app.app) \(app.cloudFlag)")
            try await app.refuse(
                cli,
                "wendy app deployment show \(app.deployment) \(app.cloudFlag)",
                guidance: "no deployment"
            )
            let command = app.deployCommand(release).replacingOccurrences(
                of: "--name workshop",
                with: "--name replacement"
            )
            _ = try await app.succeed(cli, command)
            // Independent response; show() intentionally still refers to the removed name.
            let deadline = ContinuousClock.now.advanced(by: .seconds(120))
            var responding = false
            repeat {
                let response = try await app.response(cli)
                responding =
                    response?.app == app.app && response?.attempt == app.attempt
                    && response?.version == "1.0.0" && response?.unit == "celsius"
                if responding { break }
                try await Task.sleep(for: .seconds(2))
            } while ContinuousClock.now < deadline
            #expect(responding)
        }
    }

    /**
     Isolated Agent protocol tier: block an actual pull at a synthetic blob peer,
     apply terminal removal, and reload durable Agent state. No import or start
     occurs; the reloaded ordering predicate refuses an ordinary earlier
     revision without constructing or replaying a signed request. This is not
     end-to-end delayed delivery evidence. Transport interruption,
     physical cache cleanup and real device reboot are separate live concerns.
     */
    @Test(.enabled(if: appsIsolationTestsEnabled))
    func `cancels an in-flight pull and prevents resurrection`() async throws {
        try await AppsIsolationFixture.run("TestAppsOperatorIsolationRemovalDuringPull")
    }

    /**
     On a dedicated device, temporarily stop containerd using its normal service
     control. Cloud removal acceptance cannot establish device cleanup. Once the
     runtime returns, the owned app and endpoint disappear while the catalog app
     remains. Inventory alone does not establish image-only cleanup.
     */
    @Test(.enabled(if: appsDedicatedDeviceTestsEnabled))
    func `finishes a queued removal after the container runtime recovers`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            _ = try await app.deployed(cli)
            try await app.withStoppedService(cli, .runtime) {
                _ = try await app.succeed(
                    cli,
                    "wendy app deployment remove \(app.deployment) --yes \(app.cloudFlag)"
                )
                let removed = try await app.snapshot(cli)
                #expect(removed.deployment.removedAt != nil)
                // The runtime is down; catalog acceptance cannot prove cleanup.
                #expect(
                    !removed.states.contains {
                        $0.state.reportedState == "RUNNING" && !$0.state.statementPending
                            && $0.state.outcome == "APPLY_OUTCOME_APPLIED"
                    }
                )
            }
            try await app.requireAbsent(cli)
            let catalog = try await app.catalog(cli)
            #expect(catalog.appCode == "OK")
        }
    }
    /**
     On a dedicated Pi, the actual services.ReleaseImageName record exists in
     containerd's default namespace after apply, then disappears after removal.
     A separately deployed control still responds and every unrelated baseline
     image record remains. This checks image records, not physical blob or GCS GC.
     */
    @Test(.enabled(if: appsDedicatedDeviceTestsEnabled))
    func `removes the owned imported image record and preserves unrelated images`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            try await app.withRelatedApp(cli) { control in
                try await control.writeProject(cli)
                let controlRelease = try await control.publish(cli)
                _ = try await control.succeed(cli, control.deployCommand(controlRelease))
                try await control.requireRunning(
                    cli,
                    release: controlRelease,
                    revision: 1,
                    version: "1.0.0",
                    unit: "celsius"
                )
                let controlArtifact = try await control.observe(
                    cli,
                    "artifact",
                    arguments: "--release \(controlRelease.id)",
                    as: AppsArtifact.self
                )
                let baseline = try await app.imageRecords(cli)
                try #require(
                    baseline.contains(controlArtifact.imageName),
                    "Require a real unrelated control record before the removal"
                )
                try #require(
                    !baseline.contains { $0.hasPrefix("localhost/wendy-release/\(app.app):") }
                )
                let owned = try await app.deployed(cli)
                let artifact = try await app.observe(
                    cli,
                    "artifact",
                    arguments: "--release \(owned.id)",
                    as: AppsArtifact.self
                )
                let imported = try await app.imageRecords(cli)
                try #require(imported.contains(artifact.imageName))
                #expect(baseline.isSubset(of: imported))
                _ = try await app.succeed(
                    cli,
                    "wendy app deployment remove \(app.deployment) --yes \(app.cloudFlag)"
                )
                try await app.requireAbsent(cli)
                let after = try await app.requireImageRecordAbsent(cli, name: artifact.imageName)
                #expect(baseline.isSubset(of: after))
                #expect(after.contains(controlArtifact.imageName))
                try await control.requireRunning(
                    cli,
                    release: controlRelease,
                    revision: 1,
                    version: "1.0.0",
                    unit: "celsius"
                )
                let catalog = try await app.catalog(cli)
                #expect(catalog.releases?.contains { $0.id == owned.id } == true)
            }
        }
    }

}
