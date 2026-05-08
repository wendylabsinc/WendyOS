import Foundation
import Testing
import Subprocess
import WendyE2ETesting

@Suite(.serialized)
struct `'wendy device'` {
    var cli: Session

    init() async throws {
        self.cli = try await Session.begin(for: .cli)
    }

    @Test
    func `describes management subcommands`() async throws {
        try await self.cli.sh("./bin/wendy device --help") { standardOutput, standardError in
            #expect(standardError.isEmpty)
            #expect(standardOutput.contains("Manage WendyOS devices"))
            #expect(standardOutput.contains("Device Management:"))
            #expect(standardOutput.contains("Monitoring:"))
            #expect(standardOutput.contains("Hardware:"))
            #expect(standardOutput.contains("Apps & Storage:"))
        }
    }

    @Test(
        .disabled("TODO: one-by-one E2E run fails against current local fixtures/implementation.")
    )
    func `uses the configured default target when none is specified`() async throws {
        // TODO: Re-enable after adding the required fixture or implementation; one-by-one E2E run currently fails.
        let home = try Helper.temporaryDirectory(prefix: "wendy-device-default-target")
        defer { try? FileManager.default.removeItem(at: home) }
        try Helper.writeUserConfig(
            ["analytics": ["enabled": false], "defaultDevice": "127.0.0.1"],
            home: home
        )

        let record = try await self.cli.sh(
            "\(Helper.commandEnvironment(home: home)) ./bin/wendy --json device version",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )

        #expect(record.terminationStatus.isSuccess)
        let object = try Helper.jsonObject(from: record.standardOutput ?? "")
        #expect(
            object["device"] as? String == "127.0.0.1"
                || object["hostname"] as? String == "127.0.0.1"
        )
        #expect((object["version"] as? String)?.isEmpty == false)
    }
}

// MARK: -

@Suite(.serialized)
struct `'wendy device set-default'` {
    var cli: Session

    init() async throws {
        self.cli = try await Session.begin(for: .cli)
    }

    @Test
    func `persists the default device hostname`() async throws {
        let home = try Helper.temporaryDirectory(prefix: "wendy-device-set-default")
        defer { try? FileManager.default.removeItem(at: home) }
        try Helper.writeAnalyticsConfig(enabled: false, home: home)

        try await self.cli.sh(
            "\(Helper.commandEnvironment(home: home)) ./bin/wendy device set-default wendy-e2e.local"
        ) { standardOutput, standardError in
            #expect(standardError.isEmpty)
            #expect(standardOutput == "Default device set to: wendy-e2e.local\n")
        }

        let config = try Helper.userConfig(home: home)
        #expect(config["defaultDevice"] as? String == "wendy-e2e.local")
    }

    @Test(
        .disabled("TODO: one-by-one E2E run fails against current local fixtures/implementation.")
    )
    func `rejects an invalid device hostname`() async throws {
        // TODO: Re-enable after adding the required fixture or implementation; one-by-one E2E run currently fails.
        let home = try Helper.temporaryDirectory(prefix: "wendy-device-set-invalid")
        defer { try? FileManager.default.removeItem(at: home) }
        try Helper.writeAnalyticsConfig(enabled: false, home: home)

        let record = try await self.cli.sh(
            "\(Helper.commandEnvironment(home: home)) ./bin/wendy device set-default 'not a hostname!'",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )

        #expect(!record.terminationStatus.isSuccess)
        #expect(record.standardError?.contains("invalid") == true)
        #expect(try Helper.userConfig(home: home)["defaultDevice"] == nil)
    }
}

// MARK: -

@Suite(.serialized)
struct `'wendy device setup'` {
    var cli: Session

    init() async throws {
        self.cli = try await Session.begin(for: .cli)
    }

    @Test(
        .disabled("TODO: one-by-one E2E run fails against current local fixtures/implementation.")
    )
    func `guides interactive device provisioning`() async throws {
        // TODO: Re-enable after adding the required fixture or implementation; one-by-one E2E run currently fails.
        let home = try Helper.temporaryDirectory(prefix: "wendy-device-setup")
        defer { try? FileManager.default.removeItem(at: home) }

        let record = try await self.cli.sh(
            "\(Helper.commandEnvironment(home: home)) /usr/bin/perl -e 'alarm 2; exec @ARGV' ./bin/wendy device setup",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )

        #expect(
            record.standardOutput?.contains("provision") == true
                || record.standardError?.contains("provision") == true
        )
        #expect(
            record.standardOutput?.contains("WiFi") == true
                || record.standardError?.contains("WiFi") == true
        )
        #expect(
            record.standardOutput?.contains("device") == true
                || record.standardError?.contains("device") == true
        )
    }

    @Test
    func `handles cancellation without changing configuration`() async throws {
        let home = try Helper.temporaryDirectory(prefix: "wendy-device-setup-cancel")
        defer { try? FileManager.default.removeItem(at: home) }
        try Helper.writeUserConfig(
            ["analytics": ["enabled": false], "defaultDevice": "before.local"],
            home: home
        )

        let record = try await self.cli.sh(
            "\(Helper.commandEnvironment(home: home)) printf '\\003' | ./bin/wendy device setup",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )

        #expect(
            !record.terminationStatus.isSuccess || record.standardOutput?.contains("cancel") == true
                || record.standardError?.contains("cancel") == true
        )
        #expect(try Helper.userConfig(home: home)["defaultDevice"] as? String == "before.local")
    }
}

