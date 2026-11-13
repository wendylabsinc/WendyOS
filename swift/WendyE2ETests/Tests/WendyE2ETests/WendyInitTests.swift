import Foundation
import Testing
import WendyE2ETesting

@Suite
struct `'wendy init'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays usage for `wendy init`. The output includes the command synopsis,
     local flags, inherited global flags, and concise descriptions. Help exits
     successfully, writes to stdout, emits no stderr, and leaves configuration,
     cache, project, cloud, and device state untouched.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy init --help") { result in
                let stdout = result.stdout

                #expect(result.status.isSuccess)
                #expect(stdout.contains("create a new Wendy project"))
                #expect(stdout.contains("Usage:"))
                #expect(stdout.contains("wendy init [app-id] [flags]"))
                #expect(stdout.contains("--app-id"))
                #expect(stdout.contains("--force"))
                #expect(stdout.contains("--target"))
                #expect(stdout.contains("--language"))
                #expect(stdout.contains("--entitlement"))
                #expect(stdout.contains("--assistant"))
                #expect(stdout.contains("--json"))
                #expect(stdout.contains("--device"))
                #expect(stdout.contains("--help"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     With app id, target, language, entitlement, and assistant choices
     supplied as flags, creates a complete Wendy project in the current
     empty directory. Output lists the files created and next steps, and the
     written `wendy.json` records the requested app id, platform, language,
     and default network entitlement.
     */
    @Test
    func `creates a project non-interactively from flags`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh(
                "wendy init --app-id demo-app --target wendyos --language python"
                    + " --no-extra-entitlements --assistant skip --git-init no"
            ) { result in
                #expect(result.status.isSuccess)
                // Status lines print to stderr (WDY-2435): stdout is reserved
                // for machine-readable payloads.
                #expect(result.stderr.contains("Created wendy.json for demo-app"))
                #expect(result.stderr.contains("Your project is ready!"))
            }

            try await cli.sh(
                posix: "test -f pyproject.toml && test -f Dockerfile && cat wendy.json",
                power: """
                    if (-not (Test-Path -LiteralPath 'pyproject.toml')) { throw 'pyproject.toml missing' }
                    if (-not (Test-Path -LiteralPath 'Dockerfile')) { throw 'Dockerfile missing' }
                    Get-Content -LiteralPath 'wendy.json'
                    """
            ) { result in
                let json = try #require(
                    try JSONSerialization.jsonObject(with: Data(result.stdout.utf8))
                        as? [String: Any]
                )
                #expect(json["appId"] as? String == "demo-app")
                #expect(json["platform"] as? String == "linux")
                #expect(json["language"] as? String == "python")
                let entitlements = try #require(json["entitlements"] as? [[String: Any]])
                #expect(entitlements.contains { $0["type"] as? String == "network" })
            }
        }
    }

    /**
     In a directory that already contains a `wendy.json`, reports the conflict
     before writing and returns a failure status. The existing project file
     remains unchanged.
     */
    @Test
    func `refuses to overwrite an existing project accidentally`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh(
                posix: "printf '{\"appId\":\"pre-existing\"}\\n' > wendy.json",
                power:
                    "Set-Content -LiteralPath 'wendy.json' -Value '{\"appId\":\"pre-existing\"}'"
            )

            try await cli.sh(
                "wendy init --app-id demo-app --target wendyos --language python"
                    + " --no-extra-entitlements --assistant skip --git-init no"
            ) { result in
                #expect(!result.status.isSuccess)
                #expect(result.stderr.contains("wendy.json already exists"))
            }

            try await cli.sh(
                posix: "cat wendy.json",
                power: "Get-Content -LiteralPath 'wendy.json'"
            ) { result in
                let json = try #require(
                    try JSONSerialization.jsonObject(with: Data(result.stdout.utf8))
                        as? [String: Any]
                )
                #expect(json["appId"] as? String == "pre-existing")
            }
        }
    }

    /**
     A template must not overlay an existing populated destination, even when
     it has no `wendy.json`. Refusal preserves every existing byte and writes
     no template files. This exercises the real CLI and public simple-api
     template, including download, extraction, and rendering on the baseline.
     */
    @Test
    func `refuses a template overlay without changing existing files`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh(
                posix: """
                    mkdir sapi
                    printf 'valuable original package\\n' > sapi/Package.swift
                    printf 'PRECIOUS\\n' > sapi/IMPORTANT.txt
                    """,
                power: """
                    New-Item -ItemType Directory -Path 'sapi' | Out-Null
                    [IO.File]::WriteAllText((Join-Path $PWD 'sapi/Package.swift'), "valuable original package`n")
                    [IO.File]::WriteAllText((Join-Path $PWD 'sapi/IMPORTANT.txt'), "PRECIOUS`n")
                    """
            )

            try await cli.sh(
                "wendy init --app-id sapi --target wendyos --language swift"
                    + " --template simple-api --assistant skip --git-init no"
            ) { result in
                #expect(!result.status.isSuccess)
                #expect(result.stderr.contains("not empty"))
                #expect(result.stderr.contains("--force"))
                #expect(!result.stderr.contains("Your project is ready!"))
            }

            try await cli.sh(
                posix: "cat sapi/Package.swift",
                power:
                    "[IO.File]::ReadAllText((Join-Path $PWD 'sapi/Package.swift')) | Write-Host -NoNewline"
            ) { result in
                #expect(result.status.isSuccess)
                #expect(result.stdout == "valuable original package\n")
            }
            try await cli.sh(
                posix: "cat sapi/IMPORTANT.txt",
                power:
                    "[IO.File]::ReadAllText((Join-Path $PWD 'sapi/IMPORTANT.txt')) | Write-Host -NoNewline"
            ) { result in
                #expect(result.status.isSuccess)
                #expect(result.stdout == "PRECIOUS\n")
            }
            try await cli.sh(
                posix: "find sapi -mindepth 1 -maxdepth 1 -exec basename {} \\; | sort",
                power:
                    "Get-ChildItem -LiteralPath 'sapi' -Force | Sort-Object Name | ForEach-Object { $_.Name }"
            ) { result in
                #expect(result.status.isSuccess)
                #expect(
                    result.stdout.replacingOccurrences(of: "\r\n", with: "\n")
                        == "IMPORTANT.txt\nPackage.swift\n"
                )
            }
        }
    }

    /**
     `--force` is scoped to template initialization. Using it with the manual
     scaffold fails before creating any project files.
     */
    @Test
    func `requires a template for force initialization`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh(
                "wendy init --app-id sapi --target wendyos --language swift"
                    + " --force --assistant skip --git-init no"
            ) { result in
                #expect(!result.status.isSuccess)
                #expect(result.stderr.contains("--force requires --template"))
                #expect(!result.stderr.contains("Your project is ready!"))
            }
            try await cli.sh(
                posix: "find . -mindepth 1 -maxdepth 1 -print",
                power: "Get-ChildItem -LiteralPath '.' -Force | ForEach-Object { $_.Name }"
            ) { result in
                #expect(result.status.isSuccess)
                #expect(result.stdout == "")
            }
        }
    }

    /**
     Entitlements that need extra data, such as GPIO pins, validate those
     fields before any files are written. A `gpio` entitlement without pins
     fails and creates no `wendy.json`.
     */
    @Test
    func `validates entitlement-specific fields`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh(
                "wendy init --app-id demo-app --target wendyos --language python"
                    + " --entitlement gpio --assistant skip --git-init no"
            ) { result in
                #expect(!result.status.isSuccess)
                #expect(result.stderr.contains("gpio entitlement requires --gpio-pins"))
            }

            try await cli.sh(
                posix: "test ! -e wendy.json",
                power: "if (Test-Path -LiteralPath 'wendy.json') { throw 'wendy.json was created' }"
            )
        }
    }

    /**
     `--assistant skip` creates the project without launching external AI
     tools or writing assistant configuration such as a `.claude` directory.
     */
    @Test
    func `skips assistant setup when requested`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh(
                "wendy init --app-id demo-app --target wendyos --language python"
                    + " --no-extra-entitlements --assistant skip --git-init no"
            ) { result in
                #expect(result.status.isSuccess)
                #expect(result.stderr.contains("Your project is ready!"))
                #expect(!result.stderr.contains("Launching"))
            }

            try await cli.sh(
                posix: "test -f wendy.json && test ! -e .claude",
                power: """
                    if (-not (Test-Path -LiteralPath 'wendy.json')) { throw 'wendy.json missing' }
                    if (Test-Path -LiteralPath '.claude') { throw '.claude was created' }
                    """
            )
        }
    }

    /**
     Without enough flags for non-interactive creation, prompts for missing
     project choices, validates answers, and writes the same project shape
     as the non-interactive path.
     */
    @Test(.disabled("INTERACTIVE: requires a PTY wizard harness for the project prompts."))
    func `runs the interactive project wizard`() async throws {
        // TODO: enable with a PTY-driven wizard harness.
    }

    /**
     **CURRENT OBSERVED FAILURE:** Manual initialization ignores `--git-init yes`
     and succeeds without creating `.git`. This spec is disabled by default.
     Set `WENDY_E2E_KNOWN_INIT_DEFECTS=1` to replay the retained failing assertion.

     Required behavior:
     The `--git-init` choice controls repository creation. Skipping git leaves
     no `.git` directory; enabling git creates an initial repository without
     changing generated project content.

     Tracked by [WDY-1936 — CLI: 'wendy init --git-init' is ignored by the non-template scaffold](https://linear.app/wendylabsinc/issue/WDY-1936/cli-wendy-init-git-init-is-ignored-by-the-non-template-scaffold).
     */
    @Test(
        .disabled(
            if: !FirstAppFixture.knownDefectsEnabled,
            "[WDY-1936 — CLI: 'wendy init --git-init' is ignored by the non-template scaffold](https://linear.app/wendylabsinc/issue/WDY-1936/cli-wendy-init-git-init-is-ignored-by-the-non-template-scaffold). Replay with WENDY_E2E_KNOWN_INIT_DEFECTS=1."
        )
    )
    func `initializes git only when requested`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            let args = FirstAppFixture.manual.dropLast(2) + ["--git-init", "yes"]
            _ = try await FirstAppFixture.run(cli, Array(args))
            try await cli.sh(
                posix: "test -d .git && git rev-parse --is-inside-work-tree",
                power:
                    "if (-not (Test-Path '.git' -PathType Container)) { throw 'git repository missing' }; git rev-parse --is-inside-work-tree"
            ) { result in
                #expect(result.status.isSuccess)
                #expect(result.normalizedStdout == "true\n")
            }
            try await FirstAppFixture.validate(cli)
        }
    }

    /**
     **CURRENT OBSERVED FAILURE:** Initialization with `--json` succeeds with
     empty stdout and emits no JSON creation summary. This spec is disabled by
     default. Set `WENDY_E2E_KNOWN_INIT_DEFECTS=1` to replay the retained failing
     assertion.

     Required behavior:
     With `--json`, emits one JSON object containing the app id, target,
     language, enabled entitlements, and written file paths. Human guidance
     stays out of stdout JSON.
     */
    @Test(
        .disabled(
            if: !FirstAppFixture.knownDefectsEnabled,
            "DEFECT: init --json emits no creation summary. Replay with WENDY_E2E_KNOWN_INIT_DEFECTS=1."
        )
    )
    func `prints JSON project creation summary for automation`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            let result = try await FirstAppFixture.run(cli, FirstAppFixture.manual + ["--json"])
            let summary = try FirstAppFixture.object(result.stdout)
            #expect(summary["appId"] as? String == "first-app")
            #expect(summary["target"] as? String == "wendyos")
            #expect(summary["language"] as? String == "python")
            let files = try #require(summary["files"] as? [String])
            #expect(
                Set(files).isSuperset(of: [
                    "wendy.json", "pyproject.toml", "Dockerfile", "first_app/__init__.py",
                ])
            )
            let entitlements = try #require(summary["entitlements"] as? [[String: Any]])
            #expect(entitlements.contains { $0["type"] as? String == "network" })
            try await FirstAppFixture.validate(cli)
        }
    }
}

extension `'wendy init'` {
    /**
     `--here` creates a complete manual Python app in the current directory.
     The manifest, parsed package build settings, and container command agree
     on the app name. No git repository or assistant session is started.
     This check requires Python 3.11+ for TOML parsing on the CLI machine.
     */
    @Test
    func `creates a consistent Python manifest package and container entry point`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await FirstAppFixture.requirePythonScaffoldTools(cli)
            _ = try await FirstAppFixture.run(cli, FirstAppFixture.manual)
            let manifest = try await FirstAppFixture.manifest(cli)
            #expect(
                Set(manifest.keys) == [
                    "appId", "version", "platform", "language", "entitlements", "python",
                ]
            )
            #expect(manifest["appId"] as? String == "first-app")
            #expect(manifest["version"] as? String == "0.1.0")
            #expect(manifest["platform"] as? String == "linux")
            #expect(manifest["language"] as? String == "python")
            #expect((manifest["python"] as? NSDictionary) == [:])
            let entitlements = try #require(manifest["entitlements"] as? [[String: Any]])
            #expect(entitlements.count == 1)
            #expect(entitlements[0]["type"] as? String == "network")
            let parsed = try await FirstAppFixture.python(
                cli,
                """
                import json, pathlib, tomllib
                print(json.dumps(tomllib.loads(pathlib.Path('pyproject.toml').read_text())))
                """
            )
            try parsed.requireSuccess()
            let package = try FirstAppFixture.object(parsed.stdout)
            let project = try #require(package["project"] as? [String: Any])
            #expect(project["name"] as? String == "first-app")
            #expect(project["version"] as? String == "0.1.0")
            #expect(project["requires-python"] as? String == ">=3.11")
            #expect(project["dependencies"] as? [String] == [])
            #expect(project["scripts"] as? NSDictionary == ["first_app": "first_app:main"])
            let build = try #require(package["build-system"] as? [String: Any])
            #expect(build["requires"] as? [String] == ["hatchling"])
            #expect(build["build-backend"] as? String == "hatchling.build")
            let dockerfile = try await LocalCLIAssertions.read(cli, path: "Dockerfile")
            #expect(dockerfile.contains("COPY . ."))
            #expect(dockerfile.contains("uv sync"))
            #expect(dockerfile.contains(#"CMD ["uv", "run", "--no-sync", "first_app"]"#))
            let execution = try await FirstAppFixture.python(
                cli,
                """
                import first_app
                first_app.main()
                """
            )
            try execution.requireSuccess()
            #expect(execution.normalizedStdout == "Hello from first-app!\n")
            try await FirstAppFixture.validate(cli)
            let tree = try FirstAppFixture.object(try await FirstAppFixture.snapshot(cli))
            #expect(tree[".git"] == nil)
            #expect(tree[".claude"] == nil)
            #expect(tree["first-app"] == nil)
            #expect(tree["AGENTS.md"] != nil)
        }
    }

    /**
     The generated Python project installs with its declared build backend and
     exposes a working console entry point. This offline check requires Python
     3.11+ and a preprovisioned Hatchling wheelhouse selected by
     `WENDY_E2E_PYTHON_WHEELHOUSE`. Each run installs into its own virtual
     environment and executes from outside the source directory.
     */
    @Test(
        .enabled(
            if: FirstAppFixture.pythonWheelhouse != nil,
            "FIXTURE: set WENDY_E2E_PYTHON_WHEELHOUSE to a provisioned offline Hatchling wheel directory."
        )
    )
    func `installs and runs the generated Python console entry point offline`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await FirstAppFixture.requirePythonScaffoldTools(cli)
            let wheelhouse = try #require(FirstAppFixture.pythonWheelhouse)
            _ = try await FirstAppFixture.run(cli, FirstAppFixture.manual)
            let execution = try await FirstAppFixture.python(
                cli,
                """
                import json, os, pathlib, subprocess, sys
                wheels = pathlib.Path(sys.argv[1]).resolve(strict=True)
                assert wheels.is_dir(), 'wheelhouse must be a directory'
                target = pathlib.Path(os.environ['TMPDIR']) / 'installed-first-app'
                env = {k: v for k, v in os.environ.items() if not k.startswith(('PYTHON', 'PIP_'))}
                env.update(PIP_CONFIG_FILE=os.devnull, PIP_NO_INDEX='1', PIP_DISABLE_PIP_VERSION_CHECK='1')
                def run(args, cwd=None):
                    result = subprocess.run(args, cwd=cwd, env=env, stdin=subprocess.DEVNULL,
                                            capture_output=True, text=True, timeout=60)
                    if result.returncode:
                        sys.stdout.write(result.stdout)
                        sys.stderr.write(result.stderr)
                        raise RuntimeError('isolated package command failed: ' + str(args))
                    return result
                run([sys.executable, '-m', 'venv', str(target)])
                scripts = target / ('Scripts' if os.name == 'nt' else 'bin')
                python = scripts / ('python.exe' if os.name == 'nt' else 'python')
                run([str(python), '-m', 'pip', 'install', '--no-index', '--no-cache-dir',
                     '--find-links', str(wheels), '.'])
                outside = target / 'outside-source'
                outside.mkdir()
                console = scripts / ('first_app.exe' if os.name == 'nt' else 'first_app')
                result = run([str(console)], cwd=outside)
                print(json.dumps({'stdout': result.stdout, 'stderr': result.stderr}))
                """,
                arguments: [wheelhouse]
            )
            try execution.requireSuccess()
            let output = try FirstAppFixture.object(execution.stdout)
            #expect(output["stdout"] as? String == "Hello from first-app!\n")
            #expect(output["stderr"] as? String == "")
        }
    }

    /**
     Manual Swift creation writes a Linux manifest with a matching executable
     target and source file, ready for the developer to extend.
     */
    @Test
    func `creates a complete manual Swift scaffold`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            let args = [
                "init", "--here", "--app-id", "first-app", "--target", "wendyos",
                "--language", "swift", "--no-extra-entitlements", "--assistant", "skip",
                "--git-init", "no",
            ]
            _ = try await FirstAppFixture.run(cli, args)
            let manifest = try await FirstAppFixture.manifest(cli)
            #expect(manifest["appId"] as? String == "first-app")
            #expect(manifest["version"] as? String == "0.1.0")
            #expect(manifest["platform"] as? String == "linux")
            #expect(manifest["language"] as? String == "swift")
            #expect(manifest["python"] == nil)
            #expect(manifest["entitlements"] != nil)
            let package = try await LocalCLIAssertions.read(cli, path: "Package.swift")
            #expect(package.contains("name: \"first-app\""))
            #expect(package.contains(".executableTarget"))
            #expect(!package.contains("WendyLite"))
            let source = try await LocalCLIAssertions.read(
                cli,
                path: "Sources/first-app/main.swift"
            )
            #expect(source == "print(\"Hello from first-app!\")\n")
            try await FirstAppFixture.validate(cli)
        }
    }

    /**
     Repeated and comma-separated entitlement options are combined without
     duplicates. GPIO, I2C, and persistent storage retain their supplied fields;
     framework options remain separate from device capabilities.
     */
    @Test
    func `applies field-bearing entitlements and framework options`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            let args =
                FirstAppFixture.manual.filter { $0 != "--no-extra-entitlements" } + [
                    "--entitlement", "gpu,gpio", "--entitlement", "gpu,i2c,persist", "--gpio-pins",
                    "4,17",
                    "--i2c-device", "i2c-1", "--persist-name", "first-data", "--persist-path",
                    "/data/first",
                    "--framework", "ros2", "--ros2-domain-id", "42", "--ros2-rmw", "cyclonedds",
                    "--ros2-scope", "device",
                ]
            _ = try await FirstAppFixture.run(cli, args)
            let manifest = try await FirstAppFixture.manifest(cli)
            let ents = try #require(manifest["entitlements"] as? [[String: Any]])
            #expect(ents.count == 5)
            #expect(
                Set(ents.compactMap { $0["type"] as? String }) == [
                    "network", "gpu", "gpio", "i2c", "persist",
                ]
            )
            #expect(ents.first { $0["type"] as? String == "gpio" }?["pins"] as? [Int] == [4, 17])
            #expect(ents.first { $0["type"] as? String == "i2c" }?["device"] as? String == "i2c-1")
            #expect(
                ents.first { $0["type"] as? String == "persist" }?["name"] as? String
                    == "first-data"
            )
            #expect(
                ents.first { $0["type"] as? String == "persist" }?["path"] as? String
                    == "/data/first"
            )
            let frameworks = try #require(manifest["frameworks"] as? [String: Any])
            let ros2 = try #require(frameworks["ros2"] as? [String: Any])
            #expect(ros2["domainId"] as? Int == 42)
            #expect(ros2["rmw"] as? String == "cyclonedds")
            #expect(ros2["scope"] as? String == "device")
            try await FirstAppFixture.validate(cli)
        }
    }

    /**
     Invalid choices, incomplete entitlement data, and contradictory options
     fail before any scaffold or manifest write, preserving unrelated files.
     */
    @Test
    func `rejects invalid manual input before writing`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            for (options, diagnostic) in [
                (["--target", "unknown"], "invalid target"),
                (["--language", "unknown"], "invalid language"),
                (["--app-id", " "], "app ID cannot be empty"),
                (["--assistant", "unknown"], "invalid assistant"),
                (["--entitlement", "unknown"], "invalid entitlement"),
                (["--entitlement", "gpio"], "requires --gpio-pins"),
                (["--gpio-pins", "one"], "invalid pin"),
                (["--entitlement", "i2c"], "requires --i2c-device"),
                (["--persist-name", "data"], "requires both --persist-name and --persist-path"),
                (["--persist-path", "/data"], "requires both --persist-name and --persist-path"),
                (["--all-entitlements", "--entitlement", "gpu"], "cannot be combined"),
                (["--no-extra-entitlements", "--entitlement", "gpu"], "cannot be combined"),
                (["--framework", "unknown"], "invalid framework"),
                (
                    ["--framework", "ros2", "--ros2-domain-id", "999"],
                    "--ros2-domain-id must be between"
                ),
                (
                    ["--target", "darwin", "--language", "swift", "--entitlement", "gpu"],
                    "do not support WendyOS container entitlements"
                ),
                (["--target", "wendy-lite", "--language", "python"], "wendy-lite requires swift"),
            ] {
                try await LocalCLIAssertions.write(
                    cli,
                    path: "notes.txt",
                    text: "Keep my unrelated work.\n"
                )
                let before = try await FirstAppFixture.snapshot(cli)
                let entitlementFlags = options.contains {
                    $0.hasPrefix("--entitlement") || $0.hasPrefix("--gpio")
                        || $0.hasPrefix("--persist") || $0 == "--all-entitlements"
                }
                let base =
                    entitlementFlags
                    ? FirstAppFixture.manual.filter { $0 != "--no-extra-entitlements" }
                    : FirstAppFixture.manual
                let result = try await FirstAppFixture.run(cli, base + options, succeeds: false)
                #expect(result.stdout == "")
                let error = try FirstAppFixture.object(result.stderr)
                #expect(error["error"] != nil)
                #expect(result.readableStderr.contains(diagnostic))
                #expect(!result.stderr.contains("Your project is ready"))
                #expect(try await FirstAppFixture.snapshot(cli) == before)
            }
        }
    }

    /**
     Omitted target selection fails in a noninteractive process and identifies
     the missing option. It never opens a terminal or partially creates an app.
     */
    @Test
    func `reports incomplete noninteractive selection before writing`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            let before = try await FirstAppFixture.snapshot(cli)
            let result = try await FirstAppFixture.run(
                cli,
                ["init", "--app-id", "first-app", "--assistant", "skip"],
                succeeds: false
            )
            #expect(result.readableStderr.contains("--target is required"))
            #expect(try await FirstAppFixture.snapshot(cli) == before)
        }
    }

    /**
     Python initialization fills missing scaffold files while retaining existing
     source, build recipes, agent instructions, and unrelated user content.
     A second invocation refuses the manifest conflict and changes no files.
     */
    @Test
    func `preserves partial Python projects and refuses repeated initialization`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            let setup = try await FirstAppFixture.python(
                cli,
                """
                import pathlib
                pathlib.Path('first_app').mkdir()
                for name, text in {'first_app/__init__.py':'# valuable source\\n',
                                   'Dockerfile':'# valuable recipe\\n', 'AGENTS.md':'Local instructions\\n',
                                   'CLAUDE.md':'Local assistant instructions\\n', 'notes.txt':'Keep me\\n'}.items():
                    pathlib.Path(name).write_text(text)
                """
            )
            try setup.requireSuccess()
            let before = try FirstAppFixture.object(try await FirstAppFixture.snapshot(cli))
            _ = try await FirstAppFixture.run(cli, FirstAppFixture.manual)
            let after = try FirstAppFixture.object(try await FirstAppFixture.snapshot(cli))
            for (path, content) in before {
                #expect((after[path] as? NSArray) == (content as? NSArray), "Preserve \(path)")
            }
            #expect(after["pyproject.toml"] != nil)
            try await FirstAppFixture.validate(cli)
            let completed = try await FirstAppFixture.snapshot(cli)
            let repeated = try await FirstAppFixture.run(
                cli,
                FirstAppFixture.manual,
                succeeds: false
            )
            #expect(repeated.readableStderr.contains("wendy.json already exists"))
            #expect(try await FirstAppFixture.snapshot(cli) == completed)
        }
    }

    /**
     **CURRENT OBSERVED FAILURE:** Manual Swift initialization overwrites an
     existing `Sources/first-app/main.swift` when `Package.swift` is absent.
     This spec is disabled by default. Set `WENDY_E2E_KNOWN_INIT_DEFECTS=1` to
     replay the retained failing assertion.

     Required behavior:
     An existing Swift source file belongs to the developer even when no package
     manifest exists yet. Initialization may supply the missing package, but
     must preserve source and unrelated content.

     Tracked by [WDY-4296 — wendy init overwrites existing Swift source when Package.swift is missing](https://linear.app/wendylabsinc/issue/WDY-4296/wendy-init-overwrites-existing-swift-source-when-packageswift-is).
     */
    @Test(
        .disabled(
            if: !FirstAppFixture.knownDefectsEnabled,
            "DATA LOSS: [WDY-4296 — wendy init overwrites existing Swift source when Package.swift is missing](https://linear.app/wendylabsinc/issue/WDY-4296/wendy-init-overwrites-existing-swift-source-when-packageswift-is). Replay with WENDY_E2E_KNOWN_INIT_DEFECTS=1."
        )
    )
    func `preserves existing Swift sources when adding a package`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            let setup = try await FirstAppFixture.python(
                cli,
                """
                from pathlib import Path
                Path('Sources/first-app').mkdir(parents=True)
                Path('Sources/first-app/main.swift').write_text('// valuable source\\n')
                """
            )
            try setup.requireSuccess()
            _ = try await FirstAppFixture.run(
                cli,
                FirstAppFixture.manual + ["--language", "swift"]
            )
            let source = try await LocalCLIAssertions.read(
                cli,
                path: "Sources/first-app/main.swift"
            )
            #expect(source == "// valuable source\n")
            try await FirstAppFixture.validate(cli)
        }
    }

    /**
     **CURRENT OBSERVED FAILURE:** With `Package.swift` already present, manual
     Swift initialization succeeds but leaves the missing source entry point
     absent. This spec is disabled by default. Set
     `WENDY_E2E_KNOWN_INIT_DEFECTS=1` to replay the retained failing assertion.

     Required behavior:
     When the generated package already exists but its source was never written,
     initialization supplies the missing entry point and preserves the package.
     */
    @Test(
        .disabled(
            if: !FirstAppFixture.knownDefectsEnabled,
            "DEFECT: manual Swift init skips the missing source when Package.swift exists. Replay with WENDY_E2E_KNOWN_INIT_DEFECTS=1."
        )
    )
    func `completes a partial Swift scaffold without rewriting its package`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            let package = """
                // swift-tools-version:6.2
                import PackageDescription
                let package = Package(name: "first-app", targets: [.executableTarget(name: "first-app")])
                """
            try await LocalCLIAssertions.write(cli, path: "Package.swift", text: package)
            _ = try await FirstAppFixture.run(
                cli,
                FirstAppFixture.manual + ["--language", "swift"]
            )
            #expect(try await LocalCLIAssertions.read(cli, path: "Package.swift") == package)
            let source = try await FirstAppFixture.python(
                cli,
                """
                from pathlib import Path
                p = Path('Sources/first-app/main.swift')
                print(p.read_text() if p.is_file() else 'MISSING_SOURCE', end='')
                """
            )
            try source.requireSuccess()
            #expect(source.normalizedStdout == "print(\"Hello from first-app!\")\n")
        }
    }
}
