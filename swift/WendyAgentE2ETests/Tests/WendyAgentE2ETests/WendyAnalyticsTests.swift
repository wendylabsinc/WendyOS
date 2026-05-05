import Foundation
import Testing
import WendyE2ETesting

@Suite(.serialized)
struct `wendy analytics status` {
    var cli: Machine

    init() async throws {
        self.cli = try await Machine.cli()
    }

    @Test
    func `'wendy analytics status' shows whether analytics are enabled`() async throws {
        let home = try Helper.temporaryDirectory(prefix: "wendy-analytics-status")
        defer { try? FileManager.default.removeItem(at: home) }

        try Helper.writeAnalyticsConfig(enabled: true, home: home)
        try await self.cli.run("HOME=\(Helper.shellQuote(home.path)) ./bin/wendy analytics status")
        {
            standardOutput,
            standardError in
            #expect(standardOutput.isEmpty)
            #expect(standardError == "Analytics: enabled\n")
        }

        try Helper.writeAnalyticsConfig(enabled: false, home: home)
        try await self.cli.run("HOME=\(Helper.shellQuote(home.path)) ./bin/wendy analytics status")
        {
            standardOutput,
            standardError in
            #expect(standardOutput.isEmpty)
            #expect(standardError == "Analytics: disabled\n")
        }
    }
}

@Suite(.serialized)
struct `wendy analytics enable` {
    var cli: Machine

    init() async throws {
        self.cli = try await Machine.cli()
    }

    @Test
    func `'wendy analytics enable' enables anonymous analytics`() async throws {
        let home = try Helper.temporaryDirectory(prefix: "wendy-analytics-enable")
        defer { try? FileManager.default.removeItem(at: home) }
        try Helper.writeAnalyticsConfig(enabled: false, home: home)

        try await self.cli.run("HOME=\(Helper.shellQuote(home.path)) ./bin/wendy analytics enable")
        {
            standardOutput,
            standardError in
            #expect(standardOutput.isEmpty)
            #expect(standardError == "Analytics enabled.\n")
        }

        #expect(try Helper.analyticsConfigEnabled(home: home) == true)
        try await self.cli.run("HOME=\(Helper.shellQuote(home.path)) ./bin/wendy analytics status")
        {
            standardOutput,
            standardError in
            #expect(standardOutput.isEmpty)
            #expect(standardError == "Analytics: enabled\n")
        }
    }
}

@Suite(.serialized)
struct `wendy analytics disable` {
    var cli: Machine

    init() async throws {
        self.cli = try await Machine.cli()
    }

    @Test
    func `'wendy analytics disable' disables anonymous analytics`() async throws {
        let home = try Helper.temporaryDirectory(prefix: "wendy-analytics-disable")
        defer { try? FileManager.default.removeItem(at: home) }
        try Helper.writeAnalyticsConfig(enabled: true, home: home)

        try await self.cli.run("HOME=\(Helper.shellQuote(home.path)) ./bin/wendy analytics disable")
        {
            standardOutput,
            standardError in
            #expect(standardOutput.isEmpty)
            #expect(standardError == "Analytics disabled.\n")
        }

        #expect(try Helper.analyticsConfigEnabled(home: home) == false)
        try await self.cli.run("HOME=\(Helper.shellQuote(home.path)) ./bin/wendy analytics status")
        {
            standardOutput,
            standardError in
            #expect(standardOutput.isEmpty)
            #expect(standardError == "Analytics: disabled\n")
        }
    }
}
