import Foundation
import Testing
import WendyE2ETesting

@Suite(.serialized)
struct `'wendy app deployment show'` {
    /**
     Looks up the exact app/name pair and shows its desired release, revision
     and effective configuration alongside the device result. The application
     independently confirms the version and settings shown as running.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `shows the named deployment and its responding application`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            let release = try await app.deployed(cli)
            let status = try await app.show(cli)
            #expect(
                status.contains(
                    "\(app.deployment): release \(release.tag), run it now, revision 1."
                )
            )
            #expect(status.contains("to 1 device"))
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
     A bare deployment name is insufficient. The CLI requests the exact
     app/name pair; fuzzy matching is an open idea rather than a requirement.
     */
    @Test(.enabled(if: appsDeploymentLocalTestsEnabled))
    func `requires an exact app and deployment pair`() async throws {
        try await CLIAndAgentScenario().run(authenticated: false) { cli, _ in
            try await cli.sh("wendy app deployment show workshop </dev/null") { result in
                #expect(result.status.isFailure)
                #expect(result.readableStderr.contains("<app>/<name>"))
            }
        }
    }

    /**
     While the Pi is offline or a release is failing, desired content and the
     last reported content remain distinct. Match the exact asset row, release,
     digest and entry/correlation to Cloud reads. Never accept no rows, an older
     APPLIED report, or a newer timestamp alone as completion of a new revision.
     Two apps with a deployment named workshop resolve to their own targets.
     */
    @Test(
        .enabled(
            if: appsDeploymentTestsEnabled
                && ProcessInfo.processInfo.environment["WENDY_E2E_APPS_FAILURES"] == "true"
        )
    )
    func `distinguishes desired content from delayed and failed reports`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            let a = try await app.deployed(cli)
            let failed = try await app.publish(
                cli,
                version: "3.0.0",
                layoutKey: "WENDY_E2E_APPS_LAYOUT_FAIL"
            )
            _ = try await app.succeed(
                cli,
                "wendy app deployment update \(app.deployment) --release \(failed.id) --yes \(app.cloudFlag)"
            )
            let deadline = ContinuousClock.now.advanced(by: .seconds(120))
            var observed = false
            repeat {
                let snapshot = try await app.snapshot(cli)
                let row = snapshot.states.first { $0.assetId == app.asset }
                let response = try await app.response(cli)
                observed =
                    snapshot.deployment.appReleaseId == failed.id
                    && snapshot.deployment.revision == 2
                    && row?.state.outcome == "APPLY_OUTCOME_APPLY_FAILED"
                    && row?.state.reportedReleaseId == a.id
                    && response?.version == "1.0.0" && response?.unit == "celsius"
                if observed { break }
                try await Task.sleep(for: .seconds(2))
            } while ContinuousClock.now < deadline
            #expect(observed, "Desired failure must not replace the last working reported release")
        }
    }
    /**
     Two independent apps may both have a deployment named workshop. Each exact
     app/name lookup returns its own deployment UUID, release and device report;
     both real endpoints continue serving their own application identities.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `keeps identically named deployments of different apps distinct`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            let a = try await app.deployed(cli)
            let first = try await app.snapshot(cli)
            try await app.withRelatedApp(cli) { other in
                try await other.writeProject(cli)
                let b = try await other.publish(
                    cli,
                    version: "2.0.0",
                    layoutKey: "WENDY_E2E_APPS_LAYOUT_B"
                )
                _ = try await other.succeed(cli, other.deployCommand(b))
                try await other.requireRunning(
                    cli,
                    release: b,
                    revision: 1,
                    version: "2.0.0",
                    unit: "celsius"
                )
                let second = try await other.snapshot(cli)
                #expect(second.deployment.id != first.deployment.id)
                #expect(second.deployment.appId == other.app)
                #expect(first.deployment.appId == app.app)
                try await app.requireRunning(
                    cli,
                    release: a,
                    revision: 1,
                    version: "1.0.0",
                    unit: "celsius"
                )
            }
        }
    }

}
