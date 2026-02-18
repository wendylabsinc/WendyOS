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
            if all && appName != nil {
                if JSONMode.isEnabled {
                    JSONErrorResponse(
                        error: "invalid_arguments",
                        reason: "Cannot specify both an application name and --all in --json mode",
                        suggestion: "Provide either an app name argument or --all, but not both"
                    ).print()
                    _Exit(1)
                }

                throw ValidationError(
                    "Cannot specify both an application name and --all. Please choose exactly one."
                )
            }

            if all {
                return try await client.listApps().map(\.name)
            } else if let appName {
                return [appName]
            } else {
                if JSONMode.isEnabled {
                    jsonModeRequiresArgument(
                        argument: "app-name",
                        description: "Provide an app name argument or pass --all"
                    )
                }

                throw ValidationError("Specify an application name or --all.")
            }
        }
    }

    private static func runBulkOperation(
        appNames: [String],
        hostname: String,
        operationVerb: String,
        perform: (String) async throws -> Void,
        successMessage: (String) -> String
    ) async throws {
        var failedAppNames: [String] = []

        for name in appNames {
            do {
                try await perform(name)
                cliOutput.success(successMessage(name))
            } catch {
                failedAppNames.append(name)
                cliOutput.error(
                    "Failed to \(operationVerb) app \(name) on \(hostname): \(error.localizedDescription)"
                )
            }
        }

        guard failedAppNames.isEmpty else {
            let failedNames = failedAppNames.joined(separator: ", ")
            throw ValidationError(
                "Failed to \(operationVerb) \(failedAppNames.count) of \(appNames.count) applications on \(hostname): \(failedNames)."
            )
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

        @Flag(name: .long, help: "Skip confirmation prompts")
        var force: Bool = false

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

                if apps.all && !force && !JSONMode.isEnabled {
                    let confirmed = Noora().yesOrNoChoicePrompt(
                        question: "Remove all applications on \(hostname)?",
                        defaultAnswer: false
                    )
                    guard confirmed else {
                        cliOutput.info("Aborted.")
                        return
                    }
                }

                if apps.all {
                    try await AppsCommand.runBulkOperation(
                        appNames: appsToRemove,
                        hostname: hostname,
                        operationVerb: "remove",
                        perform: { name in
                            try await client.removeApp(name: name, purgeImage: purgeImage)
                        },
                        successMessage: { name in
                            if purgeImage {
                                return "Removed app \(name) and its image on \(hostname)"
                            }
                            return "Removed app \(name) on \(hostname)"
                        }
                    )
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

                if apps.all {
                    try await AppsCommand.runBulkOperation(
                        appNames: appsToStart,
                        hostname: hostname,
                        operationVerb: "start",
                        perform: { name in
                            try await client.startApp(name: name)
                        },
                        successMessage: { name in
                            "Started app \(name) on \(hostname)"
                        }
                    )
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

                if apps.all {
                    try await AppsCommand.runBulkOperation(
                        appNames: appsToStop,
                        hostname: hostname,
                        operationVerb: "stop",
                        perform: { name in
                            try await client.stopApp(name: name)
                        },
                        successMessage: { name in
                            "Stopped app \(name) on \(hostname)"
                        }
                    )
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
