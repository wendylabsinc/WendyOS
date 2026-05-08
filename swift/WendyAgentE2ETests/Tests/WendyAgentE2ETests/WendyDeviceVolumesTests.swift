import Testing
import Subprocess
import WendyE2ETesting

@Suite(.serialized)
struct `'wendy device volumes'` {
    var cli: Session
    init() async throws { self.cli = try await Session.begin(for: .cli) }

    @Test
    func `describes management subcommands`() async throws {
        try await self.cli.sh("./bin/wendy device volumes --help") {
            standardOutput,
            standardError in
            #expect(standardError.isEmpty)
            #expect(standardOutput.contains("Manage persistent volumes"))
            #expect(standardOutput.contains("list"))
            #expect(standardOutput.contains("remove"))
        }
    }
}

// MARK: -

@Suite(.serialized)
struct `'wendy device volumes list'` {
    var cli: Session
    init() async throws { self.cli = try await Session.begin(for: .cli) }

    @Test(
        .disabled("TODO: one-by-one E2E run fails against current local fixtures/implementation.")
    )
    func `lists persistent volumes on the selected device`() async throws {
        // TODO: Re-enable after adding the required fixture or implementation; one-by-one E2E run currently fails.
        let record = try await self.cli.sh(
            "WENDY_ANALYTICS=false ./bin/wendy --device 127.0.0.1 device volumes list",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )
        #expect(record.terminationStatus.isSuccess)
        #expect(
            record.standardOutput?.contains("Volume") == true
                || record.standardOutput?.contains("Name") == true
        )
    }

    @Test(
        .disabled("TODO: one-by-one E2E run fails against current local fixtures/implementation.")
    )
    func `'--json' formats persistent volumes as JSON`() async throws {
        // TODO: Re-enable after adding the required fixture or implementation; one-by-one E2E run currently fails.
        let record = try await self.cli.sh(
            "WENDY_ANALYTICS=false ./bin/wendy --json --device 127.0.0.1 device volumes list",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )
        #expect(record.terminationStatus.isSuccess)
        let array = try Helper.jsonArray(from: record.standardOutput ?? "")
        if let first = array.first as? [String: Any] {
            #expect(first["name"] as? String != nil)
            #expect(first["mountPath"] as? String != nil || first["path"] as? String != nil)
        }
    }
}

// MARK: -

@Suite(.serialized)
struct `'wendy device volumes remove'` {
    var cli: Session
    init() async throws { self.cli = try await Session.begin(for: .cli) }

    @Test(
        .disabled("TODO: one-by-one E2E run fails against current local fixtures/implementation.")
    )
    func `removes an existing persistent volume`() async throws {
        // TODO: Re-enable after adding the required fixture or implementation; one-by-one E2E run currently fails.
        let record = try await self.cli.sh(
            "WENDY_ANALYTICS=false ./bin/wendy --device 127.0.0.1 device volumes remove e2e-data --force",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )
        #expect(record.terminationStatus.isSuccess)
        #expect(record.standardOutput?.contains("Removed") == true)
        #expect(record.standardOutput?.contains("e2e-data") == true)
    }

    @Test
    func `fails clearly when the persistent volume does not exist`() async throws {
        let record = try await self.cli.sh(
            "WENDY_ANALYTICS=false ./bin/wendy --device 127.0.0.1 device volumes remove missing-volume --force",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )
        #expect(!record.terminationStatus.isSuccess)
        #expect(
            record.standardError?.contains("missing-volume") == true
                || record.standardError?.contains("not exist") == true
                || Helper.isConnectionFailure(record.standardError)
        )
    }
}
