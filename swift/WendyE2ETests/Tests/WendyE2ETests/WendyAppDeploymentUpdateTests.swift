import Foundation
import Testing
import WendyE2ETesting

@Suite(.serialized)
struct `'wendy app deployment update'` {
    /**
     A release change advances the named deployment once and preserves its
     configuration override. Version B must actually respond with celsius even
     though its own release default is kelvin. One app instance remains.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `changes the release and preserves the deployment settings`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            _ = try await app.deployed(cli)
            let first = try await app.snapshot(cli)
            let releaseB = try await app.publish(
                cli,
                version: "2.0.0",
                layoutKey: "WENDY_E2E_APPS_LAYOUT_B"
            )
            _ = try await app.succeed(
                cli,
                "wendy app deployment update \(app.deployment) --release \(releaseB.id) --note 'Install B' --yes \(app.cloudFlag)"
            )
            try await app.requireRunning(
                cli,
                release: releaseB,
                revision: 2,
                version: "2.0.0",
                unit: "celsius"
            )
            let history = try await app.requireHistory(
                cli,
                release: releaseB,
                revision: 2,
                sameDeployment: first.deployment.id
            )
            #expect(
                history.history.first { $0.revision.revision == 2 }?.revision.note == "Install B"
            )
        }
    }

    /**
     A configuration-only change keeps release B but advances its revision.
     The response must contain the new setting; an unchanged digest or a newer
     status timestamp is not evidence that the configuration was applied.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `applies a configuration change without changing the release`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            _ = try await app.deployed(cli)
            let releaseB = try await app.publish(
                cli,
                version: "2.0.0",
                layoutKey: "WENDY_E2E_APPS_LAYOUT_B"
            )
            _ = try await app.succeed(
                cli,
                "wendy app deployment update \(app.deployment) --release \(releaseB.id) --yes \(app.cloudFlag)"
            )
            try await app.requireRunning(
                cli,
                release: releaseB,
                revision: 2,
                version: "2.0.0",
                unit: "celsius"
            )
            _ = try await app.succeed(
                cli,
                "wendy app deployment update \(app.deployment) --config DISPLAY_UNIT=kelvin --note 'Change units' --yes \(app.cloudFlag)"
            )
            try await app.requireRunning(
                cli,
                release: releaseB,
                revision: 3,
                version: "2.0.0",
                unit: "kelvin"
            )
        }
    }

    /**
     An unconfirmed scripted change leaves the prior revision and application
     settings intact, and explains how to confirm the operation.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `leaves the running revision unchanged without confirmation`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            let release = try await app.deployed(cli)
            try await app.refuse(
                cli,
                "wendy app deployment update \(app.deployment) --config DISPLAY_UNIT=kelvin \(app.cloudFlag)",
                guidance: "--yes"
            )
            try await app.requireRunning(
                cli,
                release: release,
                revision: 1,
                version: "1.0.0",
                unit: "celsius"
            )
        }
    }

    /**
     A replacement that fails to start reports failure while the prior working
     application continues serving its original version and configuration.
     This retains the intended behavior despite the documented replacement race.
     */
    @Test(
        .enabled(
            if: appsDeploymentTestsEnabled
                && ProcessInfo.processInfo.environment["WENDY_E2E_APPS_FAILURES"] == "true"
        )
    )
    func `preserves the working app when a replacement cannot start`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            _ = try await app.deployed(cli)
            let failing = try await app.publish(
                cli,
                version: "3.0.0",
                layoutKey: "WENDY_E2E_APPS_LAYOUT_FAIL"
            )
            _ = try await app.succeed(
                cli,
                "wendy app deployment update \(app.deployment) --release \(failing.id) --yes \(app.cloudFlag)"
            )
            let deadline = ContinuousClock.now.advanced(by: .seconds(120))
            var observed = false
            repeat {
                let status = try await app.show(cli)
                let response = try await app.response(cli)
                observed =
                    status.contains("revision 2.") && status.contains("1 failed")
                    && response?.app == app.app && response?.attempt == app.attempt
                    && response?.version == "1.0.0" && response?.unit == "celsius"
                if observed { break }
                try await Task.sleep(for: .seconds(2))
            } while ContinuousClock.now < deadline
            #expect(observed, "Failed replacement must leave the prior good app responding")
        }
    }

    /**
     Deploy to a private site containing A, then add B to that site. Revision
     one's membership remains A. A release-only update carries A forward.
     Update --add-tag private-overlap --yes resolves the site/tag union again:
     the next revision contains A and B once each, and both serve the release.
     Automatic enrollment on every revision is not the current intention.
     */
    @Test(.enabled(if: appsFleetTestsEnabled))
    func `preserves resolved membership until the target selection changes`() async throws {
        try await AppsDeploymentFixture.withFleet { cli, app, devices, lease in
            try await app.create(cli)
            let a = try await app.publish(cli)
            _ = try await app.succeed(cli, app.deployCommand(a, extra: "--site \(lease.site)"))
            _ = try await app.requireHistory(cli, release: a, revision: 1)
            let b = try app.target(devices[1])
            _ = try await app.succeed(cli, b.observerCommand("fleet-join"))
            let releaseB = try await app.publish(
                cli,
                version: "2.0.0",
                layoutKey: "WENDY_E2E_APPS_LAYOUT_B"
            )
            _ = try await app.succeed(
                cli,
                "wendy app deployment update \(app.deployment) --release \(releaseB.id) --yes \(app.cloudFlag)"
            )
            let second = try await app.requireHistory(cli, release: releaseB, revision: 2)
            #expect(second.history.first { $0.revision.revision == 1 }?.devices == [app.asset])
            let untouched = try await b.inventory(cli)
            #expect(!untouched.contains { $0.name == app.app })
            _ = try await app.succeed(
                cli,
                "wendy app deployment update \(app.deployment) --add-tag \(lease.tag) --yes \(app.cloudFlag)"
            )
            let third = try await app.requireHistory(
                cli,
                release: releaseB,
                revision: 3,
                targets: [app.asset, b.asset],
                sameDeployment: second.deployment.id
            )
            #expect(third.history.first { $0.revision.revision == 1 }?.devices == [app.asset])
            try await app.requireRunning(
                cli,
                release: releaseB,
                revision: 3,
                version: "2.0.0",
                unit: "celsius"
            )
            try await b.requireRunning(
                cli,
                release: releaseB,
                revision: 3,
                version: "2.0.0",
                unit: "celsius"
            )
        }
    }

    /**
     Deploy A alone, then update --add-device B --yes and verify one named
     deployment expands to A and B. Separately create disjoint canary and
     production deployments at 1.0.0. Updating canary to 2.0.0 leaves production
     serving 1.0.0 until its explicit update. These are operator-driven waves.
     */
    @Test(.enabled(if: appsFleetTestsEnabled))
    func `expands an operator-controlled canary without advancing production`() async throws {
        try await AppsDeploymentFixture.withFleet { cli, app, devices, _ in
            try await app.create(cli)
            let a = try await app.publish(cli)
            let canary = try app.target(devices[0], name: "canary")
            let production = try app.target(devices[1], name: "production")
            _ = try await canary.succeed(cli, canary.deployCommand(a))
            _ = try await production.succeed(cli, production.deployCommand(a))
            try await canary.requireRunning(
                cli,
                release: a,
                revision: 1,
                version: "1.0.0",
                unit: "celsius"
            )
            try await production.requireRunning(
                cli,
                release: a,
                revision: 1,
                version: "1.0.0",
                unit: "celsius"
            )
            let b = try await app.publish(
                cli,
                version: "2.0.0",
                layoutKey: "WENDY_E2E_APPS_LAYOUT_B"
            )
            _ = try await app.succeed(
                cli,
                "wendy app deployment update \(canary.deployment) --release \(b.id) --yes \(app.cloudFlag)"
            )
            try await canary.requireRunning(
                cli,
                release: b,
                revision: 2,
                version: "2.0.0",
                unit: "celsius"
            )
            try await production.requireRunning(
                cli,
                release: a,
                revision: 1,
                version: "1.0.0",
                unit: "celsius"
            )
            _ = try await app.succeed(
                cli,
                "wendy app deployment update \(canary.deployment) --add-device \(production.asset) --move --yes \(app.cloudFlag)"
            )
            let expanded = try await canary.requireHistory(
                cli,
                release: b,
                revision: 3,
                targets: [canary.asset, production.asset]
            )
            #expect(expanded.history.first { $0.revision.revision == 1 }?.devices == [canary.asset])
            let moved = try app.target(devices[1], name: "canary")
            try await moved.requireRunning(
                cli,
                release: b,
                revision: 3,
                version: "2.0.0",
                unit: "celsius"
            )
        }
    }

    /**
     Put the device in canary. Updating production --add-device UUID --yes
     without --move refuses with both assignments unchanged. Retry with --move
     --yes: exactly one assignment and one running app remain in production,
     serving its release. History records the move from the previous deployment.
     */
    @Test(.enabled(if: appsFleetTestsEnabled))
    func `moves an assigned device only after explicit confirmation`() async throws {
        try await AppsDeploymentFixture.withFleet { cli, app, devices, _ in
            try await app.create(cli)
            let a = try await app.publish(cli)
            let canary = try app.target(devices[0], name: "canary")
            let production = try app.target(devices[1], name: "production")
            _ = try await app.succeed(cli, canary.deployCommand(a))
            _ = try await app.succeed(cli, production.deployCommand(a))
            let original = try await canary.snapshot(cli)
            let destination = try await production.snapshot(cli)
            try await app.refuse(
                cli,
                "wendy app deployment update \(production.deployment) --add-device \(canary.asset) --yes \(app.cloudFlag)",
                guidance: "--move"
            )
            let unchanged = try await canary.snapshot(cli)
            #expect(unchanged.deployment.revision == original.deployment.revision)
            #expect(unchanged.currentTargets() == original.currentTargets())
            _ = try await app.succeed(
                cli,
                "wendy app deployment update \(production.deployment) --add-device \(canary.asset) --move --yes \(app.cloudFlag)"
            )
            let moved = try await production.requireHistory(
                cli,
                release: a,
                revision: 2,
                targets: [canary.asset, production.asset],
                sameDeployment: destination.deployment.id
            )
            let moves = moved.history.first { $0.revision.revision == 2 }?.moves ?? []
            #expect(
                moves.contains {
                    $0.deviceId == canary.asset && $0.fromDeploymentId == original.deployment.id
                        && $0.toDeploymentId == destination.deployment.id
                }
            )
            let nowProduction = try app.target(devices[0], name: "production")
            try await nowProduction.requireRunning(
                cli,
                release: a,
                revision: 2,
                version: "1.0.0",
                unit: "celsius"
            )
        }
    }

    /**
     Isolated Agent protocol tier: hold replacement B at a test-only start barrier,
     accept revision C, then let both replacements fail. The real recovery logic
     must retain the last applied A configuration. The runtime peer is synthetic;
     this does not establish the same timing on real containerd.
     */
    @Test(.enabled(if: appsIsolationTestsEnabled))
    func `preserves the correct previous runtime during overlapping starts`() async throws {
        try await AppsIsolationFixture.run("TestAppsOperatorIsolationOverlappingStarts")
    }
}
