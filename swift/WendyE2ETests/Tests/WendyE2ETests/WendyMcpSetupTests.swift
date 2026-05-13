import Testing

@Suite
struct `'wendy mcp setup'` {
    /**
     Displays usage for `wendy mcp setup`. The output includes the command
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
     Detects supported AI tools and writes configuration that launches
     `wendy mcp serve`. Existing unrelated tool configuration remains
     intact.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `adds Wendy MCP configuration to detected AI tools`() async throws {
        // TODO: implement.
    }

    /**
     Running setup repeatedly updates the managed Wendy MCP entry without
     duplicating entries or reordering unrelated configuration.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `is idempotent when configuration already exists`() async throws {
        // TODO: implement.
    }

    /**
     When no supported tool configuration is present, reports the available
     manual setup command and leaves the filesystem unchanged.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `reports unsupported or missing AI tools without writing files`() async throws {
        // TODO: implement.
    }

    /**
     With `--json`, emits one JSON object listing detected tools, changed
     files, skipped files, and manual instructions.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `prints JSON setup summary for automation`() async throws {
        // TODO: implement.
    }

    /**
     Accepts only the documented arguments and flags for `wendy mcp setup`.
     Extra positional arguments or unknown flags produce a usage diagnostic
     on stderr, return a failure status, emit no success output, and leave
     existing state unchanged.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `rejects undocumented arguments and flags`() async throws {
        // TODO: implement.
    }
}
