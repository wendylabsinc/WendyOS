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
            Start.self,
            Stop.self,
            Remove.self,
        ]
    )

    struct Apps: ParsableArguments {
        @Argument(help: "Name of the application to select")
        var appName: String?

        @Flag(
            name: .customLong("all"),
            help: "Select all applications for the operation"
        )
        var all: Bool = false

        func resolve(client: AgentClient) async throws -> [String] {
            if all {
                return try await client.listApps().map(\.name)
            } else if let appName {
                return [appName]
            } else {
                return []
            }
        }
    }

    struct Remove: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            commandName: "remove",
            abstract: "Stop and remove an application from the device"
        )

        @Flag(
            name: .customLong("purge-image"),
            help: "Also delete the application image to free disk space"
        )
        var purgeImage: Bool = false

        @OptionGroup var agentConnectionOptions: AgentConnectionOptions

        @OptionGroup var apps: Apps

        func run() async throws {
            try await withAgentClientAndHostname(
                agentConnectionOptions,
                title: "Removing application"
            ) { client, hostname in
                let appsToRemove = try await apps.resolve(client: client)
                guard !appsToRemove.isEmpty else {
                    cliOutput.info("No applications to remove on \(hostname)")
                    return
                }

                for name in appsToRemove {
                    try await client.removeApp(name: name, purgeImage: purgeImage)
                    if purgeImage {
                        cliOutput.success("Removed app \(name) and its image on \(hostname)")
                    } else {
                        cliOutput.success("Removed app \(name) on \(hostname)")
                    }
                }
            }
        }
    }

    struct Start: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            commandName: "start",
            abstract: "Start an application"
        )

        @OptionGroup var agentConnectionOptions: AgentConnectionOptions
        @OptionGroup var apps: Apps

        func run() async throws {
            try await withAgentClientAndHostname(
                agentConnectionOptions,
                title: "Starting application"
            ) { client, hostname in
                let appsToStart = try await apps.resolve(client: client)
                guard !appsToStart.isEmpty else {
                    cliOutput.info("No applications to start on \(hostname)")
                    return
                }

                for name in appsToStart {
                    try await client.startApp(name: name)
                    cliOutput.success("Started app \(name) on \(hostname)")
                }
            }
        }
    }

    struct Stop: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            commandName: "stop",
            abstract: "Stop a running application"
        )

        @OptionGroup var agentConnectionOptions: AgentConnectionOptions
        @OptionGroup var apps: Apps

        func run() async throws {
            try await withAgentClientAndHostname(
                agentConnectionOptions,
                title: "Stopping application"
            ) { client, hostname in
                let appsToStop = try await apps.resolve(client: client)
                guard !appsToStop.isEmpty else {
                    cliOutput.info("No applications to stop on \(hostname)")
                    return
                }

                for name in appsToStop {
                    try await client.stopApp(name: name)
                    cliOutput.success("Stopped app \(name) on \(hostname)")
                }
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

                if JSONMode.isEnabled {
                    cliOutput.result(apps)
                    return
                }

                guard !apps.isEmpty else {
                    cliOutput.info("No applications found.")
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

                cliOutput.table(
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
