import Testing
import WendyE2ETesting

@Suite(.serialized)
struct `'wendy app delete'` {
    /**
     Deleting a deployed app removes its Cloud catalog and separately removes
     its running application. Catalog deletion is never accepted as proof of
     device cleanup. Unrelated device applications survive fixture cleanup.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `deletes the catalog and removes the running application`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            let release = try await app.deployed(cli)
            _ = try await app.succeed(cli, "wendy app delete \(app.app) --yes \(app.cloudFlag)")
            try await app.refuse(
                cli,
                "wendy app lifecycle \(app.app) \(app.cloudFlag)",
                guidance: "No app \(app.app)"
            )
            try await app.requireAbsent(cli)
            try await app.requireCatalogDeleted(cli)
            try await app.requireArtifactUnavailable(cli, releaseID: release.id)
        }
    }

    /**
     Detach during app deletion preserves the Pi's current app, version,
     configuration and run state. The warning explains that decrypted image
     and configuration remain. Cleanup explicitly removes this surviving app.
     Direct-run reporting and ended registry pull rights need service reads.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `'--detach' leaves the application responding after catalog deletion`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            let release = try await app.deployed(cli)
            // Record before the operation so interrupted detach is also cleaned.
            try await cli.sh("touch detached-owned-app")
            let result = try await app.succeed(
                cli,
                "wendy app delete \(app.app) --detach \(app.asset) --yes \(app.cloudFlag)"
            )
            #expect(result.stdout.contains("image and its configuration"))
            #expect(result.stdout.contains("direct run"))
            try await app.refuse(
                cli,
                "wendy app lifecycle \(app.app) \(app.cloudFlag)",
                guidance: "No app \(app.app)"
            )
            try await app.requireCatalogDeleted(cli)
            try await app.requireDetached(cli, release: release)
            try await app.requireArtifactUnavailable(cli, releaseID: release.id)
            let response = try #require(try await app.response(cli))
            #expect(response.app == app.app)
            #expect(response.attempt == app.attempt)
            #expect(response.version == "1.0.0")
            #expect(response.unit == "celsius")
            let rows = try await app.inventory(cli).filter { $0.name == app.app }
            try #require(rows.count == 1)
            #expect(rows[0].runningState == "RUNNING")
        }
    }

    /**
     Interrupt deletion after a subset of its removals/detaches are accepted,
     then repeat delete app --detach UUID --yes. Remaining batches finish
     without duplicate effects. Removed devices lose the app; detached devices
     retain it as a direct run across an explicitly permitted restart. Former
     deployment instructions cannot reattach it. Catalog rows and registry
     access disappear, while physical orphan-object deletion is periodic.
     */
    @Test(.enabled(if: appsFleetTestsEnabled && appsRebootTestsEnabled))
    func `resumes interrupted deletion and preserves detached direct runs`() async throws {
        try await AppsDeploymentFixture.withFleet { cli, app, devices, _ in
            try await app.create(cli)
            let release = try await app.publish(cli)
            let b = try app.target(devices[1])
            _ = try await app.succeed(cli, app.deployCommand(release, extra: "--device \(b.asset)"))
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
            let partial = try await app.observe(cli, "partial-delete", as: AppsRPCFailure.self)
            #expect(partial.code == "FailedPrecondition")
            let unfinished = try await app.catalog(cli)
            #expect(unfinished.app != nil)
            try await cli.sh("touch detached-owned-app")
            _ = try await app.succeed(
                cli,
                "wendy app delete \(app.app) --detach \(b.asset) --yes \(app.cloudFlag)"
            )
            try await app.requireCatalogDeleted(cli)
            try await app.requireAbsent(cli)
            try await b.requireDetached(cli, release: release)
            try await b.rebootDedicatedDevice(cli)
            try await b.requireResponse(cli, version: "1.0.0", unit: "celsius")
            try await b.requireDetached(cli, release: release)
            // The surviving direct run is on B, so remove that exact app there.
            _ = try await b.succeed(cli, "\(b.deviceCommand) remove \(b.app) --force --cleanup")
            try await b.requireAbsent(cli)
        }
    }
    /**
     With its Agent offline, catalog deletion can finish while the container
     still serves its previous response. Only after reconnect and independent
     runtime absence may device removal count as complete.
     */
    @Test(.enabled(if: appsDedicatedDeviceTestsEnabled))
    func `keeps catalog deletion distinct from pending device removal`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            _ = try await app.deployed(cli)
            try await app.withStoppedService(cli, .agent) {
                _ = try await app.succeed(cli, "wendy app delete \(app.app) --yes \(app.cloudFlag)")
                try await app.requireCatalogDeleted(cli)
                try await app.requireResponse(cli, version: "1.0.0", unit: "celsius")
            }
            try await app.requireAbsent(cli)
        }
    }

}
