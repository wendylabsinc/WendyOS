import AVFoundation
import Foundation
import VideoToolbox
import WendyAgentGRPC

/// One encoded H.264 access unit in Annex-B framing (start-code delimited),
/// with SPS/PPS prepended on keyframes so the stream is self-describing.
struct CameraFrame: Equatable, Sendable {
    var annexB: Data
    var isKeyframe: Bool
    /// Capture presentation time, for consumers that carry timestamps on the
    /// wire (`WendyVideoService`). Sensor channels ignore it.
    var timestampNanoseconds: UInt64 = 0
}

/// Produces H.264 camera frames. Injectable so `SensorService` can be tested
/// with a fake that yields canned frames instead of touching real hardware.
protocol CameraCapturing: Sendable {
    func frames() -> AsyncThrowingStream<CameraFrame, any Error>
    func descriptor() -> Wendy_Lite_Sensorlink_SensorDescriptor?
}

/// A camera as `WendyVideoService` presents it: a small numeric ID (position
/// in the deterministic enumeration order), the AVFoundation unique ID it maps
/// back to, and the display name.
struct CameraDeviceInfo: Equatable, Sendable {
    let id: UInt32
    let uniqueID: String
    let name: String
    let isExternal: Bool
}

/// Enumerates cameras and opens an encoded stream on a chosen one. Injectable
/// so `WendyVideoService` can be tested without hardware.
protocol CameraManaging: Sendable {
    func devices() async -> [CameraDeviceInfo]
    func frames(for device: CameraDeviceInfo) -> AsyncThrowingStream<CameraFrame, any Error>
}

/// Converts a VideoToolbox AVCC bitstream (repeated 4-byte big-endian length +
/// NALU) to Annex-B (each length replaced by the `00 00 00 01` start code). On a
/// keyframe the parameter sets are prepended (`SC+SPS…`, `SC+PPS…`) so a consumer
/// that joined mid-stream can decode from the first keyframe alone.
///
/// Pure `Data` math — this is the one piece of the camera path that is unit
/// tested without hardware. A truncated trailing length prefix is dropped rather
/// than trusted, so a malformed buffer can never over-read.
func annexBFromAVCC(_ avcc: Data, sps: [Data], pps: [Data], isKeyframe: Bool) -> Data {
    annexBFromAVCC(avcc, nalUnitHeaderLength: 4, parameterSets: sps + pps, isKeyframe: isKeyframe)
}

/// The general form: VideoToolbox reports the NAL length-prefix width with the
/// parameter sets (1–4 bytes; 4 in practice), and the caller passes the
/// parameter sets in the order they should precede a keyframe.
func annexBFromAVCC(
    _ avcc: Data,
    nalUnitHeaderLength: Int,
    parameterSets: [Data],
    isKeyframe: Bool
) -> Data {
    guard (1...4).contains(nalUnitHeaderLength) else { return Data() }

    let startCode = Data([0, 0, 0, 1])
    var out = Data()
    if isKeyframe {
        for set in parameterSets {
            out += startCode
            out += set
        }
    }

    var index = avcc.startIndex
    while index + nalUnitHeaderLength <= avcc.endIndex {
        var length = 0
        for offset in 0..<nalUnitHeaderLength {
            length = (length << 8) | Int(avcc[index + offset])
        }
        index += nalUnitHeaderLength
        guard length > 0, index + length <= avcc.endIndex else { break }
        out += startCode
        out += avcc[index..<index + length]
        index += length
    }
    return out
}

enum CameraError: Error, CustomStringConvertible {
    case accessDenied
    case noDevice
    case deviceNotFound
    case cannotAddInput
    case cannotAddOutput
    /// Every format the camera advertises is a vendor/control mode, not a
    /// picture. The associated value lists them (`"4x12305, 16x15505"`).
    case noStreamableFormat(String)
    /// The camera could not be locked to pin a streamable format.
    case cannotPinFormat(String)
    case vtStatus(String, OSStatus)

    var description: String {
        switch self {
        case .accessDenied: return "Camera access is not allowed for Wendy Agent."
        case .noDevice: return "No default video capture device available."
        case .deviceNotFound: return "The selected camera is no longer available."
        case .cannotAddInput:
            return "The selected camera could not be added to the capture session."
        case .cannotAddOutput:
            return "The camera encoder output could not be added to the capture session."
        case .noStreamableFormat(let formats):
            return
                "The selected camera offers only vendor or control modes (\(formats)), not a video format Wendy Agent can stream."
        case .cannotPinFormat(let reason):
            return "The selected camera's video format could not be selected: \(reason)"
        case .vtStatus(let action, let status):
            return "VideoToolbox \(action) failed (OSStatus \(status))."
        }
    }
}

