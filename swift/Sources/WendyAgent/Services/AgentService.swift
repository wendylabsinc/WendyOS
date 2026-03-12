import Foundation
import GRPCCore
import WendyAgentGRPC

struct AgentService: Wendy_Agent_Services_V1_WendyAgentService.ServiceProtocol {
    func runContainer(
        request: StreamingServerRequest<Wendy_Agent_Services_V1_RunContainerRequest>,
        context: ServerContext
    ) async throws -> StreamingServerResponse<Wendy_Agent_Services_V1_RunContainerResponse> {
        fatalError("not implemented")
    }

    func updateAgent(
        request: StreamingServerRequest<Wendy_Agent_Services_V1_UpdateAgentRequest>,
        context: ServerContext
    ) async throws -> StreamingServerResponse<Wendy_Agent_Services_V1_UpdateAgentResponse> {
        fatalError("not implemented")
    }

    func getAgentVersion(
        request: ServerRequest<Wendy_Agent_Services_V1_GetAgentVersionRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_GetAgentVersionResponse> {
        let osVersion = ProcessInfo.processInfo.operatingSystemVersion
        var response = Wendy_Agent_Services_V1_GetAgentVersionResponse()
        response.version = "0.0.0-dev"
        response.os = "darwin"
        response.osVersion = "\(osVersion.majorVersion).\(osVersion.minorVersion).\(osVersion.patchVersion)"
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
        fatalError("not implemented")
    }

    func connectToWiFi(
        request: ServerRequest<Wendy_Agent_Services_V1_ConnectToWiFiRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_ConnectToWiFiResponse> {
        fatalError("not implemented")
    }

    func getWiFiStatus(
        request: ServerRequest<Wendy_Agent_Services_V1_GetWiFiStatusRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_GetWiFiStatusResponse> {
        fatalError("not implemented")
    }

    func disconnectWiFi(
        request: ServerRequest<Wendy_Agent_Services_V1_DisconnectWiFiRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_DisconnectWiFiResponse> {
        fatalError("not implemented")
    }

    func listHardwareCapabilities(
        request: ServerRequest<Wendy_Agent_Services_V1_ListHardwareCapabilitiesRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_ListHardwareCapabilitiesResponse> {
        fatalError("not implemented")
    }

    func scanBluetoothPeripherals(
        request: StreamingServerRequest<Wendy_Agent_Services_V1_ScanBluetoothPeripheralsRequest>,
        context: ServerContext
    ) async throws -> StreamingServerResponse<Wendy_Agent_Services_V1_ScanBluetoothPeripheralsResponse> {
        fatalError("not implemented")
    }

    func connectBluetoothPeripheral(
        request: ServerRequest<Wendy_Agent_Services_V1_ConnectBluetoothPeripheralRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_ConnectBluetoothPeripheralResponse> {
        fatalError("not implemented")
    }

    func disconnectBluetoothPeripheral(
        request: ServerRequest<Wendy_Agent_Services_V1_DisconnectBluetoothPeripheralRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_DisconnectBluetoothPeripheralResponse> {
        fatalError("not implemented")
    }

    func forgetBluetoothPeripheral(
        request: ServerRequest<Wendy_Agent_Services_V1_ForgetBluetoothPeripheralRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_ForgetBluetoothPeripheralResponse> {
        fatalError("not implemented")
    }

    func updateOS(
        request: ServerRequest<Wendy_Agent_Services_V1_UpdateOSRequest>,
        context: ServerContext
    ) async throws -> StreamingServerResponse<Wendy_Agent_Services_V1_UpdateOSResponse> {
        fatalError("not implemented")
    }
}
