import Testing
import WendyE2ETesting

@Suite
struct `'wendy run'` {
    let scenario = CLIAndAgentScenario()

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