/// One capture format as `CaptureFormatPolicy` sees it: plain numbers, so the
/// policy is testable without a camera.
struct CaptureFormatCandidate: Equatable, Sendable {
    var width: Int32
    var height: Int32
    var maxFrameRate: Double
}

/// What `CameraCaptureSession` does about the camera's format before it starts.
enum CaptureFormatChoice: Equatable, Sendable {
    /// Every format is a real picture: leave the choice to the session preset,
    /// exactly as before this policy existed.
    case sessionDefault
    /// Some formats are vendor/control modes the preset could land on: pin the
    /// format at this index of `AVCaptureDevice.formats`.
    case pin(index: Int)
    /// No format is a picture: refuse to start the session at all.
    case unusable
}

/// Keeps vendor/control modes away from AVFoundation.
///
/// Why: on macOS the default session preset is `.high` and `.inputPriority` is
/// unavailable, so `startRunning()` picks `activeFormat` itself. On a TOPDON
/// TC001 (`2bdf:0102`, macOS 26.2) it picked `'yuvs' 16x15505`, a control mode
/// (also the camera's largest by pixel count). The system video-effects
/// pipeline then configured itself for 16x15505 and built a Metal texture
/// that failed validation; Metal asserts on that, so the agent died with
/// SIGABRT on `com.apple.cmio.portraitblur.pteffect_init`. It did so with
/// every effect reported inactive, and the SDK documents no macOS opt-out
/// from that pipeline, so the only lever here is the format it is handed.
enum CaptureFormatPolicy {
    /// The bounds wendy-console's mac-fire-detector (`RGBCaptureSafety`) uses
    /// to keep narrow vendor modes out of Apple's graph: a picture is at least
    /// 64 px and at most 8192 px on each side, and no more than 4:1.
    static let sideRange: ClosedRange<Int32> = 64...8192
    static let maxAspectRatio: Int64 = 4

    /// Picture shapes. Vendor modes that pass the bounds above still tend to
    /// be off-standard (the TC001's 512x484 is a stacked picture-plus-
    /// temperature mode), so among usable formats these shapes are preferred.
    static let standardAspectRatios: [(Int64, Int64)] = [
        (4, 3), (16, 9), (16, 10), (3, 2), (5, 4), (1, 1),
    ]

    static func isPicture(width: Int32, height: Int32) -> Bool {
        guard sideRange.contains(width), sideRange.contains(height) else { return false }
        let long = Int64(max(width, height))
        let short = Int64(min(width, height))
        return long <= short * maxAspectRatio
    }

    static func hasStandardAspectRatio(width: Int32, height: Int32) -> Bool {
        let long = Int64(max(width, height))
        let short = Int64(min(width, height))
        return standardAspectRatios.contains { long * $0.1 == short * $0.0 }
    }

    static func choose(from candidates: [CaptureFormatCandidate]) -> CaptureFormatChoice {
        let usable = candidates.indices.filter {
            isPicture(width: candidates[$0].width, height: candidates[$0].height)
        }
        guard !usable.isEmpty else { return .unusable }
        if usable.count == candidates.count { return .sessionDefault }

        let standard = usable.filter {
            hasStandardAspectRatio(width: candidates[$0].width, height: candidates[$0].height)
        }
        let pool = standard.isEmpty ? usable : standard
        // Largest picture, then the fastest; `max(by:)` keeps the first of equals.
        let best = pool.max { left, right in
            let l = candidates[left]
            let r = candidates[right]
            let lArea = Int64(l.width) * Int64(l.height)
            let rArea = Int64(r.width) * Int64(r.height)
            if lArea != rArea { return lArea < rArea }
            return l.maxFrameRate < r.maxFrameRate
        }
        guard let best else { return .unusable }
        return .pin(index: best)
    }

    /// `"4x12305, 16x15505"`: the distinct shapes, in advertised order.
    static func summary(of candidates: [CaptureFormatCandidate]) -> String {
        var seen = Set<String>()
        return candidates.map { "\($0.width)x\($0.height)" }
            .filter { seen.insert($0).inserted }
            .joined(separator: ", ")
    }
}

