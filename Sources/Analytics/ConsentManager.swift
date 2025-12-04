import Foundation
import Logging
import Noora

/// Protocol for providing environment variables (for testability)
public protocol EnvironmentProvider: Sendable {
    func getValue(forKey key: String) -> String?
}

/// Default implementation using ProcessInfo
public struct ProcessInfoEnvironmentProvider: EnvironmentProvider, Sendable {
    public init() {}

    public func getValue(forKey key: String) -> String? {
        ProcessInfo.processInfo.environment[key]
    }
}

/// Manages user consent for analytics tracking
public struct ConsentManager: Sendable {
    private let logger = Logger(label: "sh.wendy.analytics.consent")
    private let environmentProvider: EnvironmentProvider

    public init(environmentProvider: EnvironmentProvider = ProcessInfoEnvironmentProvider()) {
        self.environmentProvider = environmentProvider
    }

    /// Checks if analytics should be disabled based on environment variables
    public func shouldDisableAnalytics() -> Bool {
        // Check WENDY_ANALYTICS environment variable
        // If set to any value other than "true", "1", or "enabled", analytics is disabled
        if let wendyAnalytics = environmentProvider.getValue(forKey: "WENDY_ANALYTICS") {
            let normalized = wendyAnalytics.lowercased()
            // Only enable if explicitly set to true, 1, or enabled
            return !(normalized == "true" || normalized == "1" || normalized == "enabled")
        }

        // Avoid sending analytics in CI environments
        if environmentProvider.getValue(forKey: "CI") != nil
            || environmentProvider.getValue(forKey: "CONTINUOUS_INTEGRATION") != nil
            || environmentProvider.getValue(forKey: "BUILD_ID") != nil
            || environmentProvider.getValue(forKey: "JENKINS_URL") != nil
            || environmentProvider.getValue(forKey: "GITHUB_ACTIONS") != nil
            || environmentProvider.getValue(forKey: "GITLAB_CI") != nil
        {
            return true
        }

        // Default: analytics not disabled (enabled by default)
        return false
    }

    /// Legacy static method for backwards compatibility
    public static func shouldDisableAnalytics() -> Bool {
        ConsentManager().shouldDisableAnalytics()
    }

    /// Checks if analytics is enabled based on config and environment
    public func isAnalyticsEnabled() async -> Bool {
        // Environment variables override config
        guard !shouldDisableAnalytics() else {
            return false
        }

        // Try to get config, but don't fail if it doesn't exist
        do {
            let configURL = FileManager.default.homeDirectoryForCurrentUser
                .appendingPathComponent(".wendy")
                .appendingPathComponent("config.json")

            guard FileManager.default.fileExists(atPath: configURL.path) else {
                // No config yet, default to enabled
                return true
            }

            let data = try Data(contentsOf: configURL)
            let decoder = JSONDecoder()
            if let config = try? decoder.decode(AnalyticsConfigWrapper.self, from: data) {
                return config.analytics?.enabled ?? true
            }

            // Config exists but doesn't have analytics section yet, default to enabled
            return true
        } catch {
            logger.debug("Failed to read config for analytics: \(error)")
            // On error reading config, default to enabled
            return true
        }
    }

    /// Disables analytics
    public func disableAnalytics() throws {
        let configDir = FileManager.default.homeDirectoryForCurrentUser
            .appendingPathComponent(".wendy")
        let configURL = configDir.appendingPathComponent("config.json")

        // Ensure directory exists
        try FileManager.default.createDirectory(
            at: configDir,
            withIntermediateDirectories: true,
            attributes: nil
        )

        // Read existing config or create new one
        var configData: [String: Any] = [:]
        if FileManager.default.fileExists(atPath: configURL.path) {
            let data = try Data(contentsOf: configURL)
            if let json = try JSONSerialization.jsonObject(with: data) as? [String: Any] {
                configData = json
            }
        }

        // Update analytics section
        var analytics = (configData["analytics"] as? [String: Any]) ?? [:]
        analytics["enabled"] = false
        analytics["optOutDate"] = ISO8601DateFormatter().string(from: Date())
        analytics["anonymousId"] = analytics["anonymousId"] ?? UUID().uuidString

        configData["analytics"] = analytics

        // Write back to file
        let updatedData = try JSONSerialization.data(
            withJSONObject: configData,
            options: [.prettyPrinted, .sortedKeys]
        )
        try updatedData.write(to: configURL)

        Noora().success("Analytics disabled")
    }

    /// Enables analytics
    public func enableAnalytics() throws {
        let configDir = FileManager.default.homeDirectoryForCurrentUser
            .appendingPathComponent(".wendy")
        let configURL = configDir.appendingPathComponent("config.json")

        // Ensure directory exists
        try FileManager.default.createDirectory(
            at: configDir,
            withIntermediateDirectories: true,
            attributes: nil
        )

        // Read existing config or create new one
        var configData: [String: Any] = [:]
        if FileManager.default.fileExists(atPath: configURL.path) {
            let data = try Data(contentsOf: configURL)
            if let json = try JSONSerialization.jsonObject(with: data) as? [String: Any] {
                configData = json
            }
        }

        // Update analytics section
        var analytics = (configData["analytics"] as? [String: Any]) ?? [:]
        analytics["enabled"] = true
        analytics["optOutDate"] = nil
        analytics["anonymousId"] = analytics["anonymousId"] ?? UUID().uuidString

        configData["analytics"] = analytics

        // Write back to file
        let updatedData = try JSONSerialization.data(
            withJSONObject: configData,
            options: [.prettyPrinted, .sortedKeys]
        )
        try updatedData.write(to: configURL)

        Noora().success("Analytics enabled")
    }

    /// Gets the current analytics status
    public func getStatus() async -> String {
        if shouldDisableAnalytics() {
            return "Analytics: Disabled (environment variable)"
        }

        let enabled = await isAnalyticsEnabled()
        return enabled ? "Analytics: Enabled" : "Analytics: Disabled"
    }
}

/// Helper struct for decoding analytics config
private struct AnalyticsConfigWrapper: Codable {
    let analytics: AnalyticsConfig?

    struct AnalyticsConfig: Codable {
        let enabled: Bool?
        let anonymousId: String?
        let optOutDate: String?
        let isInternal: Bool?
    }
}
