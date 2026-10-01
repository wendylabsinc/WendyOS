import Foundation
import Testing
import WendyE2ETesting

@Suite
struct `'wendy mcp serve'` {
    let scenario = CLIAndAgentScenario()

    /**
     Displays stdio server usage and flags without starting the protocol loop.
     */
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy mcp serve --help") { result in
                let stdout = result.stdout
                #expect(result.status.isSuccess)
                #expect(stdout.contains("Model Context Protocol server"))
                #expect(stdout.contains("Usage:"))
                #expect(stdout.contains("wendy mcp serve [flags]"))
                #expect(stdout.contains("--device"))
                #expect(stdout.contains("--help"))
                #expect(stdout.contains("--json"))
                #expect(result.stderr == "")
            }
        }
    }

    /**
     Negotiates an MCP session over newline-delimited stdio and lists Wendy's
     core tools without mixing diagnostics into protocol stdout. Specialist
     groups such as `cloud` stay unlisted until `wendy_tools` enables them.
     */
    @Test
    func `serves MCP tools over stdio`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh(
                posix: """
                    printf '%s\n' \
                      '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"swift-e2e","version":"1"}}}' \
                      '{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}' \
                      '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}' \
                    | wendy mcp serve
                    """,
                power: """
                    @(
                      '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"swift-e2e","version":"1"}}}',
                      '{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}',
                      '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}'
                    ) | wendy mcp serve
                    exit $LASTEXITCODE
                    """
            ) { result in
                #expect(result.status.isSuccess)
                #expect(result.stderr == "")

                let lines = result.normalizedStdout.split(separator: "\n")
                #expect(lines.count == 2)
                let initialize = try #require(
                    try JSONSerialization.jsonObject(with: Data(lines[0].utf8))
                        as? [String: Any]
                )
                #expect(initialize["jsonrpc"] as? String == "2.0")
                #expect(initialize["id"] as? Int == 1)
                let initializeResult = try #require(initialize["result"] as? [String: Any])
                #expect(initializeResult["protocolVersion"] as? String == "2024-11-05")
                let serverInfo = try #require(initializeResult["serverInfo"] as? [String: Any])
                #expect(serverInfo["name"] as? String == "wendy")

                let toolsResponse = try #require(
                    try JSONSerialization.jsonObject(with: Data(lines[1].utf8))
                        as? [String: Any]
                )
                #expect(toolsResponse["id"] as? Int == 2)
                let toolsResult = try #require(toolsResponse["result"] as? [String: Any])
                let tools = try #require(toolsResult["tools"] as? [[String: Any]])
                let names = Set(tools.compactMap { $0["name"] as? String })
                #expect(names.count == 15)
                #expect(names.contains("wendy_status"))
                #expect(names.contains("wendy_tools"))
                #expect(names.contains("device_connect"))
                #expect(!names.contains("cloud_connect"))
            }
        }
    }

    /**
     Explicitly enabling the cloud tool group keeps the core tools available
     and advertises cloud connection tools to MCP clients.
     */
    @Test
    func `serves selected cloud MCP tools over stdio`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh(
                posix: """
                    printf '%s\n' \
                      '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"swift-e2e","version":"1"}}}' \
                      '{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}' \
                      '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}' \
                    | wendy mcp serve --tool-groups cloud
                    """,
                power: """
                    @(
                      '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"swift-e2e","version":"1"}}}',
                      '{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}',
                      '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}'
                    ) | wendy mcp serve --tool-groups cloud
                    exit $LASTEXITCODE
                    """
            ) { result in
                #expect(result.status.isSuccess)
                #expect(result.stderr == "")

                let lines = result.normalizedStdout.split(separator: "\n")
                #expect(lines.count == 2)
                let toolsResponse = try #require(
                    try JSONSerialization.jsonObject(with: Data(lines[1].utf8))
                        as? [String: Any]
                )
                #expect(toolsResponse["id"] as? Int == 2)
                let toolsResult = try #require(toolsResponse["result"] as? [String: Any])
                let tools = try #require(toolsResult["tools"] as? [[String: Any]])
                let names = Set(tools.compactMap { $0["name"] as? String })
                #expect(names.contains("wendy_tools"))
                #expect(names.contains("device_connect"))
                #expect(names.contains("cloud_connect"))
            }
        }
    }

    /**
     An initial device failure remains disconnected and is exposed through
     structured MCP state or errors without corrupting protocol stdout.
     */
    @Test(
        .disabled(
            "WDY-1942: --device uses a lazy gRPC connection, records an unreachable target as active, and exposes the failure only as a stderr warning during container scanning rather than structured MCP state."
        )
    )
    func `connects to an initial device when provided`() async throws {
        // TODO: enable with an unreachable loopback endpoint when MCP reports accurate structured connection state (WDY-1942).
    }

    /**
     Handles protocol requests without opening interactive pickers, browser
     windows, or terminal UI.
     */
    @Test
    func `does not prompt while serving`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh(
                posix: """
                    printf '%s\n' \
                      '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"swift-e2e","version":"1"}}}' \
                      '{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}' \
                      '{"jsonrpc":"2.0","id":2,"method":"resources/list","params":{}}' \
                    | wendy mcp serve
                    """,
                power: """
                    @(
                      '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"swift-e2e","version":"1"}}}',
                      '{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}',
                      '{"jsonrpc":"2.0","id":2,"method":"resources/list","params":{}}'
                    ) | wendy mcp serve
                    exit $LASTEXITCODE
                    """
            ) { result in
                #expect(result.status.isSuccess)
                #expect(result.stderr == "")
                #expect(!result.stdout.lowercased().contains("select a device"))
                #expect(!result.stdout.lowercased().contains("press enter"))

                let lines = result.normalizedStdout.split(separator: "\n")
                #expect(lines.count == 2)
                let resourcesResponse = try #require(
                    try JSONSerialization.jsonObject(with: Data(lines[1].utf8))
                        as? [String: Any]
                )
                #expect(resourcesResponse["id"] as? Int == 2)
                #expect(resourcesResponse["error"] == nil)
                let resourcesResult = try #require(resourcesResponse["result"] as? [String: Any])
                let resources = try #require(resourcesResult["resources"] as? [[String: Any]])
                #expect(resources.contains { $0["uri"] as? String == "wendy://guide" })
            }
        }
    }

    /**
     Closing stdin terminates the server successfully without protocol output
     or a lingering child process.
     */
    @Test
    func `shuts down cleanly when stdin closes`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh(
                posix: "wendy mcp serve < /dev/null",
                power: """
                    $null | wendy mcp serve
                    exit $LASTEXITCODE
                    """
            ) { result in
                #expect(result.status.isSuccess)
                #expect(result.stdout == "")
                #expect(result.stderr == "")
            }
        }
    }

    /**
     Unknown flags fail before the stdio protocol loop starts.
     */
    @Test
    func `rejects undocumented flags`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy mcp serve --bogus") { result in
                #expect(result.status.isFailure)
                #expect(result.stdout == "")
                #expect(result.stderr.contains("unknown flag"))
                #expect(result.stderr.contains("--bogus"))
            }
        }
    }

    /**
     Extra positional arguments are rejected instead of being silently
     ignored while the server starts.
     */
    @Test(
        .disabled(
            "WDY-1934: 'wendy mcp serve' silently accepts extra positional arguments because the leaf command has no cobra.NoArgs validator."
        )
    )
    func `rejects undocumented positional arguments`() async throws {
        // TODO: enable when MCP leaf commands reject positional arguments (WDY-1934).
    }
}
