import Foundation
import Testing
import WendyE2ETesting

@Suite
struct `'wendy project show'` {
    let scenario = CLIAndAgentScenario()

    /**
     `--json` exposes the complete manifest, including capabilities, readiness,
     hooks, service overrides, and fields added by newer tools. Reading a
     project preserves the original bytes and file permissions.
     */
    @Test
    func `shows the complete manifest without rewriting it`() async throws {
        try await scenario.run(authenticated: false) { cli, _ in
            try await FirstAppFixture.prepareProject(cli)
            let before = try await FirstAppFixture.snapshot(cli)
            let result = try await FirstAppFixture.run(cli, ["project", "show", "--json"])
            #expect(result.stderr == "")
            let summary = try FirstAppFixture.object(result.stdout)
            #expect(summary["exists"] as? Bool == true)
            #expect(summary["compose"] as? Bool == false)
            #expect(summary["services"] as? [String] == ["worker"])
            #expect(
                summary["manifest"] as? NSDictionary
                    == (try FirstAppFixture.object(FirstAppFixture.project)) as NSDictionary
            )
            #expect(try await FirstAppFixture.snapshot(cli) == before)
        }
    }

    /**
     `--file` chooses the requested manifest or project directory, including
     paths containing spaces. An unrelated manifest in the current directory
     remains untouched and does not influence the selected project.
     */
    @Test
    func `reads the explicitly selected project`() async throws {
        try await scenario.run(authenticated: false) { cli, _ in
            for path in ["selected app", "selected app/custom.json"] {
                try await FirstAppFixture.prepareProject(cli)
                let setup = try await FirstAppFixture.python(
                    cli,
                    """
                    from pathlib import Path
                    Path('selected app').mkdir(exist_ok=True)
                    Path('selected app/wendy.json').write_text('{"appId":"selected-app","version":"2.0.0"}')
                    Path('selected app/custom.json').write_text('{"appId":"selected-file","version":"3.0.0"}')
                    """
                )
                try setup.requireSuccess()
                let before = try await FirstAppFixture.snapshot(cli)
                let result = try await FirstAppFixture.run(
                    cli,
                    ["project", "show", "--file", path, "--json"]
                )
                let summary = try FirstAppFixture.object(result.stdout)
                let manifest = try #require(summary["manifest"] as? [String: Any])
                #expect(
                    manifest["appId"] as? String
                        == (path.hasSuffix("custom.json") ? "selected-file" : "selected-app")
                )
                #expect(
                    manifest["version"] as? String
                        == (path.hasSuffix("custom.json") ? "3.0.0" : "2.0.0")
                )
                #expect(manifest["future"] == nil)
                #expect(try await FirstAppFixture.snapshot(cli) == before)
            }
        }
    }

    /**
     Showing a directory without a manifest reports its absence as structured
     data. Inspection never creates a project implicitly.
     */
    @Test
    func `reports an absent manifest without creating one`() async throws {
        try await scenario.run(authenticated: false) { cli, _ in
            let before = try await FirstAppFixture.snapshot(cli)
            let result = try await FirstAppFixture.run(cli, ["project", "show", "--json"])
            let summary = try FirstAppFixture.object(result.stdout)
            #expect(summary["exists"] as? Bool == false)
            #expect(summary["manifest"] as? NSDictionary == [:])
            #expect(try await FirstAppFixture.snapshot(cli) == before)
        }
    }
}
