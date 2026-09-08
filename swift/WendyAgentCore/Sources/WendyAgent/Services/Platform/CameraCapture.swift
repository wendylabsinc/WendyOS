import AVFoundation
import Foundation
import VideoToolbox

struct CameraDeviceInfo: Equatable, Sendable {
    let id: UInt32
    let uniqueID: String
    let name: String
    let isExternal: Bool
}

struct EncodedCameraFrame: Equatable, Sendable {
    let data: Data
    let timestampNanoseconds: UInt64
}

protocol CameraManaging: Sendable {
    func devices() async -> [CameraDeviceInfo]
    func frames(for device: CameraDeviceInfo) -> AsyncThrowingStream<EncodedCameraFrame, any Error>
}

enum CameraCaptureError: Error, CustomStringConvertible {
    case accessDenied
    case deviceNotFound
    case cannotOpenDevice(String)
    case cannotAddInput
    case cannotAddOutput
    case videoToolbox(String, OSStatus)

    var description: String {
        switch self {
        case .accessDenied:
            return "Camera access is not allowed for Wendy Agent."
        case .deviceNotFound:
            return "The selected camera is no longer available."
        case .cannotOpenDevice(let reason):
            return "The selected camera could not be opened: \(reason)"
        case .cannotAddInput:
            return "The selected camera could not be added to the capture session."
        case .cannotAddOutput:
            return "The camera encoder output could not be added to the capture session."
        case .videoToolbox(let action, let status):
            return "VideoToolbox \(action) failed (OSStatus \(status))."
        }
    }
}

struct AVCaptureCameraManager: CameraManaging {
    func devices() async -> [CameraDeviceInfo] {
        await BlockingExecutor.run {
            let devices = Self.captureDevices()
            return devices.enumerated().map { index, device in
                CameraDeviceInfo(
                    id: UInt32(index),
                    uniqueID: device.uniqueID,
                    name: device.localizedName,
                    isExternal: device.deviceType == .external
                )
            }
        }
    }

    func frames(
        for device: CameraDeviceInfo
    ) -> AsyncThrowingStream<EncodedCameraFrame, any Error> {
        let session = CameraCaptureSession(deviceUniqueID: device.uniqueID)
        return AsyncThrowingStream(bufferingPolicy: .bufferingNewest(1)) { continuation in
            continuation.onTermination = { _ in
                session.stop()
            }
            session.start(continuation: continuation)
        }
    }

