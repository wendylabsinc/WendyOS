import Foundation
import Testing
import WendyE2ETesting

@Suite(.serialized)
struct `'Cloud deployment intentions'` {
    /**
     Publish A and deploy with file settings and explicit overrides. Publish B,
     update only the release, then change the configuration. Cloud retains the
     exact target, authored history and merged settings through revisions 1–3.
     These observations establish stored intent; the existing device-response
     specs separately require the version and configuration to run.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `records release and configuration changes before device acknowledgment`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            try await app.create(cli)
            let a = try await app.publish(cli)
            try await cli.sh(
                #"printf '%s' '{"DISPLAY_UNIT":"file-value","FILE_ONLY":"from-file"}' > settings.json"#
            )
            _ = try await app.succeed(
                cli,
                app.deployCommand(a, extra: "--config-file settings.json")
            )
            let first = try await app.requireHistory(cli, release: a, revision: 1)
            let settings = try JSONDecoder().decode(
                [String: String].self,
                from: Data(first.deployment.configJson.utf8)
            )
            #expect(settings["DISPLAY_UNIT"] == "celsius")
            #expect(settings["FILE_ONLY"] == "from-file")
            #expect(settings["E2E_ATTEMPT"] == app.attempt)
            let b = try await app.publish(
                cli,
                version: "2.0.0",
                layoutKey: "WENDY_E2E_APPS_LAYOUT_B"
            )
            _ = try await app.succeed(
                cli,
                "wendy app deployment update \(app.deployment) --release \(b.id) --note 'Install B' --yes \(app.cloudFlag)"
            )
            let second = try await app.requireHistory(
                cli,
                release: b,
                revision: 2,
                sameDeployment: first.deployment.id
            )
            #expect(second.deployment.configJson == first.deployment.configJson)
            try await app.refuse(
                cli,
                "wendy app deployment update \(app.deployment) --config DISPLAY_UNIT=kelvin \(app.cloudFlag)",
                guidance: "--yes"
            )
            let unconfirmed = try await app.snapshot(cli)
            #expect(unconfirmed.deployment.revision == 2)
            #expect(unconfirmed.history.count == 2)
            _ = try await app.succeed(
                cli,
                "wendy app deployment update \(app.deployment) --config DISPLAY_UNIT=kelvin --note 'Change units' --yes \(app.cloudFlag)"
            )
            let third = try await app.requireHistory(
                cli,
                release: b,
                revision: 3,
                sameDeployment: first.deployment.id
            )
            let changed = try JSONDecoder().decode(
                [String: String].self,
                from: Data(third.deployment.configJson.utf8)
            )
            #expect(changed["DISPLAY_UNIT"] == "kelvin")
            #expect(changed["FILE_ONLY"] == "from-file")
            #expect(changed["E2E_ATTEMPT"] == app.attempt)
            #expect(third.history.first { $0.revision.revision == 2 }?.revision.note == "Install B")
            #expect(
                third.history.first { $0.revision.revision == 3 }?.revision.note == "Change units"
            )
            #expect(
                third.history.first { $0.revision.revision == 1 }?.revision.configJson
                    == first.deployment.configJson
            )
        }
    }

    /**
     After an ordinary initial deployment, the public update RPC stores a new
     configuration revision without submitting its returned assignments. Resume
     delivers that interrupted revision and clears its unsigned pending work.
     It must preserve revision, history, target, release and configuration. A
     queued statement is not an acknowledgment or a successful running workload.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `resumes an unsigned pending revision without changing Cloud history`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            try await app.create(cli)
            let a = try await app.publish(cli)
            _ = try await app.succeed(cli, app.deployCommand(a))
            let first = try await app.snapshot(cli)
            _ = try await app.succeed(cli, app.observerCommand("plan-update"))
            let pending = try await app.requireHistory(
                cli,
                release: a,
                revision: 2,
                sameDeployment: first.deployment.id
            )
            try #require(pending.pending.count == 1)
            #expect(pending.states.first?.state.statementPending == true)
            let recipient = "spiffe://wendy.sh/tenant/\(app.tenant)/device/\(app.asset)"
            #expect(
                pending.pending.allSatisfy {
                    $0.kind == "KIND_ASSIGN" && $0.device == recipient && $0.revision == 2
                }
            )
            let delivered = try await app.succeed(
                cli,
                "wendy app deployment resume \(app.deployment) --yes \(app.cloudFlag)"
            )
            // AssignDevice accepts a SignedRequest and returns the actual
            // postbox entry. This receipt proves signed delivery acceptance,
            // independently of the Agent's later reported entry field.
            let receipt = delivered.stdout + "\n" + delivered.readableStderr
            let expression = try NSRegularExpression(
                pattern:
                    "Sent " + NSRegularExpression.escapedPattern(for: recipient)
                    + " deployment " + NSRegularExpression.escapedPattern(for: first.deployment.id)
                    + " revision 2 \\(postbox entry [1-9][0-9]*\\)"
            )
            try #require(
                expression.firstMatch(
                    in: receipt,
                    range: NSRange(receipt.startIndex..., in: receipt)
                ) != nil
            )
            let resumed = try await app.requireHistory(
                cli,
                release: a,
                revision: 2,
                sameDeployment: first.deployment.id
            )
            #expect(resumed.pending.isEmpty)
            #expect(resumed.history == pending.history)
            #expect(resumed.deployment.configJson == pending.deployment.configJson)
            #expect(resumed.states.count == 1)
            #expect(resumed.states.first?.assetId == app.asset)
            #expect(resumed.states.first?.state.statementPending == false)
            #expect(resumed.states.first?.state.appReleaseId == a.id)
            #expect(resumed.states.first?.state.deploymentId == first.deployment.id)
            let again = try await app.succeed(
                cli,
                "wendy app deployment resume \(app.deployment) --yes \(app.cloudFlag)"
            )
            #expect(again.stdout.contains("Every device of \(app.name) has its instruction."))
            let unchanged = try await app.snapshot(cli)
            #expect(unchanged.deployment.revision == 2)
            #expect(unchanged.history.count == 2)
            #expect(unchanged.history == pending.history)
            #expect(unchanged.pending.isEmpty)
        }
    }

    /**
     The owner rotates the deployment key after Cloud accepted the initial
     instruction, without waiting for device apply. New wrapped and sealed key
     identifiers must appear while deployment, release, revision, targets and
     settings stay unchanged. This does not establish fresh device delivery;
     the existing runtime rotation example retains that separate assertion.
     */
    @Test(
        .enabled(
            if: appsDeploymentTestsEnabled
                && ProcessInfo.processInfo.environment["WENDY_E2E_APPS_ROTATE"] == "true"
        )
    )
    func `rotates pending deployment key material without changing its revision`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            try await app.create(cli)
            let a = try await app.publish(cli)
            _ = try await app.succeed(cli, app.deployCommand(a))
            let before = try await app.requireHistory(cli, release: a, revision: 1)
            let keys = try await app.observe(
                cli,
                "material",
                arguments: "--deployment \(before.deployment.id)",
                as: AppsKeyMaterial.self
            )
            try #require(!keys.keyIds.isEmpty && !keys.sealedKeyIds.isEmpty)
            _ = try await app.succeed(
                cli,
                "wendy app rotate --deployment \(before.deployment.id) --device \(app.asset) \(app.cloudFlag)"
            )
            let rotated = try await app.observe(
                cli,
                "material",
                arguments: "--deployment \(before.deployment.id)",
                as: AppsKeyMaterial.self
            )
            #expect(!Set(rotated.keyIds).subtracting(keys.keyIds).isEmpty)
            #expect(!Set(rotated.sealedKeyIds).subtracting(keys.sealedKeyIds).isEmpty)
            let after = try await app.requireHistory(
                cli,
                release: a,
                revision: 1,
                sameDeployment: before.deployment.id
            )
            #expect(after.history.count == before.history.count)
            #expect(after.deployment.configJson == before.deployment.configJson)
            #expect(after.deployment.desiredState == before.deployment.desiredState)
        }
    }

    /**
     A different well-formed digest refuses before any deployment or assignment
     is created. The same published release with its actual receipt digest then
     creates exactly the chosen deployment and target. Read the catalog and
     device inventory after refusal instead of accepting an error message alone.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `refuses a mismatched digest then accepts the published digest`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            try await app.create(cli)
            let a = try await app.publish(cli)
            let before = try await app.inventory(cli)
            let different =
                "sha256:" + String(repeating: a.digest.last == "0" ? "1" : "0", count: 64)
            let command = app.deployCommand(a).replacingOccurrences(of: a.digest, with: different)
            let refusal = try await app.command(cli, command)
            #expect(refusal.status.isFailure)
            #expect(
                refusal.readableStderr.contains("release \(a.id) is \(a.digest), not \(different)")
            )
            let absent = try await app.catalog(cli)
            #expect(absent.deployments?.isEmpty == true)
            #expect(absent.releases?.count == 1)
            let after = try await app.inventory(cli)
            #expect(after == before)
            _ = try await app.succeed(cli, app.deployCommand(a))
            let accepted = try await app.requireHistory(cli, release: a, revision: 1)
            #expect(accepted.states.count == 1)
            #expect(accepted.states.first?.assetId == app.asset)
            #expect(accepted.states.first?.state.appReleaseId == a.id)
            let artifact = try await app.requireArtifact(cli, release: a)
            #expect(artifact.computedDigest == a.digest)
        }
    }

    /**
     Resolve overlapping explicit devices, a private tag and a private site.
     Cloud records A and B once each in the initial revision and no C assignment.
     The companion runtime spec still requires both targets to serve the release.
     Fleet cleanup restores only memberships actually changed by this fixture.
     */
    @Test(.enabled(if: appsFleetTestsEnabled))
    func `records the device tag and site union once per target in Cloud`() async throws {
        try await AppsDeploymentFixture.withFleet { cli, app, devices, lease in
            try await app.create(cli)
            let a = try await app.publish(cli)
            let b = try app.target(devices[1])
            _ = try await app.succeed(
                cli,
                app.deployCommand(
                    a,
                    extra: "--device \(b.asset) --tag \(lease.tag) --site \(lease.site)"
                )
            )
            let union = try await app.requireHistory(
                cli,
                release: a,
                revision: 1,
                targets: [app.asset, b.asset]
            )
            #expect(Set(union.states.map(\.assetId)) == Set([app.asset, b.asset]))
            #expect(union.states.count == 2)
            #expect(!union.currentTargets().contains(devices[2].asset))
        }
    }

    /**
     Begin with disjoint A canary and B production deployments at version A.
     Updating canary to B leaves production's release and revision unchanged.
     Remove the owned production target, then explicitly add B to the canary.
     Cloud records its growth, retained earlier membership and exact revision.
     No assertion here substitutes for the companion running-device canary spec.
     */
    @Test(.enabled(if: appsFleetTestsEnabled))
    func `records an explicit canary wave and target growth in Cloud`() async throws {
        try await AppsDeploymentFixture.withFleet { cli, app, devices, _ in
            try await app.create(cli)
            let a = try await app.publish(cli)
            let canary = try app.target(devices[0], name: "canary")
            let production = try app.target(devices[1], name: "production")
            _ = try await canary.succeed(cli, canary.deployCommand(a))
            _ = try await production.succeed(cli, production.deployCommand(a))
            let first = try await canary.snapshot(cli)
            let stable = try await production.snapshot(cli)
            let b = try await app.publish(
                cli,
                version: "2.0.0",
                layoutKey: "WENDY_E2E_APPS_LAYOUT_B"
            )
            _ = try await app.succeed(
                cli,
                "wendy app deployment update \(canary.deployment) --release \(b.id) --note 'Canary B' --yes \(app.cloudFlag)"
            )
            _ = try await canary.requireHistory(
                cli,
                release: b,
                revision: 2,
                sameDeployment: first.deployment.id
            )
            let stillStable = try await production.requireHistory(
                cli,
                release: a,
                revision: 1,
                sameDeployment: stable.deployment.id
            )
            #expect(stillStable.deployment.configJson == stable.deployment.configJson)
            _ = try await app.succeed(
                cli,
                "wendy app deployment remove \(production.deployment) --yes \(app.cloudFlag)"
            )
            _ = try await app.succeed(
                cli,
                "wendy app deployment update \(canary.deployment) --add-device \(production.asset) --move --note 'Grow canary' --yes \(app.cloudFlag)"
            )
            let grown = try await canary.requireHistory(
                cli,
                release: b,
                revision: 3,
                targets: [canary.asset, production.asset],
                sameDeployment: first.deployment.id
            )
            #expect(grown.history.first { $0.revision.revision == 1 }?.devices == [canary.asset])
            #expect(grown.history.first { $0.revision.revision == 2 }?.devices == [canary.asset])
            #expect(
                grown.history.first { $0.revision.revision == 3 }?.revision.note == "Grow canary"
            )
            #expect(!grown.currentTargets().contains(devices[2].asset))
        }
    }
}
