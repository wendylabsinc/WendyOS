import Testing

@Suite
struct `'wendy os update'` {
    /**
     Displays usage for `wendy os update`. The output includes the command
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
     Serves the provided Mender artifact to the selected device and
     requests an OS update. Success output identifies the artifact and
     device update status.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `updates WendyOS from a local artifact`() async throws {
        // TODO: implement.
    }

    /**
     `--artifact-url` instructs the device to fetch a remote artifact
     directly. The URL is validated before the update request is sent.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `updates WendyOS from a remote artifact URL`() async throws {
        // TODO: implement.
    }

    /**
     `--nightly` selects the latest prerelease OS and agent artifacts
     from the manifest and reports the chosen versions before updating.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `uses nightly artifacts when requested`() async throws {
        // TODO: implement.
    }

    /**
     If the target device cannot be reached, the command stops any
     temporary local artifact server and exits with a clear diagnostic.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `reports unreachable devices without serving stale artifacts`() async throws {
        // TODO: implement.
    }

    /**
     With `--json`, emits one JSON object containing device, artifact,
     version, and update request status fields.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `prints JSON update metadata for automation`() async throws {
        // TODO: implement.
    }

    /**
     Accepts only the documented arguments and flags for `wendy os update`.
     Extra positional arguments or unknown flags produce a usage diagnostic
     on stderr, return a failure status, emit no success output, and leave
     existing state unchanged.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `rejects undocumented arguments and flags`() async throws {
        // TODO: implement.
    }
}
