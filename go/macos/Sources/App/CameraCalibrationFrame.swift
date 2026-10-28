import Foundation
import CoreGraphics
import CoreMedia
import CoreVideo
import VideoToolbox
import Synchronization

struct CameraCalibrationFrame: Sendable {
    var gray: Data
    var width: Int
    var height: Int
    var received: Date
    var sequence: UInt64
    var codec: String

    func isFresh(at now: Date) -> Bool { (0...3).contains(now.timeIntervalSince(received)) }

    static func jpeg(_ image: CGImage, received: Date, sequence: UInt64) -> Self? {
        let width = image.width, height = image.height
        guard width > 0, height > 0, width <= 8192, height <= 8192, width*height <= 32_000_000 else { return nil }
        var gray = Data(count: width*height)
        let success = gray.withUnsafeMutableBytes { raw -> Bool in
            guard let context = CGContext(data: raw.baseAddress, width: width, height: height, bitsPerComponent: 8,
                bytesPerRow: width, space: CGColorSpaceCreateDeviceGray(), bitmapInfo: CGImageAlphaInfo.none.rawValue) else { return false }
            context.draw(image, in: CGRect(x: 0, y: 0, width: width, height: height))
            return true
        }
        return success ? Self(gray: gray, width: width, height: height, received: received, sequence: sequence, codec: "JPEG") : nil
    }

    static func luma(_ buffer: CVPixelBuffer, received: Date, sequence: UInt64) -> Self? {
        let format = CVPixelBufferGetPixelFormatType(buffer)
        guard format == kCVPixelFormatType_420YpCbCr8BiPlanarFullRange || format == kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange,
              CVPixelBufferGetPlaneCount(buffer) == 2 else { return nil }
        let width = CVPixelBufferGetWidthOfPlane(buffer, 0), height = CVPixelBufferGetHeightOfPlane(buffer, 0)
        guard width > 0, height > 0, width <= 8192, height <= 8192, width*height <= 32_000_000,
              CVPixelBufferLockBaseAddress(buffer, .readOnly) == kCVReturnSuccess else { return nil }
        defer { CVPixelBufferUnlockBaseAddress(buffer, .readOnly) }
        guard let base = CVPixelBufferGetBaseAddressOfPlane(buffer, 0) else { return nil }
        let stride = CVPixelBufferGetBytesPerRowOfPlane(buffer, 0)
        guard stride >= width else { return nil }
        var gray = Data(count: width*height)
        gray.withUnsafeMutableBytes { raw in
            for y in 0..<height { raw.baseAddress!.advanced(by: y*width).copyMemory(from: base.advanced(by: y*stride), byteCount: width) }
            if format == kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange {
                let pixels = raw.bindMemory(to: UInt8.self)
                for i in pixels.indices { pixels[i] = UInt8(clamping: (Int(pixels[i])-16)*255/219) }
            }
        }
        return Self(gray: gray, width: width, height: height, received: received, sequence: sequence, codec: "H.264")
    }
}

/// Decodes only self-contained IDR frames while calibration is open. The normal
/// display path stays unchanged. A generation prevents delayed decoder output
/// from a previous camera/session from becoming a calibration input.
final class H264CalibrationFrames: @unchecked Sendable {
    private struct State {
        var enabled = false
        var explicitlyEnabled = false
        var owners: Set<UUID> = []
        var generation: UInt64 = 0
        var pending = false
        var latest: CameraCalibrationFrame?
    }
    private struct Sample: @unchecked Sendable { let value: CMSampleBuffer }
    private let state = Mutex(State())
    private let queue = DispatchQueue(label: "dev.wendy.calibration-frames", qos: .userInitiated)

    var latest: CameraCalibrationFrame? { state.withLock { $0.latest } }
    func setEnabled(_ enabled: Bool) {
        state.withLock { current in
            current.explicitlyEnabled = enabled
            Self.updateEnabled(&current)
        }
    }
    func acquire() -> UUID {
        let token = UUID()
        state.withLock { current in
            current.owners.insert(token)
            Self.updateEnabled(&current)
        }
        return token
    }
    func release(_ token: UUID) {
        state.withLock { current in
            current.owners.remove(token)
            Self.updateEnabled(&current)
        }
    }
    private static func updateEnabled(_ current: inout State) {
        let enabled = current.explicitlyEnabled || !current.owners.isEmpty
        guard current.enabled != enabled else { return }
        current.enabled = enabled
        current.generation &+= 1
        current.latest = nil
        current.pending = false
    }
    var isEnabled: Bool { state.withLock { $0.enabled } }
    func reset() {
        state.withLock { $0.generation &+= 1; $0.latest = nil; $0.pending = false }
    }
    func accept(_ buffer: CMSampleBuffer, sequence: UInt64, received: Date) {
        let generation = state.withLock { current -> UInt64? in
            guard current.enabled, !current.pending else { return nil }
            current.pending = true
            return current.generation
        }
        guard let generation else { return }
        let sample = Sample(value: buffer)
        queue.async { [self] in
            defer { state.withLock { if $0.generation == generation { $0.pending = false } } }
            guard state.withLock({ $0.enabled && $0.generation == generation }),
                  let format = CMSampleBufferGetFormatDescription(sample.value) else { return }
            let dimensions = CMVideoFormatDescriptionGetDimensions(format)
            guard dimensions.width > 0, dimensions.height > 0, dimensions.width <= 8192, dimensions.height <= 8192,
                  Int64(dimensions.width)*Int64(dimensions.height) <= 32_000_000 else { return }
            var session: VTDecompressionSession?
            let attributes = [kCVPixelBufferPixelFormatTypeKey: kCVPixelFormatType_420YpCbCr8BiPlanarFullRange] as CFDictionary
            guard VTDecompressionSessionCreate(allocator: nil, formatDescription: format, decoderSpecification: nil,
                imageBufferAttributes: attributes, outputCallback: nil, decompressionSessionOut: &session) == noErr,
                let session else { return }
            defer { VTDecompressionSessionInvalidate(session) }
            let status = VTDecompressionSessionDecodeFrame(session, sampleBuffer: sample.value, flags: [], infoFlagsOut: nil) { [self] status, _, image, _, _ in
                guard status == noErr, let image,
                      let frame = CameraCalibrationFrame.luma(image, received: received, sequence: sequence) else { return }
                state.withLock { if $0.enabled && $0.generation == generation { $0.latest = frame } }
            }
            if status == noErr { VTDecompressionSessionWaitForAsynchronousFrames(session) }
        }
    }
}
