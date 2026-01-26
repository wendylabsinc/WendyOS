import Bluetooth
import CLIOutput
import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import Logging
import Synchronization
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
    title: String,
    preferBluetooth: Bool = false,
    includeBluetooth: Bool = true,
    _ body: @escaping @Sendable (AgentClient) async throws -> R
) async throws -> R {
    try await withAgentClientAndHostname(
        connectionOptions,
        title: title,
        preferBluetooth: preferBluetooth,
        includeBluetooth: includeBluetooth
    ) { client, _ in
        try await body(client)
    }
}

/// Execute a command with automatic connection type selection, also providing the hostname
/// Prefers LAN when available, falls back to Bluetooth
func withAgentClientAndHostname<R: Sendable>(
    _ connectionOptions: AgentConnectionOptions,
    title: String,
    preferBluetooth: Bool = false,
    includeBluetooth: Bool = true,
    _ body: @escaping @Sendable (AgentClient, String) async throws -> R
) async throws -> R {
    let selectedDevice = try await connectionOptions.read(
        title: title,
        preferBluetooth: preferBluetooth,
        includeBluetooth: includeBluetooth
    )

    switch selectedDevice {
    case .lan(let host, let port, let defaultDevice):
        let endpoint = AgentConnectionOptions.Endpoint(
            host: host,
            port: port,
            defaultDevice: defaultDevice
        )
        let connectionSucceeded = Mutex(false)
        do {
            return try await withAgentGRPCClient(endpoint, title: title) { client in
                connectionSucceeded.withLock { $0 = true }
                return try await body(.grpc(client), host)
            }
        } catch {
            // Only retry with device selection if we never successfully connected
            guard defaultDevice && !connectionSucceeded.withLock({ $0 }) else {
                throw error
            }
            // If default device failed to connect, try again without default
            let newDevice = try await connectionOptions.read(
                title: title,
                readDefault: false,
                preferBluetooth: preferBluetooth,
                includeBluetooth: includeBluetooth
            )
            return try await executeWithDeviceAndHostname(newDevice, title: title, body)
        }

    case .bluetooth(let peripheral, let address):
        return try await BluetoothAgentClient.withConnection(to: peripheral) { client in
            try await body(.bluetooth(client), address)
        }
    }
}

private func executeWithDevice<R: Sendable>(
    _ device: SelectedDevice,
    title: String,
    _ body: @escaping @Sendable (AgentClient) async throws -> R
) async throws -> R {
    try await executeWithDeviceAndHostname(device, title: title) { client, _ in
        try await body(client)
    }
}

private func executeWithDeviceAndHostname<R: Sendable>(
    _ device: SelectedDevice,
    title: String,
    _ body: @escaping @Sendable (AgentClient, String) async throws -> R
) async throws -> R {
    switch device {
    case .lan(let host, let port, _):
        let endpoint = AgentConnectionOptions.Endpoint(
            host: host,
            port: port,
            defaultDevice: false
        )
        return try await withAgentGRPCClient(endpoint, title: title) { client in
            try await body(.grpc(client), host)
        }

    case .bluetooth(let peripheral, let address):
        return try await BluetoothAgentClient.withConnection(to: peripheral) { client in
            try await body(.bluetooth(client), address)
        }
    }
}

// MARK: - WiFi Commands

extension AgentClient {

    func discoverSSID() async throws -> String {
        let source = self

        // Fetch initial WiFi networks
        let initialNetworks = Self.deduplicateNetworks(try await source.listWiFiNetworks())

        // Create an async stream that periodically fetches updated WiFi networks
        let updates = AsyncStream<[WiFiNetworkInfo]> { continuation in
            let task = Task {
                while !Task.isCancelled {
                    try await Task.sleep(for: .seconds(3))
                    guard !Task.isCancelled else { break }
                    let networks = Self.deduplicateNetworks(
                        try await source.listWiFiNetworks()
                    )
                    continuation.yield(networks)
                }
            }
            continuation.onTermination = { _ in
                task.cancel()
            }
        }

        let selected = try await cliOutput.selectFromStreamingTable(
            initial: initialNetworks,
            updates: updates,
            pageSize: 20,
            renderTable: { networks in
                return (
                    headers: ["SSID", "Strength"],
                    rows: networks.map { network in
                        [
                            network.ssid,
                            network.signalStrength?.description ?? "Unknown",
                        ]
                    }
                )
            }
        )
        return selected.ssid
    }

    /// Deduplicate WiFi networks by SSID, keeping the one with the highest signal strength.
    private static func deduplicateNetworks(_ networks: [WiFiNetworkInfo]) -> [WiFiNetworkInfo] {
        Dictionary(grouping: networks.filter { !$0.ssid.isEmpty }) { $0.ssid }
            .compactMapValues { networksWithSameSsid -> WiFiNetworkInfo? in
                networksWithSameSsid.max(by: {
                    ($0.signalStrength ?? 0) < ($1.signalStrength ?? 0)
                })
            }
            .values
            .sorted(by: { ($0.signalStrength ?? 0) > ($1.signalStrength ?? 0) })
    }

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

