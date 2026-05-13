import Testing

@Suite
struct `'wendy device volumes remove'` {
    /**
     Displays usage for `wendy device volumes remove`. The output includes the
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
     Removes the named volume only after confirmation or `--force`. Success
     output identifies the removed volume.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `removes a persistent volume after confirmation`() async throws {
        // TODO: implement.
    }

    /**
     Missing or empty volume names produce a usage diagnostic before contacting
     the device.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `requires a volume name`() async throws {
        // TODO: implement.
    }

    /**
     An unknown volume name fails without deleting similarly named volumes or
     application containers.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `reports unknown volumes without deleting data`() async throws {
        // TODO: implement.
    }

    /**
     Accepts only the documented arguments and flags for `wendy device volumes
     remove`. Extra positional arguments or unknown flags produce a usage
     diagnostic on stderr, return a failure status, emit no success output,
     and leave existing state unchanged.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `rejects undocumented arguments and flags`() async throws {
        // TODO: implement.
    }
}
