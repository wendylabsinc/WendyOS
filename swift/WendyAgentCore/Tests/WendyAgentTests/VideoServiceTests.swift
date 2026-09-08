import Foundation
import GRPCCore
import Testing
import WendyAgentGRPC

@testable import WendyAgentCore

@Suite("VideoService adapters")
struct VideoServiceAdapterTests {
    @Test
    func `lists Mac cameras through the existing video contract`() async throws {
        let service = VideoService(
            camera: FakeCameraManager(devices: [
                CameraDeviceInfo(
                    id: 0,
                    uniqueID: "built-in",
                    name: "FaceTime HD Camera",
                    isExternal: false
                ),
                CameraDeviceInfo(
                    id: 1,
                    uniqueID: "external",
                    name: "USB Camera",
                    isExternal: true
                ),
            ])
        )

        let response = try await service.listVideoDevices(
            request: ServerRequest(
                metadata: [:],
                message: Wendy_Agent_Services_V1_ListVideoDevicesRequest()
            ),
            context: makeVideoContext(method: "ListVideoDevices")
        )
        let devices = try response.message.devices

        #expect(devices.map(\.id) == [0, 1])
        #expect(devices.map(\.name) == ["FaceTime HD Camera", "USB Camera"])
        #expect(devices.map(\.transport) == [.unknown, .usb])
        #expect(devices.map(\.online) == [true, true])
        #expect(devices.allSatisfy { $0.path.isEmpty && $0.stableID.isEmpty })
    }

    @Test
    func `streams H264 Annex B frames for a listed camera`() async throws {
        let camera = CameraDeviceInfo(
            id: 7,
            uniqueID: "camera-7",
            name: "Studio Camera",
            isExternal: true
        )
        let expected = EncodedCameraFrame(
            data: Data([0, 0, 0, 1, 0x67]),
            timestampNanoseconds: 123
        )
        let service = VideoService(
            camera: FakeCameraManager(devices: [camera], frames: [camera.uniqueID: [expected]])
        )

        var request = Wendy_Agent_Services_V1_StreamVideoRequest()
        request.deviceID = camera.id
        let response = try await service.streamVideo(
            request: ServerRequest(metadata: [:], message: request),
            context: makeVideoContext(method: "StreamVideo")
        )
        let writer = VideoCollectingWriter<Wendy_Agent_Services_V1_VideoFrame>()
        _ = try await response.accepted.get().producer(RPCWriter(wrapping: writer))

        let frames = writer.snapshot()
        #expect(frames.count == 1)
        #expect(frames[0].data == expected.data)
        #expect(frames[0].timestampNs == expected.timestampNanoseconds)
        #expect(frames[0].codec == .h264)
    }

    @Test
    func `discovers a camera when streaming starts before listing`() async throws {
        let camera = CameraDeviceInfo(
            id: 0,
            uniqueID: "camera-0",
            name: "Built-in Camera",
            isExternal: false
        )
        let service = VideoService(camera: FakeCameraManager(devices: [camera]))

        _ = try await service.streamVideo(
            request: ServerRequest(
                metadata: [:],
                message: Wendy_Agent_Services_V1_StreamVideoRequest()
            ),
            context: makeVideoContext(method: "StreamVideo")
        )
    }

    @Test
    func `rejects video modes the Mac implementation cannot honor`() async {
        let service = VideoService(camera: FakeCameraManager())

        var mutableRawRequest = Wendy_Agent_Services_V1_StreamVideoRequest()
        mutableRawRequest.codec = .raw
        let rawRequest = mutableRawRequest
        await expectVideoRPCError(code: .unimplemented) {
            _ = try await service.streamVideo(
                request: ServerRequest(metadata: [:], message: rawRequest),
                context: makeVideoContext(method: "StreamVideo")
            )
        }

        var mutableSizedRequest = Wendy_Agent_Services_V1_StreamVideoRequest()
        mutableSizedRequest.width = 640
        mutableSizedRequest.height = 480
        let sizedRequest = mutableSizedRequest
        await expectVideoRPCError(code: .unimplemented) {
            _ = try await service.streamVideo(
                request: ServerRequest(metadata: [:], message: sizedRequest),
                context: makeVideoContext(method: "StreamVideo")
            )
        }
    }

    @Test
    func `reports a missing camera without opening capture`() async {
        let service = VideoService(camera: FakeCameraManager())
        await expectVideoRPCError(code: .notFound) {
            _ = try await service.streamVideo(
                request: ServerRequest(
                    metadata: [:],
                    message: Wendy_Agent_Services_V1_StreamVideoRequest()
                ),
                context: makeVideoContext(method: "StreamVideo")
            )
        }
    }

    @Test
    func `cancelling the RPC tears down camera streaming`() async throws {
        let camera = CameraDeviceInfo(
            id: 0,
            uniqueID: "camera-0",
            name: "Built-in Camera",
            isExternal: false
        )
        let probe = VideoStreamTerminationProbe()
        let service = VideoService(
            camera: WaitingCameraManager(device: camera, probe: probe)
        )
        let response = try await service.streamVideo(
            request: ServerRequest(
                metadata: [:],
                message: Wendy_Agent_Services_V1_StreamVideoRequest()
            ),
            context: makeVideoContext(method: "StreamVideo")
        )
        let writer = VideoCollectingWriter<Wendy_Agent_Services_V1_VideoFrame>()
        let producer = Task {
            try await response.accepted.get().producer(RPCWriter(wrapping: writer))
        }

        #expect(await probe.waitUntilStarted())
        producer.cancel()
        let terminatedAfterCancellation = await probe.waitUntilTerminated()
        if !terminatedAfterCancellation {
            probe.finish()
        }
        _ = await producer.result

        #expect(terminatedAfterCancellation)
    }

