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
        #expect(output.contains("any result text was discarded"))
        #expect(!output.contains("PRIVATE_"))
    }

    @Test
    func `verbose output uses the result element of its message array`() throws {
        let output = try runStub(response: claudeVerboseSuccess)
        #expect(output.contains("MOCK_REVIEW_TEXT\n"))
        #expect(output.contains("input_tokens: 11"))
        #expect(output.contains("output_tokens: 3"))
        #expect(output.contains("Reported models: claude-sonnet-5-5"))
        #expect(!output.contains("total_cost_usd: unavailable"))
        #expect(!output.contains("discarded"))
    }

    @Test
    func `verbose output keeps the reason for a failed review`() throws {
        let output = try runStub(response: claudeVerboseOverloaded, status: 1)
        #expect(output.contains("API Error: 529 MOCK_OVERLOADED"))
        #expect(!output.contains("discarded"))
    }

    @Test(arguments: [
        "Welcome from ~/.bash_profile\n\(claudeResultLine)\n",
        "\(claudeResultLine)\nGoodbye from ~/.bash_logout\n",
        "Welcome from ~/.bash_profile\n\(claudeVerboseSuccess)\n",
        "\(claudeResultLine)\n{\"level\":\"info\",\"msg\":\"Goodbye\"}\n",
    ])
    func `shell output around the JSON line does not lose the result`(response: String) throws {
        let output = try runStub(response: response)
        #expect(output.contains("MOCK_REVIEW_TEXT\n"))
        #expect(output.contains("input_tokens: 11"))
        #expect(!output.contains("Welcome"))
        #expect(!output.contains("Goodbye"))
        #expect(!output.contains("discarded"))
    }

    @Test(arguments: [
        "PRIVATE_NON_JSON\nPRIVATE_SECOND_LINE\n",
        #"[{"type":"system","subtype":"PRIVATE_INIT"},{"type":"assistant"}]"#,
        #"["PRIVATE_NOT_AN_OBJECT"]"#,
        #"{"type":"system","subtype":"PRIVATE_INIT"}"#,
        "{\"level\":\"PRIVATE_LOG\"}\n{\"type\":\"system\",\"subtype\":\"PRIVATE_INIT\"}\n",
    ])
    func `a successful exit with unusable output says its text was discarded`(
        response: String
    ) throws {
        let output = try runStub(response: response)
        #expect(output.contains("any result text was discarded"))
        #expect(output.contains("total_cost_usd: unavailable"))
        #expect(!output.contains("PRIVATE_"))
    }

    @Test
    func `a result with no text says so and keeps its usage`() throws {
        let output = try runStub(
            response: #"{"type":"result","subtype":"error_max_turns","usage":{"input_tokens":5}}"#,
            status: 1
        )
        #expect(output.contains("any result text was discarded"))
        #expect(output.contains("input_tokens: 5"))
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

    @Test(arguments: [false, true])
    func `provider and custom shell access remains an explicit source-scope limitation`(
        capture: Bool
    ) throws {
        let response =
            capture ? #"{"result":"GENERATED_TOOL_READ_SENTINEL"}"# : "GENERATED_TOOL_READ_SENTINEL"
        let output = try runStub(
            response: response,
            capture: capture,
            responsePath: "go/proto/gen/sentinel.pb.go"
        )
        #expect(output.contains("GENERATED_TOOL_READ_SENTINEL"))
    }
}

