import Foundation
import GRPCCore
import WendyAgentGRPC

struct AgentService: Wendy_Agent_Services_V1_WendyAgentService.ServiceProtocol {
    func runContainer(
        request: StreamingServerRequest<Wendy_Agent_Services_V1_RunContainerRequest>,
        context: ServerContext
    ) async throws -> StreamingServerResponse<Wendy_Agent_Services_V1_RunContainerResponse> {
        throw RPCError(
            code: .unimplemented,
            message: "Streaming container upload and execution is currently not supported on macOS."
        )
    }

    func updateAgent(
        request: StreamingServerRequest<Wendy_Agent_Services_V1_UpdateAgentRequest>,
        context: ServerContext
    ) async throws -> StreamingServerResponse<Wendy_Agent_Services_V1_UpdateAgentResponse> {
        throw RPCError(
            code: .unimplemented,
            message: "Updating the agent is currently not supported on macOS."
        )
    }

    func getAgentVersion(
        request: ServerRequest<Wendy_Agent_Services_V1_GetAgentVersionRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_GetAgentVersionResponse> {
        let osVersion = ProcessInfo.processInfo.operatingSystemVersion
        var response = Wendy_Agent_Services_V1_GetAgentVersionResponse()
        response.version = WendyAgent.version
        response.os = "darwin"
        response.osVersion =
            "\(osVersion.majorVersion).\(osVersion.minorVersion).\(osVersion.patchVersion)"
        #if arch(arm64)
            response.cpuArchitecture = "arm64"
        #elseif arch(x86_64)
            response.cpuArchitecture = "amd64"
        #endif
        return ServerResponse(message: response)
    }

    func listWiFiNetworks(
        request: ServerRequest<Wendy_Agent_Services_V1_ListWiFiNetworksRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_ListWiFiNetworksResponse> {
        throw RPCError(
            code: .unimplemented,
            message: "Wi-Fi network scanning is currently not supported on macOS."
        )
    }

    func connectToWiFi(
        request: ServerRequest<Wendy_Agent_Services_V1_ConnectToWiFiRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_ConnectToWiFiResponse> {
        throw RPCError(
            code: .unimplemented,
            message: "Connecting to Wi-Fi networks is currently not supported on macOS."
        )
    }

    func getWiFiStatus(
        request: ServerRequest<Wendy_Agent_Services_V1_GetWiFiStatusRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_GetWiFiStatusResponse> {
        throw RPCError(
            code: .unimplemented,
            message: "Wi-Fi status reporting is currently not supported on macOS."
        )
    }

    func disconnectWiFi(
        request: ServerRequest<Wendy_Agent_Services_V1_DisconnectWiFiRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_DisconnectWiFiResponse> {
        throw RPCError(
            code: .unimplemented,
            message: "Disconnecting from Wi-Fi networks is currently not supported on macOS."
        )
    }

    func listKnownWiFiNetworks(
        request: ServerRequest<Wendy_Agent_Services_V1_ListKnownWiFiNetworksRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_ListKnownWiFiNetworksResponse> {
        throw RPCError(
            code: .unimplemented,
            message: "Listing saved Wi-Fi networks is currently not supported on macOS."
        )
    }

    func setWiFiNetworkPriority(
        request: ServerRequest<Wendy_Agent_Services_V1_SetWiFiNetworkPriorityRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_SetWiFiNetworkPriorityResponse> {
        throw RPCError(
            code: .unimplemented,
            message: "Wi-Fi network priority management is currently not supported on macOS."
        )
    }

    func reorderKnownWiFiNetworks(
        request: ServerRequest<Wendy_Agent_Services_V1_ReorderKnownWiFiNetworksRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_ReorderKnownWiFiNetworksResponse> {
        throw RPCError(
            code: .unimplemented,
            message: "Reordering saved Wi-Fi networks is currently not supported on macOS."
        )
    }

    func forgetWiFiNetwork(
        request: ServerRequest<Wendy_Agent_Services_V1_ForgetWiFiNetworkRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_ForgetWiFiNetworkResponse> {
        throw RPCError(
            code: .unimplemented,
            message: "Removing saved Wi-Fi networks is currently not supported on macOS."
        )
    }

    func listHardwareCapabilities(
        request: ServerRequest<Wendy_Agent_Services_V1_ListHardwareCapabilitiesRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_ListHardwareCapabilitiesResponse> {
        throw RPCError(
            code: .unimplemented,
            message: "Hardware capability discovery is currently not supported on macOS."
        )
    }

    func scanBluetoothPeripherals(
        request: StreamingServerRequest<Wendy_Agent_Services_V1_ScanBluetoothPeripheralsRequest>,
        context: ServerContext
    ) async throws -> StreamingServerResponse<
        Wendy_Agent_Services_V1_ScanBluetoothPeripheralsResponse
    > {
        throw RPCError(
            code: .unimplemented,
            message: "Bluetooth scanning is currently not supported on macOS."
        )
    }

    func connectBluetoothPeripheral(
        request: ServerRequest<Wendy_Agent_Services_V1_ConnectBluetoothPeripheralRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_ConnectBluetoothPeripheralResponse> {
        throw RPCError(
            code: .unimplemented,
            message: "Connecting Bluetooth peripherals is currently not supported on macOS."
        )
    }

    func disconnectBluetoothPeripheral(
        request: ServerRequest<Wendy_Agent_Services_V1_DisconnectBluetoothPeripheralRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_DisconnectBluetoothPeripheralResponse>
    {
        throw RPCError(
            code: .unimplemented,
            message: "Disconnecting Bluetooth peripherals is currently not supported on macOS."
        )
    }

    func forgetBluetoothPeripheral(
        request: ServerRequest<Wendy_Agent_Services_V1_ForgetBluetoothPeripheralRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_ForgetBluetoothPeripheralResponse> {
        throw RPCError(
            code: .unimplemented,
            message: "Forgetting Bluetooth peripherals is currently not supported on macOS."
        )
    }

    func updateOS(
        request: ServerRequest<Wendy_Agent_Services_V1_UpdateOSRequest>,
        context: ServerContext
    ) async throws -> StreamingServerResponse<Wendy_Agent_Services_V1_UpdateOSResponse> {
        throw RPCError(
            code: .unimplemented,
            message: "This setup cannot be updated with wendy os update. "
                + "Use this machine’s normal OS update tools instead. "
                + "To use WendyOS OTA updates, install WendyOS on supported hardware "
                + "with wendy os install."
        )
    }
}
