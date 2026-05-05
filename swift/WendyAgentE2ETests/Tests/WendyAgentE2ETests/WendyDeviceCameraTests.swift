import Testing

@Suite(.serialized)
struct `wendy device camera` {
    @Test
    func `describes camera subcommands`() async throws {
        // TODO: implement.
    }
}

// MARK: -

@Suite(.serialized)
struct `wendy device camera list` {
    @Test
    func `lists cameras on the selected device`() async throws {
        // TODO: implement.
    }

    @Test
    func `'--json' formats cameras as JSON`() async throws {
        // TODO: implement.
    }
}

// MARK: -

@Suite(.serialized)
struct `wendy device camera view` {
    @Test
    func `opens a camera viewer for the selected camera`() async throws {
        // TODO: implement.
    }

    @Test
    func `fails clearly when the selected camera is unavailable`() async throws {
        // TODO: implement.
    }
}
