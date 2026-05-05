import Testing

@Suite(.serialized)
struct `wendy cache` {
    @Test
    func `describes cache subcommands`() async throws {
        // TODO: implement.
    }
}

// MARK: -

@Suite(.serialized)
struct `wendy cache clear` {
    @Test
    func `removes cached CLI data`() async throws {
        // TODO: implement.
    }

    @Test
    func `reports when the cache is already empty`() async throws {
        // TODO: implement.
    }
}

// MARK: -

@Suite(.serialized)
struct `wendy cache list` {
    @Test
    func `lists cached entries`() async throws {
        // TODO: implement.
    }

    @Test
    func `'--json' formats cached entries as JSON`() async throws {
        // TODO: implement.
    }
}
