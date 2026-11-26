import Foundation
import Testing
import WendyE2ETesting

@Suite
struct `'wendy app rotate'` {
    /**
     The owner publishes and records a deployment, without waiting for device
     apply. An independently authenticated designated member has a registered
     native registry key but no APP-scoped grant on this new app. Its ordinary
     rotate command must report authorization refusal. Owner reads then confirm
     the same deployment, release, revision, configuration, complete history,
     target set and wrapped/sealed key identifiers. This checks authorization
     independently of delivery; the running variant below retains its actual
     device response requirement. Native config directories are referenced per
     command, never exported or merged.
     */
    @Test(
        .enabled(
            if: appsDeploymentTestsEnabled
                && ProcessInfo.processInfo.environment["WENDY_E2E_APPS_SECOND_OPERATOR"] == "true"
        )
    )
    func `refuses another authenticated operator before waiting for device apply`() async throws {
        _ = try AppsDeploymentFixture.setting("WENDY_E2E_APPS_MEMBER_EMAIL")
        try await AppsDeploymentFixture.withCloudApp(verifyTarget: true) { cli, app in
            try await app.create(cli)
            _ = try await app.succeed(cli, "wendy registry key show \(app.cloudFlag)")
            let release = try await app.publish(cli)
            _ = try await app.succeed(cli, app.deployCommand(release))
            let before = try await app.requireHistory(cli, release: release, revision: 1)
            let keys = try await app.observe(
                cli,
                "material",
                arguments: "--deployment \(before.deployment.id)",
                as: AppsKeyMaterial.self
            )
            try #require(!keys.keyIds.isEmpty && !keys.sealedKeyIds.isEmpty)
            try await app.withUnauthorizedOperator(cli) { nativeEnvironment in
                let member = try await app.operatorProfile(
                    cli,
                    nativeEnvironment: nativeEnvironment
                )
                try #require(member.matchesExpectedEmail && member.registeredKeyCount > 0)
                let catalog = try await app.catalog(cli)
                let bindings = try #require(catalog.bindings)
                try #require(
                    !bindings.contains {
                        $0.principal.id == member.operatorId
                            && $0.scope.type == "BINDING_SCOPE_TYPE_APP" && $0.scope.ref == app.app
                    },
                    "The designated member must have no app-specific grant"
                )
                let refusal = try await app.command(
                    cli,
                    "\(nativeEnvironment) wendy app rotate --deployment \(before.deployment.id) --device \(app.asset) \(app.cloudFlag)"
                )
                try #require(!refusal.status.description.contains("124"))
                #expect(refusal.status.isFailure)
                #expect(
                    refusal.readableStderr.lowercased().contains("permission"),
                    "Require an authorization refusal after native key preflight"
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
            let after = try await app.requireHistory(
                cli,
                release: release,
                revision: 1,
                sameDeployment: before.deployment.id
            )
            #expect(after.deployment.configJson == before.deployment.configJson)
            #expect(after.deployment.desiredState == before.deployment.desiredState)
            #expect(after.history == before.history)
            #expect(after.currentTargets() == before.currentTargets())
        }
    }

    /** A key rotation needs a deployment UUID before key lookup. */
    @Test(.enabled(if: appsDeploymentLocalTestsEnabled))
    func `requires a deployment id for key rotation`() async throws {
        try await refuseRotateInput("", guidance: "--deployment")
    }

    /** A key rotation needs explicit target UUIDs before key lookup. */
    @Test(.enabled(if: appsDeploymentLocalTestsEnabled))
    func `requires target device ids for key rotation`() async throws {
        try await refuseRotateInput(
            "--deployment 11111111-1111-4111-8111-111111111111",
            guidance: "--device"
        )
    }

    private func refuseRotateInput(
        _ flags: String,
        guidance: String,
        filePath: String = #filePath,
        function: String = #function,
        line: Int = #line
    ) async throws {
        try await CLIAndAgentScenario().run(
            authenticated: false,
            filePath: filePath,
            function: function,
            line: line
        ) { cli, _ in
            try await cli.sh("wendy app rotate \(flags) </dev/null") { result in
                #expect(result.status.isFailure)
                #expect(result.readableStderr.contains(guidance))
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
            try await app.withUnauthorizedOperator(cli) { nativeEnvironment in
                let refusal = try await app.command(
                    cli,
                    "\(nativeEnvironment) wendy app rotate --deployment \(before.deployment.id) --device \(app.asset) \(app.cloudFlag)"
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
