import Foundation
import Testing
import WendyE2ETesting

@Suite(.serialized)
struct `'wendy app create'` {
    /**
     Registers one owned Cloud app with its creator binding and empty release and
     deployment catalogs. Local project files and device inventory stay intact.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `registers a local project without deploying it`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            let before = try await app.inventory(cli)
            let project = try await cli.sh("cat wendy.json") { $0.stdout }
            try await app.create(cli)
            let policy = try await app.succeed(
                cli,
                "wendy app lifecycle \(app.app) \(app.cloudFlag)"
            )
            #expect(policy.stdout.contains("Lifecycle policy of \(app.app): Organization default"))
            let afterProject = try await cli.sh("cat wendy.json") { $0.stdout }
            #expect(afterProject == project)
            let after = try await app.inventory(cli)
            #expect(after == before)
        }
    }

    /**
     An existing app id is refused, preserving the original app and its policy.
     A second creation must never silently rename or overwrite the first app.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `refuses a duplicate app id`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            try await app.create(cli)
            let release = try await app.publish(cli)
            let before = try await app.succeed(
                cli,
                "wendy app lifecycle \(app.app) \(app.cloudFlag)"
            )
            try await app.refuse(
                cli,
                "wendy app create --app \(app.app) --name Replacement --org-default --yes \(app.cloudFlag)",
                guidance: "taken"
            )
            let after = try await app.succeed(
                cli,
                "wendy app lifecycle \(app.app) \(app.cloudFlag)"
            )
            #expect(after.stdout == before.stdout)
            let catalog = try await app.catalog(cli)
            #expect(catalog.app?.name == "E2E operator app")
            #expect(catalog.releases?.map(\.id) == [release.id])
        }
    }

    /**
     Cloud registration also works from an empty directory. It creates no
     wendy.json there; publishing still explains that a local project is needed.
     This records current command intentions while naming is under discussion.
     */
    @Test(.enabled(if: appsDeploymentTestsEnabled))
    func `registers from an empty directory and explains the next publish prerequisite`()
        async throws
    {
        try await AppsDeploymentFixture.withApp { cli, app in
            try await cli.sh("rm wendy.json")
            try await app.create(cli)
            try await cli.sh("test ! -e wendy.json")
            try await app.refuse(
                cli,
                "wendy app publish --tag 1.0.0 \(app.cloudFlag)",
                guidance: "wendy.json"
            )
        }
    }

    /** Without a terminal, missing name refuses before authentication. */
    @Test(.enabled(if: appsDeploymentLocalTestsEnabled))
    func `requires a name for scripted app creation`() async throws {
        try await refuseCreateInput("--app sh.wendy.e2e.missing --yes", guidance: "--name")
    }

    /** Without a terminal, missing app id refuses before authentication. */
    @Test(.enabled(if: appsDeploymentLocalTestsEnabled))
    func `requires an app id for scripted app creation`() async throws {
        try await refuseCreateInput("--name Example --yes", guidance: "--app")
    }

    /** Without a terminal, missing confirmation refuses before authentication. */
    @Test(.enabled(if: appsDeploymentLocalTestsEnabled))
    func `requires confirmation for scripted app creation`() async throws {
        try await refuseCreateInput("--name Example --app sh.wendy.e2e.missing", guidance: "--yes")
    }

    private func refuseCreateInput(
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
            try await cli.sh("wendy app create \(flags) </dev/null") { result in
                #expect(result.status.isFailure)
                #expect(result.readableStderr.contains(guidance))
            }
        }
    }

    /**
     Gives an existing dedicated member App operator access to this app only.
     The APP-scoped binding is present on the intended app and absent from an
     independently created control app. The creator remains App owner.
     */
    @Test(
        .enabled(
            if: appsDeploymentTestsEnabled
                && ProcessInfo.processInfo.environment["WENDY_E2E_APPS_ACCESS"] == "true"
        )
    )
    func `grants access only to the selected app`() async throws {
        try await AppsDeploymentFixture.withApp { cli, app in
            let email = try AppsDeploymentFixture.setting("WENDY_E2E_APPS_MEMBER_EMAIL")
            let member = try AppsDeploymentFixture.setting("WENDY_E2E_APPS_MEMBER_ID")
            try await app.create(
                cli,
                extra: "--access \(AppsDeploymentFixture.quote(email + "=operator"))"
            )
            let intended = try await app.catalog(cli)
            #expect(
                intended.bindings?.contains {
                    $0.principal.id == member && $0.role.name == "app-operator"
                        && $0.scope.ref == app.app
                        && $0.scope.type == "BINDING_SCOPE_TYPE_APP"
                        && $0.effect == "BINDING_EFFECT_ALLOW"
                } == true
            )
            try await app.withRelatedApp(cli) { other in
                let unrelated = try await other.catalog(cli)
                #expect(
                    unrelated.bindings?.contains {
                        $0.principal.id == member
                            && $0.scope.type == "BINDING_SCOPE_TYPE_APP"
                            && $0.scope.ref == other.app
                            && $0.effect == "BINDING_EFFECT_ALLOW"
                    } == false,
                    "An inherited organization membership is not an app-specific grant"
                )
            }
        }
    }
}
