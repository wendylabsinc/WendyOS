import Foundation
import Testing
import WendyE2ETesting

@Suite(.serialized)
struct `'wendy app deployment rollback'` {
    /**
     Cloud records A/celsius, B/celsius and B/kelvin as revisions one through
     three. Rollback --to 1 --note restores A/celsius as revision four of the
     same deployment and target. Read the complete authored history, settings,
     desired state, rollback link and note independently of device acknowledgment.
     The separate device-response spec verifies what eventually runs.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `records rollback history and restored settings in Cloud`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            try await app.create(cli)
            let releaseA = try await app.publish(cli)
            _ = try await app.succeed(cli, app.deployCommand(releaseA))
            let first = try await app.requireHistory(cli, release: releaseA, revision: 1)
            let releaseB = try await app.publish(
                cli,
                version: "2.0.0",
                layoutKey: "WENDY_E2E_APPS_LAYOUT_B"
            )
            _ = try await app.succeed(
                cli,
                "wendy app deployment update \(app.deployment) --release \(releaseB.id) --yes \(app.cloudFlag)"
            )
            _ = try await app.requireHistory(
                cli,
                release: releaseB,
                revision: 2,
                sameDeployment: first.deployment.id
            )
            _ = try await app.succeed(
                cli,
                "wendy app deployment update \(app.deployment) --config DISPLAY_UNIT=kelvin --yes \(app.cloudFlag)"
            )
            _ = try await app.requireHistory(
                cli,
                release: releaseB,
                revision: 3,
                sameDeployment: first.deployment.id
            )
            _ = try await app.succeed(
                cli,
                "wendy app deployment rollback \(app.deployment) --to 1 --note 'Restore A and its units' --yes \(app.cloudFlag)"
            )
            let restored = try await app.requireHistory(
                cli,
                release: releaseA,
                revision: 4,
                sameDeployment: first.deployment.id
            )
            #expect(restored.history.count == 4)
            #expect(restored.deployment.removedAt == nil)
            #expect(restored.deployment.configJson == first.deployment.configJson)
            #expect(restored.deployment.desiredState == first.deployment.desiredState)
            for revision in 1...4 {
                let row = try #require(restored.history.first { $0.revision.revision == revision })
                #expect(row.devices == [app.asset])
                #expect(row.moves.isEmpty)
                #expect(!row.revision.removed)
                #expect(row.revision.desiredState == "RUNNING")
                #expect(
                    row.revision.appReleaseId
                        == ([1, 4].contains(revision) ? releaseA.id : releaseB.id)
                )
                #expect(row.revision.rollbackOf == (revision == 4 ? 1 : 0))
                let settings = try JSONDecoder().decode(
                    [String: String].self,
                    from: Data(row.revision.configJson.utf8)
                )
                #expect(settings["DISPLAY_UNIT"] == (revision == 3 ? "kelvin" : "celsius"))
                #expect(settings["E2E_ATTEMPT"] == app.attempt)
            }
            #expect(
                restored.history.first { $0.revision.revision == 4 }?.revision.note
                    == "Restore A and its units"
            )
        }
    }

    /**
     After A, B and a settings change, rollback restores A and its settings as
     revision four of the same deployment UUID. Exact targets, authored history,
     rollback linkage and the intervening revisions remain. The independent HTTP
     response again serves A with celsius.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `restores earlier content as a new revision`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            let releaseA = try await app.deployed(cli)
            let first = try await app.snapshot(cli)
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
                "wendy app deployment update \(app.deployment) --config DISPLAY_UNIT=kelvin --yes \(app.cloudFlag)"
            )
            try await app.requireRunning(
                cli,
                release: releaseB,
                revision: 3,
                version: "2.0.0",
                unit: "kelvin"
            )
            _ = try await app.succeed(
                cli,
                "wendy app deployment rollback \(app.deployment) --to 1 --note 'Restore A and its units' --yes \(app.cloudFlag)"
            )
            try await app.requireRunning(
                cli,
                release: releaseA,
                revision: 4,
                version: "1.0.0",
                unit: "celsius"
            )
            let restored = try await app.requireHistory(
                cli,
                release: releaseA,
                revision: 4,
                sameDeployment: first.deployment.id
            )
            #expect(restored.history.first { $0.revision.revision == 4 }?.revision.rollbackOf == 1)
            #expect(
                restored.history.first { $0.revision.revision == 2 }?.revision.appReleaseId
                    == releaseB.id
            )
            #expect(
                restored.history.first { $0.revision.revision == 4 }?.revision.note
                    == "Restore A and its units"
            )
            #expect(
                restored.history.first { $0.revision.revision == 1 }?.revision.configJson
                    == restored.deployment.configJson
            )
            #expect(
                restored.history.first { $0.revision.revision == 1 }?.revision.desiredState
                    == restored.deployment.desiredState
            )
        }
    }
    /**
     A nonexistent rollback revision is refused, leaving both the current
     deployment and history unchanged. The actual app continues serving B.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `refuses an unknown rollback revision without changing the running app`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            _ = try await app.deployed(cli)
            let b = try await app.publish(
                cli,
                version: "2.0.0",
                layoutKey: "WENDY_E2E_APPS_LAYOUT_B"
            )
            _ = try await app.succeed(
                cli,
                "wendy app deployment update \(app.deployment) --release \(b.id) --yes \(app.cloudFlag)"
            )
            try await app.requireRunning(
                cli,
                release: b,
                revision: 2,
                version: "2.0.0",
                unit: "celsius"
            )
            let before = try await app.snapshot(cli)
            // At revision two, only revision one is a valid earlier target.
            try await app.refuse(
                cli,
                "wendy app deployment rollback \(app.deployment) --to 9 --yes \(app.cloudFlag)",
                guidance: "--to <1-1>"
            )
            let after = try await app.snapshot(cli)
            #expect(after.deployment.id == before.deployment.id)
            #expect(after.deployment.revision == before.deployment.revision)
            #expect(after.history.count == before.history.count)
            #expect(after.history.allSatisfy { $0.revision.rollbackOf == 0 })
            try await app.requireRunning(
                cli,
                release: b,
                revision: 2,
                version: "2.0.0",
                unit: "celsius"
            )
        }
    }

}