extension CaptureFormatCandidate {
    init(_ format: AVCaptureDevice.Format) {
        let dims = CMVideoFormatDescriptionGetDimensions(format.formatDescription)
        self.init(
            width: dims.width,
            height: dims.height,
            maxFrameRate: format.videoSupportedFrameRateRanges.map(\.maxFrameRate).max() ?? 0
        )
    }
}

extension AVCaptureDevice {
    /// The format `CameraCaptureSession` pins before it starts, or `nil` to
    /// keep the session preset's choice. Throws when nothing is streamable.
    func streamableFormatToPin() throws -> AVCaptureDevice.Format? {
        let formats = self.formats
        let candidates = formats.map(CaptureFormatCandidate.init)
        switch CaptureFormatPolicy.choose(from: candidates) {
        case .sessionDefault: return nil
        case .pin(let index): return formats[index]
        case .unusable:
            throw CameraError.noStreamableFormat(CaptureFormatPolicy.summary(of: candidates))
        }
    }
}

/// Camera enumeration and per-device streaming for `WendyVideoService`, on the
/// same capture pipeline `SensorService` uses for its default-camera channel.
struct AVCaptureCameraManager: CameraManaging {
    var averageBitRate: Int32 = 4_000_000

    func devices() async -> [CameraDeviceInfo] {
        await BlockingExecutor.run {
            Self.captureDevices().enumerated().map { index, device in
                CameraDeviceInfo(
                    id: UInt32(index),
                    uniqueID: device.uniqueID,
                    name: device.localizedName,
                    isExternal: device.deviceType == .external
                )
            }
        }
    }

    func frames(for device: CameraDeviceInfo) -> AsyncThrowingStream<CameraFrame, any Error> {
        let bitRate = averageBitRate
        return AsyncThrowingStream(bufferingPolicy: .bufferingNewest(1)) { continuation in
            let session = CameraCaptureSession(bitRate: bitRate, deviceUniqueID: device.uniqueID)
            do {
                try session.start(continuation: continuation)
            } catch {
                continuation.finish(throwing: error)
                return
            }
            continuation.onTermination = { _ in session.stop() }
        }
    }

    /// Built-in, Continuity, and external cameras, ordered deterministically:
    /// the system default first, then by display name, then by unique ID, so a
    /// numeric camera ID stays stable across calls while the set is unchanged.
    static func captureDevices() -> [AVCaptureDevice] {
        let session = AVCaptureDevice.DiscoverySession(
            deviceTypes: [.builtInWideAngleCamera, .continuityCamera, .external],
            mediaType: .video,
            position: .unspecified
        )
        let defaultID = AVCaptureDevice.default(for: .video)?.uniqueID
        return session.devices.sorted { left, right in
            if left.uniqueID == defaultID { return true }
            if right.uniqueID == defaultID { return false }
            if left.localizedName != right.localizedName {
                return left.localizedName.localizedStandardCompare(right.localizedName)
                    == .orderedAscending
            }
            return left.uniqueID < right.uniqueID
        }
    }
}

/// Live `AVCaptureSession` → `VTCompressionSession` H.264 capture.
///
/// NOTE: the real capture path cannot run on CI (no camera, no authorization).
/// It is exercised only by a manual hardware gate; `annexBFromAVCC` above and the
/// `SensorService` fan-in are what the automated tests cover.
struct CameraCapture: CameraCapturing {
    /// Camera occupies sensor channel 2 (mic is channel 1).
    static let channel: UInt32 = 2

    /// Target average bitrate for the H.264 encoder. Tunable per deployment; the
    /// real world (lighting, motion, link) is what decides the right value.
    var averageBitRate: Int32 = 4_000_000

    func frames() -> AsyncThrowingStream<CameraFrame, any Error> {
        let bitRate = averageBitRate
        // bufferingNewest(1) IS the source-side newest-drop: a slow gRPC consumer
        // means the encoder's older frames are discarded rather than backing up.
        return AsyncThrowingStream(bufferingPolicy: .bufferingNewest(1)) { continuation in
            let session = CameraCaptureSession(bitRate: bitRate)
            do {
                try session.start(continuation: continuation)
            } catch {
                continuation.finish(throwing: error)
                return
            }
            continuation.onTermination = { _ in session.stop() }
        }
    }

