import GRPCCore
import WendyAgentGRPC

struct AudioService: Wendy_Agent_Services_V1_WendyAudioService.ServiceProtocol {
    func listAudioDevices(
        request: ServerRequest<Wendy_Agent_Services_V1_ListAudioDevicesRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_ListAudioDevicesResponse> {
        throw RPCError(
            code: .unimplemented,
            message: "Listing audio devices is currently not supported on macOS."
        )
    }

    func setDefaultAudioDevice(
        request: ServerRequest<Wendy_Agent_Services_V1_SetDefaultAudioDeviceRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_SetDefaultAudioDeviceResponse> {
        throw RPCError(
            code: .unimplemented,
            message: "Changing the default audio device is currently not supported on macOS."
        )
    }

    func streamAudioLevels(
        request: ServerRequest<Wendy_Agent_Services_V1_StreamAudioLevelsRequest>,
        context: ServerContext
    ) async throws -> StreamingServerResponse<Wendy_Agent_Services_V1_AudioLevelUpdate> {
        throw RPCError(
            code: .unimplemented,
            message: "Streaming audio levels is currently not supported on macOS."
        )
    }

    func streamAudio(
        request: ServerRequest<Wendy_Agent_Services_V1_StreamAudioRequest>,
        context: ServerContext
    ) async throws -> StreamingServerResponse<Wendy_Agent_Services_V1_AudioChunk> {
        throw RPCError(
            code: .unimplemented,
            message: "Streaming audio is currently not supported on macOS."
        )
    }
}
