import Foundation
import Testing
import WendyE2ETesting

@Suite(.serialized)
struct `'Cloud operator acceptance'` {
    /**
     Show the exact stored release, desired state, revision and target before
     acknowledgment. A configuration update changes the authored history while
     preserving the deployment and release. This is not a running-state claim.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `shows desired deployment state and authored configuration history`() async throws {
        try await AppsDeploymentFixture.withCloudApp(verifyTarget: true) { cli, app in
            try await app.create(cli)
            let release = try await app.publish(cli)
            _ = try await app.succeed(cli, app.deployCommand(release))
            let first = try await app.requireHistory(cli, release: release, revision: 1)
            let shown = try await app.show(cli)
            #expect(shown.contains(app.deployment))
            #expect(shown.contains("revision 1"))
            #expect(shown.contains("to 1 device"))
            _ = try await app.succeed(
                cli,
                "wendy app deployment update \(app.deployment) --config DISPLAY_UNIT=kelvin --note 'Use kelvin' --yes \(app.cloudFlag)"
            )
            let changed = try await app.requireHistory(
                cli,
                release: release,
                revision: 2,
                sameDeployment: first.deployment.id
            )
            #expect(changed.deployment.appReleaseId == first.deployment.appReleaseId)
            #expect(changed.history.count == 2)
            #expect(
                changed.history.first { $0.revision.revision == 1 }?.revision.configJson
                    == first.deployment.configJson
            )
            #expect(
                changed.history.first { $0.revision.revision == 2 }?.revision.note == "Use kelvin"
            )
            let settings = try JSONDecoder().decode(
                [String: String].self,
                from: Data(changed.deployment.configJson.utf8)
            )
            #expect(settings["DISPLAY_UNIT"] == "kelvin")
            let shownAgain = try await app.show(cli)
            #expect(shownAgain.contains(app.deployment) && shownAgain.contains("revision 2"))
        }
    }

    /**
     Install-only, start and stop are authored deployment intentions. Removing
     an optional override preserves the required setting and release. Each
     accepted command creates one revision on the same exact target; no device
     acknowledgment or application state is inferred from these catalog reads.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `records install start and stop intentions on the same release`() async throws {
        try await AppsDeploymentFixture.withCloudApp(verifyTarget: true) { cli, app in
            try await app.create(cli)
            let release = try await app.publish(cli)
            _ = try await app.succeed(cli, app.deployCommand(release, run: false))
            let installed = try await app.requireHistory(cli, release: release, revision: 1)
            #expect(installed.deployment.desiredState == "STOPPED")
            #expect(installed.deployment.validity == "604800s")
            _ = try await app.succeed(
                cli,
                "wendy app deployment update \(app.deployment) --start --remove-config DISPLAY_UNIT --note 'Start without unit override' --yes \(app.cloudFlag)"
            )
            let started = try await app.requireHistory(
                cli,
                release: release,
                revision: 2,
                sameDeployment: installed.deployment.id
            )
            #expect(started.deployment.desiredState == "RUNNING")
            #expect(started.deployment.validity == installed.deployment.validity)
            let settings = try JSONDecoder().decode(
                [String: String].self,
                from: Data(started.deployment.configJson.utf8)
            )
            #expect(settings["DISPLAY_UNIT"] == nil)
            #expect(settings["E2E_ATTEMPT"] == app.attempt)
            _ = try await app.succeed(
                cli,
                "wendy app deployment update \(app.deployment) --stop --keep-trying 30d --note 'Stop without replacing release' --yes \(app.cloudFlag)"
            )
            let stopped = try await app.requireHistory(
                cli,
                release: release,
                revision: 3,
                sameDeployment: installed.deployment.id
            )
            #expect(stopped.deployment.desiredState == "STOPPED")
            #expect(stopped.deployment.validity == "2592000s")
            #expect(stopped.deployment.configJson == started.deployment.configJson)
            #expect(
                stopped.history.first { $0.revision.revision == 1 }?.revision.desiredState
                    == "STOPPED"
            )
            #expect(
                stopped.history.first { $0.revision.revision == 2 }?.revision.desiredState
                    == "RUNNING"
            )
            #expect(
                stopped.history.first { $0.revision.revision == 3 }?.revision.note
                    == "Stop without replacing release"
            )
            let shown = try await app.show(cli)
            #expect(shown.contains("install only") && shown.contains("revision 3"))
        }
    }

    /**
     Two owned apps can use the same deployment name. Exact app/name lookups
     show their own release and history, without requiring either device apply.
     The full responding-application variant remains separate.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `keeps same named Cloud deployments separate across owned apps`() async throws {
        try await AppsDeploymentFixture.withCloudApp(verifyTarget: true) { cli, app in
            try await app.create(cli)
            let a = try await app.publish(cli)
            _ = try await app.succeed(cli, app.deployCommand(a))
            let first = try await app.requireHistory(cli, release: a, revision: 1)
            try await app.withRelatedCloudApp(cli) { other in
                let b = try await other.publish(
                    cli,
                    version: "2.0.0",
                    layoutKey: "WENDY_E2E_APPS_LAYOUT_B"
                )
                _ = try await other.succeed(cli, other.deployCommand(b))
                let second = try await other.requireHistory(cli, release: b, revision: 1)
                #expect(second.deployment.id != first.deployment.id)
                #expect(second.deployment.appId == other.app)
                #expect(first.deployment.appId == app.app)
                let firstShown = try await app.show(cli)
                let secondShown = try await other.show(cli)
                #expect(firstShown.contains(app.deployment) && firstShown.contains(a.tag))
                #expect(secondShown.contains(other.deployment) && secondShown.contains(b.tag))
                #expect(!firstShown.contains(other.deployment))
                #expect(!secondShown.contains(app.deployment))
            }
        }
    }

    /**
     At revision two, revision nine is unavailable. The command refuses with
     its actual valid range and preserves complete stored history, release,
     configuration, deployment identity and target. Runtime preservation still
     belongs to the existing full device-response spec.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `refuses a nonexistent rollback revision without changing Cloud history`() async throws {
        try await AppsDeploymentFixture.withCloudApp(verifyTarget: true) { cli, app in
            try await app.create(cli)
            let a = try await app.publish(cli)
            _ = try await app.succeed(cli, app.deployCommand(a))
            let b = try await app.publish(
                cli,
                version: "2.0.0",
                layoutKey: "WENDY_E2E_APPS_LAYOUT_B"
            )
            _ = try await app.succeed(
                cli,
                "wendy app deployment update \(app.deployment) --release \(b.id) --yes \(app.cloudFlag)"
            )
            let before = try await app.requireHistory(cli, release: b, revision: 2)
            try await app.refuse(
                cli,
                "wendy app deployment rollback \(app.deployment) --to 9 --yes \(app.cloudFlag)",
                guidance: "--to <1-1>"
            )
            let after = try await app.requireHistory(
                cli,
                release: b,
                revision: 2,
                sameDeployment: before.deployment.id
            )
            #expect(after.history == before.history)
            #expect(after.deployment.configJson == before.deployment.configJson)
            #expect(after.deployment.desiredState == before.deployment.desiredState)
            #expect(after.currentTargets() == before.currentTargets())
        }
    }

    /** A refused scripted update creates no revision or authored history. */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `refuses an unconfirmed Cloud update without creating a revision`() async throws {
        try await AppsDeploymentFixture.withCloudApp(verifyTarget: true) { cli, app in
            try await app.create(cli)
            let release = try await app.publish(cli)
            _ = try await app.succeed(cli, app.deployCommand(release))
            let before = try await app.requireHistory(cli, release: release, revision: 1)
            try await app.refuse(
                cli,
                "wendy app deployment update \(app.deployment) --config DISPLAY_UNIT=kelvin \(app.cloudFlag)",
                guidance: "--yes"
            )
            let after = try await app.requireHistory(
                cli,
                release: release,
                revision: 1,
                sameDeployment: before.deployment.id
            )
            #expect(after.history == before.history)
            #expect(after.deployment.configJson == before.deployment.configJson)
            #expect(after.currentTargets() == before.currentTargets())
        }
    }

