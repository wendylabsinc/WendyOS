import ArgumentParser
import Foundation
import Testing

@testable import SwiftE2ETestingCLI

@Suite
struct ReviewHarnessTests {
    @Test(arguments: ["Claude Code subscription", "ANTHROPIC_API_KEY"])
    func `logs selected usage and preserves result text and review files`(auth: String) throws {
        let text = "Review complete.\n\nTwo observations."
        let response = """
            {"type":"result","subtype":"success","result":"Review complete.\\n\\nTwo observations.",
             "usage":{"input_tokens":123,"output_tokens":45,"cache_creation_input_tokens":67,"cache_read_input_tokens":89},
             "total_cost_usd":0.42,"modelUsage":{"claude-opus-4-7":{"costUSD":0.42}},
             "session_id":"PRIVATE_SESSION","extra":{"transcript":"PRIVATE_TRANSCRIPT"}}
            """
        let output = try runStub(response: response, auth: auth)
        #expect(output.contains(text + "\n"))
        #expect(output.contains("input_tokens: 123"))
        #expect(output.contains("output_tokens: 45"))
        #expect(output.contains("cache_creation_input_tokens: 67"))
        #expect(output.contains("cache_read_input_tokens: 89"))
        #expect(output.contains("total_cost_usd: 0.42"))
        #expect(output.contains("not actual billed dollars"))
        #expect(output.contains("Selected auth: \(auth)"))
        #expect(output.contains("Reported models: claude-opus-4-7"))
        #expect(output.contains("excludes authentication probe"))
        #expect(!output.contains("PRIVATE_"))
        #expect(!output.contains("\"usage\""))
    }

    @Test(arguments: [
        #"{"result":"Still complete."}"#,
        #"{"result":"Still complete.","usage":{"input_tokens":"bad"},"total_cost_usd":"bad"}"#,
        #"{"result":"Still complete.","usage":null,"total_cost_usd":null}"#,
    ])
    func `missing or malformed accounting does not hide result text`(response: String) throws {
        let output = try runStub(response: response)
        #expect(output.contains("Still complete.\n"))
        #expect(output.contains("input_tokens: unavailable"))
        #expect(output.contains("output_tokens: unavailable"))
        #expect(output.contains("cache_creation_input_tokens: unavailable"))
        #expect(output.contains("cache_read_input_tokens: unavailable"))
        #expect(output.contains("total_cost_usd: unavailable"))
    }

    @Test
    func `partial accounting distinguishes reported zero from missing fields`() throws {
        let output = try runStub(
            response: #"{"result":"Done.","usage":{"input_tokens":12,"output_tokens":0}}"#
        )
        #expect(output.contains("input_tokens: 12"))
        #expect(output.contains("output_tokens: 0"))
        #expect(output.contains("cache_read_input_tokens: unavailable"))
        #expect(output.contains("total_cost_usd: unavailable"))
    }

    @Test(arguments: ["", #"{"result":"PRIVATE_TRUNCATED""#, "PRIVATE_NON_JSON"])
    func `interrupted or invalid output stays unavailable and preserves failure`(
        response: String
    ) throws {
        let output = try runStub(response: response, status: 143)
        #expect(output.contains("input_tokens: unavailable"))
        #expect(output.contains("total_cost_usd: unavailable"))
        #expect(!output.contains("PRIVATE_"))
    }

    @Test
    func `nonzero exit still logs available usage and result`() throws {
        let output = try runStub(
            response:
                #"{"result":"Review failed.","usage":{"input_tokens":9},"total_cost_usd":0.01}"#,
            status: 7
        )
        #expect(output.contains("Review failed.\n"))
        #expect(output.contains("input_tokens: 9"))
        #expect(output.contains("total_cost_usd: 0.01"))
    }

    @Test
    func `large JSON does not fill a pipe or expose ignored fields`() throws {
        let response =
            #"{"result":"Complete.","transcript":""#
            + String(repeating: "PRIVATE_TRANSCRIPT", count: 100_000) + #""}"#
        let output = try runStub(response: response)
        #expect(output.contains("Complete.\n"))
        #expect(!output.contains("PRIVATE_"))
    }

    @Test(arguments: [0, 7])
    func `uncaptured commands keep stdout and exit behavior`(status: Int) throws {
        let text = "Custom or Codex output\nNo usage envelope.\n"
        let output = try runStub(response: text, status: status, capture: false)
        #expect(output == text)
    }
}

private func runStub(
    response: String,
    status: Int = 0,
    auth: String = "Claude Code subscription",
    capture: Bool = true
) throws -> String {
    let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
    let run = root.appendingPathComponent("run")
    try FileManager.default.createDirectory(at: run, withIntermediateDirectories: true)
    defer { try? FileManager.default.removeItem(at: root) }
    try Data(response.utf8).write(to: root.appendingPathComponent("response.json"))
    let review = """
        ---
        {"schema":"wendy.e2e.review.v1","title":"Fixture finding","scope":"report","reviewer":"claude-opus-4-7","severity":"info"}
        ---
        # Fixture finding

        Fixture summary.

        ## Details

        Fixture details.
        """
    try Data(review.utf8).write(to: root.appendingPathComponent("fixture-finding.md"))
    let log = root.appendingPathComponent("log.txt")
    FileManager.default.createFile(atPath: log.path, contents: nil)
    let output = try FileHandle(forWritingTo: log)
    defer { try? output.close() }
    let harness = ShellReviewHarness(
        harnessName: "stub",
        modelName: "claude-opus-4-7",
        shellCommand: """
            test -s "$WENDY_E2E_REVIEW_PROMPT" || exit 99
            printf 'review file stays unchanged\\n' > "$WENDY_E2E_REVIEW_RUN_DIR/review.md"
            mkdir "$WENDY_E2E_REVIEW_RUN_DIR/review.claude-opus-4-7"
            cp "$WENDY_E2E_REVIEW_REPO_DIR/fixture-finding.md" "$WENDY_E2E_REVIEW_RUN_DIR/review.claude-opus-4-7/"
            cat "$WENDY_E2E_REVIEW_REPO_DIR/response.json"
            exit \(status)
            """,
        commandSource: "test",
        invocationSummary: "test",
        authSummary: auth,
        modelSource: "test",
        capturesClaudeJSON: capture
    )
    if status == 0 {
        try harness.review(
            prompt: "Fixture instructions",
            repoURL: root,
            runURL: run,
            output: output
        )
    } else {
        #expect(
            performing: {
                try harness.review(
                    prompt: "Fixture instructions",
                    repoURL: root,
                    runURL: run,
                    output: output
                )
            },
            throws: { error in
                (error as? ValidationError)?.message
                    == "Review harness stub failed with exit status \(status)."
            }
        )
    }
    #expect(
        try String(contentsOf: run.appendingPathComponent("review.md"), encoding: .utf8)
            == "review file stays unchanged\n"
    )
    #expect(
        try FileManager.default.contentsOfDirectory(atPath: run.path).sorted()
            == ["review.claude-opus-4-7", "review.md"]
    )
    let reviews = try loadE2EReviews(
        in: run,
        expectedScope: "report",
        expectedReviewer: "claude-opus-4-7",
        relativeTo: run
    )
    #expect(reviews.count == 1)
    #expect(reviews.first?.summaryMarkdown == "Fixture summary.")
    return try String(contentsOf: log, encoding: .utf8)
}
