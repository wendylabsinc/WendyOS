import Foundation
import Testing
import WendyE2ETesting

@Suite(.serialized)
struct `'wendy app deploy'` {
    /**
     Delivers the published release through the registry to the explicitly
     selected approved device. Revision one asks it to run with the confirmed settings.
     A real HTTP response must identify this attempt, version A and celsius;
     CLI success or a cached older app response cannot satisfy the check.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `runs the published release on the selected device`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            _ = try await app.deployed(cli)
        }
    }

    /**
     Install-only imports the release and leaves it stopped. The endpoint
     must not serve the owned app until a later start revision is applied.
     The explicit CLI wait deadline fits inside the fixture's command guard;
     timing out while pending cannot establish a successful installation.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `installs without starting until explicitly updated`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            try await app.create(cli)
            let release = try await app.publish(cli)
            _ = try await app.succeed(
                cli,
                app.deployCommand(release, run: false, extra: "--wait --wait-timeout 15s")
            )
            let status = try await app.show(cli)
            #expect(status.contains("install only, revision 1."))
            #expect(status.contains("1 installed"))
            let rows = try await app.inventory(cli).filter { $0.name == app.app }
            try #require(rows.count == 1)
            #expect(rows[0].runningState == "STOPPED")
            let response = try await app.response(cli)
            #expect(response?.app != app.app)
            _ = try await app.succeed(
                cli,
                "wendy app deployment update \(app.deployment) --start --yes \(app.cloudFlag)"
            )
            try await app.requireRunning(
                cli,
                release: release,
                revision: 2,
                version: "1.0.0",
                unit: "celsius"
            )
        }
    }

    /**
     Release defaults are overlaid by the JSON settings file, then explicit
     config flags. Both the stored view and the app response show the values.
     The settings file supplies app settings, not a deployment/IaC definition.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `applies file settings and explicit overrides`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            try await app.create(cli)
            let release = try await app.publish(cli)
            try await cli.sh(
                #"printf '%s' '{"DISPLAY_UNIT":"file-value","FILE_ONLY":"from-file"}' > settings.json"#
            )
            _ = try await app.succeed(
                cli,
                app.deployCommand(release, extra: "--config-file settings.json")
            )
            try await app.requireRunning(
                cli,
                release: release,
                revision: 1,
                version: "1.0.0",
                unit: "celsius",
                fileOnly: "from-file"
            )
        }
    }

    /**
     A script must explicitly choose a target and confirm the rollout.
     Refusal leaves the named deployment absent and the Pi without the app.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `requires an explicit target in scripted deployment`() async throws {
        try await refuseScriptedDeployment(omitTarget: true)
    }

    /** A scripted deployment without confirmation creates no deployment. */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `requires confirmation in scripted deployment`() async throws {
        try await refuseScriptedDeployment(omitTarget: false)
    }

    private func refuseScriptedDeployment(
        omitTarget: Bool,
        filePath: String = #filePath,
        function: String = #function,
        line: Int = #line
    ) async throws {
        try await AppsDeploymentFixture.withApp(filePath: filePath, function: function, line: line)
        { cli, app in
            try await app.create(cli)
            let release = try await app.publish(cli)
            try await app.refuse(
                cli,
                app.deployCommand(release, target: !omitTarget, confirm: omitTarget),
                guidance: omitTarget ? "--device" : "--yes"
            )
            try await app.refuse(
                cli,
                "wendy app deployment show \(app.deployment) \(app.cloudFlag)",
                guidance: "no deployment"
            )
            let rows = try await app.inventory(cli)
            #expect(!rows.contains { $0.name == app.app })
        }
    }

