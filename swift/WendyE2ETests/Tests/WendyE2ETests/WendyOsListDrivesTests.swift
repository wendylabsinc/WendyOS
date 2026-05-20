import Testing

@Suite
struct `'wendy os list-drives'` {
    /**
     Displays usage for `wendy os list-drives`. The output includes the command
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
     Displays candidate removable drives with stable identifiers, sizes,
     names, and safety classification. The command performs no writes.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `lists removable drives by default`() async throws {
        // TODO: implement.
    }

    /**
     `--all` includes internal and non-removable drives and labels them
     clearly so destructive install flows can keep applying stricter
     confirmation rules.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `includes non-removable drives when requested`() async throws {
        // TODO: implement.
    }

    /**
     With `--json`, emits an array of drive objects with identifiers,
     mount state, size, removability, and safety metadata.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `prints JSON drive inventory for automation`() async throws {
        // TODO: implement.
    }

    /**
     On unsupported platforms, reports that drive listing is unavailable
     with a failure status and does not fall back to unsafe guesses.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `handles platforms without drive listing support`() async throws {
        // TODO: implement.
    }

    /**
     Accepts only the documented arguments and flags for `wendy os list-
     drives`. Extra positional arguments or unknown flags produce a usage
     diagnostic on stderr, return a failure status, emit no success output,
     and leave existing state unchanged.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `rejects undocumented arguments and flags`() async throws {
        // TODO: implement.
    }
}
