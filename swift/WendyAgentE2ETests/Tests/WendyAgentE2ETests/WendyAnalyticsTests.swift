import Foundation
import Testing
import WendyE2ETesting

@Suite(.serialized)
struct `wendy analytics` {
    var cli: Machine

    init() async throws {
        self.cli = try await Machine.cli()
    }

    @Test
    func `'wendy analytics status' shows whether analytics are enabled`() async throws {
        let home = try Helper.temporaryDirectory(prefix: "wendy-analytics-status")
        defer { try? FileManager.default.removeItem(at: home) }

        try Helper.writeAnalyticsConfig(enabled: true, home: home)
        try await self.cli.run("HOME=\(Helper.shellQuote(home.path)) ./bin/wendy analytics status") {
            standardOutput,
            standardError in
            #expect(standardOutput.isEmpty)
            #expect(standardError == "Analytics: enabled\n")
        }

        try Helper.writeAnalyticsConfig(enabled: false, home: home)
        try await self.cli.run("HOME=\(Helper.shellQuote(home.path)) ./bin/wendy analytics status") {
            standardOutput,
            standardError in
            #expect(standardOutput.isEmpty)
            #expect(standardError == "Analytics: disabled\n")
        }

        // AI:
        // - Status output clearly distinguishes enabled and disabled states.
        // - The command uses isolated test config instead of real user settings.
        // - Only the expected one-line status messages are printed.
    }

    @Test
    func `'wendy analytics enable' enables anonymous analytics`() async throws {
        let home = try Helper.temporaryDirectory(prefix: "wendy-analytics-enable")
        defer { try? FileManager.default.removeItem(at: home) }
        try Helper.writeAnalyticsConfig(enabled: false, home: home)

        try await self.cli.run("HOME=\(Helper.shellQuote(home.path)) ./bin/wendy analytics enable") {
            standardOutput,
            standardError in
            #expect(standardOutput.isEmpty)
            #expect(standardError == "Analytics enabled.\n")
        }

        #expect(try Helper.analyticsConfigEnabled(home: home) == true)
        try await self.cli.run("HOME=\(Helper.shellQuote(home.path)) ./bin/wendy analytics status") {
            standardOutput,
            standardError in
            #expect(standardOutput.isEmpty)
            #expect(standardError == "Analytics: enabled\n")
        }

        // AI:
        // - Enable output clearly confirms anonymous analytics are enabled.
        // - The enabled state persists to isolated config and is reflected by status.
        // - Only the expected confirmation and status messages are printed.
    }

    @Test
    func `'wendy analytics disable' disables anonymous analytics`() async throws {
        let home = try Helper.temporaryDirectory(prefix: "wendy-analytics-disable")
        defer { try? FileManager.default.removeItem(at: home) }
        try Helper.writeAnalyticsConfig(enabled: true, home: home)

        try await self.cli.run("HOME=\(Helper.shellQuote(home.path)) ./bin/wendy analytics disable") {
            standardOutput,
            standardError in
            #expect(standardOutput.isEmpty)
            #expect(standardError == "Analytics disabled.\n")
        }

        #expect(try Helper.analyticsConfigEnabled(home: home) == false)
        try await self.cli.run("HOME=\(Helper.shellQuote(home.path)) ./bin/wendy analytics status") {
            standardOutput,
            standardError in
            #expect(standardOutput.isEmpty)
            #expect(standardError == "Analytics: disabled\n")
        }

        // AI:
        // - Disable output clearly confirms anonymous analytics are disabled.
        // - The disabled state persists to isolated config and is reflected by status.
        // - Only the expected confirmation and status messages are printed.
    }
}
