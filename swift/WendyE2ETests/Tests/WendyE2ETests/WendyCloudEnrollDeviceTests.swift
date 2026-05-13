import Testing

@Suite
struct `'wendy cloud enroll-device'` {
    /**
     Displays usage for `wendy cloud enroll-device`. The output includes the
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
     Creates an enrollment token with the stored cloud auth session and
     provisions the selected device with mTLS credentials. Output
     matches the device enrollment flow apart from the command name.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `acts as the cloud alias for device enrollment`() async throws {
        // TODO: implement.
    }

    /**
     `--cloud-grpc` selects the cloud or pki-core endpoint when more
     than one auth session exists. Sessions for other endpoints remain
     untouched.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `uses the requested cloud endpoint`() async throws {
        // TODO: implement.
    }

    /**
     Authentication failures, token creation failures, and device
     connection failures leave the device and local credential store in
     their previous state.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `reports missing auth or unreachable devices without partial enrollment`() async throws {
        // TODO: implement.
    }

    /**
     With `--json`, emits cloud, device, certificate, and enrollment
     status fields without printing token or private key material.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `prints JSON enrollment result for automation`() async throws {
        // TODO: implement.
    }

    /**
     Reads the Wendy CLI configuration before performing work that depends on
     user state. Malformed configuration is reported as a configuration error,
     no prompts open, no network connection is attempted, and the original file
     remains byte-for-byte unchanged.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `reports invalid CLI configuration before acting`() async throws {
        // TODO: implement.
    }
}