// Claude Code 2.1.316 output against a local mock API, with `verbose` configured
// or a ~/.bash_profile that echoes. Only the init record's long lists are trimmed.
private let claudeVerboseSuccess = #"[{"type":"system","subtype":"init","cwd":"/home/runner","session_id":"66309e0b-aa81-42d5-af17-cd313dc04c8c","tools":[],"mcp_servers":[],"model":"claude-sonnet-5-5","permissionMode":"bypassPermissions","slash_commands":[],"terminal_slash_commands":[],"apiKeySource":"ANTHROPIC_API_KEY","claude_code_version":"2.1.316","output_style":"default","agents":[],"skills":[],"plugins":[],"capabilities":[],"analytics_disabled":true,"product_feedback_disabled":true,"uuid":"783723c4-5b17-4f81-a0f6-4fde2de09ee0","original_cwd":"/home/runner","additional_directories":[],"fast_mode_state":"off","fast_mode_disabled_reason":"sdk_opt_in_required","per_turn_effort_active":true,"ultrathink_available":false,"view_mode":"default"},{"type":"assistant","message":{"id":"msg_mock","type":"message","role":"assistant","model":"claude-sonnet-5-5","content":[{"type":"text","text":"MOCK_REVIEW_TEXT"}],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":11,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0},"context_management":null},"parent_tool_use_id":null,"session_id":"66309e0b-aa81-42d5-af17-cd313dc04c8c","uuid":"1c234900-700c-41ab-acdc-b9901f36640a","timestamp":"2026-11-04T18:13:41.754Z"},{"duration_api_ms":6,"stop_reason":"end_turn","session_id":"66309e0b-aa81-42d5-af17-cd313dc04c8c","total_cost_usd":0.000052000000000000004,"usage":{"input_tokens":11,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":3,"output_tokens_details":{"thinking_tokens":0},"server_tool_use":{"web_search_requests":0,"web_fetch_requests":0},"service_tier":"standard","cache_creation":{"ephemeral_1h_input_tokens":0,"ephemeral_5m_input_tokens":0},"inference_geo":"","iterations":[],"speed":"standard","fallback_credit":null},"modelUsage":{"claude-sonnet-5-5":{"inputTokens":11,"outputTokens":3,"cacheReadInputTokens":0,"cacheCreationInputTokens":0,"webSearchRequests":0,"costUSD":0.000052000000000000004,"contextWindow":1000000,"maxOutputTokens":128000,"thinkingTokens":0,"cacheCreationEphemeral5mInputTokens":0,"cacheCreationEphemeral1hInputTokens":0,"canonicalModel":"claude-sonnet-5-5","provider":"firstParty","costBasis":"list"}},"permission_denials":[],"terminal_reason":"completed","fast_mode_state":"off","fast_mode_disabled_reason":"sdk_opt_in_required","subagent_stats":{"spawned":0,"requested":{"background":0,"foreground":0,"unset":0},"started_in_background":0,"max_depth":0,"spawned_by_subagents":0,"completed":0,"failed":0,"killed":{"parent":0,"user":0,"system":0},"refused":{"depth_limit":0,"concurrency_limit":0,"budget":0},"by_type":{}},"safety_stops":0,"is_error":false,"num_turns":1,"subtype":"success","api_error_status":null,"result":"MOCK_REVIEW_TEXT","ttft_ms":21,"type":"result","duration_ms":24,"uuid":"1e4c386a-af31-4257-817f-40ccdd18c435","ttft_stream_ms":21,"time_to_request_ms":19,"first_content_frame_ms":21,"queued_turn_count":0,"result_index":0}]"#
private let claudeVerboseOverloaded = #"[{"type":"system","subtype":"init","cwd":"/home/runner","session_id":"ecd37e96-1a35-41c5-b875-09584471945b","tools":[],"mcp_servers":[],"model":"claude-sonnet-5-5","permissionMode":"bypassPermissions","slash_commands":[],"terminal_slash_commands":[],"apiKeySource":"ANTHROPIC_API_KEY","claude_code_version":"2.1.316","output_style":"default","agents":[],"skills":[],"plugins":[],"capabilities":[],"analytics_disabled":true,"product_feedback_disabled":true,"uuid":"e77b3aa1-631d-4128-9233-422a440a3ab2","original_cwd":"/home/runner","additional_directories":[],"fast_mode_state":"off","fast_mode_disabled_reason":"sdk_opt_in_required","per_turn_effort_active":true,"ultrathink_available":false,"view_mode":"default"},{"type":"assistant","message":{"diagnostics":null,"id":"41341564-c244-488d-9fbe-f764148898df","container":null,"model":"<synthetic>","role":"assistant","stop_details":null,"stop_reason":"stop_sequence","stop_sequence":"","type":"message","usage":{"output_tokens_details":null,"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"server_tool_use":{"web_search_requests":0,"web_fetch_requests":0},"service_tier":null,"cache_creation":{"ephemeral_1h_input_tokens":0,"ephemeral_5m_input_tokens":0},"inference_geo":null,"iterations":null,"speed":null,"fallback_credit":null},"content":[{"type":"text","text":"API Error: 529 MOCK_OVERLOADED. This is a server-side issue, usually temporary — try again in a moment. If it persists, check your inference gateway (127.0.0.1:23700)."}],"context_management":null},"parent_tool_use_id":null,"session_id":"ecd37e96-1a35-41c5-b875-09584471945b","uuid":"00843734-e445-490f-84a8-189d10864b53","timestamp":"2026-11-04T18:13:42.394Z","error":"server_error","is_api_error_message":true,"api_error_status":529},{"duration_api_ms":0,"stop_reason":"stop_sequence","session_id":"ecd37e96-1a35-41c5-b875-09584471945b","total_cost_usd":0,"usage":{"output_tokens_details":{"thinking_tokens":0},"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0,"server_tool_use":{"web_search_requests":0,"web_fetch_requests":0},"service_tier":"standard","cache_creation":{"ephemeral_1h_input_tokens":0,"ephemeral_5m_input_tokens":0},"inference_geo":"","iterations":[],"speed":"standard","fallback_credit":null},"modelUsage":{},"permission_denials":[],"terminal_reason":"api_error","fast_mode_state":"off","fast_mode_disabled_reason":"sdk_opt_in_required","subagent_stats":{"spawned":0,"requested":{"background":0,"foreground":0,"unset":0},"started_in_background":0,"max_depth":0,"spawned_by_subagents":0,"completed":0,"failed":0,"killed":{"parent":0,"user":0,"system":0},"refused":{"depth_limit":0,"concurrency_limit":0,"budget":0},"by_type":{}},"safety_stops":0,"is_error":true,"num_turns":1,"subtype":"success","api_error_status":529,"result":"API Error: 529 MOCK_OVERLOADED. This is a server-side issue, usually temporary — try again in a moment. If it persists, check your inference gateway (127.0.0.1:23700).","type":"result","duration_ms":23,"uuid":"7dabb79d-142d-4d19-b4db-8e7221c8ca72","queued_turn_count":0,"result_index":0}]"#
private let claudeResultLine = #"{"duration_api_ms":5,"stop_reason":"end_turn","session_id":"bbca82a5-b9d6-44a9-af82-d3ca2fa5b01b","total_cost_usd":0.000052000000000000004,"usage":{"input_tokens":11,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":3,"output_tokens_details":{"thinking_tokens":0},"server_tool_use":{"web_search_requests":0,"web_fetch_requests":0},"service_tier":"standard","cache_creation":{"ephemeral_1h_input_tokens":0,"ephemeral_5m_input_tokens":0},"inference_geo":"","iterations":[],"speed":"standard","fallback_credit":null},"modelUsage":{"claude-sonnet-5-5":{"inputTokens":11,"outputTokens":3,"cacheReadInputTokens":0,"cacheCreationInputTokens":0,"webSearchRequests":0,"costUSD":0.000052000000000000004,"contextWindow":1000000,"maxOutputTokens":128000,"thinkingTokens":0,"cacheCreationEphemeral5mInputTokens":0,"cacheCreationEphemeral1hInputTokens":0,"canonicalModel":"claude-sonnet-5-5","provider":"firstParty","costBasis":"list"}},"permission_denials":[],"terminal_reason":"completed","fast_mode_state":"off","fast_mode_disabled_reason":"sdk_opt_in_required","subagent_stats":{"spawned":0,"requested":{"background":0,"foreground":0,"unset":0},"started_in_background":0,"max_depth":0,"spawned_by_subagents":0,"completed":0,"failed":0,"killed":{"parent":0,"user":0,"system":0},"refused":{"depth_limit":0,"concurrency_limit":0,"budget":0},"by_type":{}},"safety_stops":0,"is_error":false,"num_turns":1,"subtype":"success","api_error_status":null,"result":"MOCK_REVIEW_TEXT","ttft_ms":21,"type":"result","duration_ms":24,"uuid":"b9afaec7-b9d8-44ed-995b-13aa345c7316","ttft_stream_ms":21,"time_to_request_ms":19,"first_content_frame_ms":21,"queued_turn_count":0,"result_index":0}"#

private func runStub(
    response: String,
    status: Int = 0,
    auth: String = "Claude Code subscription",
    capture: Bool = true,
    responsePath: String = "response.json"
) throws -> String {
    let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
    let run = root.appendingPathComponent("run")
    try FileManager.default.createDirectory(at: run, withIntermediateDirectories: true)
    defer { try? FileManager.default.removeItem(at: root) }
    let responseURL = root.appendingPathComponent(responsePath)
    try FileManager.default.createDirectory(
        at: responseURL.deletingLastPathComponent(),
        withIntermediateDirectories: true
    )
    try Data(response.utf8).write(to: responseURL)
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
            cat "$WENDY_E2E_REVIEW_REPO_DIR/\(responsePath)"
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
