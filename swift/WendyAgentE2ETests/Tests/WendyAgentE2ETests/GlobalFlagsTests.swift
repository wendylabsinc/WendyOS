import Foundation
import Testing
import WendyE2ETesting

@Suite(.serialized)
struct `global flags` {
    var cli: Machine

    init() async throws {
        self.cli = try await Machine.cli()
    }

    @Test
    func `'--json' formats supported command output as JSON`() async throws {
        try await self.cli.run("./bin/wendy --json info") { standardOutput, standardError in
            #expect(standardError.isEmpty)

            let data = Data(standardOutput.utf8)
            let object = try #require(JSONSerialization.jsonObject(with: data) as? [String: Any])

            #expect(object["version"] as? String == "dev")
            #expect(object["os"] as? String == "darwin")
            #expect(object["arch"] as? String == "arm64")
            #expect((object["goVersion"] as? String)?.hasPrefix("go") == true)
            #expect(!standardOutput.contains("Wendy CLI"))
        }

        // AI:
        // - The --json flag switches supported commands from human text to JSON.
        // - JSON keys are stable and useful for automation.
        // - No human-oriented headings, warnings, or noisy diagnostics are printed.
    }

    @Test
    func `'--device' overrides the selected target device`() async throws {
        let agent = try await Machine.agent()

        // TODO: this test looks quite convoluted. Make it nice.

        try await agent.run("make quit || true")
        do {
            try await agent.run("open Build/WendyAgentMac.app")
            try await agent.run(
                """
                for attempt in {1..40}; do
                  nc -z 127.0.0.1 50051 && exit 0
                  sleep 0.25
                done
                echo 'WendyAgentMac did not open port 50051' >&2
                exit 1
                """
            )

            try await self.cli.run("./bin/wendy --json --device 127.0.0.1 device version") {
                standardOutput,
                standardError in
                #expect(standardError.isEmpty)

                let data = Data(standardOutput.utf8)
                let object = try #require(
                    JSONSerialization.jsonObject(with: data) as? [String: Any]
                )

                #expect(object["os"] as? String == "darwin")
                #expect((object["version"] as? String)?.isEmpty == false)
                #expect((object["cliVersion"] as? String)?.isEmpty == false)
                #expect((object["cpuArchitecture"] as? String)?.isEmpty == false)
            }
        } catch {
            try? await agent.run("make quit || true")
            throw error
        }
        try await agent.run("make quit || true")

        // AI:
        // - The CLI reaches the explicitly selected agent via --device.
        // - The response describes the agent machine, not just local CLI state.
        // - JSON output stays stable enough for automation.
    }
}
