import Foundation
import GRPCCore
import Logging
import NIOFoundationCompat
import WendyAgentGRPC

/// gRPC service for audio device management and streaming
struct WendyAudioService: Wendy_Agent_Services_V1_WendyAudioService.ServiceProtocol {
    let logger = Logger(label: "WendyAudioService")
    let pipeWireManager = PipeWireManager()

    // MARK: - ListAudioDevices

    func listAudioDevices(
        request: GRPCCore.ServerRequest<Wendy_Agent_Services_V1_ListAudioDevicesRequest>,
        context: GRPCCore.ServerContext
    ) async throws -> GRPCCore.ServerResponse<Wendy_Agent_Services_V1_ListAudioDevicesResponse> {
        logger.info("Listing audio devices")

        // Convert proto filter to internal type
        let typeFilter: AudioDevice.AudioDeviceType?
        if request.message.hasTypeFilter {
            switch request.message.typeFilter {
            case .input:
                typeFilter = .input
            case .output:
                typeFilter = .output
            case .unspecified, .UNRECOGNIZED:
                typeFilter = nil
            }
        } else {
            typeFilter = nil
        }

        do {
            let devices = try await pipeWireManager.listDevices(typeFilter: typeFilter)

            let protoDevices = devices.map { device -> Wendy_Agent_Services_V1_AudioDevice in
                var protoDevice = Wendy_Agent_Services_V1_AudioDevice()
                protoDevice.id = device.id
                protoDevice.name = device.name
                protoDevice.description_p = device.description
                protoDevice.type =
                    device.type == .input ? .input : .output
                protoDevice.isDefault = device.isDefault
                return protoDevice
            }

            logger.info("Found \(protoDevices.count) audio devices")

            var response = Wendy_Agent_Services_V1_ListAudioDevicesResponse()
            response.devices = protoDevices
            return ServerResponse(message: response)
        } catch {
            logger.error("Failed to list audio devices", metadata: ["error": "\(error)"])
            throw RPCError(
                code: .internalError,
                message: "Failed to list audio devices: \(error.localizedDescription)"
            )
        }
    }

    // MARK: - SetDefaultAudioDevice

    func setDefaultAudioDevice(
        request: GRPCCore.ServerRequest<Wendy_Agent_Services_V1_SetDefaultAudioDeviceRequest>,
        context: GRPCCore.ServerContext
    ) async throws -> GRPCCore.ServerResponse<Wendy_Agent_Services_V1_SetDefaultAudioDeviceResponse>
    {
        let deviceId = request.message.deviceID
        logger.info("Setting default audio device", metadata: ["deviceId": "\(deviceId)"])

        do {
            try await pipeWireManager.setDefaultDevice(id: deviceId)

            var response = Wendy_Agent_Services_V1_SetDefaultAudioDeviceResponse()
            response.success = true
            return ServerResponse(message: response)
        } catch {
            logger.error(
                "Failed to set default audio device",
                metadata: ["deviceId": "\(deviceId)", "error": "\(error)"]
            )

            var response = Wendy_Agent_Services_V1_SetDefaultAudioDeviceResponse()
            response.success = false
            response.errorMessage = error.localizedDescription
            return ServerResponse(message: response)
        }
    }

    // MARK: - StreamAudioLevels

    func streamAudioLevels(
        request: GRPCCore.ServerRequest<Wendy_Agent_Services_V1_StreamAudioLevelsRequest>,
        context: GRPCCore.ServerContext
    ) async throws -> GRPCCore.StreamingServerResponse<Wendy_Agent_Services_V1_AudioLevelUpdate> {
        let deviceId = request.message.deviceID
        let updateRateHz = request.message.updateRateHz == 0 ? 20 : request.message.updateRateHz

        logger.info(
            "Starting audio level stream",
            metadata: ["deviceId": "\(deviceId)", "updateRateHz": "\(updateRateHz)"]
        )

        return StreamingServerResponse { writer in
            let levelStream = await pipeWireManager.streamAudioLevels(
                deviceId: deviceId == 0 ? nil : deviceId,
                updateRateHz: updateRateHz
            )

            for try await (peakDb, rmsDb) in levelStream {
                var update = Wendy_Agent_Services_V1_AudioLevelUpdate()
                update.peakDb = peakDb
                update.rmsDb = rmsDb
                update.timestampNs = UInt64(DispatchTime.now().uptimeNanoseconds)

                try await writer.write(update)
            }

            return Metadata()
        }
    }

    // MARK: - StreamAudio

    func streamAudio(
        request: GRPCCore.ServerRequest<Wendy_Agent_Services_V1_StreamAudioRequest>,
        context: GRPCCore.ServerContext
    ) async throws -> GRPCCore.StreamingServerResponse<Wendy_Agent_Services_V1_AudioChunk> {
        let deviceId = request.message.deviceID
        let sampleRate = request.message.sampleRate == 0 ? 48000 : request.message.sampleRate
        let channels = request.message.channels == 0 ? 1 : request.message.channels

        logger.info(
            "Starting audio stream",
            metadata: [
                "deviceId": "\(deviceId)",
                "sampleRate": "\(sampleRate)",
                "channels": "\(channels)",
            ]
        )

        return StreamingServerResponse { writer in
            try await pipeWireManager.withAudioStream(
                deviceId: deviceId == 0 ? nil : deviceId,
                sampleRate: sampleRate,
                channels: channels
            ) { buffer, timestampNs in
                var chunk = Wendy_Agent_Services_V1_AudioChunk()
                chunk.pcmData = Data(buffer: buffer)
                chunk.timestampNs = timestampNs
                chunk.sampleRate = sampleRate
                chunk.channels = channels
                try await writer.write(chunk)
            }

            return Metadata()
        }
    }
}
