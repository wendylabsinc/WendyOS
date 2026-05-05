import Testing

@Suite(.serialized)
struct `wendy device volumes` {
    @Test
    func `describes volume management subcommands`() async throws {
        // TODO: implement.
    }
}

// MARK: -

@Suite(.serialized)
struct `wendy device volumes list` {
    @Test
    func `lists persistent volumes on the selected device`() async throws {
        // TODO: implement.
    }

    @Test
    func `'--json' formats persistent volumes as JSON`() async throws {
        // TODO: implement.
    }
}

// MARK: -

@Suite(.serialized)
struct `wendy device volumes remove` {
    @Test
    func `removes an existing persistent volume`() async throws {
        // TODO: implement.
    }

    @Test
    func `fails clearly when the persistent volume does not exist`() async throws {
        // TODO: implement.
    }
}
