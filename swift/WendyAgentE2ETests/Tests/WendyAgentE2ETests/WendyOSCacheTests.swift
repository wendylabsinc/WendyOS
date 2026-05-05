import Testing

@Suite(.serialized)
struct `wendy os cache` {
    @Test
    func `describes OS cache subcommands`() async throws {
        // TODO: implement.
    }
}

// MARK: -

@Suite(.serialized)
struct `wendy os cache clear` {
    @Test
    func `removes cached WendyOS images`() async throws {
        // TODO: implement.
    }

    @Test
    func `reports when the OS cache is already empty`() async throws {
        // TODO: implement.
    }
}

// MARK: -

@Suite(.serialized)
struct `wendy os cache list` {
    @Test
    func `lists cached WendyOS images`() async throws {
        // TODO: implement.
    }

    @Test
    func `'--json' formats cached WendyOS images as JSON`() async throws {
        // TODO: implement.
    }
}
