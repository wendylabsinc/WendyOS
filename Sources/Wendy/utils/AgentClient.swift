import Bluetooth
import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import Logging
import Noora
import WendyAgentGRPC
import WendyShared

/// Unified agent client that supports both gRPC (LAN) and Bluetooth connections
enum AgentClient {
    case grpc(GRPCClient<HTTP2ClientTransport.Posix>)
    case bluetooth(BluetoothAgentClient)
}

/// Execute a command with automatic connection type selection
/// Prefers LAN when available, falls back to Bluetooth
func withAgentClient<R: Sendable>(
    _ connectionOptions: AgentConnectionOptions,
    title: TerminalText,
    preferBluetooth: Bool = false,
    _ body: @escaping @Sendable (AgentClient) async throws -> R
) async throws -> R {
    let selectedDevice = try await connectionOptions.readWithBluetooth(
        title: title,
        preferBluetooth: preferBluetooth
    )

    switch selectedDevice {
    case .lan(let host, let port, let defaultDevice):
        let endpoint = AgentConnectionOptions.Endpoint(
            host: host,
            port: port,
            defaultDevice: defaultDevice
        )
        do {
            return try await withAgentGRPCClient(endpoint, title: title) { client in
                try await body(.grpc(client))
            }
        } catch  where defaultDevice {
            // If default device failed, try again without default
            let newDevice = try await connectionOptions.readWithBluetooth(
                title: title,
                readDefault: false,
                preferBluetooth: preferBluetooth
            )
            return try await executeWithDevice(newDevice, title: title, body)
        }

    case .bluetooth(let peripheral, _):
        return try await BluetoothAgentClient.withConnection(to: peripheral) { client in
            try await body(.bluetooth(client))
        }
    }
}

private func executeWithDevice<R: Sendable>(
    _ device: SelectedDevice,
    title: TerminalText,
    _ body: @escaping @Sendable (AgentClient) async throws -> R
) async throws -> R {
    switch device {
    case .lan(let host, let port, _):
        let endpoint = AgentConnectionOptions.Endpoint(
            host: host,
            port: port,
            defaultDevice: false
        )
        return try await withAgentGRPCClient(endpoint, title: title) { client in
            try await body(.grpc(client))
        }

    case .bluetooth(let peripheral, _):
        return try await BluetoothAgentClient.withConnection(to: peripheral) { client in
            try await body(.bluetooth(client))
        }
    }
}

// MARK: - WiFi Commands via AgentClient

extension AgentClient {
    /// List available WiFi networks
    func listWiFiNetworks() async throws -> [WiFiNetworkInfo] {
        switch self {
        case .grpc(let client):
            let agent = Wendy_Agent_Services_V1_WendyAgentService.Client(wrapping: client)
            let request = Wendy_Agent_Services_V1_ListWiFiNetworksRequest()
            let response = try await agent.listWiFiNetworks(request)
            return response.networks.map { network in
                WiFiNetworkInfo(
                    ssid: network.ssid,
                    signalStrength: network.hasSignalStrength ? Int(network.signalStrength) : nil
                )
            }

        case .bluetooth(let client):
            let networks = try await client.listWiFiNetworks()
            return networks.map { network in
                WiFiNetworkInfo(
                    ssid: network.ssid,
                    signalStrength: network.hasSignalStrength ? Int(network.signalStrength) : nil
                )
            }
        }
    }

    /// Connect to a WiFi network
    func connectToWiFi(ssid: String, password: String) async throws -> WiFiConnectResult {
        switch self {
        case .grpc(let client):
            let agent = Wendy_Agent_Services_V1_WendyAgentService.Client(wrapping: client)
            var request = Wendy_Agent_Services_V1_ConnectToWiFiRequest()
            request.ssid = ssid
            request.password = password
            let response = try await agent.connectToWiFi(request)
            return WiFiConnectResult(
                success: response.success,
                errorMessage: response.hasErrorMessage ? response.errorMessage : nil
            )

        case .bluetooth(let client):
            let response = try await client.connectToWiFi(ssid: ssid, password: password)
            return WiFiConnectResult(
                success: response.success,
                errorMessage: response.hasErrorMessage ? response.errorMessage : nil
            )
        }
    }

    /// Get WiFi connection status
    func getWiFiStatus() async throws -> WiFiStatusInfo {
        switch self {
        case .grpc(let client):
            let agent = Wendy_Agent_Services_V1_WendyAgentService.Client(wrapping: client)
            let request = Wendy_Agent_Services_V1_GetWiFiStatusRequest()
            let response = try await agent.getWiFiStatus(request)
            return WiFiStatusInfo(
                connected: response.connected,
                ssid: response.hasSsid ? response.ssid : nil,
                errorMessage: response.hasErrorMessage ? response.errorMessage : nil
            )

        case .bluetooth(let client):
            let response = try await client.getWiFiStatus()
            return WiFiStatusInfo(
                connected: response.connected,
                ssid: response.hasSsid ? response.ssid : nil,
                errorMessage: response.hasErrorMessage ? response.errorMessage : nil
            )
        }
    }

    /// Disconnect from WiFi
    func disconnectWiFi() async throws -> WiFiDisconnectResult {
        switch self {
        case .grpc(let client):
            let agent = Wendy_Agent_Services_V1_WendyAgentService.Client(wrapping: client)
            let request = Wendy_Agent_Services_V1_DisconnectWiFiRequest()
            let response = try await agent.disconnectWiFi(request)
            return WiFiDisconnectResult(
                success: response.success,
                errorMessage: response.hasErrorMessage ? response.errorMessage : nil
            )

        case .bluetooth(let client):
            let response = try await client.disconnectWiFi()
            return WiFiDisconnectResult(
                success: response.success,
                errorMessage: response.hasErrorMessage ? response.errorMessage : nil
            )
        }
    }

    /// Get agent version
    func getAgentVersion() async throws -> String {
        switch self {
        case .grpc(let client):
            let agent = Wendy_Agent_Services_V1_WendyAgentService.Client(wrapping: client)
            let request = Wendy_Agent_Services_V1_GetAgentVersionRequest()
            let response = try await agent.getAgentVersion(request)
            return response.version

        case .bluetooth(let client):
            return try await client.getAgentVersion()
        }
    }
}

// MARK: - Unified Response Types

struct WiFiNetworkInfo: Sendable {
    let ssid: String
    let signalStrength: Int?
}

struct WiFiConnectResult: Sendable {
    let success: Bool
    let errorMessage: String?
}

struct WiFiStatusInfo: Sendable {
    let connected: Bool
    let ssid: String?
    let errorMessage: String?
}

struct WiFiDisconnectResult: Sendable {
    let success: Bool
    let errorMessage: String?
}
