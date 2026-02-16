import AsyncHTTPClient
import Foundation
import Logging
import WendyShared

// MARK: - Config Types

/// Analytics configuration
public struct WendyAnalyticsConfig: Codable, Sendable {
    public let anonymousId: String
    public var enabled: Bool
    public var optOutDate: Date?
    public let isInternal: Bool

    public init() {
        self.enabled = true
        self.anonymousId = UUID().uuidString
        self.optOutDate = nil
        self.isInternal = false
    }
}

// MARK: - Analytics Service

/// Main service for coordinating analytics tracking
public actor AnalyticsService {
    @TaskLocal public static var current: AnalyticsService?

    private let client: GA4Client?
    private let consentManager: ConsentManager
    private let logger = Logger(label: "sh.wendy.analytics")
    private var sessionId = UUID()
    private var anonymousId: String
    private let isInternalUser: Bool

    /// Initialize the analytics service
    public init?(config: WendyAnalyticsConfig) throws {
        // Check if analytics should be disabled
        if ConsentManager.shouldDisableAnalytics() {
            return nil
        }

        self.consentManager = ConsentManager()

        // Load config values
        self.anonymousId = config.anonymousId
        self.isInternalUser = config.isInternal

        // Create GA4 client
        // These are safe to embed, the api_secret has write-only access
        // and the measurement_id is a public identifier
        self.client = GA4Client(
            apiSecret: "PoyQfvLoSlqSQrT4ezPtHA",
            measurementId: "G-1MCX77F1SD"
        )
    }

    /// The user type for analytics events
    private var userType: String {
        isInternalUser ? "internal" : "user"
    }

    /// Tracks command execution with timing and error handling
    public func trackCommandExecution<T: Sendable>(
        _ commandExecution: @Sendable () async throws -> T
    ) async rethrows -> T {
        let startTime = Date()
        let commandName = extractCommandName()

        do {
            // Execute the command
            let result = try await AnalyticsService.$current.withValue(self) {
                try await commandExecution()
            }

            // Track success
            let duration = Date().timeIntervalSince(startTime)
            await trackCommandSuccess(
                commandName: commandName,
                duration: duration
            )

            return result
        } catch {
            // Track failure
            let duration = Date().timeIntervalSince(startTime)
            await trackCommandFailure(
                commandName: commandName,
                error: error,
                duration: duration
            )

            // Re-throw the error
            throw error
        }
    }

    /// Tracks a successful command execution
    private func trackCommandSuccess(
        commandName: String,
        duration: TimeInterval
    ) async {
        guard let client = client else { return }

        let event = AnalyticsEvent(
            name: "command_executed",
            properties: [
                "command_name": commandName,
                "success": "true",
                "duration_ms": String(Int(duration * 1000)),
                "cli_version": Version.current,
                "os": Platform.current.description,
                "os_version": Platform.osVersion,
                "arch": Platform.architecture,
                "session_id": sessionId.uuidString,
                "user_type": userType,
            ],
            distinctId: anonymousId
        )

        await client.capture(event: event)
    }

    /// Tracks a failed command execution
    private func trackCommandFailure(
        commandName: String,
        error: Error,
        duration: TimeInterval
    ) async {
        guard let client = client else { return }

        let sanitizedError = ErrorSanitizer.sanitize(error)

        let event = AnalyticsEvent(
            name: "command_failed",
            properties: [
                "command_name": commandName,
                "success": "false",
                "duration_ms": String(Int(duration * 1000)),
                "error_type": sanitizedError.type,
                "error_name": sanitizedError.name,
                "error_domain": sanitizedError.domain,
                "cli_version": Version.current,
                "os": Platform.current.description,
                "os_version": Platform.osVersion,
                "arch": Platform.architecture,
                "session_id": sessionId.uuidString,
                "user_type": userType,
            ],
            distinctId: anonymousId
        )

        await client.capture(event: event)
    }

    /// Extracts the command name from command line arguments
    private func extractCommandName() -> String {
        let args = CommandLine.arguments

        // First argument is the executable path
        guard args.count > 1 else {
            return "wendy"
        }

        // Extract just the command and subcommand, not arguments
        var commandParts = ["wendy"]
        var i = 1

        while i < args.count {
            let arg = args[i]

            // Stop at first flag or if we have command + subcommand
            if arg.starts(with: "-") || commandParts.count >= 3 {
                break
            }

            // Add non-flag arguments as command parts
            commandParts.append(arg)
            i += 1
        }

        return commandParts.joined(separator: " ")
    }

    /// Tracks a custom event
    public func trackEvent(
        name: String,
        properties: [String: String] = [:]
    ) async {
        guard let client else {
            return
        }

        var allProperties = properties
        allProperties["cli_version"] = Version.current
        allProperties["os"] = Platform.current.description
        allProperties["session_id"] = sessionId.uuidString
        allProperties["user_type"] = userType

        let event = AnalyticsEvent(
            name: name,
            properties: allProperties,
            distinctId: anonymousId
        )

        await client.capture(event: event)
    }

    /// Flushes all pending analytics events
    public func flush() async {
        await client?.flush()
    }
}

// MARK: - Platform

/// Platform information for analytics
private struct Platform {
    static var current: Self {
        #if os(macOS)
            return Platform(description: "macOS")
        #elseif os(Linux)
            return Platform(description: "Linux")
        #else
            return Platform(description: "Unknown")
        #endif
    }

    let description: String

    static var osVersion: String {
        let version = ProcessInfo.processInfo.operatingSystemVersion
        return "\(version.majorVersion).\(version.minorVersion).\(version.patchVersion)"
    }

    static var architecture: String {
        #if arch(arm64)
            return "arm64"
        #elseif arch(x86_64)
            return "x86_64"
        #else
            return "unknown"
        #endif
    }
}
