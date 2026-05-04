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
        let home = try Self.temporaryHomeDirectory(prefix: "wendy-analytics-status")
        defer { try? FileManager.default.removeItem(at: home) }

        try Self.writeAnalyticsConfig(enabled: true, home: home)
        try await self.cli.run("HOME=\(Self.shellQuote(home.path)) ./bin/wendy analytics status") {
            standardOutput,
            standardError in
            #expect(standardOutput.isEmpty)
            #expect(standardError == "Analytics: enabled\n")
        }

        try Self.writeAnalyticsConfig(enabled: false, home: home)
        try await self.cli.run("HOME=\(Self.shellQuote(home.path)) ./bin/wendy analytics status") {
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
        let home = try Self.temporaryHomeDirectory(prefix: "wendy-analytics-enable")
        defer { try? FileManager.default.removeItem(at: home) }
        try Self.writeAnalyticsConfig(enabled: false, home: home)

        try await self.cli.run("HOME=\(Self.shellQuote(home.path)) ./bin/wendy analytics enable") {
            standardOutput,
            standardError in
            #expect(standardOutput.isEmpty)
            #expect(standardError == "Analytics enabled.\n")
        }

        #expect(try Self.analyticsConfigEnabled(home: home) == true)
        try await self.cli.run("HOME=\(Self.shellQuote(home.path)) ./bin/wendy analytics status") {
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
        let home = try Self.temporaryHomeDirectory(prefix: "wendy-analytics-disable")
        defer { try? FileManager.default.removeItem(at: home) }
        try Self.writeAnalyticsConfig(enabled: true, home: home)

        try await self.cli.run("HOME=\(Self.shellQuote(home.path)) ./bin/wendy analytics disable") {
            standardOutput,
            standardError in
            #expect(standardOutput.isEmpty)
            #expect(standardError == "Analytics disabled.\n")
        }

        #expect(try Self.analyticsConfigEnabled(home: home) == false)
        try await self.cli.run("HOME=\(Self.shellQuote(home.path)) ./bin/wendy analytics status") {
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

    private static func temporaryHomeDirectory(prefix: String) throws -> URL {
        let directory = FileManager.default.temporaryDirectory
            .appendingPathComponent("\(prefix)-\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        return directory
    }

    private static func writeAnalyticsConfig(enabled: Bool, home: URL) throws {
        let configDirectory = home.appendingPathComponent(".wendy", isDirectory: true)
        try FileManager.default.createDirectory(at: configDirectory, withIntermediateDirectories: true)

        let config = ["analytics": ["enabled": enabled]]
        let data = try JSONSerialization.data(withJSONObject: config, options: [.prettyPrinted, .sortedKeys])
        try data.write(to: configDirectory.appendingPathComponent("config.json"))
    }

    private static func analyticsConfigEnabled(home: URL) throws -> Bool {
        let data = try Data(
            contentsOf: home
                .appendingPathComponent(".wendy", isDirectory: true)
                .appendingPathComponent("config.json", isDirectory: false)
        )
        let object = try #require(JSONSerialization.jsonObject(with: data) as? [String: Any])
        let analytics = try #require(object["analytics"] as? [String: Any])
        return try #require(analytics["enabled"] as? Bool)
    }

    private static func shellQuote(_ value: String) -> String {
        "'" + value.replacingOccurrences(of: "'", with: "'\\''") + "'"
    }
}
