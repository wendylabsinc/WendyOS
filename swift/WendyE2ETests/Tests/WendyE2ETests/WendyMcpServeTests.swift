import Testing

@Suite
struct `'wendy mcp serve'` {
    /**
     Displays usage for `wendy mcp serve`. The output includes the command
     synopsis, local flags, inherited global flags, and concise
     descriptions. Help exits successfully, writes to stdout, emits no
     stderr, and leaves configuration, cache, project, cloud, and device
     state untouched.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `prints command help`() async throws {
        // TODO: implement.
    }

    /**
     Starts a Model Context Protocol server on stdin and stdout with
     Wendy device tools registered. Protocol messages are written only
     to stdout; diagnostics and logs use stderr.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `serves MCP tools over stdio`() async throws {
        // TODO: implement.
    }

    /**
     `--device` preselects a device for tools that need one. Startup
     reports device connection problems through MCP errors without
     corrupting the stdio protocol stream.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `connects to an initial device when provided`() async throws {
        // TODO: implement.
    }

    /**
     The server is suitable for AI assistant launches. It does not open
     interactive pickers, browser windows, or terminal UI while handling
     protocol requests.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `does not prompt while serving`() async throws {
        // TODO: implement.
    }

    /**
     Closing stdin or sending the MCP shutdown request releases device
     connections and exits without leaving background processes.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `shuts down cleanly when stdin closes`() async throws {
        // TODO: implement.
    }

    /**
     Accepts only the documented arguments and flags for `wendy mcp serve`.
     Extra positional arguments or unknown flags produce a usage diagnostic
     on stderr, return a failure status, emit no success output, and leave
     existing state unchanged.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `rejects undocumented arguments and flags`() async throws {
        // TODO: implement.
    }
}
