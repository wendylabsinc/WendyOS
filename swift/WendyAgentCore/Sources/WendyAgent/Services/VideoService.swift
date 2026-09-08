import GRPCCore
import WendyAgentGRPC

actor VideoService: Wendy_Agent_Services_V1_WendyVideoService.ServiceProtocol {
    private let camera: any CameraManaging
    private var devicesByID: [UInt32: CameraDeviceInfo] = [:]

    init(camera: any CameraManaging = AVCaptureCameraManager()) {
        self.camera = camera
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
        return StreamingServerResponse { writer in
            do {
                for try await frame in camera.frames(for: device) {
                    var proto = Wendy_Agent_Services_V1_VideoFrame()
                    proto.data = frame.data
                    proto.timestampNs = frame.timestampNanoseconds
                    proto.codec = .h264
                    try await writer.write(proto)
                }
            } catch let error as CameraCaptureError {
                throw Self.rpcError(for: error)
            }
            return Metadata()
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
        throw Self.unsupportedV4L2Controls()
    }

    func setCameraControls(
        request: ServerRequest<Wendy_Agent_Services_V1_SetCameraControlsRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_SetCameraControlsResponse> {
        throw Self.unsupportedV4L2Controls()
    }

    func resetCameraControls(
        request: ServerRequest<Wendy_Agent_Services_V1_ResetCameraControlsRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Agent_Services_V1_ResetCameraControlsResponse> {
        throw Self.unsupportedV4L2Controls()
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

    private static func rpcError(for error: CameraCaptureError) -> RPCError {
        switch error {
        case .accessDenied:
            return RPCError(code: .permissionDenied, message: error.description)
        case .deviceNotFound:
            return RPCError(code: .notFound, message: error.description)
        case .cannotOpenDevice, .cannotAddInput, .cannotAddOutput:
            return RPCError(code: .failedPrecondition, message: error.description)
        case .videoToolbox:
            return RPCError(code: .internalError, message: error.description)
        }
    }

    private static func unsupportedNetworkCameras() -> RPCError {
        RPCError(
            code: .unimplemented,
            message: "Network camera management is currently not supported by Wendy Agent for Mac."
        )
    }

    private static func unsupportedV4L2Controls() -> RPCError {
        RPCError(
            code: .unimplemented,
            message: "V4L2 camera controls are not available on macOS."
        )
    }
}
