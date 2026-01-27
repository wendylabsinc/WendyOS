import ArgumentParser
import Foundation
import Logging
import Noora

struct WiFiCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "wifi",
        abstract: "Manage WiFi connections.",
        subcommands: [
            ListNetworksCommand.self,
            ConnectCommand.self,
            StatusCommand.self,
            DisconnectCommand.self,
        ]
    )

    struct ListNetworksCommand: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            commandName: "list",
            abstract: "List available WiFi networks."
        )

        @OptionGroup var agentConnectionOptions: AgentConnectionOptions

        func run() async throws {
            let networks = try await withAgentClient(
                agentConnectionOptions,
                title: "For which device do you want to list wifi networks?"
            ) { client in
                if JSONMode.isEnabled {
                    return try await client.listWiFiNetworks()
                } else {
                    return try await Noora().progressStep(
                        message: "Listing available WiFi networks",
                        successMessage: nil,
                        errorMessage: nil,
                        showSpinner: true
                    ) { _ in
                        try await client.listWiFiNetworks()
                    }
                }
            }

            if networks.count == 0 {
                if JSONMode.isEnabled {
                    print("[]")
                } else {
                    Noora().info("No WiFi networks found.")
                }
            } else if JSONMode.isEnabled {
                let networksJSON = try formatNetworksAsJSON(networks)
                print(networksJSON)
            } else {
                Noora().info("Available WiFi networks:")
                formatNetworksAsText(networks)
            }
        }

        private func formatNetworksAsJSON(_ networks: [WiFiNetworkInfo]) throws -> String {
            struct NetworkInfo: Codable {
                let ssid: String
                let signalStrength: Int?
            }

            let networkInfos = networks.map { network in
                NetworkInfo(
                    ssid: network.ssid,
                    signalStrength: network.signalStrength
                )
            }

            let jsonData = try JSONEncoder().encode(networkInfos)
            return String(data: jsonData, encoding: .utf8)!
        }

        private func formatNetworksAsText(_ networks: [WiFiNetworkInfo]) {
            for (index, network) in networks.enumerated() {
                let signalInfo =
                    if let strength = network.signalStrength {
                        " (Signal: \(strength))"
                    } else {
                        ""
                    }
                print("\(index + 1). \(network.ssid)\(signalInfo)")
            }
        }
    }

    struct ConnectCommand: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            commandName: "connect",
            abstract: "Connect to a WiFi network."
        )

        @Option(help: "SSID (name) of the WiFi network to connect to")
        var ssid: String?

        @Option(name: .shortAndLong, help: "Password for the WiFi network")
        var password: String?

        @OptionGroup var agentConnectionOptions: AgentConnectionOptions

        func run() async throws {
            try await withAgentClient(
                agentConnectionOptions,
                title: "Which device do you want to connect to the wifi network on?"
            ) { client in
                let ssid: String
                let password: String

                if let _ssid = self.ssid {
                    ssid = _ssid
                } else if JSONMode.isEnabled {
                    jsonModeRequiresArgument(
                        argument: "ssid",
                        description: "Provide --ssid <network_name> to specify the WiFi network"
                    )
                } else {
                    ssid = try await discoverSSID(client: client)
                }

                if let _password = self.password {
                    password = _password
                } else if JSONMode.isEnabled {
                    password = ""
                } else {
                    password = try secureTextPrompt(
                        title: "Enter the password for '\(ssid)'",
                        prompt: "Password"
                    )
                }

                let logger = Logger(label: "sh.wendy.cli.wifi.connect")
                logger.debug("Connecting to WiFi network", metadata: ["ssid": "\(ssid)"])

                if JSONMode.isEnabled {
                    let result = try await client.connectToWiFi(
                        ssid: ssid,
                        password: password
                    )
                    struct Response: Codable {
                        let success: Bool
                        let errorMessage: String?
                    }

                    let responseJSON = try JSONEncoder().encode(
                        Response(
                            success: result.success,
                            errorMessage: result.errorMessage
                        )
                    )
                    print(String(data: responseJSON, encoding: .utf8)!)
                } else {
                    _ = try await Noora().progressStep(
                        message: "Connecting to WiFi network: \(ssid)...",
                        successMessage: "Connected to \(ssid)",
                        errorMessage: "Failed to connect to \(ssid)",
                        showSpinner: true
                    ) { _ in
                        let response = try await client.connectToWiFi(
                            ssid: ssid,
                            password: password
                        )
                        guard response.success else {
                            throw CLIError.connectionFailed(
                                device: "WiFi",
                                reason: response.errorMessage ?? "Unknown error"
                            )
                        }
                    }
                }
            }
        }

        /// Interactively discover and select a WiFi network
        private func discoverSSID(client: AgentClient) async throws -> String {
            let networks = try await client.listWiFiNetworks()

            // Group networks by SSID and keep the one with highest signal strength
            let uniqueNetworks = Dictionary(grouping: networks.filter { !$0.ssid.isEmpty }) {
                $0.ssid
            }
            .compactMapValues { networksWithSameSsid -> WiFiNetworkInfo? in
                networksWithSameSsid.max(by: {
                    ($0.signalStrength ?? 0) < ($1.signalStrength ?? 0)
                })
            }
            .values
            .sorted(by: { ($0.signalStrength ?? 0) > ($1.signalStrength ?? 0) })

            guard !uniqueNetworks.isEmpty else {
                throw WiFiCommandError.noNetworksFound
            }

            // Build table rows for selection
            let rows: [[String]] = uniqueNetworks.map { network in
                [
                    network.ssid,
                    network.signalStrength.map { "\($0)" } ?? "Unknown",
                ]
            }

            let index = try await cliOutput.selectFromTable(
                title: "Select a WiFi network",
                headers: ["SSID", "Strength"],
                rows: rows,
                pageSize: 15
            )

            return uniqueNetworks[index].ssid
        }
    }

    struct StatusCommand: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            commandName: "status",
            abstract: "Check the current WiFi connection status."
        )

        @OptionGroup var agentConnectionOptions: AgentConnectionOptions

        func run() async throws {
            let logger = Logger(label: "sh.wendy.cli.wifi.status")
            logger.info("Checking WiFi connection status")

            try await withAgentClient(
                agentConnectionOptions,
                title: "For which device do you want to check the wifi status?"
            ) { client in
                let status = try await client.getWiFiStatus()

                if JSONMode.isEnabled {
                    let statusJSON = try formatStatusAsJSON(status)
                    print(statusJSON)
                } else {
                    formatStatusAsText(status)
                }
            }
        }

        private func formatStatusAsJSON(_ status: WiFiStatusInfo) throws -> String {
            struct StatusInfo: Codable {
                let connected: Bool
                let ssid: String?
                let errorMessage: String?
            }

            let statusInfo = StatusInfo(
                connected: status.connected,
                ssid: status.ssid,
                errorMessage: status.errorMessage
            )

            let jsonData = try JSONEncoder().encode(statusInfo)
            return String(data: jsonData, encoding: .utf8)!
        }

        private func formatStatusAsText(_ status: WiFiStatusInfo) {
            print("WiFi Status:")
            print("------------")

            if status.connected {
                print("Status: Connected")
                if let ssid = status.ssid {
                    print("Network: \(ssid)")
                }
            } else {
                print("Status: Disconnected")
            }

            if let errorMessage = status.errorMessage {
                print("Error: \(errorMessage)")
            }
        }
    }

    struct DisconnectCommand: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            commandName: "disconnect",
            abstract: "Disconnect from the current WiFi network."
        )

        @OptionGroup var agentConnectionOptions: AgentConnectionOptions

        func run() async throws {
            let logger = Logger(label: "sh.wendy.cli.wifi.disconnect")
            logger.info("Disconnecting from WiFi network")

            try await withAgentClient(
                agentConnectionOptions,
                title: "Which device do you want to disconnect from wifi?"
            ) { client in
                let result = try await Noora().progressStep(
                    message: "Disconnecting from WiFi network...",
                    successMessage: "Disconnected from WiFi network",
                    errorMessage: "Failed to disconnect from WiFi network",
                    showSpinner: true
                ) { _ in
                    try await client.disconnectWiFi()
                }

                if !result.success {
                    let errorMessage = result.errorMessage ?? "Unknown error"
                    Noora().warning("Disconnect reported failure: \(errorMessage)")
                }
            }
        }
    }
}

enum WiFiCommandError: Error, CustomStringConvertible {
    case noNetworksFound
    case selectionFailed

    var description: String {
        switch self {
        case .noNetworksFound:
            return "No WiFi networks found"
        case .selectionFailed:
            return "Failed to select a network"
        }
    }
}
