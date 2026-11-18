import Foundation
import Testing
import WendyE2ETesting

@Suite
struct `'wendy project validate'` {
    let scenario = CLIAndAgentScenario()

    /**
     Validation reports malformed or semantically invalid manifests without
     repairing or rewriting them. Structured semantic failures name the field
     that prevents deployment and never claim the project is valid.
     */
    @Test
    func `reports invalid manifests without rewriting them`() async throws {
        try await scenario.run(authenticated: false) { cli, _ in
            for (text, diagnostic) in FirstAppFixture.invalidManifests {
                try await LocalCLIAssertions.write(cli, path: "wendy.json", text: text)
                let before = try await FirstAppFixture.snapshot(cli)
                let result = try await FirstAppFixture.run(
                    cli,
                    ["project", "validate", "--json"],
                    succeeds: false
                )
                #expect(result.readableStderr.contains(diagnostic))
                if !result.stdout.isEmpty {
                    let summary = try FirstAppFixture.object(result.stdout)
                    #expect(summary["valid"] as? Bool == false)
                    #expect((summary["error"] as? String)?.contains(diagnostic) == true)
                }
                #expect(try await FirstAppFixture.snapshot(cli) == before)
            }
        }
    }

    /**
     A missing manifest fails validation and is left absent. Validation never
     creates scaffold files or chooses a device.
     */
    @Test
    func `rejects a missing manifest without scaffolding`() async throws {
        try await scenario.run(authenticated: false) { cli, _ in
            let before = try await FirstAppFixture.snapshot(cli)
            let result = try await FirstAppFixture.run(
                cli,
                ["project", "validate", "--json"],
                succeeds: false
            )
            #expect(result.stdout == "")
            #expect(result.readableStderr.contains("no manifest"))
            #expect(try await FirstAppFixture.snapshot(cli) == before)
        }
    }
}
