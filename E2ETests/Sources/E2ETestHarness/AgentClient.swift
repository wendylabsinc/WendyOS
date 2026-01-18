import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import Logging
import WendyAgentGRPC

/// gRPC client wrapper for wendy-agent APIs
public struct AgentClient: Sendable {
    private let configuration: TestConfiguration
    private let logger: Logger

    public init(configuration: TestConfiguration, logger: Logger = Logger(label: "E2ETestHarness.AgentClient")) {
        self.configuration = configuration
        self.logger = logger
    }

    /// Execute an operation with a gRPC client connection
    private func withClient<R: Sendable>(
        _ operation: @escaping @Sendable (GRPCClient<HTTP2ClientTransport.Posix>) async throws -> R
    ) async throws -> R {
        let transport = try HTTP2ClientTransport.Posix(
            target: .dns(
                host: configuration.agentHost,
                port: configuration.agentPort
            ),
            transportSecurity: .plaintext
        )

        return try await withGRPCClient(transport: transport) { client in
            try await operation(client)
        }
    }

    // MARK: - Agent Service APIs

    /// Get the agent version - useful for connectivity testing
    public func getAgentVersion() async throws -> String {
        try await withClient { client in
            let agent = Wendy_Agent_Services_V1_WendyAgentService.Client(wrapping: client)
            let response = try await agent.getAgentVersion(.init())
            return response.version
        }
    }

    /// List available WiFi networks
    public func listWiFiNetworks() async throws -> [Wendy_Agent_Services_V1_ListWiFiNetworksResponse.WiFiNetwork] {
        try await withClient { client in
            let agent = Wendy_Agent_Services_V1_WendyAgentService.Client(wrapping: client)
            let response = try await agent.listWiFiNetworks(.init())
            return response.networks
        }
    }

    /// Get current WiFi connection status
    public func getWiFiStatus() async throws -> Wendy_Agent_Services_V1_GetWiFiStatusResponse {
        try await withClient { client in
            let agent = Wendy_Agent_Services_V1_WendyAgentService.Client(wrapping: client)
            return try await agent.getWiFiStatus(.init())
        }
    }

    /// Get hardware capabilities
    public func getHardware() async throws -> [Wendy_Agent_Services_V1_ListHardwareCapabilitiesResponse.HardwareCapability] {
        try await withClient { client in
            let agent = Wendy_Agent_Services_V1_WendyAgentService.Client(wrapping: client)
            let response = try await agent.listHardwareCapabilities(.init())
            return response.capabilities
        }
    }

    // MARK: - Provisioning Service APIs

    /// Check if the device is provisioned
    public func isProvisioned() async throws -> ProvisioningStatus {
        try await withClient { client in
            let provisioning = Wendy_Agent_Services_V1_WendyProvisioningService.Client(wrapping: client)
            let response = try await provisioning.isProvisioned(.init())

            switch response.response {
            case .notProvisioned:
                return .notProvisioned
            case .provisioned(let info):
                return .provisioned(assetId: info.assetID, organizationId: info.organizationID)
            case .none:
                return .unknown
            }
        }
    }

    // MARK: - Container Service APIs

    /// List all containers (collects streaming response into array)
    public func listContainers() async throws -> [AppContainer] {
        try await withClient { client in
            let containers = Wendy_Agent_Services_V1_WendyContainerService.Client(wrapping: client)

            // listContainers is a streaming RPC that returns one container per message
            // Use an actor to safely accumulate results
            let collector = ContainerCollector()

            try await containers.listContainers(.init()) { response in
                for try await item in response.messages {
                    if item.hasContainer {
                        await collector.append(item.container)
                    }
                }
            }

            return await collector.containers
        }
    }

    /// Create a container with the given configuration
    public func createContainer(
        appName: String,
        imageName: String,
        cmd: String = "",
        workingDir: String = ""
    ) async throws -> Wendy_Agent_Services_V1_CreateContainerResponse {
        try await withClient { client in
            let containers = Wendy_Agent_Services_V1_WendyContainerService.Client(wrapping: client)
            var request = Wendy_Agent_Services_V1_CreateContainerRequest()
            request.appName = appName
            request.imageName = imageName
            request.cmd = cmd
            request.workingDir = workingDir
            return try await containers.createContainer(request)
        }
    }

    /// Start a container by name (returns streaming response)
    public func startContainer(appName: String) async throws {
        try await withClient { client in
            let containers = Wendy_Agent_Services_V1_WendyContainerService.Client(wrapping: client)
            var request = Wendy_Agent_Services_V1_StartContainerRequest()
            request.appName = appName

            try await containers.startContainer(request) { response in
                // Consume the streaming response
                for try await _ in response.messages {
                    // We just need to consume the response
                }
            }
        }
    }

    /// Stop a container by name
    public func stopContainer(appName: String) async throws -> Wendy_Agent_Services_V1_StopContainerResponse {
        try await withClient { client in
            let containers = Wendy_Agent_Services_V1_WendyContainerService.Client(wrapping: client)
            var request = Wendy_Agent_Services_V1_StopContainerRequest()
            request.appName = appName
            return try await containers.stopContainer(request)
        }
    }

    /// Delete a container by name
    public func deleteContainer(appName: String, deleteImage: Bool = false) async throws -> Wendy_Agent_Services_V1_DeleteContainerResponse {
        try await withClient { client in
            let containers = Wendy_Agent_Services_V1_WendyContainerService.Client(wrapping: client)
            var request = Wendy_Agent_Services_V1_DeleteContainerRequest()
            request.appName = appName
            request.deleteImage = deleteImage
            return try await containers.deleteContainer(request)
        }
    }
}

/// Provisioning status of the device
public enum ProvisioningStatus: Sendable, Equatable {
    case notProvisioned
    case provisioned(assetId: Int32, organizationId: Int32)
    case unknown
}

/// Helper actor to safely collect containers from streaming response
private actor ContainerCollector {
    var containers: [AppContainer] = []

    func append(_ container: AppContainer) {
        containers.append(container)
    }
}
