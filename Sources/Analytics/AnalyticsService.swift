import AsyncHTTPClient
import Foundation
import Logging
import WendyShared

// MARK: - Config Types

/// Wendy configuration file structure
private struct WendyConfig: Codable {
    let analytics: WendyAnalyticsConfig?
}

/// Analytics configuration
private struct WendyAnalyticsConfig: Codable {
    let enabled: Bool?
    let anonymousId: String?
    let optOutDate: String?
}

// MARK: - Analytics Service

/// Main service for coordinating analytics tracking
public actor AnalyticsService {
    /// Shared instance of the analytics service
    public static let shared: AnalyticsService? = {
        // Check if analytics should be disabled
        guard !ConsentManager.shouldDisableAnalytics() else {
            return nil
        }

        // Try to create the service
        do {
            return try AnalyticsService()
        } catch {
            // If we can't create the service for whatever reason, analytics is disabled
            return nil
        }
    }()

    private let client: PostHogClient?
    private let consentManager: ConsentManager
    private let logger = Logger(label: "sh.wendy.analytics")
    private var sessionId = UUID()
    private var anonymousId: String

    /// Initialize the analytics service
    public init() throws {
        self.consentManager = ConsentManager()

        // Try to get anonymous ID from config, or generate a new one
        self.anonymousId = Self.getAnonymousId()

        // Create PostHog client
        // This key is safe to embed, it is a public facing key with write-only access
        self.client = PostHogClient(apiKey: "phc_DCgbsvbGPdGhU6GW3CQnEwGCsNNrAHYwMhj4HkhjU4f")
    }

    /// Get or create an anonymous user ID
    private static func getAnonymousId() -> String {
        do {
            let configURL = FileManager.default.homeDirectoryForCurrentUser
                .appendingPathComponent(".wendy")
                .appendingPathComponent("config.json")

            if FileManager.default.fileExists(atPath: configURL.path) {
                let data = try Data(contentsOf: configURL)
                let config = try JSONDecoder().decode(WendyConfig.self, from: data)
                if let id = config.analytics?.anonymousId {
                    return id
                }
            }
        } catch {
            // Ignore errors, just generate a new ID
        }

        return UUID().uuidString
    }

    /// Tracks command execution with timing and error handling
    public func trackCommandExecution<T>(
        _ commandExecution: @Sendable () async throws -> T
    ) async rethrows -> T {
        // Check if analytics is enabled
        guard await consentManager.isAnalyticsEnabled(),
            client != nil
        else {
            // Analytics disabled, just run the command
            return try await commandExecution()
        }

        let startTime = Date()
        let commandName = extractCommandName()

        do {
            // Execute the command
            let result = try await commandExecution()

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
        guard await consentManager.isAnalyticsEnabled(),
            let client = client
        else {
            return
        }

        var allProperties = properties
        allProperties["cli_version"] = Version.current
        allProperties["os"] = Platform.current.description
        allProperties["session_id"] = sessionId.uuidString

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
