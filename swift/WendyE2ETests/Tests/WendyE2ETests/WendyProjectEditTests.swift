import Foundation
import Testing
import WendyE2ETesting

@Suite
struct `'wendy project edit'` {
    let scenario = CLIAndAgentScenario()

    /**
     Field edits preview the exact value change before saving, preserve unrelated
     data, and keep repeated application of the same value free of changes.
     */
    @Test
    func `previews an edit and reports repeated edits as unchanged`() async throws {
        try await scenario.run(authenticated: false) { cli, _ in
            try await FirstAppFixture.prepareProject(cli)
            let before = try await FirstAppFixture.snapshot(cli)
            let command = ["project", "edit", "network", "--mode", "bridge", "--json"]
            let preview = try await FirstAppFixture.run(cli, command + ["--dry-run"])
            let expectedChanges = [
                ["path": "/entitlements/0/mode", "before": "host", "after": "bridge"]
            ]
            #expect(
                try FirstAppFixture.editSummary(preview, saved: false, dryRun: true) as NSArray
                    == expectedChanges as NSArray
            )
            #expect(try await FirstAppFixture.snapshot(cli) == before)
            let saved = try await FirstAppFixture.run(cli, command)
            #expect(
                try FirstAppFixture.editSummary(saved, saved: true, dryRun: false) as NSArray
                    == expectedChanges as NSArray
            )
            var expected = try FirstAppFixture.object(FirstAppFixture.project)
            expected["entitlements"] = [["type": "network", "mode": "bridge"]]
            try await FirstAppFixture.sameManifest(cli, expected)
            let completed = try await FirstAppFixture.snapshot(cli)
            let repeated = try await FirstAppFixture.run(cli, command)
            #expect(try FirstAppFixture.editSummary(repeated, saved: false, dryRun: false).isEmpty)
            #expect(try await FirstAppFixture.snapshot(cli) == completed)
        }
    }

    /**
     A field edit validates the resulting manifest before saving. Unknown
     services, missing capabilities, conflicting network settings, and raw
     editor requests without a terminal fail without changing project files.
     */
    @Test
    func `rejects invalid edits without changing the manifest`() async throws {
        try await scenario.run(authenticated: false) { cli, _ in
            for (options, diagnostic) in [
                (["network", "--mode", "unknown"], "network mode must be"),
                (["network", "--mode", "mesh"], "requires --service-cidr"),
                (["network", "--mode", "host", "--service", "missing"], "unknown service"),
                (["gpio", "--pins", "4"], "not found in this scope"),
                (["--raw"], "requires an interactive terminal"),
            ] {
                try await FirstAppFixture.prepareProject(cli)
                let before = try await FirstAppFixture.snapshot(cli)
                let result = try await FirstAppFixture.run(
                    cli,
                    ["project", "edit"] + options + ["--json"],
                    succeeds: false
                )
                #expect(result.stdout == "")
                #expect(try FirstAppFixture.object(result.stderr)["error"] != nil)
                #expect(result.readableStderr.contains(diagnostic))
                #expect(try await FirstAppFixture.snapshot(cli) == before)
            }
        }
    }

    /**
     `--file` edits the selected manifest even when the current directory
     contains a different project. Paths with spaces are accepted, and no
     surrounding file or directory is changed.
     */
    @Test
    func `edits only the explicitly selected manifest`() async throws {
        try await scenario.run(authenticated: false) { cli, _ in
            try await FirstAppFixture.prepareProject(cli)
            let setup = try await FirstAppFixture.python(
                cli,
                """
                import pathlib, shutil
                pathlib.Path('selected app').mkdir()
                shutil.copyfile('wendy.json', 'selected app/custom.json')
                """
            )
            try setup.requireSuccess()
            let before = try FirstAppFixture.object(try await FirstAppFixture.snapshot(cli))
            let result = try await FirstAppFixture.run(
                cli,
                [
                    "project", "edit", "network", "--mode", "bridge", "--file",
                    "selected app/custom.json", "--json",
                ]
            )
            let summary = try FirstAppFixture.object(result.stdout)
            #expect(summary["saved"] as? Bool == true)
            let path = try #require(summary["path"] as? String)
            #expect(
                path.replacingOccurrences(of: "\\", with: "/").hasSuffix("selected app/custom.json")
            )
            var expected = try FirstAppFixture.object(FirstAppFixture.project)
            expected["entitlements"] = [["type": "network", "mode": "bridge"]]
            #expect(
                try await FirstAppFixture.manifest(cli, path: "selected app/custom.json")
                    as NSDictionary == expected as NSDictionary
            )
            let after = try FirstAppFixture.object(try await FirstAppFixture.snapshot(cli))
            #expect(Set(after.keys) == Set(before.keys))
            for (name, value) in before where name != "selected app/custom.json" {
                #expect(after[name] as? NSArray == value as? NSArray, "Preserve \(name)")
            }
        }
    }
}
