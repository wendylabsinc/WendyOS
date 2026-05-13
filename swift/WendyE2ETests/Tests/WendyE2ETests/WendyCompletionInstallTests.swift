import Testing

@Suite
struct `'wendy completion install'` {
    /**
     Displays usage for `wendy completion install`. The output includes the
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
     Detects the user's shell, writes the generated completion script to
     the conventional location, and adds an idempotent source line to
     the shell rc file when that shell needs one.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `installs completion for the detected shell`() async throws {
        // TODO: implement.
    }

    /**
     `--shell` bypasses shell detection and installs the script for the
     selected shell only. Other shell configuration files remain
     unchanged.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `uses the requested shell override`() async throws {
        // TODO: implement.
    }

    /**
     `--print-path` reports the target script and rc paths as a dry run.
     The command exits successfully without creating directories or
     editing shell configuration.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `prints install paths without writing files`() async throws {
        // TODO: implement.
    }

    /**
     `--stdout` emits the completion script to stdout and performs no
     installation. Stderr remains empty on success.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `prints the script to stdout when requested`() async throws {
        // TODO: implement.
    }

    /**
     Running installation repeatedly leaves a single managed completion
     script and a single source line in the relevant shell rc file.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `is idempotent when completion is already installed`() async throws {
        // TODO: implement.
    }

    /**
     Accepts only the documented arguments and flags for `wendy completion
     install`. Extra positional arguments or unknown flags produce a usage
     diagnostic on stderr, return a failure status, emit no success output,
     and leave existing state unchanged.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `rejects undocumented arguments and flags`() async throws {
        // TODO: implement.
    }
}
