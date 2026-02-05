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

    struct Remove: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            commandName: "remove",
            abstract: "Stop and remove an application from the device"
        )

        @Argument(help: "Application name used when the app was created")
        var appName: String?

        @Flag(
            name: .customLong("all"),
            help: "Remove all applications from the device"
        )
        var all: Bool = false

        @Flag(
            name: .customLong("purge-image"),
            help: "Also delete the application image to free disk space"
        )
        var purgeImage: Bool = false

        @OptionGroup var agentConnectionOptions: AgentConnectionOptions

        func validate() throws {
            if !all && appName == nil {
                throw ValidationError("Please provide an app name or use --all")
            }
            if all && appName != nil {
                throw ValidationError("Cannot specify both app name and --all")
            }
        }

        func run() async throws {
            try await withAgentClientAndHostname(
                agentConnectionOptions,
                title: "Removing application"
            ) { client, hostname in
                let appsToRemove: [String]
                if all {
                    let apps = try await client.listApps()
                    appsToRemove = apps.map(\.name)
                    guard !appsToRemove.isEmpty else {
                        cliOutput.info("No applications to remove on \(hostname)")
                        return
                    }
                } else {
                    appsToRemove = [appName!]
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

        @Argument(help: "Application name used when the app was created")
        var appName: String?

        @Flag(
            name: .customLong("all"),
            help: "Start all applications on the device"
        )
        var all: Bool = false

        @OptionGroup var agentConnectionOptions: AgentConnectionOptions

        func validate() throws {
            if !all && appName == nil {
                throw ValidationError("Please provide an app name or use --all")
            }
            if all && appName != nil {
                throw ValidationError("Cannot specify both app name and --all")
            }
        }

        func run() async throws {
            try await withAgentClientAndHostname(
                agentConnectionOptions,
                title: "Starting application"
            ) { client, hostname in
                let appsToStart: [String]
                if all {
                    let apps = try await client.listApps()
                    appsToStart = apps.map(\.name)
                    guard !appsToStart.isEmpty else {
                        cliOutput.info("No applications to start on \(hostname)")
                        return
                    }
                } else {
                    appsToStart = [appName!]
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

        @Argument(help: "Application name used when the app was created")
        var appName: String?

        @Flag(
            name: .customLong("all"),
            help: "Stop all running applications on the device"
        )
        var all: Bool = false

        @OptionGroup var agentConnectionOptions: AgentConnectionOptions

        func validate() throws {
            if !all && appName == nil {
                throw ValidationError("Please provide an app name or use --all")
            }
            if all && appName != nil {
                throw ValidationError("Cannot specify both app name and --all")
            }
        }

        func run() async throws {
            try await withAgentClientAndHostname(
                agentConnectionOptions,
                title: "Stopping application"
            ) { client, hostname in
                let appsToStop: [String]
                if all {
                    let apps = try await client.listApps()
                    appsToStop = apps.map(\.name)
                    guard !appsToStop.isEmpty else {
                        cliOutput.info("No applications to stop on \(hostname)")
                        return
                    }
                } else {
                    appsToStop = [appName!]
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