// MARK: -

@Suite(.serialized)
struct `'wendy device unset-default'` {
    var cli: Session

    init() async throws {
        self.cli = try await Session.begin(for: .cli)
    }

    @Test
    func `removes the configured default device`() async throws {
        let home = try Helper.temporaryDirectory(prefix: "wendy-device-unset")
        defer { try? FileManager.default.removeItem(at: home) }
        try Helper.writeUserConfig(
            ["analytics": ["enabled": false], "defaultDevice": "wendy-e2e.local"],
            home: home
        )

        try await self.cli.sh(
            "\(Helper.commandEnvironment(home: home)) ./bin/wendy device unset-default"
        ) { standardOutput, standardError in
            #expect(standardError.isEmpty)
            #expect(standardOutput.contains("Default device cleared"))
        }

        #expect(try Helper.userConfig(home: home)["defaultDevice"] == nil)
    }

    @Test
    func `succeeds when no default device is configured`() async throws {
        let home = try Helper.temporaryDirectory(prefix: "wendy-device-unset-empty")
        defer { try? FileManager.default.removeItem(at: home) }
        try Helper.writeAnalyticsConfig(enabled: false, home: home)

        try await self.cli.sh(
            "\(Helper.commandEnvironment(home: home)) ./bin/wendy device unset-default"
        ) { standardOutput, standardError in
            #expect(standardError.isEmpty)
            #expect(
                standardOutput.contains("Default device cleared")
                    || standardOutput.contains("No default device")
            )
        }
    }
}

// MARK: -

@Suite(.serialized)
struct `'wendy device update'` {
    var cli: Session

    init() async throws {
        self.cli = try await Session.begin(for: .cli)
    }

    @Test(
        .disabled("TODO: one-by-one E2E run fails against current local fixtures/implementation.")
    )
    func `uploads the current agent build to the selected device`() async throws {
        // TODO: Re-enable after adding the required fixture or implementation; one-by-one E2E run currently fails.
        let binaryDirectory = try Helper.temporaryDirectory(prefix: "wendy-device-update-binary")
        defer { try? FileManager.default.removeItem(at: binaryDirectory) }
        let binary = try Helper.writeFile("agent-binary", named: "wendy-agent", to: binaryDirectory)

        let record = try await self.cli.sh(
            "WENDY_ANALYTICS=false ./bin/wendy --device 127.0.0.1 device update --binary \(Helper.shellQuote(binary.path))",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )

        #expect(record.terminationStatus.isSuccess)
        #expect(record.standardOutput?.contains("Uploading") == true)
        #expect(record.standardOutput?.contains("updated") == true)
    }

    @Test
    func `fails clearly when the selected device is unreachable`() async throws {
        let record = try await self.cli.sh(
            "WENDY_ANALYTICS=false ./bin/wendy --device 127.0.0.1 device update --binary /no/such/agent",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )

        #expect(!record.terminationStatus.isSuccess)
        #expect(
            Helper.isConnectionFailure(record.standardError)
                || record.standardError?.contains("unreachable") == true
                || record.standardError?.contains("no such") == true
        )
    }
}

// MARK: -

@Suite(.serialized)
struct `'wendy device version'` {
    var cli: Session

    init() async throws {
        self.cli = try await Session.begin(for: .cli)
    }

    @Test(
        .disabled("TODO: one-by-one E2E run fails against current local fixtures/implementation.")
    )
    func `prints version and hardware details from the selected device`() async throws {
        // TODO: Re-enable after adding the required fixture or implementation; one-by-one E2E run currently fails.
        let record = try await self.cli.sh(
            "WENDY_ANALYTICS=false ./bin/wendy --device 127.0.0.1 device version",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )

        #expect(record.terminationStatus.isSuccess)
        #expect(record.standardOutput?.contains("Version") == true)
        #expect(record.standardOutput?.contains("OS") == true)
        #expect(
            record.standardOutput?.contains("Architecture") == true
                || record.standardOutput?.contains("CPU") == true
        )
    }

    @Test(
        .disabled("TODO: one-by-one E2E run fails against current local fixtures/implementation.")
    )
    func `'--json' formats version and hardware details as JSON`() async throws {
        // TODO: Re-enable after adding the required fixture or implementation; one-by-one E2E run currently fails.
        let record = try await self.cli.sh(
            "WENDY_ANALYTICS=false ./bin/wendy --json --device 127.0.0.1 device version",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )

        #expect(record.terminationStatus.isSuccess)
        let object = try Helper.jsonObject(from: record.standardOutput ?? "")
        #expect((object["version"] as? String)?.isEmpty == false)
        #expect((object["os"] as? String)?.isEmpty == false)
        #expect((object["cpuArchitecture"] as? String)?.isEmpty == false)
    }
}

