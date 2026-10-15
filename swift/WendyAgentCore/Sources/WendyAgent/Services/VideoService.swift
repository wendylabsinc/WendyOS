import GRPCCore
import GRPCProtobuf
import Logging
import WendyAgentGRPC

actor VideoService: RegistrableRPCService {
    private static let maxConcurrentStreams = 2

    private let camera: any CameraManaging
    private let controls: any CameraControlManaging
    private let logger = Logger(label: "sh.wendy.agent.video")
    private var devicesByID: [UInt32: CameraDeviceInfo] = [:]
    private var activeStreamCount = 0

    init(
        camera: any CameraManaging = AVCaptureCameraManager(),
        controls: any CameraControlManaging = CameraControls()
    ) {
        self.camera = camera
        self.controls = controls
    }

    func restoreCameraControls() async {
        for device in await discoverDevices() where device.isExternal {
            do { try await controls.restore(uniqueID: device.uniqueID) } catch {
                logger.warning(
                    "Camera controls could not be restored",
                    metadata: ["camera_id": "\(device.id)", "error": "\(error)"]
                )
            }
        }
    }

    nonisolated func registerMethods<Transport>(
        with router: inout RPCRouter<Transport>
    ) where Transport: ServerTransport {
        router.registerHandler(
            forMethod: Self.method("ListVideoDevices"),
            deserializer: ProtobufDeserializer<Wendy_Agent_Services_V1_ListVideoDevicesRequest>(),
            serializer: ProtobufSerializer<Wendy_Agent_Services_V1_ListVideoDevicesResponse>()
        ) { request, context in
            let response = try await self.listVideoDevices(
                request: ServerRequest(stream: request),
                context: context
            )
            return StreamingServerResponse(single: response)
        }
        router.registerHandler(
            forMethod: Self.method("StreamVideo", type: .serverStreaming),
            deserializer: ProtobufDeserializer<Wendy_Agent_Services_V1_StreamVideoRequest>(),
            serializer: ProtobufSerializer<Wendy_Agent_Services_V1_VideoFrame>()
        ) { request, context in
            try await self.streamVideo(
                request: ServerRequest(stream: request),
                context: context
            )
        }
        router.registerHandler(
            forMethod: Self.method("SetCameraCredentials"),
            deserializer: ProtobufDeserializer<
                Wendy_Agent_Services_V1_SetCameraCredentialsRequest
            >(),
            serializer: ProtobufSerializer<
                Wendy_Agent_Services_V1_SetCameraCredentialsResponse
            >()
        ) { request, context in
            let response = try await self.setCameraCredentials(
                request: ServerRequest(stream: request),
                context: context
            )
            return StreamingServerResponse(single: response)
        }
        router.registerHandler(
            forMethod: Self.method("ForgetCamera"),
            deserializer: ProtobufDeserializer<Wendy_Agent_Services_V1_ForgetCameraRequest>(),
            serializer: ProtobufSerializer<Wendy_Agent_Services_V1_ForgetCameraResponse>()
        ) { request, context in
            let response = try await self.forgetCamera(
                request: ServerRequest(stream: request),
                context: context
            )
            return StreamingServerResponse(single: response)
        }
        router.registerHandler(
            forMethod: Self.method("RefreshCameras"),
            deserializer: ProtobufDeserializer<Wendy_Agent_Services_V1_RefreshCamerasRequest>(),
            serializer: ProtobufSerializer<Wendy_Agent_Services_V1_RefreshCamerasResponse>()
        ) { request, context in
            let response = try await self.refreshCameras(
                request: ServerRequest(stream: request),
                context: context
            )
            return StreamingServerResponse(single: response)
        }
        router.registerHandler(
            forMethod: Self.method("TestCameraCredentials"),
            deserializer: ProtobufDeserializer<
                Wendy_Agent_Services_V1_TestCameraCredentialsRequest
            >(),
            serializer: ProtobufSerializer<
                Wendy_Agent_Services_V1_TestCameraCredentialsResponse
            >()
        ) { request, context in
            let response = try await self.testCameraCredentials(
                request: ServerRequest(stream: request),
                context: context
            )
            return StreamingServerResponse(single: response)
        }
        router.registerHandler(
            forMethod: Self.method("GetCameraControls"),
            deserializer: ProtobufDeserializer<Wendy_Agent_Services_V1_GetCameraControlsRequest>(),
            serializer: ProtobufSerializer<Wendy_Agent_Services_V1_GetCameraControlsResponse>()
        ) { request, context in
            let response = try await self.getCameraControls(
                request: ServerRequest(stream: request),
                context: context
            )
            return StreamingServerResponse(single: response)
        }
        router.registerHandler(
            forMethod: Self.method("SetCameraControls"),
            deserializer: ProtobufDeserializer<Wendy_Agent_Services_V1_SetCameraControlsRequest>(),
            serializer: ProtobufSerializer<Wendy_Agent_Services_V1_SetCameraControlsResponse>()
        ) { request, context in
            let response = try await self.setCameraControls(
                request: ServerRequest(stream: request),
                context: context
            )
            return StreamingServerResponse(single: response)
        }
        router.registerHandler(
            forMethod: Self.method("ResetCameraControls"),
            deserializer: ProtobufDeserializer<
                Wendy_Agent_Services_V1_ResetCameraControlsRequest
            >(),
            serializer: ProtobufSerializer<
                Wendy_Agent_Services_V1_ResetCameraControlsResponse
            >()
        ) { request, context in
            let response = try await self.resetCameraControls(
                request: ServerRequest(stream: request),
                context: context
            )
            return StreamingServerResponse(single: response)
        }
    }

    func listVideoDevices(
        request: ServerRequest<Wendy_Agent_Services_V1_ListVideoDevicesRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_ListVideoDevicesResponse> {
        let devices = await discoverDevices()
        var response = Wendy_Agent_Services_V1_ListVideoDevicesResponse()
        response.devices = devices.map(Self.protoDevice)
        return ServerResponse(message: response)
    }

    func streamVideo(
        request: ServerRequest<Wendy_Agent_Services_V1_StreamVideoRequest>,
        context: ServerContext
    ) async throws -> StreamingServerResponse<Wendy_Agent_Services_V1_VideoFrame> {
        let message = request.message
        guard message.codec == .h264 else {
            throw RPCError(
                code: .unimplemented,
                message: "Wendy Agent for Mac currently streams H.264 video only."
            )
        }
        guard message.stableID.isEmpty else {
            throw RPCError(
                code: .unimplemented,
                message: "Stable V4L2 camera identifiers are not available on macOS."
            )
        }
        guard message.width == 0, message.height == 0, message.framerate == 0 else {
            throw RPCError(
                code: .unimplemented,
                message:
                    "Custom camera dimensions and frame rates are not yet supported by Wendy Agent for Mac."
            )
        }

        let device = try await device(id: message.deviceID)
        let camera = self.camera
        let controls = self.controls
        let remotePeer = context.remotePeer
        return StreamingServerResponse { writer in
            try await self.reserveStream(device: device, remotePeer: remotePeer)
            do {
                var restored = false
                for try await frame in camera.frames(for: device) {
                    // Capture may restore firmware defaults when opening. Reapply
                    // once its first frame arrives; that frame can precede tuning.
                    if !restored {
                        restored = true
                        do { try await controls.restore(uniqueID: device.uniqueID) } catch {
                            await self.logControlRestoreFailure(device: device, error: error)
                        }
                    }
                    var proto = Wendy_Agent_Services_V1_VideoFrame()
                    proto.data = frame.annexB
                    proto.timestampNs = frame.timestampNanoseconds
                    proto.codec = .h264
                    try await writer.write(proto)
                }
                await self.releaseStream(device: device, remotePeer: remotePeer)
                return Metadata()
            } catch {
                await self.releaseStream(device: device, remotePeer: remotePeer)
                if let error = error as? CameraError {
                    throw Self.rpcError(for: error)
                }
                throw error
            }
        }
    }

    func setCameraCredentials(
        request: ServerRequest<Wendy_Agent_Services_V1_SetCameraCredentialsRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_SetCameraCredentialsResponse> {
        throw Self.unsupportedNetworkCameras()
    }

    func forgetCamera(
        request: ServerRequest<Wendy_Agent_Services_V1_ForgetCameraRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_ForgetCameraResponse> {
        throw Self.unsupportedNetworkCameras()
    }

    func refreshCameras(
        request: ServerRequest<Wendy_Agent_Services_V1_RefreshCamerasRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_RefreshCamerasResponse> {
        let devices = await discoverDevices()
        var response = Wendy_Agent_Services_V1_RefreshCamerasResponse()
        response.devices = devices.map(Self.protoDevice)
        return ServerResponse(message: response)
    }

    func testCameraCredentials(
        request: ServerRequest<Wendy_Agent_Services_V1_TestCameraCredentialsRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_TestCameraCredentialsResponse> {
        throw Self.unsupportedNetworkCameras()
    }

    func getCameraControls(
        request: ServerRequest<Wendy_Agent_Services_V1_GetCameraControlsRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_GetCameraControlsResponse> {
        let device = try await controlDevice(id: request.message.deviceID)
        do {
            var response = Wendy_Agent_Services_V1_GetCameraControlsResponse()
            response.controls = try await controls.list(uniqueID: device.uniqueID).map { control in
                var proto = Wendy_Agent_Services_V1_CameraControl()
                proto.name = control.name
                proto.value = control.value
                proto.minimum = control.minimum
                proto.maximum = control.maximum
                proto.step = control.step
                proto.defaultValue = control.defaultValue
                proto.mutable = control.mutable
                return proto
            }
            return ServerResponse(message: response)
        } catch { throw Self.controlRPCError(error) }
    }

    func setCameraControls(
        request: ServerRequest<Wendy_Agent_Services_V1_SetCameraControlsRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_SetCameraControlsResponse> {
        let device = try await controlDevice(id: request.message.deviceID)
        do {
            var response = Wendy_Agent_Services_V1_SetCameraControlsResponse()
            response.results = try await controls.set(
                uniqueID: device.uniqueID,
                values: request.message.controls.map { ($0.name, $0.value) },
                persist: request.message.persist
            ).map(Self.controlResult)
            return ServerResponse(message: response)
        } catch { throw Self.controlRPCError(error) }
    }

    func resetCameraControls(
        request: ServerRequest<Wendy_Agent_Services_V1_ResetCameraControlsRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_ResetCameraControlsResponse> {
        let device = try await controlDevice(id: request.message.deviceID)
        do {
            var response = Wendy_Agent_Services_V1_ResetCameraControlsResponse()
            response.results = try await controls.reset(
                uniqueID: device.uniqueID,
                names: request.message.names
            ).map(Self.controlResult)
            return ServerResponse(message: response)
        } catch { throw Self.controlRPCError(error) }
    }

    private func controlDevice(id: UInt32) async throws -> CameraDeviceInfo {
        // Numeric IDs are discovery positions, not persistent USB identities.
        // Refresh every control request; never act on a stale cached device.
        let devices = await discoverDevices()
        guard let device = devices.first(where: { $0.id == id }) else {
            throw RPCError(code: .notFound, message: "Camera \(id) was not found on this Mac.")
        }
        guard device.isExternal, devices.filter({ $0.uniqueID == device.uniqueID }).count == 1
        else {
            throw RPCError(
                code: .unimplemented,
                message: "Controls require one exact external USB camera."
            )
        }
        do { _ = try UVCIdentity(uniqueID: device.uniqueID) } catch {
            throw Self.controlRPCError(error)
        }
        return device
    }

    private func logControlRestoreFailure(device: CameraDeviceInfo, error: any Error) {
        logger.warning(
            "Camera controls could not be restored after capture opened",
            metadata: ["camera_id": "\(device.id)", "error": "\(error)"]
        )
    }

    private static func controlResult(
        _ result: CameraControlOutcome
    ) -> Wendy_Agent_Services_V1_CameraControlResult {
        var proto = Wendy_Agent_Services_V1_CameraControlResult()
        proto.name = result.name
        proto.applied = result.applied
        proto.detail = result.detail
        return proto
    }

    private static func controlRPCError(_ error: any Error) -> RPCError {
        let code: RPCError.Code
        switch error {
        case UVCError.unsupported: code = .unimplemented
        case UVCError.invalidValue: code = .invalidArgument
        default: code = .failedPrecondition
        }
        return RPCError(code: code, message: "\(error)")
    }

    private func reserveStream(
        device: CameraDeviceInfo,
        remotePeer: String
    ) throws {
        guard activeStreamCount < Self.maxConcurrentStreams else {
            throw RPCError(
                code: .resourceExhausted,
                message: "This Mac is already serving the maximum number of camera streams."
            )
        }
        activeStreamCount += 1
        logger.info(
            "Camera stream started",
            metadata: [
                "camera_id": "\(device.id)",
                "camera_name": "\(device.name)",
                "remote_peer": "\(remotePeer)",
            ]
        )
    }

    private func releaseStream(
        device: CameraDeviceInfo,
        remotePeer: String
    ) {
        activeStreamCount -= 1
        logger.info(
            "Camera stream stopped",
            metadata: [
                "camera_id": "\(device.id)",
                "camera_name": "\(device.name)",
                "remote_peer": "\(remotePeer)",
            ]
        )
    }

    private func discoverDevices() async -> [CameraDeviceInfo] {
        let devices = await camera.devices()
        devicesByID = Dictionary(uniqueKeysWithValues: devices.map { ($0.id, $0) })
        return devices
    }

    private func device(id: UInt32) async throws -> CameraDeviceInfo {
        if let device = devicesByID[id] {
            return device
        }
        let devices = await discoverDevices()
        guard let device = devices.first(where: { $0.id == id }) else {
            throw RPCError(code: .notFound, message: "Camera \(id) was not found on this Mac.")
        }
        return device
    }

    private static func protoDevice(
        _ device: CameraDeviceInfo
    ) -> Wendy_Agent_Services_V1_VideoDevice {
        var proto = Wendy_Agent_Services_V1_VideoDevice()
        proto.id = device.id
        proto.name = device.name
        proto.transport = device.isExternal ? .usb : .unknown
        proto.online = true
        return proto
    }

    private static func rpcError(for error: CameraError) -> RPCError {
        switch error {
        case .accessDenied:
            return RPCError(code: .permissionDenied, message: error.description)
        case .deviceNotFound, .noDevice:
            return RPCError(code: .notFound, message: error.description)
        case .cannotAddInput, .cannotAddOutput, .noStreamableFormat, .cannotPinFormat:
            return RPCError(code: .failedPrecondition, message: error.description)
        case .vtStatus:
            return RPCError(code: .internalError, message: error.description)
        }
    }

    private static func method(
        _ name: String,
        type: MethodDescriptor.RPCType = .unary
    ) -> MethodDescriptor {
        MethodDescriptor(
            fullyQualifiedService: "wendy.agent.services.v1.WendyVideoService",
            method: name,
            type: type
        )
    }

    private static func unsupportedNetworkCameras() -> RPCError {
        RPCError(
            code: .unimplemented,
            message: "Network camera management is currently not supported by Wendy Agent for Mac."
        )
    }

}
