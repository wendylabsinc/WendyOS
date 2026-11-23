import Foundation
import Testing
import WendyE2ETesting

@Suite
struct `'wendy app rotate'` {
    /**
     Rotation requires a deployment UUID and an explicit list of device UUIDs.
     Invalid input refuses before key lookup or service calls. A working cached
     app alone is insufficient proof of successful key rotation.
     */
    @Test(
        .enabled(if: appsDeploymentLocalTestsEnabled),
        arguments: [
            ("", "--deployment"),
            ("--deployment 11111111-1111-4111-8111-111111111111", "--device"),
        ]
    )
    func `requires the deployment and target device ids`(input: (String, String)) async throws {
        try await CLIAndAgentScenario().run(authenticated: false) { cli, _ in
            try await cli.sh("wendy app rotate \(input.0) </dev/null") { result in
                #expect(result.status.isFailure)
                #expect(result.readableStderr.contains(input.1))
            }
        }
    }

    /**
     The creator rotates the named deployment's key without changing its release
     or revision. After removing only the owned runtime/image, new wrapped and
     sealed key identifiers appear and a fresh device instruction applies the
     same release with the same independent version/configuration response.
     A separate second-operator case establishes authorization refusal.
     */
    @Test(
        .enabled(
            if: appsDeploymentTestsEnabled
                && ProcessInfo.processInfo.environment["WENDY_E2E_APPS_ROTATE"] == "true"
        )
    )
    func `rotates key material and delivers the same release to the named device`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            let release = try await app.deployed(cli)
            let before = try await app.snapshot(cli)
            let keys = try await app.observe(
                cli,
                "material",
                arguments: "--deployment \(before.deployment.id)",
                as: AppsKeyMaterial.self
            )
            // Force the next assignment to deliver/import the owned image again.
            _ = try await app.succeed(
                cli,
                "\(app.deviceCommand) remove \(app.app) --force --cleanup"
            )
            try await app.requireAbsent(cli)
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
            try await app.requireRunning(
                cli,
                release: release,
                revision: 1,
                version: "1.0.0",
                unit: "celsius"
            )
            let after = try await app.snapshot(cli)
            #expect(after.deployment.id == before.deployment.id)
            #expect(after.deployment.revision == before.deployment.revision)
            #expect(
                after.states.first?.state.postboxEntryId
                    != before.states.first?.state.postboxEntryId
            )
        }
    }
    /**
     A second valid same-tenant operator without app/key authority is refused.
     After returning to the owner identity, wrapped-key identifiers, revision,
     running release and independent application response remain unchanged.
     */
    @Test(
        .enabled(
            if: appsDeploymentTestsEnabled
                && ProcessInfo.processInfo.environment["WENDY_E2E_APPS_SECOND_OPERATOR"] == "true"
        )
    )
    func `refuses rotation by another authenticated unauthorized operator`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            let release = try await app.deployed(cli)
            let before = try await app.snapshot(cli)
            let keys = try await app.observe(
                cli,
                "material",
                arguments: "--deployment \(before.deployment.id)",
                as: AppsKeyMaterial.self
            )
            try await app.withUnauthorizedOperator(cli) {
                let refusal = try await app.command(
                    cli,
                    "wendy app rotate --deployment \(before.deployment.id) --device \(app.asset) \(app.cloudFlag)"
                )
                #expect(refusal.status.isFailure)
                #expect(
                    refusal.readableStderr.lowercased().contains("permission"),
                    "Require authorization refusal, not expired auth or missing local key"
                )
            }
            let afterKeys = try await app.observe(
                cli,
                "material",
                arguments: "--deployment \(before.deployment.id)",
                as: AppsKeyMaterial.self
            )
            #expect(afterKeys.keyIds == keys.keyIds)
            #expect(afterKeys.sealedKeyIds == keys.sealedKeyIds)
            let after = try await app.snapshot(cli)
            #expect(after.deployment.revision == before.deployment.revision)
            #expect(
                after.states.first?.state.postboxEntryId
                    == before.states.first?.state.postboxEntryId
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

}
