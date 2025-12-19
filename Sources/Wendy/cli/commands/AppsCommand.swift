import ArgumentParser
import Foundation
import Logging
import Noora
import WendyAgentGRPC
import WendyShared

struct AppsCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "apps",
        abstract: "Manage applications on the device",
        subcommands: [
            ListCommand.self,
            Stop.self,
        ]
    )

    struct Stop: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            commandName: "stop",
            abstract: "Stop a running application"
        )

        @Argument(help: "Application name used when the app was created")
        var appName: String

        @OptionGroup var agentConnectionOptions: AgentConnectionOptions

        func run() async throws {
            try await withAgentGRPCClient(
                agentConnectionOptions,
                title: "Stopping application"
            ) { client in
                let containers = Wendy_Agent_Services_V1_WendyContainerService.Client(
                    wrapping: client
                )
                _ = try await containers.stopContainer(
                    .with { $0.appName = appName }
                )
                Noora().info("Stop request sent")
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
            try await withAgentGRPCClient(
                agentConnectionOptions,
                title: "Listing applications"
            ) { client in
                let rows: [[String]] =
                    try await Wendy_Agent_Services_V1_WendyContainerService.Client(
                        wrapping: client
                    )
                    .listContainers(.init()) { containers in
                        var rows: [[String]] = []

                        for try await container in containers.messages {
                            let status =
                                switch container.container.runningState {
                                case .running: "✅"
                                case .stopped: "🛑"
                                case .UNRECOGNIZED: "❓"
                                }

                            let state =
                                switch container.container.runningState {
                                case .running: "Running"
                                case .stopped: "Stopped"
                                case .UNRECOGNIZED: "Unknown"
                                }
                            let failures = "\(container.container.failureCount)"

                            rows.append([
                                status,
                                container.container.appName,
                                container.container.appVersion,
                                state,
                                failures,
                            ])
                        }

                        return rows
                    }

                guard !rows.isEmpty else {
                    Noora().info("No applications found.")
                    return
                }

                Noora().table(
                    headers: [
                        "",
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
