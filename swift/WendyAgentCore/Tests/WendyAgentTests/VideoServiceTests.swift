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
        let expected = CameraFrame(
            annexB: Data([0, 0, 0, 1, 0x67]),
            isKeyframe: true,
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
        #expect(frames[0].data == expected.annexB)
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
    func `limits concurrent capture sessions`() async throws {
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
        let firstResponse = try await makeStreamResponse(service: service)
        let secondResponse = try await makeStreamResponse(service: service)
        let firstWriter = VideoCollectingWriter<Wendy_Agent_Services_V1_VideoFrame>()
        let secondWriter = VideoCollectingWriter<Wendy_Agent_Services_V1_VideoFrame>()
        let firstProducer = Task {
            try await firstResponse.accepted.get().producer(RPCWriter(wrapping: firstWriter))
        }
        let secondProducer = Task {
            try await secondResponse.accepted.get().producer(RPCWriter(wrapping: secondWriter))
        }
        #expect(await probe.waitUntilStarted(count: 2))

        let thirdResponse = try await makeStreamResponse(service: service)
        let thirdWriter = VideoCollectingWriter<Wendy_Agent_Services_V1_VideoFrame>()
        do {
            _ = try await thirdResponse.accepted.get().producer(RPCWriter(wrapping: thirdWriter))
            Issue.record("Expected the concurrent stream limit to reject a third session")
        } catch let error as RPCError {
            #expect(error.code == .resourceExhausted)
        } catch {
            Issue.record("Expected RPCError, got \(error)")
        }

        firstProducer.cancel()
        secondProducer.cancel()
        let bothTerminated = await probe.waitUntilTerminated(count: 2)
        if !bothTerminated {
            probe.finishAll()
        }
        _ = await firstProducer.result
        _ = await secondProducer.result
        #expect(bothTerminated)
    }

    @Test
    func `camera control RPCs preserve IDs values persistence and reset names`() async throws {
        let controls = VideoControlsProbe()
        let uid = "0x1000000c456366"
        let service = VideoService(
            camera: FakeCameraManager(devices: [
                CameraDeviceInfo(id: 0, uniqueID: uid, name: "USB Camera", isExternal: true)
            ]),
            controls: controls
        )
        let listed = try await service.getCameraControls(
            request: ServerRequest(
                metadata: [:],
                message: Wendy_Agent_Services_V1_GetCameraControlsRequest()
            ),
            context: makeVideoContext(method: "GetCameraControls")
        )
        let metadata = try #require(listed.message.controls.first)
        #expect(metadata.name == "gain" && metadata.minimum == 0 && metadata.maximum == 100)
        #expect(metadata.defaultValue == 0 && metadata.mutable)
        var setting = Wendy_Agent_Services_V1_CameraControlSetting()
        setting.name = "gain"
        setting.value = 20
        var set = Wendy_Agent_Services_V1_SetCameraControlsRequest()
        set.controls = [setting]
        set.persist = true
        let applied = try await service.setCameraControls(
            request: ServerRequest(metadata: [:], message: set),
            context: makeVideoContext(method: "SetCameraControls")
        )
        #expect(try applied.message.results.first?.applied == true)
        var reset = Wendy_Agent_Services_V1_ResetCameraControlsRequest()
        reset.names = ["gain"]
        let restored = try await service.resetCameraControls(
            request: ServerRequest(metadata: [:], message: reset),
            context: makeVideoContext(method: "ResetCameraControls")
        )
        #expect(try restored.message.results.first?.name == "gain")
        #expect(await controls.identities == [uid, uid, uid])
        #expect(await controls.persist == true)
        #expect(await controls.setValue == 20)
        #expect(await controls.resetNames == ["gain"])
    }

    @Test
    func `restores persisted controls at startup and once per reopened stream`() async throws {
        let uid = "0x1000000c456366"
        let camera = CameraDeviceInfo(id: 0, uniqueID: uid, name: "USB Camera", isExternal: true)
        let frame = CameraFrame(
            annexB: Data([0, 0, 0, 1, 0x67]),
            isKeyframe: true,
            timestampNanoseconds: 1
        )
        let controls = VideoControlsProbe()
        let service = VideoService(
            camera: FakeCameraManager(devices: [camera], frames: [uid: [frame, frame]]),
            controls: controls
        )
        await service.restoreCameraControls()
        for _ in 0..<2 {
            let response = try await makeStreamResponse(service: service)
            let writer = VideoCollectingWriter<Wendy_Agent_Services_V1_VideoFrame>()
            _ = try await response.accepted.get().producer(RPCWriter(wrapping: writer))
            #expect(writer.snapshot().count == 2)
        }
        #expect(await controls.restoredIdentities == [uid, uid, uid])
    }

    @Test
    func
        `controls reject built-in cameras missing IDs and ambiguous identities before backend access`()
        async
    {
        let controls = VideoControlsProbe()
        for devices in [
            [],
            [CameraDeviceInfo(id: 0, uniqueID: "built-in", name: "FaceTime", isExternal: false)],
            [CameraDeviceInfo(id: 0, uniqueID: "unknown", name: "USB Camera", isExternal: true)],
            [
                CameraDeviceInfo(
                    id: 0,
                    uniqueID: "0x1000000c456366",
                    name: "USB Camera",
                    isExternal: true
                ),
                CameraDeviceInfo(
                    id: 1,
                    uniqueID: "0x1000000c456366",
                    name: "USB Camera",
                    isExternal: true
                ),
            ],
        ] {
            let service = VideoService(
                camera: FakeCameraManager(devices: devices),
                controls: controls
            )
            do {
                _ = try await service.getCameraControls(
                    request: ServerRequest(
                        metadata: [:],
                        message: Wendy_Agent_Services_V1_GetCameraControlsRequest()
                    ),
                    context: makeVideoContext(method: "GetCameraControls")
                )
                Issue.record("Expected unavailable camera controls")
            } catch is RPCError {} catch { Issue.record("Expected RPCError") }
        }
        #expect(await controls.identities.isEmpty)
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
                streamError: CameraError.accessDenied
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
    var frames: [String: [CameraFrame]] = [:]
    var streamError: (any Error)?

    func devices() async -> [CameraDeviceInfo] {
        devices
    }

    func frames(
        for device: CameraDeviceInfo
    ) -> AsyncThrowingStream<CameraFrame, any Error> {
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
    ) -> AsyncThrowingStream<CameraFrame, any Error> {
        probe.stream()
    }
}

private final class VideoStreamTerminationProbe: @unchecked Sendable {
    private let lock = NSLock()
    private var startedCount = 0
    private var terminatedCount = 0
    private var continuations:
        [UUID:
            AsyncThrowingStream<CameraFrame, any Error>.Continuation] = [:]

    func stream() -> AsyncThrowingStream<CameraFrame, any Error> {
        let id = UUID()
        return AsyncThrowingStream { continuation in
            lock.withLock {
                startedCount += 1
                continuations[id] = continuation
            }
            continuation.onTermination = { [weak self] _ in
                self?.lock.withLock {
                    self?.terminatedCount += 1
                    self?.continuations[id] = nil
                }
            }
        }
    }

    func finish() {
        let continuation = lock.withLock { continuations.values.first }
        continuation?.finish()
    }

    func finishAll() {
        let continuations = lock.withLock { Array(self.continuations.values) }
        for continuation in continuations {
            continuation.finish()
        }
    }

    func waitUntilStarted(count: Int = 1) async -> Bool {
        await waitUntil { self.startedCount >= count }
    }

    func waitUntilTerminated(count: Int = 1) async -> Bool {
        await waitUntil { self.terminatedCount >= count }
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

private func makeStreamResponse(
    service: VideoService
) async throws -> StreamingServerResponse<Wendy_Agent_Services_V1_VideoFrame> {
    try await service.streamVideo(
        request: ServerRequest(
            metadata: [:],
            message: Wendy_Agent_Services_V1_StreamVideoRequest()
        ),
        context: makeVideoContext(method: "StreamVideo")
    )
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

private actor VideoControlsProbe: CameraControlManaging {
    var identities: [String] = []
    var persist = false
    var setValue: Int32?
    var resetNames: [String] = []
    var restoredIdentities: [String] = []
    func list(uniqueID: String) -> [UVCCameraControl] {
        identities.append(uniqueID)
        return [
            UVCCameraControl(
                name: "gain",
                value: 0,
                minimum: 0,
                maximum: 100,
                step: 1,
                defaultValue: 0,
                mutable: true,
                supportedModes: nil
            )
        ]
    }
    func set(uniqueID: String, values: [(String, Int32)], persist: Bool) -> [CameraControlOutcome] {
        identities.append(uniqueID)
        self.persist = persist
        setValue = values.first?.1
        return values.map { CameraControlOutcome(name: $0.0, applied: true, detail: "") }
    }
    func reset(uniqueID: String, names: [String]) -> [CameraControlOutcome] {
        identities.append(uniqueID)
        resetNames = names
        return names.map { CameraControlOutcome(name: $0, applied: true, detail: "") }
    }
    func restore(uniqueID: String) { restoredIdentities.append(uniqueID) }
}
