import Foundation
import Testing
import WendyE2ETesting

@Suite(.serialized)
struct `wendy json` {
    var cli: Machine

    init() async throws {
        self.cli = try await Machine.cli()
    }

    @Test
    func `'wendy json schema' prints the wendy.json schema`() async throws {
        let expectedSchema = try String(
            contentsOf: Helper.repositoryRootDirectoryURL()
                .appendingPathComponent("go/internal/shared/appconfig/wendy.schema.json"),
            encoding: .utf8
        )

        try await self.cli.run("./bin/wendy json schema") { standardOutput, standardError in
            #expect(standardError.isEmpty)
            #expect(standardOutput == expectedSchema + "\n")

            let schema = try Helper.jsonObject(from: standardOutput)
            let properties = try #require(schema["properties"] as? [String: Any])
            let required = try #require(schema["required"] as? [Any])

            #expect(schema["$schema"] as? String == "https://json-schema.org/draft/2020-12/schema")
            #expect(schema["$id"] as? String == "https://wendy.sh/schemas/wendy.json")
            #expect(schema["title"] as? String == "Wendy App Configuration")
            #expect(required.contains { ($0 as? String) == "appId" })
            #expect(properties["appId"] != nil)
            #expect(properties["entitlements"] != nil)
        }

        // AI:
        // - Schema output matches the embedded wendy.schema.json file exactly.
        // - Schema output is valid JSON and readable as documentation.
        // - No warnings or noisy diagnostics are printed.
    }

    @Test
    func `'wendy json validate' accepts a valid wendy.json file`() async throws {
        let directory = try Helper.temporaryDirectory(prefix: "wendy-json-valid")
        defer { try? FileManager.default.removeItem(at: directory) }
        let file = try Helper.writeWendyJSON(
            """
            {
              "appId": "sh.wendy.e2e.valid-json",
              "version": "1.0.0",
              "platform": "wendyos",
              "language": "swift",
              "entitlements": [
                { "type": "network", "mode": "host" },
                { "type": "gpio", "pins": [17, 27] }
              ]
            }
            """,
            to: directory
        )

        try await self.cli.run("./bin/wendy json validate \(Helper.shellQuote(file.path))") {
            standardOutput,
            standardError in
            #expect(standardOutput == "wendy.json is valid.\n")
            #expect(standardError.isEmpty)
        }

        // AI:
        // - Success message clearly tells the user the file is valid.
        // - A valid config produces no warnings or noisy stderr output.
    }

    @Test
    func `'wendy json validate' rejects an invalid wendy.json file`() async throws {
        let directory = try Helper.temporaryDirectory(prefix: "wendy-json-invalid")
        defer { try? FileManager.default.removeItem(at: directory) }
        let file = try Helper.writeWendyJSON(
            """
            {
              "version": "1.0.0",
              "language": "swift"
            }
            """,
            to: directory
        )

        let record = try await self.cli.run(
            "./bin/wendy json validate \(Helper.shellQuote(file.path))",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )

        #expect(!record.terminationStatus.isSuccess)
        #expect(record.standardOutput == "")
        #expect(record.standardError?.contains("Error: appId is required") == true)

        // AI:
        // - Failure message clearly identifies the missing required appId field.
        // - Invalid config exits non-zero without printing a success message.
        // - Diagnostics are concise and not noisy.
    }
}
