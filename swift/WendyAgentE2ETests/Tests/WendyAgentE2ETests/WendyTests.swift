import Testing
import WendyE2ETesting

@Suite(.serialized)
struct `wendy` {
    var cli: Machine

    init() async throws {
        self.cli = try await Machine.cli()
    }

    @Test
    func `describes the top-level command groups`() async throws {
        try await self.cli.run("./bin/wendy --help") { standardOutput, standardError in
            #expect(standardError.isEmpty)
            #expect(standardOutput.contains("Project Commands:"))
            #expect(standardOutput.contains("Manage Your Cloud:"))
            #expect(standardOutput.contains("Manage Your Devices:"))
            #expect(standardOutput.contains("Misc.:"))
        }

        // AI:
        // - Help text is readable and well-grouped.
        // - Group names match the CLI docs.
    }

    @Test
    func `prints the CLI version`() async throws {
        try await self.cli.run("./bin/wendy --version") { standardOutput, standardError in
            #expect(standardError.isEmpty)
            #expect(standardOutput.contains(/wendy version \S+/))
        }

        // AI:
        // - Version string is readable.
        // - Version matches the expected CLI build.
    }

    @Test
    func `'--device' selects the target device explicitly`() async throws {
        // REFACTOR: Starting WendyAgentMac and shutting it down are test fixture
        // concerns. Replace this inline lifecycle management with a dedicated
        // DSL or something. This is good enough for the first draft.

        let agent = try await Machine.agent()
        try await Helper.withAsyncCleanup {

            try await agent.run("make quit || true")
            try await agent.run("open Build/WendyAgentMac.app")
            try await agent
                .command("nc -z 127.0.0.1 50051")
                .poll(until: .success, timeoutMessage: "WendyAgentMac did not open port 50051")
                .run()

            try await self.cli.run("./bin/wendy --json --device 127.0.0.1 device version") {
                standardOutput,
                standardError in
                #expect(standardError.isEmpty)

                let object = try Helper.jsonObject(from: standardOutput)

                #expect(object["os"] as? String == "darwin")
                #expect((object["version"] as? String)?.isEmpty == false)
                #expect((object["cliVersion"] as? String)?.isEmpty == false)
                #expect((object["cpuArchitecture"] as? String)?.isEmpty == false)
            }

        } cleanup: {
            try await agent.run("make quit || true")
        }

        // AI:
        // - The CLI reaches the explicitly selected agent via --device.
        // - The response describes the agent machine, not just local CLI state.
    }
}

// MARK: -

@Suite(.serialized)
struct `wendy info` {
    var cli: Machine

    init() async throws {
        self.cli = try await Machine.cli()
    }

    @Test
    func `prints CLI and system information`() async throws {
        try await self.cli.run("./bin/wendy info") { standardOutput, standardError in
            #expect(standardError.isEmpty)
            #expect(standardOutput.contains("Wendy CLI"))
            #expect(standardOutput.contains(/Version:\s+\S+/))
            #expect(standardOutput.contains(/OS:\s+\S+/))
            #expect(standardOutput.contains(/Arch:\s+\S+/))
            #expect(standardOutput.contains(/Go Version:\s+\S+/))
        }

        // AI:
        // - CLI/system details are complete and sensible.
        // - No unexpected warnings or noisy diagnostics.
    }

    @Test
    func `'--json' formats CLI and system information as JSON`() async throws {
        try await self.cli.run("./bin/wendy --json info") { standardOutput, standardError in
            #expect(standardError.isEmpty)

            let object = try Helper.jsonObject(from: standardOutput)

            #expect(object["version"] as? String == "dev")
            #expect(object["os"] as? String == "darwin")
            #expect(object["arch"] as? String == "arm64")
            #expect((object["goVersion"] as? String)?.hasPrefix("go") == true)
            #expect(!standardOutput.contains("Wendy CLI"))
        }
    }
}
