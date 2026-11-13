import Foundation
import Testing
import WendyE2ETesting

@Suite
struct `'wendy project add'` {
    let scenario = CLIAndAgentScenario()

    /**
     Explicit capability fields are validated and preserved in the saved
     manifest. A dry run previews the same changes without writing. Existing
     capabilities, service settings, readiness, hooks, and unknown fields survive.
     */
    @Test
    func `previews and adds a capability with its configuration`() async throws {
        try await scenario.run(authenticated: false) { cli, _ in
            for type in ["gpio", "i2c", "persist", "http"] {
                try await FirstAppFixture.prepareProject(cli)
                let flags: [String]
                let entry: [String: Any]
                switch type {
                case "gpio":
                    flags = ["--pins", "4,17"]
                    entry = ["type": type, "pins": [4, 17]]
                case "i2c":
                    flags = ["--bus", "i2c-1"]
                    entry = ["type": type, "device": "i2c-1"]
                case "persist":
                    flags = ["--name", "recordings", "--path", "/data/recordings"]
                    entry = ["type": type, "name": "recordings", "path": "/data/recordings"]
                default:
                    flags = ["--port", "3001"]
                    entry = ["type": type, "port": 3001]
                }
                let before = try await FirstAppFixture.snapshot(cli)
                let command = ["project", "add", type, "--json"] + flags
                let preview = try await FirstAppFixture.run(cli, command + ["--dry-run"])
                let planned = try FirstAppFixture.editSummary(preview, saved: false, dryRun: true)
                #expect(!planned.isEmpty)
                #expect(
                    planned.allSatisfy {
                        ($0["path"] as? String)?.hasPrefix("/entitlements/1/") == true
                    }
                )
                #expect(try await FirstAppFixture.snapshot(cli) == before)
                let saved = try await FirstAppFixture.run(cli, command)
                let changes = try FirstAppFixture.editSummary(saved, saved: true, dryRun: false)
                #expect(changes as NSArray == planned as NSArray)
                var expected = try FirstAppFixture.object(FirstAppFixture.project)
                expected["entitlements"] = [
                    ["type": "network", "mode": "host"] as [String: Any], entry,
                ]
                try await FirstAppFixture.sameManifest(cli, expected)
                try await FirstAppFixture.validate(cli)
            }
        }
    }

    /**
     Invalid capability data, irrelevant flags, and missing required values
     fail with a diagnostic before saving. A failed add leaves every project
     file and permission unchanged.
     */
    @Test
    func `rejects incomplete or invalid capability data before saving`() async throws {
        try await scenario.run(authenticated: false) { cli, _ in
            for (options, diagnostic) in [
                (["persist", "--name", "recordings"], "requires --path"),
                (
                    ["persist", "--name", "recordings", "--path", "relative"],
                    "persist path must be absolute"
                ),
                (["http", "--port", "0"], "port must be between"),
                (["http", "--port", "65536"], "port must be between"),
                (["http", "--port", "three"], "must be an integer"),
                (["i2c", "--bus", "not-a-bus"], "i2c-N format"),
                (["gpio", "--pins", "one"], "invalid pin"),
                (["gpio", "--port", "3001"], "does not use --port"),
                (["unknown"], "unknown capability"),
            ] {
                try await FirstAppFixture.prepareProject(cli)
                let before = try await FirstAppFixture.snapshot(cli)
                let result = try await FirstAppFixture.run(
                    cli,
                    ["project", "add"] + options + ["--json"],
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
     `--service` adds a local capability override only to that service. App
     defaults and the other fields of the service are preserved.
     */
    @Test
    func `adds a capability only to the selected service`() async throws {
        try await scenario.run(authenticated: false) { cli, _ in
            try await FirstAppFixture.prepareProject(cli)
            let result = try await FirstAppFixture.run(
                cli,
                ["project", "add", "http", "--port", "3002", "--service", "worker", "--json"]
            )
            let changes = try FirstAppFixture.editSummary(result, saved: true, dryRun: false)
            #expect(!changes.isEmpty)
            #expect(
                changes.allSatisfy {
                    ($0["path"] as? String)?.hasPrefix("/services/worker/entitlements/") == true
                }
            )
            var expected = try FirstAppFixture.object(FirstAppFixture.project)
            var services = try #require(expected["services"] as? [String: Any])
            var worker = try #require(services["worker"] as? [String: Any])
            worker["entitlements"] = [["type": "audio"], ["type": "http", "port": 3002]]
            services["worker"] = worker
            expected["services"] = services
            try await FirstAppFixture.sameManifest(cli, expected)
        }
    }
}
