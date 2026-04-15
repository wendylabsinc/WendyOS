import GRPCCore
import WendyAgentGRPC

struct AudioService: Wendy_Agent_Services_V1_WendyAudioService.ServiceProtocol {
    func listAudioDevices(
        request: ServerRequest<Wendy_Agent_Services_V1_ListAudioDevicesRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_ListAudioDevicesResponse> {
        fatalError("not implemented")
    }

    func setDefaultAudioDevice(
        request: ServerRequest<Wendy_Agent_Services_V1_SetDefaultAudioDeviceRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_SetDefaultAudioDeviceResponse> {
        fatalError("not implemented")
    }

    func streamAudioLevels(
        request: ServerRequest<Wendy_Agent_Services_V1_StreamAudioLevelsRequest>,
        context: ServerContext
    ) async throws -> StreamingServerResponse<Wendy_Agent_Services_V1_AudioLevelUpdate> {
        fatalError("not implemented")
    }

    func streamAudio(
        request: ServerRequest<Wendy_Agent_Services_V1_StreamAudioRequest>,
        context: ServerContext
    ) async throws -> StreamingServerResponse<Wendy_Agent_Services_V1_AudioChunk> {
        fatalError("not implemented")
    }
}
