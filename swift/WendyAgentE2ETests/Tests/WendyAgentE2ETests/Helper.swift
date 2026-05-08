import Foundation
import Testing
import Subprocess
import WendyE2ETesting

enum Helper {
    static func repositoryRootDirectoryURL() -> URL {
        URL(fileURLWithPath: #filePath, isDirectory: false)
            .deletingLastPathComponent()  // Tests/WendyAgentE2ETests
            .deletingLastPathComponent()  // Tests
            .deletingLastPathComponent()  // swift/WendyAgentE2ETests
            .deletingLastPathComponent()  // swift
            .deletingLastPathComponent()  // repository root
    }

    static func fixturesDirectoryURL() -> URL {
        guard let path = ProcessInfo.processInfo.environment["WENDY_AGENT_E2E_FIXTURES_DIR"],
            !path.isEmpty
        else {
            return repositoryRootDirectoryURL()
                .appendingPathComponent(".github", isDirectory: true)
                .appendingPathComponent("swift-e2e-tests", isDirectory: true)
        }

        return URL(fileURLWithPath: path, isDirectory: true)
    }

    static func temporaryDirectory(prefix: String) throws -> URL {
        let directory = FileManager.default.temporaryDirectory
            .appendingPathComponent("\(prefix)-\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        return directory
    }

    @discardableResult
    static func writeWendyJSON(_ contents: String, to directory: URL) throws -> URL {
        let file = directory.appendingPathComponent("wendy.json", isDirectory: false)
        try contents.write(to: file, atomically: true, encoding: .utf8)
        return file
    }

    @discardableResult
    static func writeFile(_ contents: String, named name: String, to directory: URL) throws -> URL {
        let file = directory.appendingPathComponent(name, isDirectory: false)
        try contents.write(to: file, atomically: true, encoding: .utf8)
        return file
    }

    static func writeAnalyticsConfig(enabled: Bool, home: URL) throws {
        try writeUserConfig(["analytics": ["enabled": enabled]], home: home)
    }

    static func writeUserConfig(_ config: [String: Any], home: URL) throws {
        let configDirectory = home.appendingPathComponent(".wendy", isDirectory: true)
        try FileManager.default.createDirectory(
            at: configDirectory,
            withIntermediateDirectories: true
        )

        let data = try JSONSerialization.data(
            withJSONObject: config,
            options: [.prettyPrinted, .sortedKeys]
        )
        try data.write(to: configDirectory.appendingPathComponent("config.json"))
    }

    static func userConfig(home: URL) throws -> [String: Any] {
        let data = try Data(
            contentsOf:
                home
                .appendingPathComponent(".wendy", isDirectory: true)
                .appendingPathComponent("config.json", isDirectory: false)
        )
        return try #require(JSONSerialization.jsonObject(with: data) as? [String: Any])
    }

    static func analyticsConfigEnabled(home: URL) throws -> Bool {
        let object = try userConfig(home: home)
        let analytics = try #require(object["analytics"] as? [String: Any])
        return try #require(analytics["enabled"] as? Bool)
    }

    static func jsonObject(from string: String) throws -> [String: Any] {
        let data = Data(string.utf8)
        return try #require(JSONSerialization.jsonObject(with: data) as? [String: Any])
    }

    static func jsonArray(from string: String) throws -> [Any] {
        let data = Data(string.utf8)
        return try #require(JSONSerialization.jsonObject(with: data) as? [Any])
    }

    static func wendyJSONContents(
        appId: String = "sh.wendy.e2e.app",
        entitlements: String = "{ \"type\": \"network\" }"
    ) -> String {
        """
        {
          "appId": "\(appId)",
          "version": "1.0.0",
          "platform": "wendyos",
          "language": "swift",
          "entitlements": [
            \(entitlements)
          ]
        }
        """
    }

    static func commandEnvironment(home: URL? = nil) -> String {
        if let home {
            return "HOME=\(shellQuote(home.path)) WENDY_ANALYTICS=false"
        }
        return "WENDY_ANALYTICS=false"
    }

    static func isConnectionFailure(_ message: String?) -> Bool {
        guard let message else {
            return false
        }

        let lowercased = message.lowercased()
        return lowercased.contains("could not connect")
            || lowercased.contains("connection error")
            || lowercased.contains("connection refused")
            || lowercased.contains("error reading server preface")
            || lowercased.contains("use of closed network connection")
    }

    static func expectedGoOS(for os: MachineOS = Machine.cli.os) -> String {
        switch os {
        case .macOS:
            return "darwin"
        case .linux, .wendyOS:
            return "linux"
        case .windows:
            return "windows"
        }
    }

    static func cliCacheDirectory(home: URL, os: MachineOS = Machine.cli.os) -> URL {
        switch os {
        case .macOS:
            return home.appendingPathComponent("Library/Caches/wendy", isDirectory: true)
        case .linux, .wendyOS:
            return home.appendingPathComponent(".cache/wendy", isDirectory: true)
        case .windows:
            return home.appendingPathComponent("AppData/Local/wendy", isDirectory: true)
        }
    }

    static func osImageCacheDirectory(home: URL, os: MachineOS = Machine.cli.os) -> URL {
        cliCacheDirectory(home: home, os: os)
            .appendingPathComponent("os-images", isDirectory: true)
    }

    static func withAsyncCleanup<Result>(
        _ operation: () async throws -> Result,
        cleanup: () async throws -> Void
    ) async throws -> Result {
        do {
            let result = try await operation()
            try? await cleanup()
            return result
        } catch {
            try? await cleanup()
            throw error
        }
    }

    static func shellQuote(_ value: String) -> String {
        "'" + value.replacingOccurrences(of: "'", with: "'\\''") + "'"
    }
}
