import Testing

@Suite
struct `'wendy auth login'` {
    /**
     Displays usage for `wendy auth login`. The output includes the command
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
     Without `--api-key`, starts the Wendy Cloud browser login flow,
     receives the callback token, generates client credentials, and
     stores the resulting session in the CLI configuration. Success
     output gives concise next-step guidance and stderr stays empty.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `starts browser-based login and stores the auth session`() async throws {
        // TODO: implement.
    }

    /**
     With `--api-key`, authenticates against the selected cloud gRPC or
     local pki-core endpoint using the provided bearer token. The token
     is not echoed to stdout, stderr, command records, or saved config.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `logs in with an API key without opening a browser`() async throws {
        // TODO: implement.
    }

    /**
     `--cloud` and `--cloud-grpc` bind the login to a specific Wendy
     Cloud environment. The stored session records enough endpoint
     identity for later commands to choose the same environment.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `selects the intended cloud endpoint explicitly`() async throws {
        // TODO: implement.
    }

    /**
     Cancelling or timing out the browser callback reports a login
     failure, exits non-zero, and leaves prior authentication state
     unchanged.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `reports cancelled browser login without storing credentials`() async throws {
        // TODO: implement.
    }

    /**
     A successful login for a cloud that already has stored credentials
     replaces the old credentials atomically while preserving sessions
     for other clouds.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `replaces an existing session for the same cloud`() async throws {
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
