import Testing

@Suite
struct `'wendy device hardware list'` {
    /**
     Displays usage for `wendy device hardware list`. The output includes the
     command synopsis, local flags, inherited global flags, and concise
     descriptions. Help exits successfully, writes to stdout, emits no
     stderr, and leaves configuration, cache, project, cloud, and device
     state untouched.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `prints command help`() async throws {
        // TODO: implement.
    }

    /**
     `--device` selects the target device hostname and skips discovery and
     pickers. The command does not read or change the saved default device when
     an explicit target is supplied.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `uses explicit device selection without prompting`() async throws {
        // TODO: implement.
    }

    /**
     Without an explicit or configured device in a non-interactive context,
     reports that a device selection is required, emits no prompt escape
     sequences, and performs no device operation.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `reports missing device selection in non-interactive mode`() async throws {
        // TODO: implement.
    }

    /**
     Connection failures, timeouts, and incompatible agent responses produce
     stderr diagnostics and a failure status. Output does not claim that the
     operation succeeded.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `reports unreachable devices without partial success`() async throws {
        // TODO: implement.
    }

    /**
     Displays hardware capabilities such as GPU, audio, camera, storage, and
     buses with ids and user-facing labels.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `lists hardware capabilities`() async throws {
        // TODO: implement.
    }

    /**
     `--category` restricts output to matching capabilities. Unknown categories
     produce an empty successful result or clear validation diagnostic.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `filters by category`() async throws {
        // TODO: implement.
    }

    /**
     With `--json`, emits hardware objects grouped or labeled by category with
     stable field names.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `prints JSON hardware inventory`() async throws {
        // TODO: implement.
    }

    /**
     Accepts only the documented arguments and flags for `wendy device hardware
     list`. Extra positional arguments or unknown flags produce a usage
     diagnostic on stderr, return a failure status, emit no success output,
     and leave existing state unchanged.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `rejects undocumented arguments and flags`() async throws {
        // TODO: implement.
    }
}
