import Testing

@Suite
struct `'wendy build'` {
    /**
     Displays usage for `wendy build`. The output includes the command
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
     Reads `wendy.json` and project markers from the current directory,
     selects the build strategy, and produces a container image for the
     target WendyOS architecture. Success output names the image and build
     type.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `builds the project in the current directory`() async throws {
        // TODO: implement.
    }

    /**
     When Docker, Swift, or Python markers coexist, `--build-type` selects
     the intended builder. The chosen strategy is reflected in output and no
     other builder mutates the project.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `uses the requested build type when markers overlap`() async throws {
        // TODO: implement.
    }

    /**
     Outside a Wendy project, or with an invalid `wendy.json`, reports the
     project problem on stderr, exits non-zero, and does not create build
     artifacts.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `reports missing or invalid project configuration`() async throws {
        // TODO: implement.
    }

    /**
     A directory without a recognized build marker fails with actionable
     guidance instead of guessing a language or generating files.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `reports unsupported project layouts without guessing`() async throws {
        // TODO: implement.
    }

    /**
     With `--json`, emits one JSON object describing the selected builder,
     image reference, target architecture, cache usage, and build result.
     Progress logs stay out of stdout JSON.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `prints JSON build metadata for automation`() async throws {
        // TODO: implement.
    }

    /**
     Accepts only the documented arguments and flags for `wendy build`. Extra
     positional arguments or unknown flags produce a usage diagnostic on
     stderr, return a failure status, emit no success output, and leave
     existing state unchanged.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `rejects undocumented arguments and flags`() async throws {
        // TODO: implement.
    }
}
