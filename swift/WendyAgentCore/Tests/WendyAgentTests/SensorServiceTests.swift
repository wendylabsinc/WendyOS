import Foundation
import GRPCCore
import Testing

@testable import WendyAgentCore
@testable import WendyAgentGRPC

private struct FakeAudio: AudioManaging {
    func listDevices(typeFilter: AudioKind?) async throws -> [AudioDeviceInfo] { [] }
    func setDefault(deviceID: UInt32) async throws {}
    func levels(
        deviceID: UInt32,
        rateHz: UInt32
    ) -> AsyncThrowingStream<(peakDb: Float, rmsDb: Float), any Error> {
        AsyncThrowingStream { $0.finish() }
    }
    func audio(
        deviceID: UInt32,
        sampleRate: UInt32,
        channels: UInt32
    ) -> AsyncThrowingStream<(pcm: Data, sampleRate: UInt32, channels: UInt32), any Error> {
        AsyncThrowingStream { continuation in
            continuation.yield((pcm: Data([1, 2, 3, 4]), sampleRate: 48000, channels: 1))
            continuation.yield((pcm: Data([5, 6, 7, 8]), sampleRate: 48000, channels: 1))
            continuation.finish()
        }
    }
}

private struct FakeCamera: CameraCapturing {
    var descriptorValue: Wendy_Lite_Sensorlink_SensorDescriptor?
    var frameList: [CameraFrame] = []

    func descriptor() -> Wendy_Lite_Sensorlink_SensorDescriptor? { descriptorValue }

    func frames() -> AsyncThrowingStream<CameraFrame, any Error> {
        let frames = frameList
        return AsyncThrowingStream { continuation in
            for frame in frames { continuation.yield(frame) }
            continuation.finish()
        }
    }
}

private func cameraDescriptor() -> Wendy_Lite_Sensorlink_SensorDescriptor {
    var descriptor = Wendy_Lite_Sensorlink_SensorDescriptor()
    descriptor.channelID = CameraCapture.channel
    descriptor.kind = .camera
    descriptor.name = "cam0"
    var video = Wendy_Lite_Sensorlink_VideoFormat()
    video.codec = .h264
    video.width = 1920
    video.height = 1080
    video.fps = 30
    descriptor.video = video
    return descriptor
}

private final class CollectingWriter<Element: Sendable>: RPCWriterProtocol, @unchecked Sendable {
    private let queue = DispatchQueue(label: "wendy.tests.sensor-collecting-writer")
    private var elements: [Element] = []

    func write(_ element: Element) async throws {
        queue.sync { elements.append(element) }
    }

    func write(contentsOf elements: some Sequence<Element>) async throws {
        queue.sync { self.elements.append(contentsOf: elements) }
    }

    func snapshot() -> [Element] {
        queue.sync { elements }
    }
}

private func makeServerContext(method: String) -> ServerContext {
    ServerContext(
        descriptor: MethodDescriptor(
            fullyQualifiedService: "wendy.agent.services.v2.WendySensorService",
            method: method
        ),
        remotePeer: "in-process:test",
        localPeer: "in-process:test",
        cancellation: .init()
    )
}

@Test func manifestReportsMicAndStreamYieldsPCMFrames() async throws {
    // Inject a no-op camera so this test never touches real capture hardware.
    let svc = SensorService(audio: FakeAudio(), camera: FakeCamera())

    let manifestResponse = try await svc.getSensorManifest(
        request: ServerRequest(
            metadata: [:],
            message: Wendy_Agent_Services_V2_GetSensorManifestRequest()
        ),
        context: makeServerContext(method: "GetSensorManifest")
    )
    let manifest = try manifestResponse.message
    #expect(manifest.sensors.contains { $0.kind == .microphone })
    let mic = try #require(manifest.sensors.first { $0.kind == .microphone })
    #expect(mic.channelID == SensorService.micChannel)
    #expect(mic.audio.codec == .pcmS16Le)
    #expect(mic.audio.sampleRate == 48000)
    #expect(mic.audio.channels == 1)

    var request = Wendy_Agent_Services_V2_StreamSensorsRequest()
    request.channelID = [SensorService.micChannel]
    let streamResponse = try await svc.streamSensors(
        request: ServerRequest(metadata: [:], message: request),
        context: makeServerContext(method: "StreamSensors")
    )
    let contents = try streamResponse.accepted.get()
    let writer = CollectingWriter<Wendy_Lite_Sensorlink_SensorFrame>()
    _ = try await contents.producer(RPCWriter(wrapping: writer))

    let frames = writer.snapshot()
    #expect(frames.count == 2)
    #expect(frames.allSatisfy { $0.channelID == SensorService.micChannel })
    #expect(frames.first?.payload == Data([1, 2, 3, 4]))
    #expect(frames.last?.payload == Data([5, 6, 7, 8]))
    #expect(frames.map(\.seq) == [0, 1])
}

