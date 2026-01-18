import Analytics
import ArgumentParser
import Foundation
import Noora

struct AnalyticsCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "analytics",
        abstract: "Manage analytics preferences",
        discussion: """
            Control how Wendy collects anonymous usage data to improve the CLI.

            Analytics data collected includes:
            • Command names and success/failure status
            • Error types (no sensitive data)
            • CLI version and operating system
            • Anonymous identifier (no personal information)

            We never collect:
            • File paths or project names
            • Hostnames or IP addresses
            • User names or email addresses
            • Code content or command arguments
            • Credentials or tokens

            Analytics is automatically disabled when:
            • DO_NOT_TRACK=1 environment variable is set
            • Running in CI environments
            • WENDY_ANALYTICS=false environment variable is set
            """,
        subcommands: [
            EnableCommand.self,
            DisableCommand.self,
            StatusCommand.self,
        ],
        defaultSubcommand: StatusCommand.self
    )
}

// MARK: - Enable Command

extension AnalyticsCommand {
    struct EnableCommand: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            commandName: "enable",
            abstract: "Enable anonymous analytics"
        )

        func run() async throws {
            var config = getConfig()
            config.analytics.enableAnalytics()
            try config.save()

            if JSONMode.isEnabled {
                struct Response: Codable {
                    let success: Bool
                    let enabled: Bool
                }
                let response = Response(success: true, enabled: true)
                let encoder = JSONEncoder()
                encoder.outputFormatting = .prettyPrinted
                let data = try encoder.encode(response)
                print(String(data: data, encoding: .utf8)!)
            } else {
                // Show what data we collect
                Noora().info(
                    """
                    Analytics has been enabled.

                    We collect:
                    • Command names and success/failure
                    • Error types (no sensitive data)
                    • CLI version and OS
                    • Anonymous ID (no personal information)

                    You can disable at anytime with: wendy analytics disable
                    Or set environment variable: WENDY_ANALYTICS=false
                    """
                )
            }
        }
    }
}

// MARK: - Disable Command

extension AnalyticsCommand {
    struct DisableCommand: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            commandName: "disable",
            abstract: "Disable analytics"
        )

        func run() async throws {
            var config = getConfig()
            config.analytics.disableAnalytics()
            try config.save()

            if JSONMode.isEnabled {
                struct Response: Codable {
                    let success: Bool
                    let enabled: Bool
                }
                let response = Response(success: true, enabled: false)
                let encoder = JSONEncoder()
                encoder.outputFormatting = .prettyPrinted
                let data = try encoder.encode(response)
                print(String(data: data, encoding: .utf8)!)
            } else {
                Noora().info(
                    """
                    Analytics has been disabled.

                    You can re-enable anytime with: wendy analytics enable
                    Or set environment variable: WENDY_ANALYTICS=true
                    """
                )
            }
        }
    }
}

// MARK: - Status Command

extension AnalyticsCommand {
    struct StatusCommand: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            commandName: "status",
            abstract: "Show current analytics status"
        )

        @Flag(name: .shortAndLong, help: "Show verbose analytics configuration")
        var verbose = false

        func run() async throws {
            let config = getConfig()

            if JSONMode.isEnabled {
                struct StatusResponse: Codable {
                    let enabled: Bool
                    let optOutDate: String?
                    let anonymousId: String?
                    let configPath: String
                }
                let response = StatusResponse(
                    enabled: config.analytics.enabled,
                    optOutDate: config.analytics.optOutDate?.formatted(),
                    anonymousId: config.analytics.enabled ? config.analytics.anonymousId : nil,
                    configPath: FileManager.default.homeDirectoryForCurrentUser
                        .appendingPathComponent(".wendy/config.json").path
                )
                let encoder = JSONEncoder()
                encoder.outputFormatting = .prettyPrinted
                let data = try encoder.encode(response)
                print(String(data: data, encoding: .utf8)!)
                return
            }

            // Display the status
            print(config.analytics.enabled ? "✅ Analytics is enabled" : "❌ Analytics is disabled")
            if let optOutDate = config.analytics.optOutDate {
                Noora().info("Opt Out Date: \(optOutDate.formatted().underline)")
            }

            if verbose {
                // Show environment variables that affect analytics
                let env = ProcessInfo.processInfo.environment

                var envInfo: [String] = []

                if let doNotTrack = env["DO_NOT_TRACK"] {
                    envInfo.append("  DO_NOT_TRACK=\(doNotTrack)")
                }

                if let wendyAnalytics = env["WENDY_ANALYTICS"] {
                    envInfo.append("  WENDY_ANALYTICS=\(wendyAnalytics)")
                }

                if env["CI"] != nil {
                    envInfo.append("  CI environment detected (analytics auto-disabled)")
                }

                if !envInfo.isEmpty {
                    print("\nEnvironment variables:")
                    for info in envInfo {
                        print(info)
                    }
                }

                // Show config file location
                let configPath = FileManager.default.homeDirectoryForCurrentUser
                    .appendingPathComponent(".wendy/config.json")
                    .path
                print("\nConfig file: \(configPath)")

                // Show anonymous ID if analytics is enabled
                if config.analytics.enabled {
                    print("Anonymous ID: \(config.analytics.anonymousId)")
                }
            }

            // Show help for changing settings
            if config.analytics.enabled {
                print("\nTo disable analytics: wendy analytics disable")
            } else if !ConsentManager.shouldDisableAnalytics() {
                print("\nTo enable analytics: wendy analytics enable")
            }
        }
    }
}
