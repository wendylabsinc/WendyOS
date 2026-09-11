import AVFoundation
import Foundation
import VideoToolbox
import WendyAgentGRPC

/// One encoded H.264 access unit in Annex-B framing (start-code delimited),
/// with SPS/PPS prepended on keyframes so the stream is self-describing.
struct CameraFrame: Sendable {
    var annexB: Data
    var isKeyframe: Bool
}

/// Produces H.264 camera frames. Injectable so `SensorService` can be tested
/// with a fake that yields canned frames instead of touching real hardware.
protocol CameraCapturing: Sendable {
    func frames() -> AsyncThrowingStream<CameraFrame, any Error>
    func descriptor() -> Wendy_Lite_Sensorlink_SensorDescriptor?
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
    let startCode = Data([0, 0, 0, 1])
    var out = Data()
    if isKeyframe {
        for set in sps {
            out += startCode
            out += set
        }
        for set in pps {
            out += startCode
            out += set
        }
    }

    var index = avcc.startIndex
    while index + 4 <= avcc.endIndex {
        var length = 0
        for offset in 0..<4 {
            length = (length << 8) | Int(avcc[index + offset])
        }
        index += 4
        guard length > 0, index + length <= avcc.endIndex else { break }
        out += startCode
        out += avcc[index..<index + length]
        index += length
    }
    return out
}

enum CameraError: Error, CustomStringConvertible {
    case noDevice
    case vtStatus(String, OSStatus)

    var description: String {
        switch self {
        case .noDevice: return "No default video capture device available."
        case .vtStatus(let action, let status):
            return "VideoToolbox \(action) failed (OSStatus \(status))."
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
        let dims = CMVideoFormatDescriptionGetDimensions(device.activeFormat.formatDescription)

        var descriptor = Wendy_Lite_Sensorlink_SensorDescriptor()
        descriptor.channelID = Self.channel
        descriptor.kind = .camera
        descriptor.name = device.localizedName
        var video = Wendy_Lite_Sensorlink_VideoFormat()
        video.codec = .h264
        video.width = UInt32(max(0, dims.width))
        video.height = UInt32(max(0, dims.height))
        video.fps = UInt32(
            device.activeFormat.videoSupportedFrameRateRanges.first?.maxFrameRate ?? 30
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
    private let lock = NSLock()

    private var compressionSession: VTCompressionSession?
    private var continuation: AsyncThrowingStream<CameraFrame, any Error>.Continuation?
    private var forceNextKeyframe = false
    private var isFirstFrame = true

    init(bitRate: Int32) {
        self.bitRate = bitRate
        super.init()
    }

    func start(
        continuation: AsyncThrowingStream<CameraFrame, any Error>.Continuation
    ) throws {
        self.continuation = continuation

        guard let device = AVCaptureDevice.default(for: .video) else { throw CameraError.noDevice }
        let input = try AVCaptureDeviceInput(device: device)

        captureSession.beginConfiguration()
        if captureSession.canAddInput(input) { captureSession.addInput(input) }
        videoOutput.videoSettings = [
            kCVPixelBufferPixelFormatTypeKey as String: kCVPixelFormatType_32BGRA
        ]
        videoOutput.alwaysDiscardsLateVideoFrames = true
        videoOutput.setSampleBufferDelegate(self, queue: queue)
        if captureSession.canAddOutput(videoOutput) { captureSession.addOutput(videoOutput) }
        captureSession.commitConfiguration()

        let dims = CMVideoFormatDescriptionGetDimensions(device.activeFormat.formatDescription)
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
        let frame = CameraFrame(
            annexB: annexBFromAVCC(avcc, sps: sps, pps: pps, isKeyframe: isKeyframe),
            isKeyframe: isKeyframe
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
