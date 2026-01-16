import ArgumentParser
import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import Logging
import Noora
import WendyAgentGRPC
import WendyShared

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
            let networks: [WiFiNetworkInfo] = try await withAgentConnection(
                agentConnectionOptions,
                title: "For which device do you want to list wifi networks?",
                grpcOperation: { client in
                    let agent = Wendy_Agent_Services_V1_WendyAgentService.Client(wrapping: client)
                    let request = Wendy_Agent_Services_V1_ListWiFiNetworksRequest()

                    let grpcNetworks: [Wendy_Agent_Services_V1_ListWiFiNetworksResponse.WiFiNetwork]
                    if JSONMode.isEnabled {
                        grpcNetworks = try await agent.listWiFiNetworks(request).networks
                    } else {
                        grpcNetworks = try await Noora().progressStep(
                            message: "Listing available WiFi networks",
                            successMessage: nil,
                            errorMessage: nil,
                            showSpinner: true
                        ) { progress in
                            try await agent.listWiFiNetworks(request)
                        }.networks
                    }

                    return grpcNetworks.map { network in
                        WiFiNetworkInfo(
                            ssid: network.ssid,
                            signalStrength: network.hasSignalStrength ? Int(network.signalStrength) : nil
                        )
                    }
                },
                bluetoothOperation: { deviceIdentifier in
                    #if canImport(Bluetooth)
                    let response = try await executeBluetoothCommand(.wifiList, deviceIdentifier: deviceIdentifier)
                    if case .wifiList(let networks) = response {
                        return networks
                    } else if case .error(let message) = response {
                        throw WiFiCommandError.operationFailed(message)
                    }
                    return []
                    #else
                    throw BluetoothNotAvailableError()
                    #endif
                }
            )

            if networks.isEmpty {
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
            let jsonData = try JSONEncoder().encode(networks)
            return String(data: jsonData, encoding: .utf8)!
        }

        private func formatNetworksAsText(_ networks: [WiFiNetworkInfo]) {
            for (index, network) in networks.enumerated() {
                let signalInfo = network.signalStrength.map { " (Signal: \($0))" } ?? ""
                print("\(index + 1). \(network.ssid)\(signalInfo)")
            }
        }
    }

    enum WiFiCommandError: Error, LocalizedError {
        case operationFailed(String)

        var errorDescription: String? {
            switch self {
            case .operationFailed(let message):
                return message
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
            // Determine SSID - need to handle differently for Bluetooth vs gRPC
            let ssid: String
            if let providedSsid = self.ssid {
                ssid = providedSsid
            } else if JSONMode.isEnabled {
                jsonModeRequiresArgument(
                    argument: "ssid",
                    description: "Provide --ssid <network_name> to specify the WiFi network"
                )
            } else if agentConnectionOptions.bluetooth != nil {
                // For Bluetooth, we need to prompt for SSID since we can't use the Agent helper
                ssid = Noora().textPrompt(
                    title: "Enter the WiFi network name",
                    prompt: "SSID"
                )
            } else {
                // For gRPC, we'll discover the SSID inside the connection
                ssid = ""
            }

            let password: String
            if let providedPassword = self.password {
                password = providedPassword
            } else if JSONMode.isEnabled {
                password = ""
            } else {
                password = Noora().textPrompt(
                    title: "Enter the password for the WiFi network",
                    prompt: "Password"
                )
            }

            let logger = Logger(label: "sh.wendy.cli.wifi.connect")

            try await withAgentConnection(
                agentConnectionOptions,
                title: "Which device do you want to connect to the wifi network on?",
                grpcOperation: { client in
                    let agent = Agent(client: client)
                    let finalSsid: String

                    if ssid.isEmpty {
                        finalSsid = try await agent.discoverSSID()
                    } else {
                        finalSsid = ssid
                    }

                    logger.debug("Connecting to WiFi network", metadata: ["ssid": "\(finalSsid)"])

                    if JSONMode.isEnabled {
                        let response = try await agent.connectToWiFi(
                            ssid: finalSsid,
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
                            message: "Connecting to WiFi network: \(finalSsid)...",
                            successMessage: "Connected to \(finalSsid)",
                            errorMessage: "Failed to connect to \(finalSsid)",
                            showSpinner: true
                        ) { progress in
                            let response = try await agent.connectToWiFi(
                                ssid: finalSsid,
                                password: password
                            )
                            guard response.success else {
                                throw WiFiCommandError.operationFailed(response.errorMessage)
                            }
                        }
                    }
                },
                bluetoothOperation: { deviceIdentifier in
                    #if canImport(Bluetooth)
                    logger.debug("Connecting to WiFi network via Bluetooth", metadata: ["ssid": "\(ssid)"])

                    let response = try await Noora().progressStep(
                        message: "Connecting to WiFi network: \(ssid)...",
                        successMessage: "Connected to \(ssid)",
                        errorMessage: "Failed to connect to \(ssid)",
                        showSpinner: true
                    ) { _ in
                        try await executeBluetoothCommand(
                            .wifiConnect(ssid: ssid, password: password),
                            deviceIdentifier: deviceIdentifier
                        )
                    }

                    if case .wifiConnect(let success, let errorMessage) = response {
                        if JSONMode.isEnabled {
                            struct Response: Codable {
                                let success: Bool
                                let errorMessage: String?
                            }
                            let responseJSON = try JSONEncoder().encode(
                                Response(success: success, errorMessage: errorMessage)
                            )
                            print(String(data: responseJSON, encoding: .utf8)!)
                        } else if !success {
                            let message = errorMessage ?? "Unknown error"
                            Noora().error("Failed to connect: \(message)")
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

    struct StatusCommand: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            commandName: "status",
            abstract: "Check the current WiFi connection status."
        )

        @OptionGroup var agentConnectionOptions: AgentConnectionOptions

        struct WiFiStatus {
            let connected: Bool
            let ssid: String?
            let errorMessage: String?
        }

        func run() async throws {
            let logger = Logger(label: "sh.wendy.cli.wifi.status")
            logger.debug("Checking WiFi connection status")

            let status: WiFiStatus = try await withAgentConnection(
                agentConnectionOptions,
                title: "For which device do you want to check the wifi status?",
                grpcOperation: { client in
                    let agent = Wendy_Agent_Services_V1_WendyAgentService.Client(wrapping: client)
                    let request = Wendy_Agent_Services_V1_GetWiFiStatusRequest()
                    let response = try await agent.getWiFiStatus(request)

                    return WiFiStatus(
                        connected: response.connected,
                        ssid: response.hasSsid ? response.ssid : nil,
                        errorMessage: response.hasErrorMessage ? response.errorMessage : nil
                    )
                },
                bluetoothOperation: { deviceIdentifier in
                    #if canImport(Bluetooth)
                    let response = try await executeBluetoothCommand(.wifiStatus, deviceIdentifier: deviceIdentifier)
                    if case .wifiStatus(let connected, let ssid, let errorMessage) = response {
                        return WiFiStatus(connected: connected, ssid: ssid, errorMessage: errorMessage)
                    } else if case .error(let message) = response {
                        throw WiFiCommandError.operationFailed(message)
                    }
                    return WiFiStatus(connected: false, ssid: nil, errorMessage: "Unknown response")
                    #else
                    throw BluetoothNotAvailableError()
                    #endif
                }
            )

            if JSONMode.isEnabled {
                let statusJSON = try formatStatusAsJSON(status)
                print(statusJSON)
            } else {
                formatStatusAsText(status)
            }
        }

        private func formatStatusAsJSON(_ status: WiFiStatus) throws -> String {
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

        private func formatStatusAsText(_ status: WiFiStatus) {
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
                Noora().error("\(errorMessage)")
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
            logger.debug("Disconnecting from WiFi network")

            try await withAgentConnection(
                agentConnectionOptions,
                title: "Which device do you want to disconnect from wifi?",
                grpcOperation: { client in
                    let agent = Wendy_Agent_Services_V1_WendyAgentService.Client(wrapping: client)
                    let request = Wendy_Agent_Services_V1_DisconnectWiFiRequest()

                    let response = try await agent.disconnectWiFi(request)

                    if response.success {
                        Noora().success("Successfully disconnected from WiFi network")
                    } else {
                        let errorMessage = response.hasErrorMessage ? response.errorMessage : "Unknown error"
                        Noora().error("Failed to disconnect: \(errorMessage)")
                    }
                },
                bluetoothOperation: { deviceIdentifier in
                    #if canImport(Bluetooth)
                    let response = try await executeBluetoothCommand(.wifiDisconnect, deviceIdentifier: deviceIdentifier)
                    if case .wifiDisconnect(let success, let errorMessage) = response {
                        if success {
                            Noora().success("Successfully disconnected from WiFi network")
                        } else {
                            let message = errorMessage ?? "Unknown error"
                            Noora().error("Failed to disconnect: \(message)")
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
}
