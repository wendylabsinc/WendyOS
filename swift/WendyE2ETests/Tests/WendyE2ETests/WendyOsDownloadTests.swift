import Testing

@Suite
struct `'wendy os download'` {
    /**
     Displays usage for `wendy os download`. The output includes the command
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
     Downloads the requested WendyOS image version, verifies the artifact,
     and stores it in the OS cache for later installation. Success
     output includes device type, version, cache path, and size.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `downloads a selected WendyOS image into the cache`() async throws {
        // TODO: implement.
    }

    /**
     When the requested image already exists and verifies successfully,
     uses the cached artifact. `--overwrite` replaces it after a
     successful new download.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `uses cached images unless overwrite is requested`() async throws {
        // TODO: implement.
    }

    /**
     Unknown versions, unavailable manifests, network failures, or failed
     verification leave existing cached artifacts untouched and report a
     failure on stderr.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `reports unavailable versions without changing the cache`() async throws {
        // TODO: implement.
    }

    /**
     With `--json`, emits one JSON object containing version, device type,
     artifact path, checksum, byte count, and cache-hit status.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `prints JSON download metadata for automation`() async throws {
        // TODO: implement.
    }

    /**
     Accepts only the documented arguments and flags for `wendy os download`.
     Extra positional arguments or unknown flags produce a usage diagnostic
     on stderr, return a failure status, emit no success output, and leave
     existing state unchanged.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `rejects undocumented arguments and flags`() async throws {
        // TODO: implement.
    }
}
