import Foundation
import Logging

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
}

extension WendyAnalyticsConfig {
    public mutating func disableAnalytics() {
        self.enabled = false
        self.optOutDate = Date()
    }

    public mutating func enableAnalytics() {
        self.enabled = true
        self.optOutDate = nil
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