    /**
     Malformed settings refuse before a named deployment is created. After
     correcting the file, deployment can proceed with the same chosen name.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `refuses malformed settings without creating a deployment`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            try await app.create(cli)
            let release = try await app.publish(cli)
            try await cli.sh("printf '{broken' > settings.json")
            let failed = try await app.command(
                cli,
                app.deployCommand(release, extra: "--config-file settings.json")
            )
            #expect(failed.status.isFailure)
            #expect(
                failed.readableStderr.contains("settings.json")
                    || failed.readableStderr.contains("invalid character")
            )
            try await app.refuse(
                cli,
                "wendy app deployment show \(app.deployment) \(app.cloudFlag)",
                guidance: "no deployment"
            )
            _ = try await app.succeed(cli, app.deployCommand(release))
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
     Every required release setting must have a nonempty value before a
     deployment is created. Missing E2E_ATTEMPT refuses and leaves both the
     named deployment and the owned device application absent.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `refuses a missing required setting before creating a deployment`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            try await app.create(cli)
            let release = try await app.publish(cli)
            let missing = try app.deployCommand(release).replacingOccurrences(
                of: "--config E2E_ATTEMPT=\(app.attempt) ",
                with: ""
            )
            try await app.refuse(cli, missing, guidance: "E2E_ATTEMPT")
            try await app.refuse(
                cli,
                "wendy app deployment show \(app.deployment) \(app.cloudFlag)",
                guidance: "no deployment"
            )
            let rows = try await app.inventory(cli)
            #expect(!rows.contains { $0.name == app.app })
        }
    }

    /**
     Waiting for a release that cannot start reports failure and exits nonzero.
     A terminal failed-device summary is required, not just a nonzero exit.
     The explicit CLI wait deadline fits inside the fixture's command guard;
     a pending-device timeout must not pass as an application startup failure.
     No successful application response exists.
     */
    @Test(
        .enabled(
            if: appsDeploymentTestsEnabled
                && ProcessInfo.processInfo.environment["WENDY_E2E_APPS_FAILURES"] == "true"
        )
    )
    func `'--wait' fails when the application cannot start`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            try await app.create(cli)
            let release = try await app.publish(
                cli,
                version: "3.0.0",
                layoutKey: "WENDY_E2E_APPS_LAYOUT_FAIL"
            )
            let failed = try await app.command(
                cli,
                app.deployCommand(release, extra: "--wait --wait-timeout 15s")
            )
            try #require(
                !failed.status.description.contains("124"),
                "Wrapper timeout is not a device failure result"
            )
            try #require(!failed.stderr.contains("E2E command timed out"))
            #expect(failed.status.isFailure)
            #expect(
                failed.stdout.contains("1 failed"),
                "Require the CLI's own failed-device summary"
            )
            #expect(failed.stdout.contains(app.app))
            let status = try await app.show(cli)
            #expect(status.contains("1 failed"))
            let response = try await app.response(cli)
            #expect(response?.app != app.app)
        }
    }

    /**
     With --device A --tag private-overlap --site private-workshop, the review
     and stored revision contain the union exactly once per matching asset.
     Each target serves this release and configuration; an unrelated C gets
     no instruction. Use multiple owned assets and private tag/site fixtures.
     */
    @Test(.enabled(if: appsFleetTestsEnabled))
    func `targets the union of devices tags and sites without duplicates`() async throws {
        try await AppsDeploymentFixture.withFleet { cli, app, devices, lease in
            try await app.create(cli)
            let release = try await app.publish(cli)
            let b = try app.target(devices[1])
            let c = try app.target(devices[2])
            let control = try await c.inventory(cli)
            _ = try await app.succeed(
                cli,
                app.deployCommand(
                    release,
                    extra: "--device \(b.asset) --tag \(lease.tag) --site \(lease.site)"
                )
            )
            _ = try await app.requireHistory(
                cli,
                release: release,
                revision: 1,
                targets: [app.asset, b.asset]
            )
            try await app.requireRunning(
                cli,
                release: release,
                revision: 1,
                version: "1.0.0",
                unit: "celsius"
            )
            try await b.requireRunning(
                cli,
                release: release,
                revision: 1,
                version: "1.0.0",
                unit: "celsius"
            )
            let unrelated = try await c.inventory(cli)
            #expect(unrelated == control)
        }
    }

    /**
     A newly declared, never-enrolled compute asset has no verifiable recipient
     key. Deployment to that owned declaration refuses before creating a named
     deployment. Cleanup deletes only the declaration marked as this test's own.
     */
    @Test(
        .enabled(
            if: appsDeploymentTestsEnabled
                && ProcessInfo.processInfo.environment["WENDY_E2E_APPS_CATALOG_FIXTURES"] == "true"
        )
    )
    func `refuses an unverified device key before creating instructions`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            try await app.create(cli)
            let release = try await app.publish(cli)
            let declared = try await app.observe(cli, "unbound-create", as: AppsDeclaredAsset.self)
            try await cli.sh(
                "printf '%s\\n' \(AppsDeploymentFixture.quote("unenrolled-asset=\(declared.id)")) >> owned-resources.txt"
            )
            var primaryError: (any Error)?
            do {
                let command = try app.deployCommand(release).replacingOccurrences(
                    of: "--device \(app.asset)",
                    with: "--device \(declared.id)"
                )
                try await app.refuse(cli, command, guidance: "verifiable registry key")
                let catalog = try await app.catalog(cli)
                #expect(catalog.deployments?.isEmpty == true)
            } catch { primaryError = error }
            do {
                _ = try await app.succeed(
                    cli,
                    app.observerCommand("unbound-delete", arguments: "--asset \(declared.id)")
                )
                struct AssetState: Decodable { let exists: Bool }
                let state = try await app.observe(
                    cli,
                    "unbound-state",
                    arguments: "--asset \(declared.id)",
                    as: AssetState.self
                )
                #expect(!state.exists)
            } catch {
                Issue.record("Declared unbound asset remains: \(declared.id)")
                primaryError = primaryError ?? error
            }
            if let primaryError { throw primaryError }
        }
    }

    /**
     Isolated Agent protocol tier: synthetic invalid signature, target, revision,
     manifest, blob and wrapped-key data cover their respective boundaries.
     Target binding uses unsigned values at the pure validator, with a valid
     own-target control. The acceptance wrapper requires all six existing
     subcases. Any focused benign investigation is separate partial evidence.
     No import/start occurs and the prior working runtime-peer state remains.
     This is not live Cloud, cross-target delivery or containerd validation.
     */
    @Test(.enabled(if: appsIsolationTestsEnabled))
    func `refuses instructions and artifacts it cannot verify`() async throws {
        try await AppsIsolationFixture.run("TestAppsOperatorIsolationBadDataRefused")
    }
    /**
     The verified dedicated Pi is offline when its deployment is recorded.
     Cloud immediately stores the exact desired release and target, while the
     CLI reports waiting. Starting its normal Agent lets it catch up to that
     same revision, with a fresh report and independent HTTP response.
     */
    @Test(.enabled(if: appsDedicatedDeviceTestsEnabled))
    func `catches up an offline target when its Agent reconnects`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            try await app.create(cli)
            let release = try await app.publish(cli)
            try await app.withStoppedService(cli, .agent) {
                _ = try await app.succeed(cli, app.deployCommand(release))
                let desired = try await app.requireHistory(cli, release: release, revision: 1)
                #expect(desired.states.allSatisfy { $0.state.outcome != "APPLY_OUTCOME_APPLIED" })
                let status = try await app.show(cli)
                #expect(status.contains("waiting"))
                let response = try await app.response(cli)
                #expect(response?.app != app.app)
            }
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
     The same release starts on A and deliberately fails on B using the tiny
     fixture app's declared boot-id condition. A script waiting for both must
     return its own failed-device summary and nonzero exit while preserving
     A's successful response. A wrapper timeout cannot satisfy the test.
     */
    @Test(
        .enabled(
            if: appsFleetTestsEnabled && appsDedicatedDeviceTestsEnabled
                && ProcessInfo.processInfo.environment["WENDY_E2E_APPS_FAILURES"] == "true"
        )
    )
    func `'--wait' reports the successful and failed devices in a mixed rollout`() async throws {
        try await AppsDeploymentFixture.withFleet { cli, app, devices, _ in
            try await app.create(cli)
            let release = try await app.publish(cli)
            let b = try app.target(devices[1])
            let aBoot = try await app.bootID(cli)
            let bBoot = try await b.bootID(cli)
            try #require(aBoot != bBoot)
            let result = try await app.command(
                cli,
                app.deployCommand(
                    release,
                    extra: "--device \(b.asset) --config E2E_FAIL_ON_BOOT_ID=\(bBoot) --wait"
                )
            )
            try #require(!result.status.description.contains("124"))
            try #require(!result.stderr.contains("E2E command timed out"))
            #expect(result.status.isFailure)
            #expect(result.stdout.contains("1 running"))
            #expect(result.stdout.contains("1 failed"))
            let snapshot = try await app.requireHistory(
                cli,
                release: release,
                revision: 1,
                targets: [app.asset, b.asset]
            )
            let failed = try #require(snapshot.states.first { $0.assetId == b.asset })
            #expect(failed.state.outcome == "APPLY_OUTCOME_APPLY_FAILED")
            try #require(!failed.assetName.isEmpty)
            #expect(result.stdout.contains(failed.assetName))
            try await app.requireRunning(
                cli,
                release: release,
                revision: 1,
                version: "1.0.0",
                unit: "celsius"
            )
            let response = try await b.response(cli)
            #expect(response?.app != b.app)
        }
    }

}
