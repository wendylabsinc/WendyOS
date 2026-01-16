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
            Remove.self,
        ]
    )

    enum AppsCommandError: Error, LocalizedError {
        case operationFailed(String)

        var errorDescription: String? {
            switch self {
            case .operationFailed(let message):
                return message
            }
        }
    }

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
            try await withAgentConnection(
                agentConnectionOptions,
                title: "Removing application",
                grpcOperation: { client in
                    let containers = Wendy_Agent_Services_V1_WendyContainerService.Client(
                        wrapping: client
                    )

                    _ = try await containers.deleteContainer(
                        .with {
                            $0.appName = appName
                            $0.deleteImage = purgeImage
                        }
                    )

                    if purgeImage {
                        Noora().success("Removed application and its image.")
                    } else {
                        Noora().success("Removed application.")
                    }
                },
                bluetoothOperation: { deviceIdentifier in
                    #if canImport(Bluetooth)
                        let response = try await executeBluetoothCommand(
                            .appsRemove(appName: appName, purgeImage: purgeImage),
                            deviceIdentifier: deviceIdentifier
                        )
                        if case .appsRemove(let success, let errorMessage) = response {
                            if success {
                                if purgeImage {
                                    Noora().success("Removed application and its image.")
                                } else {
                                    Noora().success("Removed application.")
                                }
                            } else {
                                let message = errorMessage ?? "Unknown error"
                                Noora().error("Failed to remove application: \(message)")
                            }
                        } else if case .error(let message) = response {
                            Noora().error("Error: \(message)")
                        }
                    #else
                        throw BluetoothNotAvailableError()
                    #endif
                }
            )
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
            try await withAgentConnection(
                agentConnectionOptions,
                title: "Stopping application",
                grpcOperation: { client in
                    let containers = Wendy_Agent_Services_V1_WendyContainerService.Client(
                        wrapping: client
                    )
                    _ = try await containers.stopContainer(
                        .with { $0.appName = appName }
                    )
                    Noora().info("Stop request sent")
                },
                bluetoothOperation: { deviceIdentifier in
                    #if canImport(Bluetooth)
                        let response = try await executeBluetoothCommand(
                            .appsStop(appName: appName),
                            deviceIdentifier: deviceIdentifier
                        )
                        if case .appsStop(let success, let errorMessage) = response {
                            if success {
                                Noora().success("Application stopped")
                            } else {
                                let message = errorMessage ?? "Unknown error"
                                Noora().error("Failed to stop application: \(message)")
                            }
                        } else if case .error(let message) = response {
                            Noora().error("Error: \(message)")
                        }
                    #else
                        throw BluetoothNotAvailableError()
                    #endif
                }
            )
        }
    }

    struct ListCommand: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            commandName: "list",
            abstract: "List applications on the device"
        )

        @OptionGroup var agentConnectionOptions: AgentConnectionOptions

        func run() async throws {
            let apps: [AppInfo] = try await withAgentConnection(
                agentConnectionOptions,
                title: "Listing applications",
                grpcOperation: { client in
                    try await Wendy_Agent_Services_V1_WendyContainerService.Client(
                        wrapping: client
                    )
                    .listContainers(.init()) { containers in
                        var apps: [AppInfo] = []

                        for try await container in containers.messages {
                            let state =
                                switch container.container.runningState {
                                case .running: "Running"
                                case .stopped: "Stopped"
                                case .UNRECOGNIZED: "Unknown"
                                }

                            apps.append(
                                AppInfo(
                                    appName: container.container.appName,
                                    appVersion: container.container.appVersion,
                                    state: state,
                                    failureCount: Int(container.container.failureCount)
                                )
                            )
                        }

                        return apps
                    }
                },
                bluetoothOperation: { deviceIdentifier in
                    #if canImport(Bluetooth)
                        let response = try await executeBluetoothCommand(
                            .appsList,
                            deviceIdentifier: deviceIdentifier
                        )
                        if case .appsList(let apps) = response {
                            return apps
                        } else if case .error(let message) = response {
                            throw AppsCommandError.operationFailed(message)
                        }
                        return []
                    #else
                        throw BluetoothNotAvailableError()
                    #endif
                }
            )

            guard !apps.isEmpty else {
                Noora().info("No applications found.")
                return
            }

            Noora().table(
                headers: [
                    "App",
                    "Version",
                    "State",
                    "Failures",
                ],
                rows: apps.map { app in
                    [app.appName, app.appVersion, app.state, "\(app.failureCount)"]
                }
            )
        }
    }

}