    fileprivate static func captureDevices() -> [AVCaptureDevice] {
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

/// Converts length-prefixed H.264 NAL units into start-code-delimited Annex-B.
/// Parameter sets are prepended to keyframes so a newly connected decoder can
/// begin at the next keyframe without out-of-band codec configuration.
func annexBFromAVCC(
    _ avcc: Data,
    nalUnitHeaderLength: Int,
    parameterSets: [Data],
    isKeyframe: Bool
) -> Data {
    guard (1...4).contains(nalUnitHeaderLength) else { return Data() }

    let startCode = Data([0, 0, 0, 1])
    var output = Data()
    if isKeyframe {
        for parameterSet in parameterSets {
            output += startCode
            output += parameterSet
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
        output += startCode
        output += avcc[index..<index + length]
        index += length
    }
    return output
}

/// Owns one AVFoundation capture session and its VideoToolbox encoder.
///
/// `@unchecked Sendable` invariant: AVFoundation calls `captureOutput` only on
/// `frameQueue`; that queue also owns `compressionSession`. The continuation,
/// termination flag, and force-keyframe flag are protected by `lock`. Capture
/// startup and shutdown are serialized on `sessionQueue`, and encoder teardown
/// is enqueued behind every in-flight capture callback before invalidation.
final class CameraCaptureSession: NSObject, AVCaptureVideoDataOutputSampleBufferDelegate,
    @unchecked Sendable
{
    private let captureSession = AVCaptureSession()
    private let videoOutput = AVCaptureVideoDataOutput()
    private let sessionQueue = DispatchQueue(label: "sh.wendy.agent.camera.session")
    private let frameQueue = DispatchQueue(label: "sh.wendy.agent.camera.frames")
    private let lock = NSLock()
    private let deviceUniqueID: String

    private var compressionSession: VTCompressionSession?
    private var continuation: AsyncThrowingStream<EncodedCameraFrame, any Error>.Continuation?
    private var stopped = false
    private var forceNextKeyframe = false
    private var isFirstFrame = true

    init(deviceUniqueID: String) {
        self.deviceUniqueID = deviceUniqueID
        super.init()
    }

    func start(
        continuation: AsyncThrowingStream<EncodedCameraFrame, any Error>.Continuation
    ) {
        lock.withLock {
            self.continuation = continuation
        }
        sessionQueue.async { [self] in
            do {
                try configureAndStart()
            } catch {
                finish(throwing: error)
            }
        }
    }

    func stop() {
        let shouldStop = lock.withLock {
            guard !stopped else { return false }
            stopped = true
            continuation = nil
            return true
        }
        guard shouldStop else { return }

        sessionQueue.async { [self] in
            videoOutput.setSampleBufferDelegate(nil, queue: nil)
            if captureSession.isRunning {
                captureSession.stopRunning()
            }
            frameQueue.async { [self] in
                if let compressionSession {
                    VTCompressionSessionCompleteFrames(
                        compressionSession,
                        untilPresentationTimeStamp: .invalid
                    )
                    VTCompressionSessionInvalidate(compressionSession)
                    self.compressionSession = nil
                }
            }
        }
    }

    private func configureAndStart() throws {
        guard !lock.withLock({ stopped }) else { return }
        guard AVCaptureDevice.authorizationStatus(for: .video) == .authorized else {
            throw CameraCaptureError.accessDenied
        }
        guard
            let device = AVCaptureCameraManager.captureDevices().first(where: {
                $0.uniqueID == deviceUniqueID
            })
        else {
            throw CameraCaptureError.deviceNotFound
        }

        let input: AVCaptureDeviceInput
        do {
            input = try AVCaptureDeviceInput(device: device)
        } catch {
            throw CameraCaptureError.cannotOpenDevice(error.localizedDescription)
        }
        captureSession.beginConfiguration()
        if captureSession.canSetSessionPreset(.hd1280x720) {
            captureSession.sessionPreset = .hd1280x720
        }
        guard captureSession.canAddInput(input) else {
            captureSession.commitConfiguration()
            throw CameraCaptureError.cannotAddInput
        }
        captureSession.addInput(input)

        videoOutput.videoSettings = [
            kCVPixelBufferPixelFormatTypeKey as String: kCVPixelFormatType_32BGRA
        ]
        videoOutput.alwaysDiscardsLateVideoFrames = true
        videoOutput.setSampleBufferDelegate(self, queue: frameQueue)
        guard captureSession.canAddOutput(videoOutput) else {
            captureSession.commitConfiguration()
            throw CameraCaptureError.cannotAddOutput
        }
        captureSession.addOutput(videoOutput)
        captureSession.commitConfiguration()
        captureSession.startRunning()
    }

    func captureOutput(
        _ output: AVCaptureOutput,
        didOutput sampleBuffer: CMSampleBuffer,
        from connection: AVCaptureConnection
    ) {
        guard let imageBuffer = CMSampleBufferGetImageBuffer(sampleBuffer) else { return }

        do {
            if compressionSession == nil {
                try makeCompressionSession(
                    width: Int32(CVPixelBufferGetWidth(imageBuffer)),
                    height: Int32(CVPixelBufferGetHeight(imageBuffer))
                )
            }
            guard let compressionSession else { return }

            let forceKeyframe = lock.withLock {
                let force = forceNextKeyframe || isFirstFrame
                forceNextKeyframe = false
                isFirstFrame = false
                return force
            }
            let properties: CFDictionary? =
                forceKeyframe
                ? [kVTEncodeFrameOptionKey_ForceKeyFrame: true] as CFDictionary
                : nil
            let status = VTCompressionSessionEncodeFrame(
                compressionSession,
                imageBuffer: imageBuffer,
                presentationTimeStamp: CMSampleBufferGetPresentationTimeStamp(sampleBuffer),
                duration: CMSampleBufferGetDuration(sampleBuffer),
                frameProperties: properties,
                sourceFrameRefcon: nil,
                infoFlagsOut: nil
            )
            guard status == noErr else {
                throw CameraCaptureError.videoToolbox("encode", status)
            }
        } catch {
            finish(throwing: error)
        }
    }

    private func makeCompressionSession(width: Int32, height: Int32) throws {
        var session: VTCompressionSession?
        let status = unsafe VTCompressionSessionCreate(
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
        guard status == noErr, let session else {
            throw CameraCaptureError.videoToolbox("session creation", status)
        }

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
            value: NSNumber(value: 4_000_000)
        )
        let prepareStatus = VTCompressionSessionPrepareToEncodeFrames(session)
        guard prepareStatus == noErr else {
            VTCompressionSessionInvalidate(session)
            throw CameraCaptureError.videoToolbox("encoder preparation", prepareStatus)
        }
        compressionSession = session
    }

    fileprivate func handleEncoded(status: OSStatus, sampleBuffer: CMSampleBuffer?) {
        guard status == noErr else {
            finish(throwing: CameraCaptureError.videoToolbox("output", status))
            return
        }
        guard let sampleBuffer else {
            lock.withLock {
                forceNextKeyframe = true
            }
            return
        }
        guard
            CMSampleBufferDataIsReady(sampleBuffer),
            let format = CMSampleBufferGetFormatDescription(sampleBuffer),
            let dataBuffer = CMSampleBufferGetDataBuffer(sampleBuffer)
        else { return }

        var nalUnitHeaderLength: Int32 = 0
        var parameterSetCount = 0
        let countStatus = unsafe CMVideoFormatDescriptionGetH264ParameterSetAtIndex(
            format,
            parameterSetIndex: 0,
            parameterSetPointerOut: nil,
            parameterSetSizeOut: nil,
            parameterSetCountOut: &parameterSetCount,
            nalUnitHeaderLengthOut: &nalUnitHeaderLength
        )
        guard countStatus == noErr else {
            finish(throwing: CameraCaptureError.videoToolbox("format inspection", countStatus))
            return
        }

        let isKeyframe = Self.isKeyframe(sampleBuffer)
        let parameterSets = isKeyframe ? Self.parameterSets(format, count: parameterSetCount) : []
        let length = CMBlockBufferGetDataLength(dataBuffer)
        var avcc = Data(count: length)
        let copyStatus = unsafe avcc.withUnsafeMutableBytes { bytes in
            guard let destination = bytes.baseAddress else {
                return kCMBlockBufferBadPointerParameterErr
            }
            return unsafe CMBlockBufferCopyDataBytes(
                dataBuffer,
                atOffset: 0,
                dataLength: length,
                destination: destination
            )
        }
        guard copyStatus == kCMBlockBufferNoErr else {
            finish(throwing: CameraCaptureError.videoToolbox("buffer copy", copyStatus))
            return
        }

        let annexB = annexBFromAVCC(
            avcc,
            nalUnitHeaderLength: Int(nalUnitHeaderLength),
            parameterSets: parameterSets,
            isKeyframe: isKeyframe
        )
        guard !annexB.isEmpty else { return }

        let timestamp = max(0, Date().timeIntervalSince1970 * 1_000_000_000)
        let frame = EncodedCameraFrame(
            data: annexB,
            timestampNanoseconds: UInt64(timestamp)
        )
        let continuation = lock.withLock { self.continuation }
        switch continuation?.yield(frame) {
        case .dropped:
            lock.withLock {
                forceNextKeyframe = true
            }
        case .terminated:
            stop()
        case .enqueued, nil:
            break
        @unknown default:
            break
        }
    }

    private func finish(throwing error: any Error) {
        let continuation = lock.withLock { self.continuation }
        continuation?.finish(throwing: error)
        stop()
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
            return true
        }
        return !notSync
    }

    private static func parameterSets(
        _ format: CMFormatDescription,
        count: Int
    ) -> [Data] {
        (0..<count).compactMap { index in
            var pointer: UnsafePointer<UInt8>?
            var size = 0
            let status = unsafe CMVideoFormatDescriptionGetH264ParameterSetAtIndex(
                format,
                parameterSetIndex: index,
                parameterSetPointerOut: &pointer,
                parameterSetSizeOut: &size,
                parameterSetCountOut: nil,
                nalUnitHeaderLengthOut: nil
            )
            guard status == noErr, let pointer = unsafe pointer else { return nil }
            return unsafe Data(bytes: pointer, count: size)
        }
    }
}

private func cameraCompressionOutputCallback(
    outputCallbackRefCon: UnsafeMutableRawPointer?,
    sourceFrameRefCon: UnsafeMutableRawPointer?,
    status: OSStatus,
    infoFlags: VTEncodeInfoFlags,
    sampleBuffer: CMSampleBuffer?
) {
    guard let outputCallbackRefCon = unsafe outputCallbackRefCon else { return }
    let session = unsafe Unmanaged<CameraCaptureSession>.fromOpaque(outputCallbackRefCon)
        .takeUnretainedValue()
    session.handleEncoded(status: status, sampleBuffer: sampleBuffer)
}