    @Test
    func `maps camera access denial onto the stream`() async throws {
        let camera = CameraDeviceInfo(
            id: 0,
            uniqueID: "camera-0",
            name: "Built-in Camera",
            isExternal: false
        )
        let service = VideoService(
            camera: FakeCameraManager(
                devices: [camera],
                streamError: CameraCaptureError.accessDenied
            )
        )
        let response = try await service.streamVideo(
            request: ServerRequest(
                metadata: [:],
                message: Wendy_Agent_Services_V1_StreamVideoRequest()
            ),
            context: makeVideoContext(method: "StreamVideo")
        )
        let writer = VideoCollectingWriter<Wendy_Agent_Services_V1_VideoFrame>()

        do {
            _ = try await response.accepted.get().producer(RPCWriter(wrapping: writer))
            Issue.record("Expected camera access denial")
        } catch let error as RPCError {
            #expect(error.code == .permissionDenied)
            #expect(error.message.contains("Camera access"))
        } catch {
            Issue.record("Expected RPCError, got \(error)")
        }
    }
}

@Suite("VideoToolbox H264 framing")
struct VideoToolboxH264FramingTests {
    @Test
    func `prepends parameter sets to keyframes`() {
        let avcc = Data([0, 0, 0, 2, 0x65, 0xAA])
        let output = annexBFromAVCC(
            avcc,
            nalUnitHeaderLength: 4,
            parameterSets: [Data([0x67, 0x01]), Data([0x68, 0x02])],
            isKeyframe: true
        )

        #expect(
            output
                == Data([
                    0, 0, 0, 1, 0x67, 0x01,
                    0, 0, 0, 1, 0x68, 0x02,
                    0, 0, 0, 1, 0x65, 0xAA,
                ])
        )
    }

    @Test
    func `converts multiple NAL units with the reported header width`() {
        let avcc = Data([0, 1, 0x41, 0, 3, 0x01, 0x02, 0x03])
        let output = annexBFromAVCC(
            avcc,
            nalUnitHeaderLength: 2,
            parameterSets: [],
            isKeyframe: false
        )

        #expect(output == Data([0, 0, 0, 1, 0x41, 0, 0, 0, 1, 0x01, 0x02, 0x03]))
    }

    @Test
    func `drops a truncated trailing NAL unit safely`() {
        let avcc = Data([0, 0, 0, 1, 0x41, 0, 0, 0, 3, 0x01])
        let output = annexBFromAVCC(
            avcc,
            nalUnitHeaderLength: 4,
            parameterSets: [],
            isKeyframe: false
        )

        #expect(output == Data([0, 0, 0, 1, 0x41]))
    }
}

private struct FakeCameraManager: CameraManaging {
    var devices: [CameraDeviceInfo] = []
    var frames: [String: [EncodedCameraFrame]] = [:]
    var streamError: (any Error)?

    func devices() async -> [CameraDeviceInfo] {
        devices
    }

    func frames(
        for device: CameraDeviceInfo
    ) -> AsyncThrowingStream<EncodedCameraFrame, any Error> {
        let frames = frames[device.uniqueID] ?? []
        let streamError = self.streamError
        return AsyncThrowingStream { continuation in
            for frame in frames {
                continuation.yield(frame)
            }
            continuation.finish(throwing: streamError)
        }
    }
}

private struct WaitingCameraManager: CameraManaging {
    let device: CameraDeviceInfo
    let probe: VideoStreamTerminationProbe

    func devices() async -> [CameraDeviceInfo] {
        [device]
    }

    func frames(
        for device: CameraDeviceInfo
    ) -> AsyncThrowingStream<EncodedCameraFrame, any Error> {
        probe.stream()
    }
}

private final class VideoStreamTerminationProbe: @unchecked Sendable {
    private let lock = NSLock()
    private var started = false
    private var terminated = false
    private var continuation: AsyncThrowingStream<EncodedCameraFrame, any Error>.Continuation?

    func stream() -> AsyncThrowingStream<EncodedCameraFrame, any Error> {
        AsyncThrowingStream { continuation in
            lock.withLock {
                started = true
                self.continuation = continuation
            }
            continuation.onTermination = { [weak self] _ in
                self?.lock.withLock {
                    self?.terminated = true
                    self?.continuation = nil
                }
            }
        }
    }

    func finish() {
        let continuation = lock.withLock { self.continuation }
        continuation?.finish()
    }

    func waitUntilStarted() async -> Bool {
        await waitUntil { self.started }
    }

    func waitUntilTerminated() async -> Bool {
        await waitUntil { self.terminated }
    }

    private func waitUntil(_ predicate: @escaping @Sendable () -> Bool) async -> Bool {
        for _ in 0..<100 {
            if lock.withLock(predicate) { return true }
            try? await Task.sleep(for: .milliseconds(1))
        }
        return false
    }
}

private final class VideoCollectingWriter<Element: Sendable>: RPCWriterProtocol,
    @unchecked Sendable
{
    private let queue = DispatchQueue(label: "wendy.tests.video-collecting-writer")
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

private func expectVideoRPCError(
    code: RPCError.Code,
    operation: @Sendable () async throws -> Void
) async {
    do {
        try await operation()
        Issue.record("Expected RPCError with code \(code)")
    } catch let error as RPCError {
        #expect(error.code == code)
    } catch {
        Issue.record("Expected RPCError, got \(error)")
    }
}

private func makeVideoContext(method: String) -> ServerContext {
    ServerContext(
        descriptor: MethodDescriptor(
            fullyQualifiedService: "wendy.agent.services.v1.WendyVideoService",
            method: method
        ),
        remotePeer: "in-process:test",
        localPeer: "in-process:test",
        cancellation: .init()
    )
}
