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
     Templates still initialize a new subdirectory, an existing empty
     subdirectory, or an empty current directory with `--here`. The actual
     public template is extracted and rendered into a usable project tree.
     */
    @Test
    func `creates a template project in new and empty directories`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            for destination in ["new", "empty", "here"] {
                let project = destination + "/sapi"
                let setupDirectory = destination == "new" ? destination : project
                try await cli.sh(
                    posix: "mkdir -p " + setupDirectory,
                    power: "New-Item -ItemType Directory -Path '" + setupDirectory
                        + "' -Force | Out-Null"
                )
                let command =
                    "wendy init --app-id sapi --target wendyos --language swift"
                    + " --template simple-api --assistant skip --git-init no"
                    + (destination == "here" ? " --here" : "")
                let workingDirectory = destination == "here" ? project : destination
                try await cli.sh(
                    posix: "cd " + workingDirectory + " && " + command,
                    power: "Set-Location '" + workingDirectory + "'; " + command
                ) { result in
                    #expect(result.status.isSuccess)
                    #expect(result.stderr.contains("Your project is ready!"))
                }
                try await cli.sh(
                    posix: """
                        test -f \(project)/Package.swift && test -f \(project)/Sources/sapi/App.swift &&
                          test ! -e \(project)/.git && test ! -e \(project)/sapi && cat \(project)/wendy.json
                        """,
                    power: """
                        foreach ($path in @('\(project)/Package.swift', '\(project)/Sources/sapi/App.swift')) {
                            if (-not (Test-Path -LiteralPath $path -PathType Leaf)) { throw "$path missing" }
                        }
                        if (Test-Path -LiteralPath '\(project)/.git') { throw 'git unexpectedly initialized' }
                        if (Test-Path -LiteralPath '\(project)/sapi') { throw 'project unexpectedly nested' }
                        Get-Content -LiteralPath '\(project)/wendy.json'
                        """
                ) { result in
                    #expect(result.status.isSuccess)
                    let json = try #require(
                        try JSONSerialization.jsonObject(with: Data(result.stdout.utf8))
                            as? [String: Any]
                    )
                    #expect(json["appId"] as? String == "sapi")
                    #expect(json["platform"] as? String == "linux")
                }
                try await cli.sh(
                    posix: "cat " + project + "/Package.swift",
                    power: "Get-Content -LiteralPath '" + project + "/Package.swift'"
                ) { result in
                    #expect(result.status.isSuccess)
                    #expect(result.stdout.contains("name: \"sapi\""))
                    #expect(!result.stdout.contains("{{.APP_ID}}"))
                }
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
     The `--git-init` choice controls repository creation. Skipping git leaves
     no `.git` directory; enabling git creates an initial repository without
     changing generated project content.
     */
    @Test(
        .disabled(
            "WDY-1936: '--git-init' is only honored in the template flow (which requires network); the non-template scaffold never initializes git regardless of --git-init yes/no."
        )
    )
    func `initializes git only when requested`() async throws {
        // TODO: enable once the non-template scaffold honors --git-init, or via a networked template fixture.
    }

    /**
     With `--json`, emits one JSON object containing the app id, target,
     language, enabled entitlements, and written file paths. Human guidance
     stays out of stdout JSON.
     */
    @Test(
        .disabled(
            "WDY-1909: 'wendy init' ignores global --json and prints human scaffolding output only; JSON output is not implemented."
        )
    )
    func `prints JSON project creation summary for automation`() async throws {
        // TODO: enable once 'wendy init' emits a JSON creation summary under --json (WDY-1909).
    }
}
