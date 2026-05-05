import Testing

@Suite(.serialized)
struct `wendy device bluetooth` {
    @Test
    func `describes Bluetooth subcommands`() async throws {
        // TODO: implement.
    }
}

// MARK: -

@Suite(.serialized)
struct `wendy device bluetooth connect` {
    @Test
    func `connects to a known Bluetooth device`() async throws {
        // TODO: implement.
    }

    @Test
    func `fails clearly when the Bluetooth device is unavailable`() async throws {
        // TODO: implement.
    }
}

// MARK: -

@Suite(.serialized)
struct `wendy device bluetooth disconnect` {
    @Test
    func `disconnects a connected Bluetooth device`() async throws {
        // TODO: implement.
    }

    @Test
    func `handles an already disconnected Bluetooth device`() async throws {
        // TODO: implement.
    }
}

// MARK: -

@Suite(.serialized)
struct `wendy device bluetooth forget` {
    @Test
    func `forgets a paired Bluetooth device`() async throws {
        // TODO: implement.
    }

    @Test
    func `fails clearly when the Bluetooth device is not paired`() async throws {
        // TODO: implement.
    }
}

// MARK: -

@Suite(.serialized)
struct `wendy device bluetooth list` {
    @Test
    func `lists known Bluetooth devices`() async throws {
        // TODO: implement.
    }

    @Test
    func `'--json' formats Bluetooth devices as JSON`() async throws {
        // TODO: implement.
    }
}
