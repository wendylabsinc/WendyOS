import Testing

@Suite(.serialized)
struct `wendy project entitlements` {
    @Test
    func `describes entitlement subcommands`() async throws {
        // TODO: implement.
    }
}

@Suite(.serialized)
struct `wendy project entitlements list` {
    @Test
    func `shows all available entitlement types`() async throws {
        // TODO: implement.
    }

    @Test
    func `reports when the project has no entitlements`() async throws {
        // TODO: implement.
    }

    @Test
    func `lists configured project entitlements`() async throws {
        // TODO: implement.
    }
}

@Suite(.serialized)
struct `wendy project entitlements add` {
    @Test
    func `adds a non-interactive entitlement and persists it`() async throws {
        // TODO: implement.
    }

    @Test
    func `rejects an unknown entitlement type without changing wendy json`() async throws {
        // TODO: implement.
    }

    @Test
    func `rejects an entitlement that already exists`() async throws {
        // TODO: implement.
    }
}

@Suite(.serialized)
struct `wendy project entitlements remove` {
    @Test
    func `removes an existing entitlement and persists it`() async throws {
        // TODO: implement.
    }

    @Test
    func `fails clearly when the entitlement is not configured`() async throws {
        // TODO: implement.
    }
}
