import AVFoundation
import Foundation
import GRPCCore
import WendyAgentGRPC

/// Implements `wendy.agent.services.v2.WendySensorService`: reports the sensors
/// available on this host and streams their frames over one multiplexed RPC.
///
/// The microphone channel (1) reuses `AudioController` (int16-LE PCM); the camera
/// channel (2) reuses `CameraCapture` (H.264 Annex-B). Both producers are
/// injectable so the service can be tested without hardware.
struct SensorService: Wendy_Agent_Services_V2_WendySensorService.ServiceProtocol {
    static let micChannel: UInt32 = 1
    /// SensorFrame.flags bit 0: this frame is an H.264 keyframe (IDR).
    static let keyframeFlag: UInt32 = 1

    var audio: any AudioManaging = AudioController()
    var camera: any CameraCapturing = CameraCapture()
    /// This device's asset ID, echoed in the manifest so the Plan-1 consumer's
    /// `deviceAssetID != SourceAssetID` guard doesn't reject every stream. `nil`
    /// (unprovisioned) reports 0, matching the unset proto default.
    var assetID: Int32? = nil

    func getSensorManifest(
        request: ServerRequest<Wendy_Agent_Services_V2_GetSensorManifestRequest>,
        context: ServerContext
    ) async throws -> ServerResponse<Wendy_Lite_Sensorlink_SensorManifest> {
        var manifest = Wendy_Lite_Sensorlink_SensorManifest()
        manifest.deviceAssetID = assetID ?? 0
        if micAuthorized() {
            var descriptor = Wendy_Lite_Sensorlink_SensorDescriptor()
            descriptor.channelID = Self.micChannel
            descriptor.kind = .microphone
            descriptor.name = "mic0"
            let format = Self.micFormat(audio)
            var audioFormat = Wendy_Lite_Sensorlink_AudioFormat()
            audioFormat.codec = .pcmS16Le
            audioFormat.sampleRate = format.sampleRate
            audioFormat.channels = format.channels
            descriptor.audio = audioFormat
            manifest.sensors.append(descriptor)
        }
        if let cameraDescriptor = camera.descriptor() {
            manifest.sensors.append(cameraDescriptor)
        }
        return ServerResponse(message: manifest)
    }

    func streamSensors(
        request: ServerRequest<Wendy_Agent_Services_V2_StreamSensorsRequest>,
        context: ServerContext
    ) async throws -> StreamingServerResponse<Wendy_Lite_Sensorlink_SensorFrame> {
        let channels = Set(request.message.channelID)
        let audio = self.audio
        let camera = self.camera
        let wantsMic = channels.contains(Self.micChannel) && micAuthorized()
        let cameraDescriptor = camera.descriptor()
        let wantsCamera = cameraDescriptor.map { channels.contains($0.channelID) } ?? false

        return StreamingServerResponse { writer in
            // grpc-swift's writer is not safe for concurrent writes, so both
            // producers funnel through one actor that serializes every write.
            let serial = SerializedFrameWriter(writer)
            try await withThrowingTaskGroup(of: Void.self) { group in
                if wantsMic {
                    group.addTask { try await Self.pumpMic(audio, into: serial) }
                }
                if wantsCamera, let channel = cameraDescriptor?.channelID {
                    group.addTask {
                        try await Self.pumpCamera(camera, channel: channel, into: serial)
                    }
                }
                try await group.waitForAll()
            }
            return Metadata()
        }
    }

    private static func pumpMic(
        _ audio: any AudioManaging,
        into writer: SerializedFrameWriter
    ) async throws {
        var seq: UInt32 = 0
        for try await chunk in audio.audio(deviceID: 0, sampleRate: 48000, channels: 1) {
            var frame = Wendy_Lite_Sensorlink_SensorFrame()
            frame.channelID = micChannel
            frame.seq = seq
            seq &+= 1
            frame.payload = chunk.pcm
            try await writer.write(frame)
        }
    }

    private static func pumpCamera(
        _ camera: any CameraCapturing,
        channel: UInt32,
        into writer: SerializedFrameWriter
    ) async throws {
        var seq: UInt32 = 0
        for try await cameraFrame in camera.frames() {
            var frame = Wendy_Lite_Sensorlink_SensorFrame()
            frame.channelID = channel
            frame.seq = seq
            seq &+= 1
            frame.flags = cameraFrame.isKeyframe ? keyframeFlag : 0
            frame.payload = cameraFrame.annexB
            try await writer.write(frame)
        }
    }

    /// `AudioController.audio` ignores the requested sample rate/channels and
    /// streams whatever the hardware input format actually is, so the manifest
    /// must report that real format rather than the request we happen to pass.
    /// Probing needs live CoreAudio/AVAudioEngine access, which only the real
    /// `AudioController` has — a fake used in tests gets a fixed, documented
    /// default instead of a live probe.
    private static func micFormat(
        _ audio: any AudioManaging
    ) -> (
        sampleRate: UInt32, channels: UInt32
    ) {
        guard audio is AudioController else { return (sampleRate: 48000, channels: 1) }
        // Retain the engine for the whole probe: reading `inputFormat` off a
        // temporary `AVAudioEngine()` lets ARC deallocate the engine while its
        // input node still references it, which segfaults deep in AVFAudio
        // (AVAudioIONodeImpl::AUI() == nil). Keeping `engine` in a local and
        // extending its lifetime past the read avoids the use-after-free.
        let engine = AVAudioEngine()
        let format = engine.inputNode.inputFormat(forBus: 0)
        defer { withExtendedLifetime(engine) {} }
        guard format.sampleRate > 0, format.channelCount > 0 else { return (48000, 1) }
        return (sampleRate: UInt32(format.sampleRate), channels: UInt32(format.channelCount))
    }

    func micAuthorized() -> Bool {
        AVCaptureDevice.authorizationStatus(for: .audio) == .authorized
    }
}

/// Serializes writes to the single gRPC response stream shared by the concurrent
/// sensor producers. `RPCWriter` is not safe for concurrent writes; actor
/// isolation gives us the required one-at-a-time ordering.
private actor SerializedFrameWriter {
    private let writer: RPCWriter<Wendy_Lite_Sensorlink_SensorFrame>

    init(_ writer: RPCWriter<Wendy_Lite_Sensorlink_SensorFrame>) {
        self.writer = writer
    }

    func write(_ frame: Wendy_Lite_Sensorlink_SensorFrame) async throws {
        try await writer.write(frame)
    }
}
