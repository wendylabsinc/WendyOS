import Testing

@Suite(.serialized)
struct `wendy os` {
    @Test
    func `describes WendyOS management subcommands`() async throws {
        // TODO: implement.
    }
}

// MARK: -

@Suite(.serialized)
struct `wendy os download` {
    @Test
    func `downloads a WendyOS image into the local cache`() async throws {
        // TODO: implement.
    }

    @Test
    func `reuses an already cached WendyOS image`() async throws {
        // TODO: implement.
    }

    @Test
    func `fails clearly when the requested image is unavailable`() async throws {
        // TODO: implement.
    }
}

// MARK: -

@Suite(.serialized)
struct `wendy os install` {
    @Test
    func `installs a WendyOS image onto the selected drive`() async throws {
        // TODO: implement.
    }

    @Test
    func `requires explicit confirmation before writing a drive`() async throws {
        // TODO: implement.
    }

    @Test
    func `fails clearly when the target drive is invalid`() async throws {
        // TODO: implement.
    }
}

// MARK: -

@Suite(.serialized)
struct `wendy os list-drives` {
    @Test
    func `lists removable drives that can receive WendyOS`() async throws {
        // TODO: implement.
    }

    @Test
    func `'--json' formats removable drives as JSON`() async throws {
        // TODO: implement.
    }
}

// MARK: -

@Suite(.serialized)
struct `wendy os update` {
    @Test
    func `updates WendyOS on the selected device`() async throws {
        // TODO: implement.
    }

    @Test
    func `fails clearly when the selected device cannot be updated`() async throws {
        // TODO: implement.
    }
}
