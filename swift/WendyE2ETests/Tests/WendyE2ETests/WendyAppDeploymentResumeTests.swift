import Foundation
import Testing
import WendyE2ETesting

@Suite
struct `'wendy app deployment resume'` {
    /**
     Interrupt a delivery after Cloud records its revision but before all
     per-device statements arrive. Run resume app/workshop --yes, review only
     that revision's pending content and finish delivery. The same deployment
     and revision remain; each device eventually serves the intended settings.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `finishes interrupted delivery without creating a new revision`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            let release = try await app.deployed(cli)
            // A public config-only update stores revision two without sending
            // assignments. The existing deployment key is still usable by
            // resume; no process-kill timing guess is required.
            _ = try await app.succeed(cli, app.observerCommand("plan-update"))
            let pending = try await app.snapshot(cli)
            try #require(!pending.pending.isEmpty)
            #expect(pending.deployment.revision == 2)
            _ = try await app.succeed(
                cli,
                "wendy app deployment resume \(app.deployment) --yes \(app.cloudFlag)"
            )
            try await app.requireRunning(
                cli,
                release: release,
                revision: 2,
                version: "1.0.0",
                unit: "kelvin"
            )
            let resumed = try await app.snapshot(cli)
            #expect(resumed.deployment.id == pending.deployment.id)
            #expect(resumed.deployment.revision == pending.deployment.revision)
            #expect(resumed.history.count == pending.history.count)
            #expect(resumed.pending.isEmpty)
        }
    }

    /**
     A stored newer revision cannot be overwritten using an earlier expected
     revision. The public signed update refuses and history remains intact.
     The separate isolated case below covers the resume confirmation barrier.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `preserves a newer revision when a stale change is refused`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            _ = try await app.deployed(cli)
            let previous = try await app.snapshot(cli)
            _ = try await app.succeed(
                cli,
                "wendy app deployment update \(app.deployment) --config DISPLAY_UNIT=kelvin --yes \(app.cloudFlag)"
            )
            let newer = try await app.snapshot(cli)
            let stale = try await app.command(
                cli,
                app.observerCommand(
                    "stale-update",
                    arguments: "--expected-revision \(previous.deployment.revision)"
                )
            )
            #expect(stale.status.isFailure)
            let refusal = try JSONDecoder().decode(
                AppsRPCFailure.self,
                from: Data(stale.stdout.utf8)
            )
            #expect(["Aborted", "FailedPrecondition"].contains(refusal.code))
            let after = try await app.snapshot(cli)
            #expect(after.deployment.revision == newer.deployment.revision)
            #expect(after.history.count == newer.history.count)
        }
    }
    /**
     Isolated CLI signing tier: scripted resume confirms pending revision N.
     A forwarding client barrier commits N+1 immediately before current-key
     loading and signPending. Resume may refuse or sign N's original content;
     it must never sign N+1 under the earlier confirmation. This exercises the
     actual resume path, independently of the stale-update RPC test.
     */
    @Test(.enabled(if: appsIsolationTestsEnabled))
    func `never signs a later revision under an earlier resume confirmation`() async throws {
        try await AppsIsolationFixture.run("TestAppsOperatorIsolationResumeConfirmationRace")
    }

}