@Test func manifestReportsCameraAndStreamYieldsAnnexBFrames() async throws {
    let keyframe = CameraFrame(annexB: Data([0, 0, 0, 1, 0x67]), isKeyframe: true)
    let delta = CameraFrame(annexB: Data([0, 0, 0, 1, 0x41]), isKeyframe: false)
    let camera = FakeCamera(descriptorValue: cameraDescriptor(), frameList: [keyframe, delta])
    let svc = SensorService(audio: FakeAudio(), camera: camera)

    let manifestResponse = try await svc.getSensorManifest(
        request: ServerRequest(
            metadata: [:],
            message: Wendy_Agent_Services_V2_GetSensorManifestRequest()
        ),
        context: makeServerContext(method: "GetSensorManifest")
    )
    let manifest = try manifestResponse.message
    let cam = try #require(manifest.sensors.first { $0.kind == .camera })
    #expect(cam.channelID == CameraCapture.channel)
    #expect(cam.video.codec == .h264)
    #expect(cam.video.width == 1920)
    #expect(cam.video.height == 1080)

    var request = Wendy_Agent_Services_V2_StreamSensorsRequest()
    request.channelID = [CameraCapture.channel]
    let streamResponse = try await svc.streamSensors(
        request: ServerRequest(metadata: [:], message: request),
        context: makeServerContext(method: "StreamSensors")
    )
    let contents = try streamResponse.accepted.get()
    let writer = CollectingWriter<Wendy_Lite_Sensorlink_SensorFrame>()
    _ = try await contents.producer(RPCWriter(wrapping: writer))

    let frames = writer.snapshot()
    #expect(frames.count == 2)
    #expect(frames.allSatisfy { $0.channelID == CameraCapture.channel })
    #expect(frames.first?.payload == Data([0, 0, 0, 1, 0x67]))
    #expect(frames.first?.flags == SensorService.keyframeFlag)
    #expect(frames.last?.payload == Data([0, 0, 0, 1, 0x41]))
    #expect(frames.last?.flags == 0)
    #expect(frames.map(\.seq) == [0, 1])
}

@Test func manifestCarriesAssetIDAndBothChannelsFanInThroughOneWriter() async throws {
    let keyframe = CameraFrame(annexB: Data([0, 0, 0, 1, 0x67]), isKeyframe: true)
    let camera = FakeCamera(descriptorValue: cameraDescriptor(), frameList: [keyframe])
    let svc = SensorService(audio: FakeAudio(), camera: camera, assetID: 42)

    let manifestResponse = try await svc.getSensorManifest(
        request: ServerRequest(
            metadata: [:],
            message: Wendy_Agent_Services_V2_GetSensorManifestRequest()
        ),
        context: makeServerContext(method: "GetSensorManifest")
    )
    let manifest = try manifestResponse.message
    #expect(manifest.deviceAssetID == 42)
    #expect(manifest.sensors.contains { $0.kind == .microphone })
    #expect(manifest.sensors.contains { $0.kind == .camera })

    var request = Wendy_Agent_Services_V2_StreamSensorsRequest()
    request.channelID = [SensorService.micChannel, CameraCapture.channel]
    let streamResponse = try await svc.streamSensors(
        request: ServerRequest(metadata: [:], message: request),
        context: makeServerContext(method: "StreamSensors")
    )
    let contents = try streamResponse.accepted.get()
    let writer = CollectingWriter<Wendy_Lite_Sensorlink_SensorFrame>()
    _ = try await contents.producer(RPCWriter(wrapping: writer))

    // Both producers funnel through the one SerializedFrameWriter actor: assert
    // frames from both channels actually landed in its output.
    let frames = writer.snapshot()
    #expect(frames.contains { $0.channelID == SensorService.micChannel })
    #expect(frames.contains { $0.channelID == CameraCapture.channel })
    #expect(frames.count == 3)
}
