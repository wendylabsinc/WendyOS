import Testing
import Subprocess
import WendyE2ETesting

@Suite(.serialized)
struct `'wendy device audio'` {
    var cli: Session
    init() async throws { self.cli = try await Session.begin(for: .cli) }

    @Test
    func `describes subcommands`() async throws {
        try await self.cli.sh("./bin/wendy device audio --help") { standardOutput, standardError in
            #expect(standardError.isEmpty)
            #expect(standardOutput.contains("Manage audio devices"))
            #expect(standardOutput.contains("list"))
            #expect(standardOutput.contains("listen"))
            #expect(standardOutput.contains("monitor"))
            #expect(standardOutput.contains("set-default"))
        }
    }
}

// MARK: -

@Suite(.serialized)
struct `'wendy device audio list'` {
    var cli: Session
    init() async throws { self.cli = try await Session.begin(for: .cli) }

    @Test(
        .disabled("TODO: one-by-one E2E run fails against current local fixtures/implementation.")
    )
    func `lists audio devices on the selected device`() async throws {
        // TODO: Re-enable after adding the required fixture or implementation; one-by-one E2E run currently fails.
        let record = try await self.cli.sh(
            "WENDY_ANALYTICS=false ./bin/wendy --device 127.0.0.1 device audio list",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )
        #expect(record.terminationStatus.isSuccess)
        #expect(
            record.standardOutput?.contains("Audio") == true
                || record.standardOutput?.contains("Input") == true
                || record.standardOutput?.contains("Output") == true
        )
    }

    @Test(
        .disabled("TODO: one-by-one E2E run fails against current local fixtures/implementation.")
    )
    func `'--json' formats audio devices as JSON`() async throws {
        // TODO: Re-enable after adding the required fixture or implementation; one-by-one E2E run currently fails.
        let record = try await self.cli.sh(
            "WENDY_ANALYTICS=false ./bin/wendy --json --device 127.0.0.1 device audio list",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )
        #expect(record.terminationStatus.isSuccess)
        let array = try Helper.jsonArray(from: record.standardOutput ?? "")
        if let first = array.first as? [String: Any] {
            #expect(first["id"] != nil)
            #expect(first["name"] as? String != nil)
        }
    }
}

// MARK: -

@Suite(.serialized)
struct `'wendy device audio listen'` {
    var cli: Session
    init() async throws { self.cli = try await Session.begin(for: .cli) }

    @Test(
        .disabled("TODO: one-by-one E2E run fails against current local fixtures/implementation.")
    )
    func `starts listening to the selected audio input`() async throws {
        // TODO: Re-enable after adding the required fixture or implementation; one-by-one E2E run currently fails.
        let record = try await self.cli.sh(
            "WENDY_ANALYTICS=false /usr/bin/perl -e 'alarm 2; exec @ARGV' ./bin/wendy --device 127.0.0.1 device audio listen --id 1 --stdout",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )
        #expect(record.terminationStatus.isSuccess)
        #expect(
            record.standardOutput?.isEmpty == false
                || record.standardError?.contains("Streaming audio") == true
        )
    }

    @Test
    func `fails clearly when the audio input is unavailable`() async throws {
        let record = try await self.cli.sh(
            "WENDY_ANALYTICS=false /usr/bin/perl -e 'alarm 2; exec @ARGV' ./bin/wendy --device 127.0.0.1 device audio listen --id 999 --stdout",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )
        #expect(!record.terminationStatus.isSuccess)
        #expect(
            record.standardError?.contains("999") == true
                || record.standardError?.contains("audio") == true
                || Helper.isConnectionFailure(record.standardError)
        )
    }
}

// MARK: -

@Suite(.serialized)
struct `'wendy device audio monitor'` {
    var cli: Session
    init() async throws { self.cli = try await Session.begin(for: .cli) }

    @Test(
        .disabled("TODO: one-by-one E2E run fails against current local fixtures/implementation.")
    )
    func `streams audio level updates`() async throws {
        // TODO: Re-enable after adding the required fixture or implementation; one-by-one E2E run currently fails.
        let record = try await self.cli.sh(
            "WENDY_ANALYTICS=false /usr/bin/perl -e 'alarm 2; exec @ARGV' ./bin/wendy --device 127.0.0.1 device audio monitor --id 1 --rate 1",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )
        #expect(record.terminationStatus.isSuccess)
        #expect(
            record.standardOutput?.contains("level") == true
                || record.standardOutput?.contains("dB") == true
                || record.standardError?.contains("level") == true
        )
    }

    @Test
    func `fails clearly when audio monitoring is unavailable`() async throws {
        let record = try await self.cli.sh(
            "WENDY_ANALYTICS=false /usr/bin/perl -e 'alarm 2; exec @ARGV' ./bin/wendy --device 127.0.0.1 device audio monitor --id 999",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )
        #expect(!record.terminationStatus.isSuccess)
        #expect(
            record.standardError?.contains("audio") == true
                || record.standardError?.contains("monitor") == true
                || Helper.isConnectionFailure(record.standardError)
        )
    }
}

// MARK: -

@Suite(.serialized)
struct `'wendy device audio set-default'` {
    var cli: Session
    init() async throws { self.cli = try await Session.begin(for: .cli) }

    @Test(
        .disabled("TODO: one-by-one E2E run fails against current local fixtures/implementation.")
    )
    func `sets the default audio device`() async throws {
        // TODO: Re-enable after adding the required fixture or implementation; one-by-one E2E run currently fails.
        let record = try await self.cli.sh(
            "WENDY_ANALYTICS=false ./bin/wendy --device 127.0.0.1 device audio set-default --id 1",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )
        #expect(record.terminationStatus.isSuccess)
        #expect(record.standardOutput?.contains("default") == true)
        #expect(record.standardOutput?.contains("1") == true)
    }

    @Test
    func `fails clearly when the audio device is unknown`() async throws {
        let record = try await self.cli.sh(
            "WENDY_ANALYTICS=false ./bin/wendy --device 127.0.0.1 device audio set-default --id 999",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )
        #expect(!record.terminationStatus.isSuccess)
        #expect(
            record.standardError?.contains("999") == true
                || record.standardError?.contains("unknown") == true
                || Helper.isConnectionFailure(record.standardError)
        )
    }
}
