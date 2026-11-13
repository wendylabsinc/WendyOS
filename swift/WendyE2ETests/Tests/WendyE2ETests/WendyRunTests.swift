import Foundation
import Testing
import WendyE2ETesting

@Suite
struct `'wendy run'` {
    let scenario = CLIAndAgentScenario()

    /**
     Redeploying a changed prebuilt Compose service applies its changed runtime
     environment: HTTP reports the new payload version from a new process.
     Exactly one entry remains for that app; an unrelated owned app retains its
     inventory, payload version and process while continuing to serve requests.
     This checks the served version; the known companion inventory-version
     defect has a separate opt-in spec.
     */
    @Test(.enabled(if: httpAppTestsEnabled))
    func `redeploys changed HTTP content without replacing an unrelated app`() async throws {
        try await HTTPAppFixture.scenario().run { cli, _ in
            try await HTTPAppFixture.withComposeApp(cli: cli) { other in
                try await HTTPAppFixture.withComposeApp(cli: cli) { app in
                    let before = try await app.response(cli)
                    let otherBefore = try await other.response(cli)
                    let inventory = try await app.inventory(cli)
                    try await app.setVersion(cli, "2.0.0")
                    try await app.deploy(cli, expectedVersion: "2.0.0")
                    let after = try await app.response(
                        cli,
                        retryStartup: false,
                        expectedVersion: "2.0.0"
                    )
                    #expect(after.instance != before.instance)
                    try await app.requireState(cli, "RUNNING", expectedVersion: nil)
                    let otherAfter = try await other.response(cli, retryStartup: false)
                    #expect(otherAfter.instance == otherBefore.instance)
                    #expect(otherAfter.requests > otherBefore.requests)
                    #expect(
                        try await app.inventory(cli).filter { $0.name != app.app }
                            == inventory.filter { $0.name != app.app }
                    )
                }
            }
        }
    }

    /**
     A replacement with a locally invalid manifest fails with the original
     configuration diagnostic, before deployment. The last working app keeps
     its inventory and process and continues serving requests. Repairing the
     manifest permits a later changed version to deploy successfully.

     This is preflight preservation, not a promise of rollback after an image
     has been accepted or a replacement has started and then crashed.
     */
    @Test(.enabled(if: httpAppTestsEnabled))
    func `preserves the working HTTP app after invalid replacement configuration`() async throws {
        try await HTTPAppFixture.scenario().run { cli, _ in
            try await HTTPAppFixture.withComposeApp(cli: cli) { app in
                let before = try await app.response(cli)
                let inventory = try await app.inventory(cli)
                let path = app.app + "/wendy.json"
                let original = try await LocalCLIAssertions.read(cli, path: path)
                var config = try #require(
                    JSONSerialization.jsonObject(with: Data(original.utf8)) as? [String: Any]
                )
                config["entitlements"] = [["type": "persist", "name": "data", "path": "relative"]]
                let invalid = try JSONSerialization.data(
                    withJSONObject: config,
                    options: [.sortedKeys]
                )
                try await LocalCLIAssertions.write(
                    cli,
                    path: path,
                    text: String(decoding: invalid, as: UTF8.self)
                )
                try await cli.sh(
                    "\(app.command) run --prefix \(app.app) --skip-cloud-registration --detach --no-restart"
                ) { result in
                    #expect(result.status.isFailure)
                    #expect(result.stdout.isEmpty)
                    #expect(result.stderr.contains("persist path must be absolute"))
                    #expect(result.stderr.contains("config_invalid"))
                }
                #expect(try await app.inventory(cli) == inventory)
                let survived = try await app.response(cli, retryStartup: false)
                #expect(survived.instance == before.instance)
                #expect(survived.requests > before.requests)
                try await LocalCLIAssertions.write(cli, path: path, text: original)
                try await app.setVersion(cli, "2.0.0")
                try await app.deploy(cli, expectedVersion: "2.0.0")
                let repaired = try await app.response(
                    cli,
                    retryStartup: false,
                    expectedVersion: "2.0.0"
                )
                #expect(repaired.instance != before.instance)
            }
        }
    }

    /**
     A single prebuilt Compose service deploys and starts without a local
     builder. Detached JSON reports unchecked readiness and a usable HTTP
     endpoint whose response identifies this app and version.

     The fixture pins a small public Python image by digest and removes its
     exact app afterward, retaining the shared public image cache.
     */
    @Test(.enabled(if: httpAppTestsEnabled))
    func `deploys a prebuilt HTTP service without a local builder`() async throws {
        try await HTTPAppFixture.scenario().run { cli, _ in
            try await HTTPAppFixture.withComposeApp(cli: cli) { app in
                _ = try await app.response(cli)
                // Companion-version preservation has a separate disabled defect spec.
                try await app.requireState(cli, "RUNNING", expectedVersion: nil)
            }
        }
    }

    /**
     An explicitly selected remote builder without BuildKit fails with a
     diagnostic naming that dependency. It does not fall back to a local
     builder, announce a started app, or change existing deployed app state.

     Currently deferred: the supported LAN-only setting exempts build-host
     resolution, so this fixture cannot safely execute the remote-builder path.
     */
    @Test(
        .disabled(
            "Deferred: supported no-Cloud-fallback setting exempts build-host resolution; LAN-only harness cannot contain this remote-builder path"
        )
    )
    func `reports unavailable remote builder without creating an app`() async throws {
        try await HTTPAppFixture.scenario().run { cli, _ in
            try await HTTPAppFixture.withProject(cli: cli, imageMayBeImported: false) { app in
                try await cli.sh(
                    "\(app.command) run --prefix \(app.app) \(app.buildHostArgument) --skip-cloud-registration --detach"
                ) { result in
                    #expect(result.status.isFailure)
                    #expect(result.stdout.isEmpty)
                    #expect(result.stderr.contains("BuildKit"))
                    #expect(result.stderr.contains(app.device))
                }
                let after = try await app.inventory(cli)
                #expect(!after.contains { $0.name == app.app })
            }
        }
    }

    /**
     Remote builds currently reject `--wait-ready` before building or deploying.
     The command returns a failed readiness result and a diagnostic identifying
     the incompatible flags, leaving existing deployed app state untouched.
     Currently deferred with remote-builder cases until the harness can safely
     contain every build-host resolution path without Cloud access.
     */
    @Test(
        .disabled(
            "Deferred with remote-builder cases: build-host resolution is exempt from supported no-Cloud-fallback setting"
        )
    )
    func `rejects unsupported remote readiness before changing deployed apps`() async throws {
        try await HTTPAppFixture.scenario().run { cli, _ in
            try await HTTPAppFixture.withProject(cli: cli, imageMayBeImported: false) { app in
                try await cli.sh(
                    "\(app.command) run --prefix \(app.app) \(app.buildHostArgument) --skip-cloud-registration --detach --wait-ready"
                ) { result in
                    #expect(result.status.isFailure)
                    #expect(result.stderr.contains("--wait-ready"))
                    #expect(result.stderr.contains("--build-host"))
                    let output = try JSONDecoder().decode(
                        HTTPAppFixture.ReadinessFailure.self,
                        from: Data(result.stdout.utf8)
                    )
                    #expect(output.status == "failed")
                    #expect(output.readiness == "not_checked")
                }
                let after = try await app.inventory(cli)
                #expect(!after.contains { $0.name == app.app })
            }
        }
    }

    /**
     An invalid manifest fails before connecting to the explicitly selected
     target or starting a build. A recognized source tree does not bypass
     capability validation or mutate project files on failure.
     */
    @Test
    func `rejects invalid project configuration before contacting a target`() async throws {
        try await scenario.run(authenticated: false) { cli, _ in
            try await LocalCLIAssertions.prepare(cli)
            try await LocalCLIAssertions.write(
                cli,
                path: "wendy.json",
                text:
                    #"{"appId":"first-app","entitlements":[{"type":"persist","name":"data","path":"relative"}]}"#
            )
            try await LocalCLIAssertions.write(cli, path: "Dockerfile", text: "FROM scratch\n")
            let before = try await FirstAppFixture.snapshot(cli)
            let result = try await LocalCLIAssertions.run(
                cli,
                ["run", "--json", "--device", "127.0.0.1:1"],
                exit: 1,
                monitorNetwork: true
            )
            try LocalCLIAssertions.error(
                result,
                code: "config_invalid",
                exit: 1,
                message: "persist path must be absolute"
            )
            try await cli.sh(
                posix: "rm cli-exit network-observation.json",
                power: "Remove-Item -LiteralPath 'cli-exit', 'network-observation.json'"
            )
            #expect(try await FirstAppFixture.snapshot(cli) == before)
        }
    }

    /**
     Displays usage for `wendy run`. The output includes the command synopsis,
     local flags, inherited global flags, and concise descriptions. Help exits
     successfully, writes to stdout, emits no stderr, and leaves configuration,
     cache, project, cloud, and device state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy run --help") { result in
                let stdout = result.stdout
                #expect(result.status.isSuccess)
                #expect(stdout.contains("Reads wendy.json"))
                #expect(stdout.contains("wendy run [flags]"))
                #expect(stdout.contains("--deploy"))
                #expect(stdout.contains("--detach"))
                #expect(stdout.contains("--user-args"))
                #expect(stdout.contains("--prefix"))
                #expect(stdout.contains("--device"))
                #expect(stdout.contains("--build-host"))
                #expect(stdout.contains("--json"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     Reads the project configuration, builds the application image,
     deploys it over the selected direct device connection, and starts the
     container. Success output makes the running app and target device
     clear.
     */
    @Test(
        .disabled(
            "WDY-1950: end-to-end build/deploy/start needs pinned sample projects, isolated image namespaces, and a disposable managed-agent target."
        )
    )
    func `builds, deploys, and starts the current project`() async throws {
        // TODO: enable with isolated build and managed-deploy fixtures (WDY-1950).
    }

    /**
     `--deploy` creates or updates the container on the target device and
     leaves it stopped. The command exits successfully after deployment and
     prints no live log stream.
     */
    @Test(
        .disabled(
            "WDY-1950: stopped deployment state needs a disposable managed agent with observable container lifecycle and cleanup."
        )
    )
    func `deploys without starting when requested`() async throws {
        // TODO: enable with the managed-deploy fixture (WDY-1950).
    }

    /**
     `--detach` starts the application and returns after start-up status is
     known. Output includes the app name and how to view logs later.
     */
    @Test(
        .disabled(
            "WDY-1950: detach success and follow-up log guidance need an isolated built app and disposable managed-agent target."
        )
    )
    func `detaches after starting when requested`() async throws {
        // TODO: enable with isolated build and managed-deploy fixtures (WDY-1950).
    }

    /**
     `--detach --wait-ready` reports readiness only after the declared probe
     passes. The selected device serves an actual HTTP response identifying
     this deployment, and inventory names the same app, version, and HTTP port.

     Requires `WENDY_E2E_HTTP_APP=true` and an explicit device. The image builds
     on that same device, so the CLI host needs no container runtime.
     */
    @Test(
        .disabled(
            "CLI rejects --wait-ready with --build-host; this Mac has no local container runtime"
        )
    )
    func `'--wait-ready' confirms a responding HTTP app on the selected device`() async throws {
        try await HTTPAppFixture.scenario().run { cli, _ in
            try await HTTPAppFixture.withApp(cli: cli, waitReady: true) { app in
                _ = try await app.response(cli)
                try await app.requireState(cli, "RUNNING")
            }
        }
    }

    /**
     `--detach` without `--wait-ready` acknowledges start and reports
     `readiness: "not_checked"`. A configured endpoint is useful connection
     information, not a readiness verdict; the caller verifies HTTP separately.
     */
    @Test(
        .enabled(if: httpAppTestsEnabled),
        .disabled(
            "Fixture blocked: Jetson build host has no BuildKit and the CLI host has no container builder; validate a supported builder path before enabling"
        )
    )
    func `'--detach' reports unchecked readiness and a usable HTTP endpoint`() async throws {
        try await HTTPAppFixture.scenario().run { cli, _ in
            try await HTTPAppFixture.withApp(cli: cli, waitReady: false) { app in
                _ = try await app.response(cli)
                try await app.requireState(cli, "RUNNING")
            }
        }
    }

    /**
     `--user-args` preserves argument boundaries and forwards the provided
     values to the started application without interpreting secrets or shell
     metacharacters locally.
     */
    @Test(
        .disabled(
            "WDY-1950: user-argument boundary verification needs a fixture app that reports received argv from a disposable managed target."
        )
    )
    func `passes user arguments to the container`() async throws {
        // TODO: enable with an argv-reporting app fixture (WDY-1950).
    }

    /**
     `--prefix` selects the project directory and `--device` selects the target
     device and skips the picker. The command does not read unrelated
     `wendy.json` files or open interactive device selection.
     */
    @Test(
        .disabled(
            "WDY-1950: explicit prefix/device success needs isolated projects and a disposable managed target without physical device selection."
        )
    )
    func `uses explicit project and device selection`() async throws {
        // TODO: enable with isolated project and managed-target fixtures (WDY-1950).
    }

    /**
     Build failures, invalid project configuration, unreachable devices, or
     deployment errors return a failure status. Partial remote resources are
     either cleaned up or identified clearly for manual cleanup.
     */
    @Test(
        .disabled(
            "WDY-1950: build/deploy failure cleanup needs controllable builder and managed-agent failure modes with observable remote resources."
        )
    )
    func `reports build or deployment failure without claiming success`() async throws {
        // TODO: enable with isolated build/deploy failure fixtures (WDY-1950).
    }

    /**
     With `--json`, emits structured build, deploy, start, and app metadata.
     Progress and streamed container logs do not corrupt stdout JSON.
     */
    @Test(
        .disabled(
            "WDY-1909: 'wendy run' does not implement a complete global --json result; WDY-1950 tracks the isolated deployment fixture needed to verify log separation."
        )
    )
    func `prints JSON run metadata for automation`() async throws {
        // TODO: enable when run JSON and managed-deploy fixtures exist (WDY-1909, WDY-1950).
    }

    /**
     Validates local run options before selecting or contacting a target device.

     Invalid option combinations fail without discovery, build, deployment, or
     device state changes.
     */
    @Test
    func `validates local options before selecting a device`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy run --prefix missing-project") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("resolving working directory"))
                #expect(result.stderr.contains("does not exist"))
            }
            try await cli.sh("wendy run --chunking nonsense") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("invalid --chunking value"))
                #expect(result.stderr.contains("auto, force, or off"))
            }
            try await cli.sh("wendy run --build-host spark-office --builder buildkit") {
                result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("--builder selects a local image builder"))
                #expect(result.stderr.contains("cannot be combined with --build-host"))
            }
        }
    }

    /**
     Reports a missing device target before starting a project build.

     Non-interactive failure leaves build artifacts, deployments, and device state
     unchanged.
     */
    @Test
    func `reports missing target before building`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy run") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("no device specified"))
                #expect(result.stderr.contains("--device"))
            }
        }
    }

    /**
     Rejects flags that are not part of the command's documented interface.

     The command reports a usage error on stderr and does not perform the
     requested operation.
     */
    @Test
    func `rejects unknown flags`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy run --bogus") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("unknown flag"))
                #expect(result.stderr.contains("--bogus"))
            }
        }
    }

    /**
     Rejects ordinary stray positional arguments while preserving space-separated
     device values for --hil/--build-host and application arguments after --.

     The command reports a usage error instead of treating undocumented input as
     a valid request.
     */
    @Test
    func `rejects undocumented positional arguments`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await LocalCLIAssertions.prepare(cli)
            let result = try await LocalCLIAssertions.run(
                cli,
                ["run", "unexpected", "--json", "--device", "127.0.0.1:1"],
                exit: 2,
                monitorNetwork: true
            )
            try LocalCLIAssertions.error(result, code: "cli_usage", exit: 2, message: "unexpected")
            try await LocalCLIAssertions.unchanged(cli)

            // Both optional flags still accept a space-separated DEVICE. Stop at
            // a distinct local lifecycle check, before any device/build action.
            for flags in [
                ["--hil", "127.0.0.1:1"], ["--build-host", "127.0.0.1:1", "--hil=127.0.0.1:1"],
            ] {
                let accepted = try await LocalCLIAssertions.run(
                    cli,
                    ["run"] + flags + ["--detach", "--json"],
                    exit: 1
                )
                try LocalCLIAssertions.error(
                    accepted,
                    code: "error",
                    exit: 1,
                    message: "HIL requires an attached, single-app run"
                )
                try await LocalCLIAssertions.unchanged(cli)
            }
            // Application arguments after -- pass parsing and reach RunE's
            // independent environment validation without contacting a target.
            let accepted = try await LocalCLIAssertions.run(
                cli,
                ["run", "--env", "INVALID", "--json", "--", "app-argument", "--app-flag"],
                exit: 2
            )
            try LocalCLIAssertions.error(
                accepted,
                code: "cli_usage",
                exit: 2,
                message: "must be KEY=VALUE"
            )
            try await LocalCLIAssertions.unchanged(cli)
        }
    }
}
