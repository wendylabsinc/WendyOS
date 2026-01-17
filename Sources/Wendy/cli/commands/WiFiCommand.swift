import ArgumentParser
import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import Logging
import Noora
import WendyAgentGRPC

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

        @Flag(name: [.customShort("j"), .long], help: "Output in JSON format")
        var json: Bool = false

        @OptionGroup var agentConnectionOptions: AgentConnectionOptions

        func run() async throws {
            let networks = try await withAgentGRPCClient(
                agentConnectionOptions,
                title: "For which device do you want to list wifi networks?"
            ) { client in
                let agent = Wendy_Agent_Services_V1_WendyAgentService.Client(wrapping: client)
                let request = Wendy_Agent_Services_V1_ListWiFiNetworksRequest()

                if json {
                    return try await agent.listWiFiNetworks(request).networks
                } else {
                    return try await Noora().progressStep(
                        message: "Listing available WiFi networks",
                        successMessage: nil,
                        errorMessage: nil,
                        showSpinner: true
                    ) { progress in
                        try await agent.listWiFiNetworks(request)
                    }.networks
                }
            }

            if networks.count == 0 {
                Noora().info("No WiFi networks found.")
            } else if json {
                let networksJSON = try formatNetworksAsJSON(networks)
                print(networksJSON)
            } else {
                Noora().info("Available WiFi networks:")
                formatNetworksAsText(networks)
            }
        }

        private func formatNetworksAsJSON(
            _ networks: [Wendy_Agent_Services_V1_ListWiFiNetworksResponse.WiFiNetwork]
        ) throws -> String {
            struct NetworkInfo: Codable {
                let ssid: String
                let signalStrength: Int?
            }

            let networkInfos = networks.map { network in
                NetworkInfo(
                    ssid: network.ssid,
                    signalStrength: network.hasSignalStrength ? Int(network.signalStrength) : nil
                )
            }

            let jsonData = try JSONEncoder().encode(networkInfos)
            return String(data: jsonData, encoding: .utf8)!
        }

        private func formatNetworksAsText(
            _ networks: [Wendy_Agent_Services_V1_ListWiFiNetworksResponse.WiFiNetwork]
        ) {
            for (index, network) in networks.enumerated() {
                let signalInfo =
                    network.hasSignalStrength ? " (Signal: \(network.signalStrength))" : ""
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

        @Flag(name: [.customShort("j"), .long], help: "Output in JSON format")
        var json: Bool = false

        @OptionGroup var agentConnectionOptions: AgentConnectionOptions

        func run() async throws {
            try await withAgentGRPCClient(
                agentConnectionOptions,
                title: "Which device do you want to connect to the wifi network on?"
            ) { client in
                let agent = Agent(client: client)
                let ssid: String
                let password: String

                if let _ssid = self.ssid {
                    ssid = _ssid
                } else {
                    ssid = try await agent.discoverSSID()
                }

                if let _password = self.password {
                    password = _password
                } else if json {
                    password = ""
                } else {
                    password = try secureTextPrompt(
                        title: "Enter the password for '\(ssid)'",
                        prompt: "Password"
                    )
                }

                let logger = Logger(label: "sh.wendy.cli.wifi.connect")
                logger.debug("Connecting to WiFi network", metadata: ["ssid": "\(ssid)"])

                if json {
                    let response = try await agent.connectToWiFi(
                        ssid: ssid,
                        password: password
                    )
                    struct Response: Codable {
                        let success: Bool
                        let errorMessage: String?
                    }

                    let responseJSON = try JSONEncoder().encode(
                        Response(
                            success: response.success,
                            errorMessage: response.hasErrorMessage ? response.errorMessage : nil
                        )
                    )
                    print(String(data: responseJSON, encoding: .utf8)!)
                } else {
                    _ = try await Noora().progressStep(
                        message: "Connecting to WiFi network: \(ssid)...",
                        successMessage: "Connected to \(ssid)",
                        errorMessage: "Failed to connect to \(ssid)",
                        showSpinner: true
                    ) { progress in
                        let response = try await agent.connectToWiFi(
                            ssid: ssid,
                            password: password
                        )
                        guard response.success else {
                            struct UnableToConnectToWiFiError: Error {
                                let errorMessage: String
                            }

                            throw UnableToConnectToWiFiError(errorMessage: response.errorMessage)
                        }
                    }
                }
            }
        }
    }

    struct StatusCommand: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            commandName: "status",
            abstract: "Check the current WiFi connection status."
        )

        @Flag(name: [.customShort("j"), .long], help: "Output in JSON format")
        var json: Bool = false

        @OptionGroup var agentConnectionOptions: AgentConnectionOptions

        func run() async throws {
            let logger = Logger(label: "sh.wendy.cli.wifi.status")
            logger.info("Checking WiFi connection status")

            try await withAgentGRPCClient(
                agentConnectionOptions,
                title: "For which device do you want to check the wifi status?"
            ) { client in
                let agent = Wendy_Agent_Services_V1_WendyAgentService.Client(wrapping: client)
                let request = Wendy_Agent_Services_V1_GetWiFiStatusRequest()
                let response = try await agent.getWiFiStatus(request)

                if json {
                    let statusJSON = try formatStatusAsJSON(response)
                    print(statusJSON)
                } else {
                    formatStatusAsText(response)
                }
            }
        }

        private func formatStatusAsJSON(
            _ response: Wendy_Agent_Services_V1_GetWiFiStatusResponse
        ) throws -> String {
            struct StatusInfo: Codable {
                let connected: Bool
                let ssid: String?
                let errorMessage: String?
            }

            let statusInfo = StatusInfo(
                connected: response.connected,
                ssid: response.hasSsid ? response.ssid : nil,
                errorMessage: response.hasErrorMessage ? response.errorMessage : nil
            )

            let jsonData = try JSONEncoder().encode(statusInfo)
            return String(data: jsonData, encoding: .utf8)!
        }

        private func formatStatusAsText(_ response: Wendy_Agent_Services_V1_GetWiFiStatusResponse) {
            print("WiFi Status:")
            print("------------")

            if response.connected {
                print("Status: Connected")
                if response.hasSsid {
                    print("Network: \(response.ssid)")
                }
            } else {
                print("Status: Disconnected")
            }

            if response.hasErrorMessage {
                print("Error: \(response.errorMessage)")
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

            try await withAgentGRPCClient(
                agentConnectionOptions,
                title: "Which device do you want to disconnect from wifi?"
            ) { client in
                let agent = Wendy_Agent_Services_V1_WendyAgentService.Client(wrapping: client)
                let request = Wendy_Agent_Services_V1_DisconnectWiFiRequest()

                print("Disconnecting from WiFi network...")

                let response = try await agent.disconnectWiFi(request)

                if response.success {
                    print("✅ Successfully disconnected from WiFi network")
                } else {
                    let errorMessage =
                        response.hasErrorMessage ? response.errorMessage : "Unknown error"
                    print("❌ Failed to disconnect: \(errorMessage)")
                }
            }
        }
    }
}