    /**
     Without confirmation, removal preserves the current target and history.
     Confirmed removal records an empty target set and the operator's note.
     Catalog removal is not proof of runtime removal.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `requires confirmation before recording deployment removal`() async throws {
        try await AppsDeploymentFixture.withCloudApp(verifyTarget: true) { cli, app in
            try await app.create(cli)
            let release = try await app.publish(cli)
            _ = try await app.succeed(cli, app.deployCommand(release))
            let before = try await app.requireHistory(cli, release: release, revision: 1)
            try await app.refuse(
                cli,
                "wendy app deployment remove \(app.deployment) \(app.cloudFlag)",
                guidance: "--yes"
            )
            let refused = try await app.requireHistory(
                cli,
                release: release,
                revision: 1,
                sameDeployment: before.deployment.id
            )
            #expect(refused.history == before.history)
            _ = try await app.succeed(
                cli,
                "wendy app deployment remove \(app.deployment) --note 'Confirmed removal' --yes \(app.cloudFlag)"
            )
            let removed = try await app.requireHistory(
                cli,
                release: release,
                revision: 2,
                targets: [],
                sameDeployment: before.deployment.id
            )
            #expect(removed.deployment.removedAt != nil)
            #expect(
                removed.history.first { $0.revision.revision == 2 }?.revision.note
                    == "Confirmed removal"
            )
        }
    }

    /**
     An old removed deployment no longer owns the target. A fresh named
     deployment reuses the retained release without requiring a move from that
     tombstone. Stored intent and the actual artifact are independently read.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `reuses a retained release after removing its old deployment`() async throws {
        try await AppsDeploymentFixture.withCloudApp(verifyTarget: true) { cli, app in
            try await app.create(cli)
            let release = try await app.publish(cli)
            _ = try await app.succeed(cli, app.deployCommand(release))
            let before = try await app.requireHistory(cli, release: release, revision: 1)
            _ = try await app.succeed(
                cli,
                "wendy app deployment remove \(app.deployment) --yes \(app.cloudFlag)"
            )
            _ = try await app.requireHistory(
                cli,
                release: release,
                revision: 2,
                targets: [],
                sameDeployment: before.deployment.id
            )
            let replacement = try AppsDeploymentFixture(
                assetID: app.asset,
                appID: app.app,
                attemptID: app.attempt,
                deploymentName: "replacement",
                portNumber: app.port,
                requirement: .cloudTarget
            )
            _ = try await replacement.succeed(cli, replacement.deployCommand(release))
            let next = try await replacement.requireHistory(cli, release: release, revision: 1)
            #expect(next.deployment.id != before.deployment.id)
            #expect(next.deployment.appReleaseId == release.id)
            _ = try await app.requireArtifact(cli, release: release)
        }
    }

    /**
     Deleting an assigned app still requires explicit confirmation. The
     confirmed operation removes its actual catalog, releases and deployments.
     The full runtime and detach examples keep their independent HTTP checks.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `confirms catalog deletion of an assigned app without waiting for apply`() async throws {
        try await AppsDeploymentFixture.withCloudApp(verifyTarget: true) { cli, app in
            try await app.create(cli)
            let release = try await app.publish(cli)
            _ = try await app.succeed(cli, app.deployCommand(release))
            try await app.refuse(
                cli,
                "wendy app delete \(app.app) \(app.cloudFlag)",
                guidance: "--yes"
            )
            let present = try await app.catalog(cli)
            #expect(present.app?.id == app.app && present.deployments?.count == 1)
            _ = try await app.succeed(cli, "wendy app delete \(app.app) --yes \(app.cloudFlag)")
            let gone = try await app.catalog(cli)
            #expect(gone.appCode == "NotFound" && gone.app == nil)
            #expect(gone.releases?.isEmpty == true)
            #expect(gone.deployments?.isEmpty == true)
        }
    }

    /**
     The actual organization limit bounds an app's retention policy even when
     no workload is running. Refusal leaves the policy and release catalog
     intact. This does not simulate a sweep or replace in-use retention checks.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `enforces the actual retention limit without requiring device apply`() async throws {
        try await AppsDeploymentFixture.withCloudApp { cli, app in
            try await app.create(cli)
            let release = try await app.publish(cli)
            let catalog = try await app.catalog(cli)
            let limit = try #require(catalog.app?.lifecyclePolicyLimit?.keepTagged)
            try #require(limit < Int(Int32.max))
            let before = try await app.succeed(
                cli,
                "wendy app lifecycle \(app.app) \(app.cloudFlag)"
            )
            try await app.refuse(
                cli,
                "wendy app lifecycle \(app.app) --keep-tagged \(limit + 1) --yes \(app.cloudFlag)",
                guidance: "or less"
            )
            let after = try await app.succeed(
                cli,
                "wendy app lifecycle \(app.app) \(app.cloudFlag)"
            )
            #expect(after.stdout == before.stdout)
            let retained = try await app.catalog(cli)
            #expect(retained.releases?.contains { $0.id == release.id } == true)
        }
    }
}
