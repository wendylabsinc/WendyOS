import Testing

@Suite(.serialized)
struct `wendy device` {
    @Test
    func `describes device management subcommands`() async throws {
        // TODO: implement.
    }

    @Test
    func `uses the configured default device when none is specified`() async throws {
        // TODO: implement.
    }
}

// MARK: -

@Suite(.serialized)
struct `wendy device set-default` {
    @Test
    func `persists the default device hostname`() async throws {
        // TODO: implement.
    }

    @Test
    func `rejects an invalid device hostname`() async throws {
        // TODO: implement.
    }
}

// MARK: -

@Suite(.serialized)
struct `wendy device setup` {
    @Test
    func `guides interactive device provisioning`() async throws {
        // TODO: implement.
    }

    @Test
    func `handles cancellation without changing configuration`() async throws {
        // TODO: implement.
    }
}

// MARK: -

@Suite(.serialized)
struct `wendy device unset-default` {
    @Test
    func `removes the configured default device`() async throws {
        // TODO: implement.
    }

    @Test
    func `succeeds when no default device is configured`() async throws {
        // TODO: implement.
    }
}

// MARK: -

@Suite(.serialized)
struct `wendy device update` {
    @Test
    func `uploads the current agent build to the selected device`() async throws {
        // TODO: implement.
    }

    @Test
    func `fails clearly when the selected device is unreachable`() async throws {
        // TODO: implement.
    }
}

// MARK: -

@Suite(.serialized)
struct `wendy device version` {
    @Test
    func `prints version and hardware details from the selected device`() async throws {
        // TODO: implement.
    }

    @Test
    func `'--json' formats version and hardware details as JSON`() async throws {
        // TODO: implement.
    }
}

// MARK: -

@Suite(.serialized)
struct `wendy device dashboard` {
    @Test
    func `opens a live dashboard for the selected device`() async throws {
        // TODO: implement.
    }

    @Test
    func `fails clearly when dashboard data cannot be reached`() async throws {
        // TODO: implement.
    }
}

// MARK: -

@Suite(.serialized)
struct `wendy device logs` {
    @Test
    func `streams logs from applications on the selected device`() async throws {
        // TODO: implement.
    }

    @Test
    func `'--app' filters logs by application`() async throws {
        // TODO: implement.
    }
}

// MARK: -

@Suite(.serialized)
struct `wendy device telemetry-stream` {
    @Test
    func `streams telemetry as JSON lines`() async throws {
        // TODO: implement.
    }

    @Test
    func `fails clearly when telemetry cannot be reached`() async throws {
        // TODO: implement.
    }
}
