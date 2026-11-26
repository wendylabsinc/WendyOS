import Foundation
import Testing
import WendyE2ETesting

@Suite
struct `'Console Apps and Deployments'` {
    /**
     In the same Dev organization, visit Apps and the owned app's Overview,
     Deployments, Releases, Access and Settings. After CLI creation/publish,
     rendered names, app id, release version and owner/access match the owned
     resources; creation first shows No release yet. Check the independent
     access/lifecycle views before publication. Capture rendered evidence
     at the same ids. A successful backing RPC is not a browser observation.
     */
    @Test(.enabled(if: appsBrowserTestsEnabled))
    func `shows the same app release access and lifecycle as the CLI journey`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            try await app.create(cli)
            let created = try await app.console(cli, path: "/apps/\(app.app)/overview")
            #expect(created.text.contains(app.app))
            #expect(created.text.contains("No release yet"))
            let access = try await app.console(cli, path: "/apps/\(app.app)/access")
            #expect(access.text.contains("App owner"))
            _ = try await app.succeed(
                cli,
                "wendy app lifecycle \(app.app) --keep-tagged 1 --yes \(app.cloudFlag)"
            )
            let settings = try await app.pollConsole(cli, path: "/apps/\(app.app)/settings") {
                dom in
                dom.facts.contains {
                    $0.panel == "Lifecycle policy" && $0.term.lowercased() == "tagged releases"
                        && $0.value == "Keep the last 1."
                }
            }
            #expect(
                settings.facts.contains {
                    $0.panel == "Lifecycle policy" && $0.term.lowercased() == "tagged releases"
                        && $0.value == "Keep the last 1."
                }
            )
            let cliPolicy = try await app.succeed(
                cli,
                "wendy app lifecycle \(app.app) \(app.cloudFlag)"
            )
            #expect(cliPolicy.stdout.contains("keep the last 1"))
            let release = try await app.publish(cli)
            let releases = try await app.pollConsole(cli, path: "/apps/\(app.app)/releases") {
                $0.rows.contains {
                    $0.titles.contains(release.tag) && $0.titles.contains(release.digest)
                }
            }
            #expect(
                releases.rows.contains {
                    $0.titles.contains(release.tag) && $0.titles.contains(release.digest)
                }
            )
        }
    }

    /**
     After deploy, release update, configuration change and rollback, visit
     deployment Overview, Devices and History. Check one exact target, desired
     and reported release, current revision and retained authored history against
     the service observations and the app's HTTP response. A sent instruction,
     an empty row list or stale APPLIED report is never rendered as completion.
     */
    @Test(.enabled(if: appsBrowserTestsEnabled))
    func `distinguishes rollout intent from the devices actual result`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            let a = try await app.deployed(cli)
            let first = try await app.snapshot(cli)
            try await app.requireConsoleDevice(
                cli,
                deploymentID: first.deployment.id,
                release: a,
                state: "running"
            )
            let b = try await app.publish(
                cli,
                version: "2.0.0",
                layoutKey: "WENDY_E2E_APPS_LAYOUT_B"
            )
            _ = try await app.succeed(
                cli,
                "wendy app deployment update \(app.deployment) --release \(b.id) --note 'Install B' --yes \(app.cloudFlag)"
            )
            try await app.requireRunning(
                cli,
                release: b,
                revision: 2,
                version: "2.0.0",
                unit: "celsius"
            )
            try await app.requireConsoleDevice(
                cli,
                deploymentID: first.deployment.id,
                release: b,
                state: "running"
            )
            _ = try await app.succeed(
                cli,
                "wendy app deployment update \(app.deployment) --config DISPLAY_UNIT=kelvin --note 'Change units' --yes \(app.cloudFlag)"
            )
            try await app.requireRunning(
                cli,
                release: b,
                revision: 3,
                version: "2.0.0",
                unit: "kelvin"
            )
            let config = try await app.pollConsole(
                cli,
                path: "/deployments/\(first.deployment.id)/overview"
            ) { $0.text.contains("kelvin") }
            #expect(config.text.contains("kelvin"))
            _ = try await app.succeed(
                cli,
                "wendy app deployment rollback \(app.deployment) --to 1 --note 'Restore A' --yes \(app.cloudFlag)"
            )
            try await app.requireRunning(
                cli,
                release: a,
                revision: 4,
                version: "1.0.0",
                unit: "celsius"
            )
            try await app.requireConsoleDevice(
                cli,
                deploymentID: first.deployment.id,
                release: a,
                state: "running"
            )
            let history = try await app.pollConsole(
                cli,
                path: "/deployments/\(first.deployment.id)/history"
            ) { $0.rows.count == 4 && $0.text.contains("Rolled back to change 1") }
            #expect(history.rows.count == 4)
            #expect(history.text.contains("Install B"))
            #expect(history.text.contains("Change units"))
            #expect(history.text.contains("Rolled back to change 1"))
        }
    }

    /**
     Removal renders a removed deployment and retained removal history.
     After catalog deletion, a proven loaded, complete Apps catalog omits the
     owned app. A sign-in, error or loading screen cannot prove absence.
     */
    @Test(.enabled(if: appsBrowserTestsEnabled))
    func `shows removed history and catalog deletion truthfully`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            _ = try await app.deployed(cli)
            let running = try await app.snapshot(cli)
            _ = try await app.succeed(
                cli,
                "wendy app deployment remove \(app.deployment) --yes \(app.cloudFlag)"
            )
            try await app.requireAbsent(cli)
            let removed = try await app.pollConsole(
                cli,
                path: "/deployments/\(running.deployment.id)/overview"
            ) { $0.text.contains("This deployment was removed") }
            #expect(removed.text.contains("This deployment was removed"))
            let history = try await app.pollConsole(
                cli,
                path: "/deployments/\(running.deployment.id)/history"
            ) { $0.text.contains("Removed from every device") }
            #expect(history.text.contains("Removed from every device"))
            _ = try await app.succeed(cli, "wendy app delete \(app.app) --yes \(app.cloudFlag)")
            try await app.requireCatalogDeleted(cli)
            let catalog = try await app.pollConsole(cli, path: "/apps") {
                !$0.text.contains(app.app)
            }
            #expect(!catalog.links.contains("/apps/\(app.app)/overview"))
            #expect(!catalog.text.contains(app.app))
        }
    }
    /**
     After deletion with detach, the device page renders the surviving owned
     application as Run directly. The catalog omits the deleted app, and the
     same independently observed version and configuration remain responsive.
     */
    @Test(.enabled(if: appsBrowserTestsEnabled))
    func `shows the surviving detached application as a direct run`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            let release = try await app.deployed(cli)
            try await cli.sh("touch detached-owned-app")
            _ = try await app.succeed(
                cli,
                "wendy app delete \(app.app) --detach \(app.asset) --yes \(app.cloudFlag)"
            )
            try await app.requireCatalogDeleted(cli)
            try await app.requireDetached(cli, release: release)
            try await app.requireResponse(cli, version: "1.0.0", unit: "celsius")
            let device = try await app.pollConsole(cli, path: "/assets/\(app.asset)") { dom in
                dom.rows.contains {
                    $0.cells.joined(separator: " ").contains(app.app)
                        && $0.cells.joined(separator: " ").contains("Run directly")
                }
            }
            let rows = device.rows.filter { $0.cells.joined(separator: " ").contains(app.app) }
            try #require(rows.count == 1)
            #expect(rows[0].cells.joined(separator: " ").contains("Run directly"))
            let catalog = try await app.pollConsole(cli, path: "/apps") {
                !$0.text.contains(app.app)
            }
            #expect(!catalog.text.contains(app.app))
        }
    }

    /**
     A first deployment recorded while its Agent is offline renders the desired
     release, no reported release, Not reported yet and Never. This is the fresh
     assignment's waiting state; Waiting: offline requires a stale prior report.
     After reconnect, the full runtime/HTTP assertion and bounded DOM polling
     must observe that same asset running the release.
     */
    @Test(.enabled(if: appsBrowserTestsEnabled && appsDedicatedDeviceTestsEnabled))
    func `shows an offline target waiting and then running after catch-up`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            try await app.create(cli)
            let release = try await app.publish(cli)
            try await app.withStoppedService(cli, .agent) {
                _ = try await app.succeed(cli, app.deployCommand(release))
                let snapshot = try await app.snapshot(cli)
                let waiting = try await app.pollConsole(
                    cli,
                    path: "/deployments/\(snapshot.deployment.id)/devices"
                ) { dom in
                    dom.rows.contains {
                        $0.links.contains("/assets/\(app.asset)") && $0.cells.count >= 5
                            && $0.cells[1] == release.tag && $0.cells[2] == "--"
                            && $0.cells[3] == "Not reported yet" && $0.cells[4] == "Never"
                    }
                }
                #expect(waiting.rows.count == 1)
            }
            try await app.requireRunning(
                cli,
                release: release,
                revision: 1,
                version: "1.0.0",
                unit: "celsius"
            )
            let applied = try await app.snapshot(cli)
            try await app.requireConsoleDevice(
                cli,
                deploymentID: applied.deployment.id,
                release: release,
                state: "running"
            )
        }
    }

    /**
     A failed replacement remains visibly distinct from the previous reported
     release and responding application. The DOM row shows the requested failed
     version, the last working version and a failure state for the exact asset.
     */
    @Test(
        .enabled(
            if: appsBrowserTestsEnabled
                && ProcessInfo.processInfo.environment["WENDY_E2E_APPS_FAILURES"] == "true"
        )
    )
    func `shows a failed replacement separately from the working release`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            let working = try await app.deployed(cli)
            let failure = try await app.publish(
                cli,
                version: "3.0.0",
                layoutKey: "WENDY_E2E_APPS_LAYOUT_FAIL"
            )
            let before = try await app.snapshot(cli)
            _ = try await app.succeed(
                cli,
                "wendy app deployment update \(app.deployment) --release \(failure.id) --yes \(app.cloudFlag)"
            )
            _ = try await app.pollConsole(cli, path: "/deployments/\(before.deployment.id)/devices")
            { dom in
                dom.rows.contains {
                    $0.links.contains("/assets/\(app.asset)") && $0.cells.count >= 4
                        && $0.cells[1] == failure.tag && $0.cells[2] == working.tag
                        && $0.cells[3].lowercased().contains("failed")
                }
            }
            try await app.requireResponse(cli, version: "1.0.0", unit: "celsius")
        }
    }

    /**
     Default catalog deletion while the Agent is offline must not erase the
     device's last reported app or relabel it as a direct run. After reconnect
     and real removal, the loaded device Apps section omits the owned app.
     */
    @Test(.enabled(if: appsBrowserTestsEnabled && appsDedicatedDeviceTestsEnabled))
    func `shows pending device removal separately from catalog deletion`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            _ = try await app.deployed(cli)
            try await app.withStoppedService(cli, .agent) {
                _ = try await app.succeed(cli, "wendy app delete \(app.app) --yes \(app.cloudFlag)")
                try await app.requireCatalogDeleted(cli)
                try await app.requireResponse(cli, version: "1.0.0", unit: "celsius")
                let pending = try await app.pollConsole(cli, path: "/assets/\(app.asset)") { dom in
                    dom.rows.contains { $0.cells.joined(separator: " ").contains(app.app) }
                }
                let row = try #require(
                    pending.rows.first { $0.cells.joined(separator: " ").contains(app.app) }
                )
                #expect(!row.cells.joined(separator: " ").contains("Run directly"))
            }
            try await app.requireAbsent(cli)
            _ = try await app.pollConsole(cli, path: "/assets/\(app.asset)") { dom in
                !dom.rows.contains { $0.cells.joined(separator: " ").contains(app.app) }
            }
        }
    }

}
