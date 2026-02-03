import ArgumentParser
import Foundation
import Logging
import Noora

struct AppsCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "apps",
        abstract: "Manage applications on the device",
        subcommands: [
            ListCommand.self,
            Stop.self,
            Remove.self,
        ]
    )

    struct Remove: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            commandName: "remove",
            abstract: "Stop and remove an application from the device"
        )

        @Argument(help: "Application name used when the app was created")
        var appName: String

        @Flag(
            name: .customLong("purge-image"),
            help: "Also delete the application image to free disk space"
        )
        var purgeImage: Bool = false

        @OptionGroup var agentConnectionOptions: AgentConnectionOptions

        func run() async throws {
            try await withAgentClientAndHostname(
                agentConnectionOptions,
                title: "Removing application"
            ) { client, hostname in
                try await client.removeApp(name: appName, purgeImage: purgeImage)

                if purgeImage {
                    Noora().success("Removed app \(appName) and its image on \(hostname)")
                } else {
                    Noora().success("Removed app \(appName) on \(hostname)")
                }
            }
        }
    }

    struct Stop: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            commandName: "stop",
            abstract: "Stop a running application"
        )

        @Argument(help: "Application name used when the app was created")
        var appName: String

        @OptionGroup var agentConnectionOptions: AgentConnectionOptions

        func run() async throws {
            try await withAgentClientAndHostname(
                agentConnectionOptions,
                title: "Stopping application"
            ) { client, hostname in
                try await client.stopApp(name: appName)
                Noora().success("Stopped app \(appName) on \(hostname)")
            }
        }
    }

    struct ListCommand: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            commandName: "list",
            abstract: "List applications on the device"
        )

        @OptionGroup var agentConnectionOptions: AgentConnectionOptions

        func run() async throws {
            try await withAgentClient(
                agentConnectionOptions,
                title: "Listing applications"
            ) { client in
                let apps = try await client.listApps()

                guard !apps.isEmpty else {
                    Noora().info("No applications found.")
                    return
                }

                let rows: [[String]] = apps.map { app in
                    [
                        app.name,
                        app.version,
                        app.runningState.rawValue,
                        "\(app.failureCount)",
                    ]
                }

                Noora().table(
                    headers: [
                        "App",
                        "Version",
                        "State",
                        "Failures",
                    ],
                    rows: rows
                )
            }
        }
    }

}