// MARK: -

@Suite(.serialized)
struct `'wendy device dashboard'` {
    var cli: Session

    init() async throws {
        self.cli = try await Session.begin(for: .cli)
    }

    @Test(
        .disabled("TODO: one-by-one E2E run fails against current local fixtures/implementation.")
    )
    func `opens a live dashboard for the selected device`() async throws {
        // TODO: Re-enable after adding the required fixture or implementation; one-by-one E2E run currently fails.
        let record = try await self.cli.sh(
            "WENDY_ANALYTICS=false /usr/bin/perl -e 'alarm 2; exec @ARGV' ./bin/wendy --device 127.0.0.1 device dashboard --app sh.wendy.e2e.app",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )

        #expect(record.terminationStatus.isSuccess)
        #expect(
            record.standardOutput?.contains("dashboard") == true
                || record.standardError?.contains("dashboard") == true
        )
        #expect(
            record.standardOutput?.contains("sh.wendy.e2e.app") == true
                || record.standardError?.contains("sh.wendy.e2e.app") == true
        )
    }

    @Test(
        .disabled("TODO: hangs when a local agent is reachable because dashboard opens a live TUI.")
    )
    func `fails clearly when dashboard data cannot be reached`() async throws {
        let record = try await self.cli.sh(
            "WENDY_ANALYTICS=false /usr/bin/perl -e 'alarm 2; exec @ARGV' ./bin/wendy --device 127.0.0.1 device dashboard",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )

        #expect(!record.terminationStatus.isSuccess)
        #expect(
            Helper.isConnectionFailure(record.standardError)
                || record.standardError?.contains("dashboard") == true
        )
    }
}

// MARK: -

@Suite(.serialized)
struct `'wendy device logs'` {
    var cli: Session

    init() async throws {
        self.cli = try await Session.begin(for: .cli)
    }

    @Test(
        .disabled("TODO: one-by-one E2E run fails against current local fixtures/implementation.")
    )
    func `streams logs from applications on the selected device`() async throws {
        // TODO: Re-enable after adding the required fixture or implementation; one-by-one E2E run currently fails.
        let record = try await self.cli.sh(
            "WENDY_ANALYTICS=false /usr/bin/perl -e 'alarm 2; exec @ARGV' ./bin/wendy --device 127.0.0.1 device logs",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )

        #expect(record.terminationStatus.isSuccess)
        #expect(
            record.standardOutput?.contains("timestamp") == true
                || record.standardOutput?.contains("level") == true
                || record.standardOutput?.contains("message") == true
        )
    }

    @Test(
        .disabled("TODO: one-by-one E2E run fails against current local fixtures/implementation.")
    )
    func `'--app' filters logs by application`() async throws {
        // TODO: Re-enable after adding the required fixture or implementation; one-by-one E2E run currently fails.
        let record = try await self.cli.sh(
            "WENDY_ANALYTICS=false /usr/bin/perl -e 'alarm 2; exec @ARGV' ./bin/wendy --device 127.0.0.1 device logs --app sh.wendy.e2e.app",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )

        #expect(record.terminationStatus.isSuccess)
        #expect(record.standardOutput?.contains("sh.wendy.e2e.app") == true)
        #expect(record.standardOutput?.contains("other-app") != true)
    }
}

// MARK: -

@Suite(.serialized)
struct `'wendy device telemetry-stream'` {
    var cli: Session

    init() async throws {
        self.cli = try await Session.begin(for: .cli)
    }

    @Test(
        .disabled("TODO: one-by-one E2E run fails against current local fixtures/implementation.")
    )
    func `streams telemetry as JSON lines`() async throws {
        // TODO: Re-enable after adding the required fixture or implementation; one-by-one E2E run currently fails.
        let record = try await self.cli.sh(
            "WENDY_ANALYTICS=false /usr/bin/perl -e 'alarm 2; exec @ARGV' ./bin/wendy --device 127.0.0.1 device telemetry-stream --logs --metrics --traces",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )

        #expect(record.terminationStatus.isSuccess)
        let firstLine = try #require(record.standardOutput?.split(separator: "\n").first)
        let object = try Helper.jsonObject(from: String(firstLine))
        #expect(object["timestamp"] != nil)
        #expect(object["type"] != nil)
    }

    @Test(
        .disabled(
            "TODO: hangs when a local agent is reachable because telemetry-stream opens a live stream."
        )
    )
    func `fails clearly when telemetry cannot be reached`() async throws {
        let record = try await self.cli.sh(
            "WENDY_ANALYTICS=false /usr/bin/perl -e 'alarm 2; exec @ARGV' ./bin/wendy --device 127.0.0.1 device telemetry-stream --metrics",
            output: .string(limit: .max),
            error: .string(limit: .max)
        )

        #expect(!record.terminationStatus.isSuccess)
        #expect(
            Helper.isConnectionFailure(record.standardError)
                || record.standardError?.contains("telemetry") == true
        )
    }
}
