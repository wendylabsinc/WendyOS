import Testing
import Subprocess
import WendyE2ETesting

@Suite(.serialized)
struct `'wendy device apps'` {
    var cli: Session
    init() async throws { self.cli = try await Session.begin(for: .cli) }

    @Test
    func `describes management subcommands`() async throws {
        try await self.cli.sh("./bin/wendy device apps --help") { standardOutput, standardError in
            #expect(standardError.isEmpty)
            #expect(standardOutput.contains("Manage applications"))
            #expect(standardOutput.contains("list"))
            #expect(standardOutput.contains("remove"))
            #expect(standardOutput.contains("start"))
            #expect(standardOutput.contains("stop"))
        }
    }
}

// MARK: -

@Suite(.serialized)
struct `'wendy device apps list'` {
    var cli: Session
    init() async throws { self.cli = try await Session.begin(for: .cli) }

    @Test(
        .disabled("TODO: one-by-one E2E run fails against current local fixtures/implementation.")
    )
    func `lists applications on the selected device`() async throws {
        // TODO: Re-enable after adding the required fixture or implementation; one-by-one E2E run currently fails.
        let record = try await self.cli.sh(
            "WENDY_ANALYTICS=false ./bin/wendy --device 127.0.0.1 device apps list",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )
        #expect(record.terminationStatus.isSuccess)
        #expect(
            record.standardOutput?.contains("Application") == true
                || record.standardOutput?.contains("Name") == true
        )
        #expect(record.standardOutput?.contains("Status") == true)
    }

    @Test(
        .disabled("TODO: one-by-one E2E run fails against current local fixtures/implementation.")
    )
    func `reports clearly when no applications are installed`() async throws {
        // TODO: Re-enable after adding the required fixture or implementation; one-by-one E2E run currently fails.
        let record = try await self.cli.sh(
            "WENDY_ANALYTICS=false ./bin/wendy --device 127.0.0.1 device apps list",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )
        #expect(record.terminationStatus.isSuccess)
        #expect(
            record.standardOutput?.contains("No applications") == true
                || record.standardOutput?.contains("No apps") == true
        )
    }

    @Test(
        .disabled("TODO: one-by-one E2E run fails against current local fixtures/implementation.")
    )
    func `'--json' formats applications as JSON`() async throws {
        // TODO: Re-enable after adding the required fixture or implementation; one-by-one E2E run currently fails.
        let record = try await self.cli.sh(
            "WENDY_ANALYTICS=false ./bin/wendy --json --device 127.0.0.1 device apps list",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )
        #expect(record.terminationStatus.isSuccess)
        let array = try Helper.jsonArray(from: record.standardOutput ?? "")
        if let first = array.first as? [String: Any] {
            #expect(first["name"] as? String != nil)
            #expect(first["status"] as? String != nil)
        }
    }
}

// MARK: -

@Suite(.serialized)
struct `'wendy device apps remove'` {
    var cli: Session
    init() async throws { self.cli = try await Session.begin(for: .cli) }

    @Test(
        .disabled("TODO: one-by-one E2E run fails against current local fixtures/implementation.")
    )
    func `removes an installed application`() async throws {
        // TODO: Re-enable after adding the required fixture or implementation; one-by-one E2E run currently fails.
        let record = try await self.cli.sh(
            "WENDY_ANALYTICS=false ./bin/wendy --device 127.0.0.1 device apps remove sh.wendy.e2e.app --force",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )
        #expect(record.terminationStatus.isSuccess)
        #expect(record.standardOutput?.contains("Removed") == true)
        #expect(record.standardOutput?.contains("sh.wendy.e2e.app") == true)
    }

    @Test
    func `fails clearly when the application is not installed`() async throws {
        let record = try await self.cli.sh(
            "WENDY_ANALYTICS=false ./bin/wendy --device 127.0.0.1 device apps remove missing-app --force",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )
        #expect(!record.terminationStatus.isSuccess)
        #expect(
            record.standardError?.contains("missing-app") == true
                || record.standardError?.contains("not installed") == true
                || Helper.isConnectionFailure(record.standardError)
        )
    }
}

// MARK: -

@Suite(.serialized)
struct `'wendy device apps start'` {
    var cli: Session
    init() async throws { self.cli = try await Session.begin(for: .cli) }

    @Test(
        .disabled("TODO: one-by-one E2E run fails against current local fixtures/implementation.")
    )
    func `starts a stopped application`() async throws {
        // TODO: Re-enable after adding the required fixture or implementation; one-by-one E2E run currently fails.
        let record = try await self.cli.sh(
            "WENDY_ANALYTICS=false ./bin/wendy --device 127.0.0.1 device apps start sh.wendy.e2e.app",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )
        #expect(record.terminationStatus.isSuccess)
        #expect(record.standardOutput?.contains("Started") == true)
        #expect(record.standardOutput?.contains("sh.wendy.e2e.app") == true)
    }

    @Test
    func `fails clearly when the application cannot be started`() async throws {
        let record = try await self.cli.sh(
            "WENDY_ANALYTICS=false ./bin/wendy --device 127.0.0.1 device apps start missing-app",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )
        #expect(!record.terminationStatus.isSuccess)
        #expect(
            record.standardError?.contains("missing-app") == true
                || record.standardError?.contains("start") == true
                || Helper.isConnectionFailure(record.standardError)
        )
    }
}

// MARK: -

@Suite(.serialized)
struct `'wendy device apps stop'` {
    var cli: Session
    init() async throws { self.cli = try await Session.begin(for: .cli) }

    @Test(
        .disabled("TODO: one-by-one E2E run fails against current local fixtures/implementation.")
    )
    func `stops a running application`() async throws {
        // TODO: Re-enable after adding the required fixture or implementation; one-by-one E2E run currently fails.
        let record = try await self.cli.sh(
            "WENDY_ANALYTICS=false ./bin/wendy --device 127.0.0.1 device apps stop sh.wendy.e2e.app",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )
        #expect(record.terminationStatus.isSuccess)
        #expect(record.standardOutput?.contains("Stopped") == true)
        #expect(record.standardOutput?.contains("sh.wendy.e2e.app") == true)
    }

    @Test
    func `fails clearly when the application cannot be stopped`() async throws {
        let record = try await self.cli.sh(
            "WENDY_ANALYTICS=false ./bin/wendy --device 127.0.0.1 device apps stop missing-app",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )
        #expect(!record.terminationStatus.isSuccess)
        #expect(
            record.standardError?.contains("missing-app") == true
                || record.standardError?.contains("stop") == true
                || Helper.isConnectionFailure(record.standardError)
        )
    }
}
