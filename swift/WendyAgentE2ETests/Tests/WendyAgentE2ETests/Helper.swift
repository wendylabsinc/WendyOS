import Foundation
import Testing

enum Helper {
    static func repositoryRootDirectoryURL() -> URL {
        URL(fileURLWithPath: #filePath, isDirectory: false)
            .deletingLastPathComponent()  // Tests/WendyAgentE2ETests
            .deletingLastPathComponent()  // Tests
            .deletingLastPathComponent()  // swift/WendyAgentE2ETests
            .deletingLastPathComponent()  // swift
            .deletingLastPathComponent()  // repository root
    }

    static func temporaryDirectory(prefix: String) throws -> URL {
        let directory = FileManager.default.temporaryDirectory
            .appendingPathComponent("\(prefix)-\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        return directory
    }

    static func writeWendyJSON(_ contents: String, to directory: URL) throws -> URL {
        let file = directory.appendingPathComponent("wendy.json", isDirectory: false)
        try contents.write(to: file, atomically: true, encoding: .utf8)
        return file
    }

    static func writeAnalyticsConfig(enabled: Bool, home: URL) throws {
        let configDirectory = home.appendingPathComponent(".wendy", isDirectory: true)
        try FileManager.default.createDirectory(at: configDirectory, withIntermediateDirectories: true)

        let config = ["analytics": ["enabled": enabled]]
        let data = try JSONSerialization.data(
            withJSONObject: config,
            options: [.prettyPrinted, .sortedKeys]
        )
        try data.write(to: configDirectory.appendingPathComponent("config.json"))
    }

    static func analyticsConfigEnabled(home: URL) throws -> Bool {
        let data = try Data(
            contentsOf: home
                .appendingPathComponent(".wendy", isDirectory: true)
                .appendingPathComponent("config.json", isDirectory: false)
        )
        let object = try #require(JSONSerialization.jsonObject(with: data) as? [String: Any])
        let analytics = try #require(object["analytics"] as? [String: Any])
        return try #require(analytics["enabled"] as? Bool)
    }

    static func jsonObject(from string: String) throws -> [String: Any] {
        let data = Data(string.utf8)
        return try #require(JSONSerialization.jsonObject(with: data) as? [String: Any])
    }

    static func shellQuote(_ value: String) -> String {
        "'" + value.replacingOccurrences(of: "'", with: "'\\''") + "'"
    }
}