    func listBluetoothDevices() async throws -> [BluetoothDeviceInfo] {
        switch self {
        case .grpc(let client):
            let agent = Wendy_Agent_Services_V1_WendyAgentService.Client(wrapping: client)
            let request = Wendy_Agent_Services_V1_ListBluetoothDevicesRequest()
            let response = try await agent.listBluetoothDevices(request)
            return response.devices.map { BluetoothDeviceInfo(from: $0) }
        case .bluetooth(let client):
            return try await client.listBluetoothDevices(pairedOnly: false)
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
}

// MARK: - Agent Info Commands

extension AgentClient {
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

// MARK: - Apps Commands

extension AgentClient {
    /// List applications on the device
    func listApps() async throws -> [AppInfo] {
        switch self {
        case .grpc(let client):
            let containers = Wendy_Agent_Services_V1_WendyContainerService.Client(wrapping: client)
            return try await containers.listContainers(.init()) { response in
                var apps: [AppInfo] = []
                for try await container in response.messages {
                    let state: AppInfo.RunningState =
                        switch container.container.runningState {
                        case .running: .running
                        case .stopped: .stopped
                        case .UNRECOGNIZED: .unknown
                        }
                    apps.append(
                        AppInfo(
                            name: container.container.appName,
                            version: container.container.appVersion,
                            runningState: state,
                            failureCount: Int(container.container.failureCount)
                        )
                    )
                }
                return apps
            }

        case .bluetooth(let client):
            let apps = try await client.listApps()
            return apps.map { app in
                // Bluetooth proto uses string state field instead of enum
                let state: AppInfo.RunningState =
                    switch app.state.lowercased() {
                    case "running": .running
                    case "stopped": .stopped
                    default: .unknown
                    }
                return AppInfo(
                    name: app.appName,
                    version: app.appVersion,
                    runningState: state,
                    failureCount: Int(app.failureCount)
                )
            }
        }
    }

    /// Start an application
    func startApp(name: String) async throws {
        switch self {
        case .grpc(let client):
            let containers = Wendy_Agent_Services_V1_WendyContainerService.Client(wrapping: client)
            _ = try await containers.startContainer(
                request: .init(message: .with { $0.appName = name })
            ) { response in
                // Wait for the container to start, then return
                for try await message in response.messages {
                    if case .started = message.responseType {
                        return
                    }
                }
            }

        case .bluetooth:
            throw CLIError.unsupportedPlatform(
                reason: "Starting apps over Bluetooth is not yet supported"
            )
        }
    }

    /// Stop a running application
    func stopApp(name: String) async throws {
        switch self {
        case .grpc(let client):
            let containers = Wendy_Agent_Services_V1_WendyContainerService.Client(wrapping: client)
            _ = try await containers.stopContainer(.with { $0.appName = name })

        case .bluetooth(let client):
            _ = try await client.stopApp(name: name)
        }
    }

    /// Remove an application from the device
    func removeApp(name: String, purgeImage: Bool) async throws {
        switch self {
        case .grpc(let client):
            let containers = Wendy_Agent_Services_V1_WendyContainerService.Client(wrapping: client)
            _ = try await containers.deleteContainer(
                .with {
                    $0.appName = name
                    $0.deleteImage = purgeImage
                }
            )

        case .bluetooth(let client):
            _ = try await client.removeApp(name: name, purgeImage: purgeImage)
        }
    }
}

// MARK: - Hardware Commands

extension AgentClient {
    /// List hardware capabilities on the device
    /// Note: Bluetooth uses a simplified HardwareInfo proto with different fields
    func listHardware(categoryFilter: String? = nil) async throws -> [HardwareCapability] {
        switch self {
        case .grpc(let client):
            let agent = Wendy_Agent_Services_V1_WendyAgentService.Client(wrapping: client)
            var request = Wendy_Agent_Services_V1_ListHardwareCapabilitiesRequest()
            if let categoryFilter {
                request.categoryFilter = categoryFilter
            }
            let response = try await agent.listHardwareCapabilities(request)
            return response.capabilities.map { capability in
                HardwareCapability(
                    category: capability.category,
                    devicePath: capability.devicePath,
                    description: capability.description_p,
                    properties: capability.properties
                )
            }

        case .bluetooth(let client):
            // Bluetooth proto uses simplified HardwareInfo with type/name/available
            let capabilities = try await client.listHardware()
            return capabilities.map { capability in
                HardwareCapability(
                    category: capability.type,
                    devicePath: capability.name,
                    description: capability.available ? "Available" : "Unavailable",
                    properties: [:]
                )
            }
        }
    }
}

// MARK: - Unified Response Types

struct WiFiNetworkInfo: Sendable, Comparable {
    let ssid: String
    let signalStrength: Int?

    static func < (lhs: WiFiNetworkInfo, rhs: WiFiNetworkInfo) -> Bool {
        // Sort by signal strength descending (stronger first), then by SSID
        let lhsStrength = lhs.signalStrength ?? 0
        let rhsStrength = rhs.signalStrength ?? 0
        if lhsStrength != rhsStrength {
            return lhsStrength > rhsStrength
        }
        return lhs.ssid < rhs.ssid
    }
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

struct AppInfo: Sendable, Encodable {
    enum RunningState: String, Sendable, Encodable {
        case running = "Running"
        case stopped = "Stopped"
        case unknown = "Unknown"
    }

    let name: String
    let version: String
    let runningState: RunningState
    let failureCount: Int
}

struct HardwareCapability: Sendable {
    let category: String
    let devicePath: String
    let description: String
    let properties: [String: String]
}