    func descriptor() -> Wendy_Lite_Sensorlink_SensorDescriptor? {
        guard AVCaptureDevice.authorizationStatus(for: .video) == .authorized else { return nil }
        guard let device = AVCaptureDevice.default(for: .video) else { return nil }
        // Advertise what `frames()` will actually capture, and no camera
        // channel at all when the default camera has nothing streamable.
        let format: AVCaptureDevice.Format
        do {
            format = try device.streamableFormatToPin() ?? device.activeFormat
        } catch {
            return nil
        }
        let dims = CMVideoFormatDescriptionGetDimensions(format.formatDescription)

        var descriptor = Wendy_Lite_Sensorlink_SensorDescriptor()
        descriptor.channelID = Self.channel
        descriptor.name = device.localizedName
        var video = Wendy_Lite_Sensorlink_VideoFormat()
        video.codec = .h264
        video.width = UInt32(max(0, dims.width))
        video.height = UInt32(max(0, dims.height))
        video.fps = UInt32(
            format.videoSupportedFrameRateRanges.first?.maxFrameRate ?? 30
        )
        descriptor.video = video
        return descriptor
    }
}

/// Owns the live capture + encode pipeline for one `frames()` stream.
///
/// `@unchecked Sendable` invariant: `continuation` and `compressionSession` are
/// assigned once in `start` before capture begins; every later read or clear of
/// either (in `stop`, `captureOutput`, and `handleEncoded`) is guarded by `lock`,
/// as are `forceNextKeyframe` / `isFirstFrame`. `stop` clears both under the lock
/// before invalidating the VT session, so a concurrent snapshot never observes
/// a session that's mid-invalidation. `start` and `stop` are each called once
/// from the owning `AsyncThrowingStream` closures.
final class CameraCaptureSession: NSObject, AVCaptureVideoDataOutputSampleBufferDelegate,
    @unchecked Sendable
{
    private let captureSession = AVCaptureSession()
    private let videoOutput = AVCaptureVideoDataOutput()
    private let queue = DispatchQueue(label: "wendy.sensor.camera")
    private let bitRate: Int32
    /// `nil` selects the system default camera (sensor channel); a value picks
    /// the camera `WendyVideoService` was asked for.
    private let deviceUniqueID: String?
    private let lock = NSLock()

    private var compressionSession: VTCompressionSession?
    private var continuation: AsyncThrowingStream<CameraFrame, any Error>.Continuation?
    private var forceNextKeyframe = false
    private var isFirstFrame = true

    init(bitRate: Int32, deviceUniqueID: String? = nil) {
        self.bitRate = bitRate
        self.deviceUniqueID = deviceUniqueID
        super.init()
    }

    func start(
        continuation: AsyncThrowingStream<CameraFrame, any Error>.Continuation
    ) throws {
        self.continuation = continuation

        // Never trigger the system permission prompt from a capture request:
        // the user grants camera access in the app's onboarding.
        guard AVCaptureDevice.authorizationStatus(for: .video) == .authorized else {
            throw CameraError.accessDenied
        }
        let device: AVCaptureDevice
        if let deviceUniqueID {
            guard
                let selected = AVCaptureCameraManager.captureDevices().first(where: {
                    $0.uniqueID == deviceUniqueID
                })
            else { throw CameraError.deviceNotFound }
            device = selected
        } else {
            guard let fallback = AVCaptureDevice.default(for: .video) else {
                throw CameraError.noDevice
            }
            device = fallback
        }
        // Decide before anything opens the camera: a camera with nothing
        // streamable must fail this RPC, never reach the effects pipeline.
        let pinnedFormat = try device.streamableFormatToPin()
        let input = try AVCaptureDeviceInput(device: device)

        // Keep configuration in its own scope so the deferred commit runs
        // before startRunning(). AVFoundation raises an uncaught Objective-C
        // exception if capture starts while configuration is still open.
        captureSession.beginConfiguration()
        do {
            defer { captureSession.commitConfiguration() }
            guard captureSession.canAddInput(input) else {
                throw CameraError.cannotAddInput
            }
            captureSession.addInput(input)
            videoOutput.videoSettings = [
                kCVPixelBufferPixelFormatTypeKey as String: kCVPixelFormatType_32BGRA
            ]
            videoOutput.alwaysDiscardsLateVideoFrames = true
            videoOutput.setSampleBufferDelegate(self, queue: queue)
            guard captureSession.canAddOutput(videoOutput) else {
                throw CameraError.cannotAddOutput
            }
            captureSession.addOutput(videoOutput)
        }

        guard let pinnedFormat else {
            let dims = CMVideoFormatDescriptionGetDimensions(device.activeFormat.formatDescription)
            try makeCompressionSession(width: dims.width, height: dims.height)
            captureSession.startRunning()
            return
        }

        // The `.high` preset re-picks `activeFormat` inside startRunning(), and
        // macOS has no `.inputPriority`. Holding the configuration lock across
        // startRunning() keeps the pinned format (the pattern wendy-console's
        // mac-fire-detector uses for its AVFoundation thermal source).
        do {
            try device.lockForConfiguration()
        } catch {
            throw CameraError.cannotPinFormat(error.localizedDescription)
        }
        defer { device.unlockForConfiguration() }
        device.activeFormat = pinnedFormat
        let dims = CMVideoFormatDescriptionGetDimensions(pinnedFormat.formatDescription)
        try makeCompressionSession(width: dims.width, height: dims.height)
        captureSession.startRunning()
    }

    func stop() {
        captureSession.stopRunning()

        // Nil the continuation and snapshot+clear the session under the lock
        // BEFORE invalidating, so a concurrent captureOutput/handleEncoded
        // snapshot (also taken under lock) either sees a still-valid session or
        // a nil continuation/session — never a session mid-invalidation.
        lock.lock()
        let session = compressionSession
        compressionSession = nil
        continuation = nil
        lock.unlock()

        if let session {
            VTCompressionSessionCompleteFrames(session, untilPresentationTimeStamp: .invalid)
            VTCompressionSessionInvalidate(session)
        }
    }

    private func makeCompressionSession(width: Int32, height: Int32) throws {
        var session: VTCompressionSession?
        let status = VTCompressionSessionCreate(
            allocator: kCFAllocatorDefault,
            width: width,
            height: height,
            codecType: kCMVideoCodecType_H264,
            encoderSpecification: nil,
            imageBufferAttributes: nil,
            compressedDataAllocator: nil,
            outputCallback: cameraCompressionOutputCallback,
            refcon: Unmanaged.passUnretained(self).toOpaque(),
            compressionSessionOut: &session
        )
        guard status == noErr, let session else { throw CameraError.vtStatus("create", status) }

        VTSessionSetProperty(
            session,
            key: kVTCompressionPropertyKey_RealTime,
            value: kCFBooleanTrue
        )
        VTSessionSetProperty(
            session,
            key: kVTCompressionPropertyKey_ProfileLevel,
            value: kVTProfileLevel_H264_Main_AutoLevel
        )
        // No B-frames: low latency, and every access unit is decodable in order.
        VTSessionSetProperty(
            session,
            key: kVTCompressionPropertyKey_AllowFrameReordering,
            value: kCFBooleanFalse
        )
        VTSessionSetProperty(
            session,
            key: kVTCompressionPropertyKey_MaxKeyFrameInterval,
            value: NSNumber(value: 60)
        )
        VTSessionSetProperty(
            session,
            key: kVTCompressionPropertyKey_AverageBitRate,
            value: NSNumber(value: bitRate)
        )
        VTCompressionSessionPrepareToEncodeFrames(session)
        compressionSession = session
    }

    // MARK: - Capture delegate (capture queue)

    func captureOutput(
        _ output: AVCaptureOutput,
        didOutput sampleBuffer: CMSampleBuffer,
        from connection: AVCaptureConnection
    ) {
        guard let imageBuffer = CMSampleBufferGetImageBuffer(sampleBuffer) else { return }

        // Snapshot the session and the force-keyframe state under one lock hold
        // so a concurrent stop() can't invalidate the session between reading it
        // and using it below.
        lock.lock()
        guard let compressionSession else {
            lock.unlock()
            return
        }
        let force = forceNextKeyframe || isFirstFrame
        forceNextKeyframe = false
        isFirstFrame = false
        lock.unlock()

        var properties: CFDictionary?
        if force, let forceKey = kCFBooleanTrue {
            properties = [kVTEncodeFrameOptionKey_ForceKeyFrame: forceKey] as CFDictionary
        }

        VTCompressionSessionEncodeFrame(
            compressionSession,
            imageBuffer: imageBuffer,
            presentationTimeStamp: CMSampleBufferGetPresentationTimeStamp(sampleBuffer),
            duration: CMSampleBufferGetDuration(sampleBuffer),
            frameProperties: properties,
            sourceFrameRefcon: nil,
            infoFlagsOut: nil
        )
    }

    // MARK: - Encode callback (VideoToolbox thread)

    fileprivate func handleEncoded(_ sampleBuffer: CMSampleBuffer) {
        lock.lock()
        let continuation = self.continuation
        lock.unlock()
        guard let continuation, CMSampleBufferDataIsReady(sampleBuffer),
            let format = CMSampleBufferGetFormatDescription(sampleBuffer),
            let dataBuffer = CMSampleBufferGetDataBuffer(sampleBuffer)
        else { return }

        let isKeyframe = Self.isKeyframe(sampleBuffer)
        var sps: [Data] = []
        var pps: [Data] = []
        if isKeyframe {
            (sps, pps) = Self.parameterSets(format)
        }

        var lengthAtOffset = 0
        var totalLength = 0
        var pointer: UnsafeMutablePointer<Int8>?
        // ponytail: assumes the encoder emits a contiguous block (true for H.264
        // access units); a non-contiguous buffer would need CMBlockBufferCopyDataBytes.
        guard
            CMBlockBufferGetDataPointer(
                dataBuffer,
                atOffset: 0,
                lengthAtOffsetOut: &lengthAtOffset,
                totalLengthOut: &totalLength,
                dataPointerOut: &pointer
            ) == kCMBlockBufferNoErr,
            let pointer
        else { return }

        let avcc = Data(bytes: pointer, count: totalLength)
        let presentation = CMSampleBufferGetPresentationTimeStamp(sampleBuffer)
        let timestampNanoseconds: UInt64 =
            presentation.isNumeric && presentation.seconds >= 0
            ? UInt64(presentation.seconds * 1_000_000_000) : 0
        let frame = CameraFrame(
            annexB: annexBFromAVCC(avcc, sps: sps, pps: pps, isKeyframe: isKeyframe),
            isKeyframe: isKeyframe,
            timestampNanoseconds: timestampNanoseconds
        )

        switch continuation.yield(frame) {
        case .dropped:
            // A congested consumer dropped a frame; force the next encode to be a
            // keyframe so the stream re-syncs instead of showing corruption.
            lock.lock()
            forceNextKeyframe = true
            lock.unlock()
        case .terminated:
            stop()
        default:
            break
        }
    }

    private static func isKeyframe(_ sampleBuffer: CMSampleBuffer) -> Bool {
        guard
            let attachments = CMSampleBufferGetSampleAttachmentsArray(
                sampleBuffer,
                createIfNecessary: false
            ) as? [[CFString: Any]],
            let first = attachments.first,
            let notSync = first[kCMSampleAttachmentKey_NotSync] as? Bool
        else {
            // Absent NotSync attachment means a sync sample (keyframe).
            return true
        }
        return !notSync
    }

    private static func parameterSets(_ format: CMFormatDescription) -> ([Data], [Data]) {
        var count = 0
        CMVideoFormatDescriptionGetH264ParameterSetAtIndex(
            format,
            parameterSetIndex: 0,
            parameterSetPointerOut: nil,
            parameterSetSizeOut: nil,
            parameterSetCountOut: &count,
            nalUnitHeaderLengthOut: nil
        )

        var sps: [Data] = []
        var pps: [Data] = []
        for index in 0..<count {
            var pointer: UnsafePointer<UInt8>?
            var size = 0
            guard
                CMVideoFormatDescriptionGetH264ParameterSetAtIndex(
                    format,
                    parameterSetIndex: index,
                    parameterSetPointerOut: &pointer,
                    parameterSetSizeOut: &size,
                    parameterSetCountOut: nil,
                    nalUnitHeaderLengthOut: nil
                ) == noErr,
                let pointer
            else { continue }
            let data = Data(bytes: pointer, count: size)
            // Convention: index 0 is the SPS, the rest are PPS.
            if index == 0 { sps.append(data) } else { pps.append(data) }
        }
        return (sps, pps)
    }
}

/// C output callback — no captures, so it bridges to a C function pointer. Routes
/// back to the owning session via the unretained refcon set at create time.
private func cameraCompressionOutputCallback(
    outputCallbackRefCon: UnsafeMutableRawPointer?,
    sourceFrameRefCon: UnsafeMutableRawPointer?,
    status: OSStatus,
    infoFlags: VTEncodeInfoFlags,
    sampleBuffer: CMSampleBuffer?
) {
    guard status == noErr, let sampleBuffer, let refcon = outputCallbackRefCon else { return }
    let session = Unmanaged<CameraCaptureSession>.fromOpaque(refcon).takeUnretainedValue()
    session.handleEncoded(sampleBuffer)
}
