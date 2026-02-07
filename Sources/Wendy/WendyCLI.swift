import Analytics
import ArgumentParser
import CLIOutput
import Foundation
import Logging
import WendyShared

#if canImport(Darwin)
    import Darwin
#elseif canImport(Glibc)
    @preconcurrency import Glibc
#elseif canImport(Musl)
    @preconcurrency import Musl
#endif

@main
struct WendyCLI {
    static func main() async throws {
        LoggingSystem.bootstrap { label in
            let level =
                ProcessInfo.processInfo.environment["LOG_LEVEL"]
                .flatMap(Logger.Level.init(rawValue:)) ?? .info

            var logger = StreamLogHandler.standardError(label: label)
            logger.logLevel = level
            return logger
        }

        // Check for global --json flag in arguments or non-interactive shell
        let jsonMode =
            ProcessInfo.processInfo.arguments.contains("--json")
            || ProcessInfo.processInfo.arguments.contains("-j")
            || isatty(STDOUT_FILENO) == 0

        // Check for CLI updates (runs once per day, non-blocking)
        await UpdateChecker.checkForUpdatesIfNeeded()

        // Track command execution with analytics
        if let analytics = try? AnalyticsService(config: getConfig().analytics) {
            await analytics.trackCommandExecution {
                await withJSONMode(enabled: jsonMode) {
                    await WendyCommand.main()
                }
            }
            // Ensure all events are sent before exiting
            await analytics.flush()
        } else {
            // Analytics not available, run normally
            await withJSONMode(enabled: jsonMode) {
                await WendyCommand.main()
            }
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
            BuildCommand.self,
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
                    UpdateCommand.self,
                    InfoCommand.self,
                ]
            ),
        ]
    )

    @Flag(
        name: [.customShort("j"), .long],
        help: "Output in JSON format. Disables interactive prompts."
    )
    var json: Bool = false
}
