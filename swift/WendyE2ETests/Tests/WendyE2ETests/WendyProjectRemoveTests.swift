import Foundation
import Testing
import WendyE2ETesting

@Suite
struct `'wendy project remove'` {
    let scenario = CLIAndAgentScenario()

    /**
     Multiple persistent mounts require an explicit entry selection. Previewing
     removal changes no files; saving removes only the selected mount and
     retains the other mount, capabilities, and unrelated project fields.
     */
    @Test
    func `requires an unambiguous entry and removes only the selected mount`() async throws {
        try await scenario.run(authenticated: false) { cli, _ in
            try await FirstAppFixture.prepareProject(cli)
            var expected = try FirstAppFixture.object(FirstAppFixture.project)
            let entries: [[String: Any]] = [
                ["type": "network", "mode": "host"],
                ["type": "persist", "name": "recordings", "path": "/data/recordings"],
                ["type": "persist", "name": "models", "path": "/data/models"],
            ]
            expected["entitlements"] = entries
            let text = String(
                decoding: try JSONSerialization.data(withJSONObject: expected),
                as: UTF8.self
            )
            try await LocalCLIAssertions.write(cli, path: "wendy.json", text: text)
            let before = try await FirstAppFixture.snapshot(cli)
            let ambiguous = try await FirstAppFixture.run(
                cli,
                ["project", "remove", "persist", "--json"],
                succeeds: false
            )
            #expect(ambiguous.readableStderr.contains("--entry"))
            #expect(try await FirstAppFixture.snapshot(cli) == before)
            let command = ["project", "remove", "persist", "--entry", "2", "--json"]
            let preview = try await FirstAppFixture.run(cli, command + ["--dry-run"])
            let planned = try FirstAppFixture.editSummary(preview, saved: false, dryRun: true)
            #expect(!planned.isEmpty)
            #expect(
                planned.allSatisfy {
                    ($0["path"] as? String)?.hasPrefix("/entitlements/2/") == true
                }
            )
            #expect(try await FirstAppFixture.snapshot(cli) == before)
            let result = try await FirstAppFixture.run(cli, command)
            #expect(
                try FirstAppFixture.editSummary(result, saved: true, dryRun: false) as NSArray
                    == planned as NSArray
            )
            expected["entitlements"] = Array(entries.prefix(2))
            try await FirstAppFixture.sameManifest(cli, expected)
            try await FirstAppFixture.validate(cli)
        }
    }

    /**
     Removing a missing capability, an out-of-range entry, or an entry of a
     different type reports failure and preserves every file.
     */
    @Test
    func `rejects absent or mismatched entries without saving`() async throws {
        try await scenario.run(authenticated: false) { cli, _ in
            for options in [["gpio"], ["network", "--entry", "9"], ["gpio", "--entry", "0"]] {
                try await FirstAppFixture.prepareProject(cli)
                let before = try await FirstAppFixture.snapshot(cli)
                let result = try await FirstAppFixture.run(
                    cli,
                    ["project", "remove"] + options + ["--json"],
                    succeeds: false
                )
                #expect(result.stdout == "")
                #expect(try FirstAppFixture.object(result.stderr)["error"] != nil)
                #expect(try await FirstAppFixture.snapshot(cli) == before)
            }
        }
    }
}
