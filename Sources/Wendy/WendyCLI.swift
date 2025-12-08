import Analytics
import ArgumentParser
import Foundation
import Logging
import ServiceLifecycle
import WendyShared

@main
struct WendyCLI {
    static func main() async throws {
        LoggingSystem.bootstrap { label in
            let level =
                ProcessInfo.processInfo.environment["LOG_LEVEL"]
                .flatMap(Logger.Level.init) ?? .info

            var logger = StreamLogHandler.standardError(label: label)
            logger.logLevel = level
            return logger
        }

        // Initialize analytics service
        let analytics = AnalyticsService.shared

        // Install signal handlers at top level to handle Ctrl+C gracefully
        try await withGracefulShutdownHandler {
            // Track command execution with analytics
            if let analytics = analytics {
                await analytics.trackCommandExecution {
                    await WendyCommand.main()
                }
                // Ensure all events are sent before exiting
                await analytics.flush()
            } else {
                // Analytics not available, run normally
                await WendyCommand.main()
            }
        } onGracefulShutdown: {
            print("\nReceived shutdown signal, cleaning up...")
        }
    }
}

struct WendyCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "wendy",
        abstract: "Wendy CLI",
        version: Version.current,
        subcommands: [
            RunCommand.self,
            InitCommand.self,
            ProjectCommand.self,
        ],
        groupedSubcommands: [
            CommandGroup(
                name: "Manage your cloud",
                subcommands: [
                    AuthCommand.self
                ]
            ),
            CommandGroup(
                name: "Manage your devices",
                subcommands: [
                    DeviceCommand.self,
                    DiscoverCommand.self,
                    OSCommand.self,
                ]
            ),
            CommandGroup(
                name: "Misc.",
                subcommands: [
                    HelperCommand.self,
                    AnalyticsCommand.self,
                    CacheCommand.self,
                ]
            ),
        ]
    )
}
