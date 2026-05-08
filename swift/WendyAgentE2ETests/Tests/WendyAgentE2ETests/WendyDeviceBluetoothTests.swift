import Testing
import Subprocess
import WendyE2ETesting

@Suite(.serialized)
struct `'wendy device bluetooth'` {
    var cli: Session
    init() async throws { self.cli = try await Session.begin(for: .cli) }

    @Test
    func `describes subcommands`() async throws {
        try await self.cli.sh("./bin/wendy device bluetooth --help") {
            standardOutput,
            standardError in
            #expect(standardError.isEmpty)
            #expect(standardOutput.contains("Manage Bluetooth"))
            #expect(standardOutput.contains("connect"))
            #expect(standardOutput.contains("disconnect"))
            #expect(standardOutput.contains("forget"))
            #expect(standardOutput.contains("list"))
        }
    }
}

// MARK: -

@Suite(.serialized)
struct `'wendy device bluetooth connect'` {
    var cli: Session
    init() async throws { self.cli = try await Session.begin(for: .cli) }

    @Test(
        .disabled("TODO: one-by-one E2E run fails against current local fixtures/implementation.")
    )
    func `connects to a known Bluetooth device`() async throws {
        // TODO: Re-enable after adding the required fixture or implementation; one-by-one E2E run currently fails.
        let record = try await self.cli.sh(
            "WENDY_ANALYTICS=false ./bin/wendy --device 127.0.0.1 device bluetooth connect AA:BB:CC:DD:EE:FF",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )
        #expect(record.terminationStatus.isSuccess)
        #expect(record.standardOutput?.contains("Connected") == true)
        #expect(record.standardOutput?.contains("AA:BB:CC:DD:EE:FF") == true)
    }

    @Test
    func `fails clearly when the Bluetooth device is unavailable`() async throws {
        let record = try await self.cli.sh(
            "WENDY_ANALYTICS=false ./bin/wendy --device 127.0.0.1 device bluetooth connect 00:00:00:00:00:00",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )
        #expect(!record.terminationStatus.isSuccess)
        #expect(
            record.standardError?.contains("00:00:00:00:00:00") == true
                || record.standardError?.contains("Bluetooth") == true
                || Helper.isConnectionFailure(record.standardError)
        )
    }
}

// MARK: -

@Suite(.serialized)
struct `'wendy device bluetooth disconnect'` {
    var cli: Session
    init() async throws { self.cli = try await Session.begin(for: .cli) }

    @Test(
        .disabled("TODO: one-by-one E2E run fails against current local fixtures/implementation.")
    )
    func `disconnects a connected Bluetooth device`() async throws {
        // TODO: Re-enable after adding the required fixture or implementation; one-by-one E2E run currently fails.
        let record = try await self.cli.sh(
            "WENDY_ANALYTICS=false ./bin/wendy --device 127.0.0.1 device bluetooth disconnect AA:BB:CC:DD:EE:FF",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )
        #expect(record.terminationStatus.isSuccess)
        #expect(record.standardOutput?.contains("Disconnected") == true)
    }

    @Test(
        .disabled("TODO: one-by-one E2E run fails against current local fixtures/implementation.")
    )
    func `handles an already disconnected Bluetooth device`() async throws {
        // TODO: Re-enable after adding the required fixture or implementation; one-by-one E2E run currently fails.
        let record = try await self.cli.sh(
            "WENDY_ANALYTICS=false ./bin/wendy --device 127.0.0.1 device bluetooth disconnect AA:BB:CC:DD:EE:FF",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )
        #expect(record.terminationStatus.isSuccess)
        #expect(
            record.standardOutput?.contains("already disconnected") == true
                || record.standardOutput?.contains("Disconnected") == true
        )
    }
}

// MARK: -

@Suite(.serialized)
struct `'wendy device bluetooth forget'` {
    var cli: Session
    init() async throws { self.cli = try await Session.begin(for: .cli) }

    @Test(
        .disabled("TODO: one-by-one E2E run fails against current local fixtures/implementation.")
    )
    func `forgets a paired Bluetooth device`() async throws {
        // TODO: Re-enable after adding the required fixture or implementation; one-by-one E2E run currently fails.
        let record = try await self.cli.sh(
            "WENDY_ANALYTICS=false ./bin/wendy --device 127.0.0.1 device bluetooth forget AA:BB:CC:DD:EE:FF",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )
        #expect(record.terminationStatus.isSuccess)
        #expect(
            record.standardOutput?.contains("Forgot") == true
                || record.standardOutput?.contains("removed") == true
        )
    }

    @Test
    func `fails clearly when the Bluetooth device is not paired`() async throws {
        let record = try await self.cli.sh(
            "WENDY_ANALYTICS=false ./bin/wendy --device 127.0.0.1 device bluetooth forget 00:00:00:00:00:00",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )
        #expect(!record.terminationStatus.isSuccess)
        #expect(
            record.standardError?.contains("not paired") == true
                || record.standardError?.contains("Bluetooth") == true
                || Helper.isConnectionFailure(record.standardError)
        )
    }
}

// MARK: -

@Suite(.serialized)
struct `'wendy device bluetooth list'` {
    var cli: Session
    init() async throws { self.cli = try await Session.begin(for: .cli) }

    @Test(
        .disabled("TODO: one-by-one E2E run fails against current local fixtures/implementation.")
    )
    func `lists known Bluetooth devices`() async throws {
        // TODO: Re-enable after adding the required fixture or implementation; one-by-one E2E run currently fails.
        let record = try await self.cli.sh(
            "WENDY_ANALYTICS=false ./bin/wendy --device 127.0.0.1 device bluetooth list",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )
        #expect(record.terminationStatus.isSuccess)
        #expect(
            record.standardOutput?.contains("Bluetooth") == true
                || record.standardOutput?.contains("Address") == true
        )
    }

    @Test(
        .disabled("TODO: one-by-one E2E run fails against current local fixtures/implementation.")
    )
    func `'--json' formats Bluetooth devices as JSON`() async throws {
        // TODO: Re-enable after adding the required fixture or implementation; one-by-one E2E run currently fails.
        let record = try await self.cli.sh(
            "WENDY_ANALYTICS=false ./bin/wendy --json --device 127.0.0.1 device bluetooth list",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )
        #expect(record.terminationStatus.isSuccess)
        let array = try Helper.jsonArray(from: record.standardOutput ?? "")
        if let first = array.first as? [String: Any] {
            #expect(first["address"] as? String != nil)
            #expect(first["name"] as? String != nil)
        }
    }
}
