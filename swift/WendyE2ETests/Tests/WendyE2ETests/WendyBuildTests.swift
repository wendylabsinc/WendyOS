import Testing
import WendyE2ETesting

@Suite
struct `'wendy build'` {
    let scenario = CLIAndAgentScenario()

    /**
     Invalid capability settings in a recognized project fail before target
     access or builder invocation. The diagnostic identifies the manifest field
     and leaves the source, recipe, and configuration unchanged.
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
                ["build", "--json", "--device", "127.0.0.1:1"],
                exit: 1,
                monitorNetwork: true
            )
            try LocalCLIAssertions.error(
                result,
                code: "config_invalid",
                exit: 1,
                message: "persist path must be absolute"
            )
            // LocalCLIAssertions writes only its observation files in this directory.
            try await cli.sh(
                posix: "rm cli-exit network-observation.json",
                power: "Remove-Item -LiteralPath 'cli-exit', 'network-observation.json'"
            )
            #expect(try await FirstAppFixture.snapshot(cli) == before)
        }
    }

    /**
     Displays usage for `wendy build`. The output includes the command
     synopsis, local flags, inherited global flags, and concise
     descriptions. Help exits successfully, writes to stdout, emits no
     stderr, and leaves configuration, cache, project, cloud, and device
     state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy build --help") { result in
                let stdout = result.stdout
                #expect(result.status.isSuccess)
                #expect(stdout.contains("Detects the project type"))
                #expect(stdout.contains("wendy build [flags]"))
                #expect(stdout.contains("--build-type"))
                #expect(stdout.contains("--builder"))
                #expect(stdout.contains("--dockerfile"))
                #expect(!stdout.contains("--build-host"))
                #expect(stdout.contains("--json"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     Reads `wendy.json` and project markers from the current directory,
     selects the build strategy, and produces a container image for the
     target WendyOS architecture. Success output names the image and build
     type.
     */
    @Test(
        .disabled(
            "WDY-1950: successful builds need pinned sample projects, builders/toolchains, image namespaces, and cleanup rather than ambient runner installations."
        )
    )
    func `builds the project in the current directory`() async throws {
        // TODO: enable with isolated build fixtures (WDY-1950).
    }

    /**
     When Docker, Swift, or Python markers coexist, `--build-type` selects
     the intended builder. The chosen strategy is reflected in output and no
     other builder mutates the project.
     */
    @Test(
        .disabled(
            "WDY-1950: overlapping-marker strategy coverage needs maintained Docker/Swift/Python fixtures and controlled builder availability."
        )
    )
    func `uses the requested build type when markers overlap`() async throws {
        // TODO: enable with overlapping project fixtures (WDY-1950).
    }

    /**
     Reports that the working directory is not a Wendy project when required
     project configuration is absent.

     The command fails before invoking a builder or producing build artifacts.
     */
    @Test
    func `reports missing project configuration`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy build") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("no supported build type found"))
            }
            try await cli.sh(
                posix: "test -z \"$(find . -mindepth 1 -maxdepth 1 -print -quit)\"",
                power:
                    "if (Get-ChildItem -Force | Select-Object -First 1) { throw 'build artifacts created' }"
            )
        }
    }

    /**
     Validates the requested builder against the command's supported builder
     choices.

     Invalid values fail before project compilation or remote builder access.
     */
    @Test
    func `validates builder selection locally`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy build --builder nonsense") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.readableStderr.contains("invalid value \"nonsense\" for --builder"))
            }
            try await cli.sh("wendy build --dockerfile Dockerfile.prod --build-type swift") {
                result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(
                    result.stderr.contains("--dockerfile cannot be used with --build-type=swift")
                )
            }
        }
    }

    /**
     Keeps the friendly redirect for callers that try remote building while
     avoiding advertising an option this command cannot honour.
     */
    @Test
    func `redirects unsupported remote builds to wendy run`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy build --build-host spark-office") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("supported by `wendy run`, not `wendy build`"))
                #expect(result.stderr.contains("use `wendy run --build-host` instead"))
            }
        }
    }

    /**
     With `--json`, emits one JSON object describing the selected builder,
     image reference, target architecture, cache usage, and build result.
     Progress logs stay out of stdout JSON.
     */
    @Test(
        .disabled(
            "WDY-1909: 'wendy build' does not implement global --json; WDY-1950 tracks the isolated successful build fixture needed for metadata."
        )
    )
    func `prints JSON build metadata for automation`() async throws {
        // TODO: enable when build implements JSON and has isolated fixtures (WDY-1909, WDY-1950).
    }

    /**
     Rejects flags that are not part of the command's documented interface.

     The command reports a usage error on stderr and does not perform the
     requested operation.
     */
    @Test
    func `rejects undocumented flags`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy build --bogus") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("unknown flag"))
                #expect(result.stderr.contains("--bogus"))
            }
        }
    }

    /**
     Rejects positional arguments because this command is entirely flag-driven.

     The command reports a usage error instead of treating undocumented input as
     a valid request.
     */
    @Test(
        .disabled(
            "WDY-1934: 'wendy build' silently accepts extra positional arguments because the command has no cobra.NoArgs validator."
        )
    )
    func `rejects undocumented positional arguments`() async throws {
        // TODO: enable when build rejects positional arguments (WDY-1934).
    }
}
