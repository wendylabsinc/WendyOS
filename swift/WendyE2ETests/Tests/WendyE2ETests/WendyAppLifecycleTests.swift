import Foundation
import Testing
import WendyE2ETesting

@Suite(.serialized)
struct `'wendy app lifecycle'` {
    /**
     Shows inherited policy, saves a permitted app override and restores the
     organization default. It never changes the organization's own settings.
     The separate natural-sweep case observes eventual protected-release retention.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `saves an app override and restores the organization default`() async throws {
        try await AppsDeploymentFixture.withCloudApp { cli, app in
            try await app.create(cli)
            let inherited = try await app.succeed(
                cli,
                "wendy app lifecycle \(app.app) \(app.cloudFlag)"
            )
            #expect(inherited.stdout.contains("Organization default"))
            _ = try await app.succeed(
                cli,
                "wendy app lifecycle \(app.app) --keep-tagged 1 --yes \(app.cloudFlag)"
            )
            let own = try await app.succeed(cli, "wendy app lifecycle \(app.app) \(app.cloudFlag)")
            #expect(own.stdout.contains("Own policy"))
            #expect(own.stdout.contains("Tagged releases: keep the last 1."))
            _ = try await app.succeed(
                cli,
                "wendy app lifecycle \(app.app) --org-default --yes \(app.cloudFlag)"
            )
            let restored = try await app.succeed(
                cli,
                "wendy app lifecycle \(app.app) \(app.cloudFlag)"
            )
            #expect(restored.stdout == inherited.stdout)
        }
    }

    /**
     Retention counts below one refuse without changing the effective app
     policy. A subsequent read must return the same policy as before.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `refuses an invalid count without changing policy`() async throws {
        try await AppsDeploymentFixture.withCloudApp { cli, app in
            try await app.create(cli)
            let before = try await app.succeed(
                cli,
                "wendy app lifecycle \(app.app) \(app.cloudFlag)"
            )
            try await app.refuse(
                cli,
                "wendy app lifecycle \(app.app) --keep-tagged 0 --yes \(app.cloudFlag)",
                guidance: "1 or more"
            )
            let after = try await app.succeed(
                cli,
                "wendy app lifecycle \(app.app) \(app.cloudFlag)"
            )
            #expect(after.stdout == before.stdout)
        }
    }

    /**
     A count above the service's actual effective organization limit refuses.
     The policy and active release remain unchanged. The separate natural-sweep
     case establishes retention after an eligible control is actually removed.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `refuses counts above the actual organization limit`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            let release = try await app.deployed(cli)
            let catalog = try await app.catalog(cli)
            let limit = try #require(
                catalog.app?.lifecyclePolicyLimit?.keepTagged,
                "The service must expose its effective limit"
            )
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
            // This does not claim to have advanced the hourly lifecycle sweep.
        }
    }
    /**
     A natural lifecycle sweep removes an eligible old unused release while
     retaining both the older in-use release and the newest unused release.
     Observe actual catalog deletion and the still-responding app; no admin
     sweep trigger, simulated clock or pre-recorded response is used.
     */
    @Test(
        .enabled(
            if: appsDeploymentTestsEnabled
                && ProcessInfo.processInfo.environment["WENDY_E2E_APPS_NATURAL_SWEEP"] == "true"
        )
    )
    func `retains an in-use release when a natural sweep removes an eligible control`() async throws
    {
        try await AppsDeploymentFixture.withApp { cli, app in
            let active = try await app.deployed(cli)
            let eligible = try await app.publish(
                cli,
                version: "2.0.0",
                layoutKey: "WENDY_E2E_APPS_LAYOUT_B"
            )
            let newest = try await app.publish(
                cli,
                version: "2.0.1",
                layoutKey: "WENDY_E2E_APPS_LAYOUT_B"
            )
            let before = try await app.catalog(cli)
            try #require(before.releases?.contains { $0.id == eligible.id } == true)
            _ = try await app.succeed(
                cli,
                "wendy app lifecycle \(app.app) --keep-tagged 1 --yes \(app.cloudFlag)"
            )
            let configured =
                ProcessInfo.processInfo.environment["WENDY_E2E_APPS_SWEEP_TIMEOUT_SECONDS"]
                ?? "7500"
            let seconds = try #require(Int(configured))
            try #require(
                (3600...10800).contains(seconds),
                "Allow at least one hourly sweep, at most three hours"
            )
            let deadline = ContinuousClock.now.advanced(by: .seconds(seconds))
            var controlRemoved = false
            repeat {
                let catalog = try await app.catalog(cli)
                let rows = try #require(catalog.releases)
                try #require(
                    rows.contains { $0.id == active.id },
                    "In-use release must survive every observed sweep state"
                )
                try #require(
                    rows.contains { $0.id == newest.id },
                    "Newest retained release must survive"
                )
                if !rows.contains(where: { $0.id == eligible.id }) {
                    controlRemoved = true
                    break
                }
                try await Task.sleep(for: .seconds(30))
            } while ContinuousClock.now < deadline
            try #require(
                controlRemoved,
                "No eligible control was removed; retention was not established"
            )
            try await app.requireRunning(
                cli,
                release: active,
                revision: 1,
                version: "1.0.0",
                unit: "celsius"
            )
        }
    }

}
