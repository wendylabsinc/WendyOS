import Testing

@Suite(.serialized)
struct `wendy device apps` {
    @Test
    func `describes app management subcommands`() async throws {
        // TODO: implement.
    }
}

// MARK: -

@Suite(.serialized)
struct `wendy device apps list` {
    @Test
    func `lists applications on the selected device`() async throws {
        // TODO: implement.
    }

    @Test
    func `reports clearly when no applications are installed`() async throws {
        // TODO: implement.
    }

    @Test
    func `'--json' formats applications as JSON`() async throws {
        // TODO: implement.
    }
}

// MARK: -

@Suite(.serialized)
struct `wendy device apps remove` {
    @Test
    func `removes an installed application`() async throws {
        // TODO: implement.
    }

    @Test
    func `fails clearly when the application is not installed`() async throws {
        // TODO: implement.
    }
}

// MARK: -

@Suite(.serialized)
struct `wendy device apps start` {
    @Test
    func `starts a stopped application`() async throws {
        // TODO: implement.
    }

    @Test
    func `fails clearly when the application cannot be started`() async throws {
        // TODO: implement.
    }
}

// MARK: -

@Suite(.serialized)
struct `wendy device apps stop` {
    @Test
    func `stops a running application`() async throws {
        // TODO: implement.
    }

    @Test
    func `fails clearly when the application cannot be stopped`() async throws {
        // TODO: implement.
    }
}
