import ArgumentParser
import Foundation
import Logging
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

        await WendyCommand.main()
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
                    CacheCommand.self,
                ]
            ),
        ]
    )
}
