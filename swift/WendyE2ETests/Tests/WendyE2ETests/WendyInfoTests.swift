import Testing

@Suite
struct `'wendy info'` {
    /**
     Displays usage for `wendy info`. The output includes the command synopsis,
     local flags, inherited global flags, and concise descriptions. Help exits
     successfully, writes to stdout, emits no stderr, and leaves configuration,
     cache, project, cloud, and device state untouched.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `prints command help`() async throws {
        // TODO: implement.
    }

    /**
     Reports the Wendy CLI version and local system details useful for
     support, including operating system and architecture. The command does
     not contact devices, cloud services, or update endpoints.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `prints CLI and system information`() async throws {
        // TODO: implement.
    }

    /**
     With `--json`, emits one JSON object containing version and system
     fields with stable names and value types. JSON mode emits no stderr on
     success.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `prints JSON info for automation`() async throws {
        // TODO: implement.
    }

    /**
     Runs successfully outside a Wendy project, with no default device, and
     with no auth session. Malformed optional config is reported separately
     from local system probing.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `does not require project, device, or auth state`() async throws {
        // TODO: implement.
    }

    /**
     Accepts only the documented arguments and flags for `wendy info`. Extra
     positional arguments or unknown flags produce a usage diagnostic on
     stderr, return a failure status, emit no success output, and leave
     existing state unchanged.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `rejects undocumented arguments and flags`() async throws {
        // TODO: implement.
    }
}
