import Testing

@Suite(.serialized)
struct `wendy device wifi` {
    @Test
    func `describes WiFi subcommands`() async throws {
        // TODO: implement.
    }
}

// MARK: -

@Suite(.serialized)
struct `wendy device wifi connect` {
    @Test
    func `connects to a WiFi network`() async throws {
        // TODO: implement.
    }

    @Test
    func `fails clearly when WiFi credentials are rejected`() async throws {
        // TODO: implement.
    }
}

// MARK: -

@Suite(.serialized)
struct `wendy device wifi disconnect` {
    @Test
    func `disconnects from the active WiFi network`() async throws {
        // TODO: implement.
    }

    @Test
    func `handles an already disconnected WiFi interface`() async throws {
        // TODO: implement.
    }
}

// MARK: -

@Suite(.serialized)
struct `wendy device wifi forget` {
    @Test
    func `forgets a saved WiFi network`() async throws {
        // TODO: implement.
    }

    @Test
    func `fails clearly when the WiFi network is not saved`() async throws {
        // TODO: implement.
    }
}

// MARK: -

@Suite(.serialized)
struct `wendy device wifi list` {
    @Test
    func `lists visible WiFi networks`() async throws {
        // TODO: implement.
    }

    @Test
    func `'--json' formats WiFi networks as JSON`() async throws {
        // TODO: implement.
    }
}

// MARK: -

@Suite(.serialized)
struct `wendy device wifi rank` {
    @Test
    func `updates saved WiFi network priority`() async throws {
        // TODO: implement.
    }

    @Test
    func `fails clearly when the WiFi network is unknown`() async throws {
        // TODO: implement.
    }
}

// MARK: -

@Suite(.serialized)
struct `wendy device wifi status` {
    @Test
    func `shows the current WiFi connection state`() async throws {
        // TODO: implement.
    }

    @Test
    func `'--json' formats WiFi status as JSON`() async throws {
        // TODO: implement.
    }
}
