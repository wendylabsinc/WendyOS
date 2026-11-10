// Adapted from wendy-sandbox/desktop-native H264VideoDecoder.swift.
import Foundation

/// Splits an Annex-B H.264 byte stream into NAL units.
///
/// Handles 3- and 4-byte start codes (`00 00 01` / `00 00 00 01`) and start
/// codes split across `append` calls (network chunk boundaries). A NAL unit is
/// only emitted once the *next* start code is seen — that start code is what
/// delimits the current NAL's end — so the final NAL of a stream stays buffered
/// until more data arrives. This is inherent to Annex-B framing and is fine for
/// a continuous camera stream.
///
/// Pure value type with no CoreMedia dependency, so the byte-level logic (where
/// framing bugs hide) is unit-testable in isolation. NAL-type routing and
/// decoding live in `H264AnnexBDecoder`.
struct AnnexBParser {
    private var buffer: [UInt8] = []

    /// Append a chunk and return any NAL units completed by it (start code stripped).
    mutating func append(_ chunk: Data) -> [Data] {
        // Bound malformed or unterminated input before appending another chunk.
        if buffer.count + chunk.count > 16 * 1024 * 1024 { buffer.removeAll(keepingCapacity: false) }
        guard chunk.count <= 16 * 1024 * 1024 else { return [] }
        buffer.append(contentsOf: chunk)
        var nals: [Data] = []
        while true {
            guard let first = findStartCode(from: 0) else {
                // No start code in the buffer. Drop leading garbage but keep the
                // last 3 bytes: a start code may straddle this chunk and the next.
                if buffer.count > 3 { buffer.removeFirst(buffer.count - 3) }
                return nals
            }
            if first.index > 0 { buffer.removeFirst(first.index) }
            guard let next = findStartCode(from: first.length) else { return nals }
            let nalStart = first.length
            let nalEnd = next.index
            if nalEnd > nalStart { nals.append(Data(buffer[nalStart ..< nalEnd])) }
            buffer.removeFirst(next.index)
        }
    }

    /// Finds the next Annex-B start code at or after `startIndex`.
    /// Returns its offset and length (3 or 4), or nil if none is fully present.
    private func findStartCode(from startIndex: Int) -> (index: Int, length: Int)? {
        guard buffer.count >= 3, startIndex < buffer.count - 2 else { return nil }
        var index = max(0, startIndex)
        while index <= buffer.count - 3 {
            if buffer[index] == 0, buffer[index + 1] == 0 {
                if buffer[index + 2] == 1 { return (index, 3) }
                if index <= buffer.count - 4, buffer[index + 2] == 0, buffer[index + 3] == 1 {
                    return (index, 4)
                }
            }
            index += 1
        }
        return nil
    }
}

import AVFoundation
import CoreMedia
import Observation

/// Decodes an Annex-B H.264 stream and renders it to an `AVSampleBufferDisplayLayer`.
///
/// Direct port of the iOS companion's `H264Decoder` CoreMedia path, with the
/// byte-level start-code scanning delegated to `AnnexBParser`. `@MainActor`
/// because it drives a display layer owned by the view layer.
@available(macOS 15.0, *)
@MainActor
@Observable
final class H264AnnexBDecoder {
    private final class WeakLayer {
        weak var value: AVSampleBufferDisplayLayer?
        init(_ value: AVSampleBufferDisplayLayer) { self.value = value }
    }
    private var layers: [WeakLayer] = []
    private var displayLayers: [AVSampleBufferDisplayLayer] { layers.compactMap(\.value) }
    private var parser = AnnexBParser()
    private var sps: Data?
    private var pps: Data?
    private var formatDescription: CMVideoFormatDescription?
    private var currentAU = [Data]()
    private var frameCount = 0
    let calibrationFrames = H264CalibrationFrames()

    var imageSize: (width: Int, height: Int)? {
        guard let formatDescription else { return nil }
        let size = CMVideoFormatDescriptionGetDimensions(formatDescription)
        return (Int(size.width), Int(size.height))
    }

    func attach(layer: AVSampleBufferDisplayLayer) {
        layer.videoGravity = .resizeAspect
        layers.removeAll { $0.value == nil || $0.value === layer }
        layers.append(WeakLayer(layer))
    }

    func detach(layer: AVSampleBufferDisplayLayer) {
        layers.removeAll { $0.value == nil || $0.value === layer }
    }

    /// Clears all decode state and flushes the layer. Call when switching cameras
    /// so the new stream is not decoded against the previous stream's SPS/PPS.
    func reset() {
        calibrationFrames.reset()
        parser = AnnexBParser()
        sps = nil
        pps = nil
        formatDescription = nil
        currentAU.removeAll()
        frameCount = 0
        displayLayers.forEach { $0.sampleBufferRenderer.flush() }
    }

    /// Feed raw Annex-B bytes (one network chunk). Complete NALs are routed and,
    /// as access units close, enqueued on the display layer.
    func feed(_ chunk: Data) {
        for nal in parser.append(chunk) {
            process(nal: nal)
        }
    }

    // MARK: - NAL routing

    private func process(nal: Data) {
        guard let first = nal.first else { return }
        let nalType = first & 0x1F
        switch nalType {
        case 9: // Access Unit Delimiter
            flushAccessUnit()
        case 7: // SPS
            flushAccessUnit()
            sps = nal
            rebuildFormatDescription()
        case 8: // PPS
            pps = nal
            rebuildFormatDescription()
        case 1, 5: // Coded slice / IDR slice
            // Bit 7 of the second byte marks the first slice of a picture.
            if nal.count >= 2, (nal[nal.startIndex + 1] & 0x80) != 0, !currentAU.isEmpty {
                flushAccessUnit()
            }
            currentAU.append(nal)
        default:
            break
        }
    }

    // MARK: - Access unit -> display layer

    private func flushAccessUnit() {
        guard !currentAU.isEmpty, let formatDescription, (!displayLayers.isEmpty || calibrationFrames.isEnabled) else {
            currentAU.removeAll()
            return
        }

        let keyframe = currentAU.contains { ($0.first ?? 0) & 0x1f == 5 }

        // Build AVCC payload: [4-byte big-endian length][NAL] repeated.
        var avcc = Data()
        for nal in currentAU {
            let length = UInt32(nal.count).bigEndian
            withUnsafeBytes(of: length) { avcc.append(contentsOf: $0) }
            avcc.append(nal)
        }
        currentAU.removeAll()

        var blockBuffer: CMBlockBuffer?
        guard CMBlockBufferCreateWithMemoryBlock(
            allocator: nil, memoryBlock: nil, blockLength: avcc.count,
            blockAllocator: nil, customBlockSource: nil,
            offsetToData: 0, dataLength: avcc.count,
            flags: kCMBlockBufferAssureMemoryNowFlag, blockBufferOut: &blockBuffer
        ) == noErr, let blockBuffer else { return }

        guard avcc.withUnsafeBytes({ raw in
            CMBlockBufferReplaceDataBytes(
                with: raw.baseAddress!, blockBuffer: blockBuffer,
                offsetIntoDestination: 0, dataLength: avcc.count
            )
        }) == noErr else { return }

        var timing = CMSampleTimingInfo(
            duration: CMTime(value: 1, timescale: 30),
            presentationTimeStamp: CMTime(value: Int64(frameCount), timescale: 30),
            decodeTimeStamp: .invalid
        )
        var sampleBuffer: CMSampleBuffer?
        guard CMSampleBufferCreateReady(
            allocator: nil, dataBuffer: blockBuffer, formatDescription: formatDescription,
            sampleCount: 1, sampleTimingEntryCount: 1, sampleTimingArray: &timing,
            sampleSizeEntryCount: 1, sampleSizeArray: [avcc.count], sampleBufferOut: &sampleBuffer
        ) == noErr, let sampleBuffer else { return }

        if let attachments = CMSampleBufferGetSampleAttachmentsArray(sampleBuffer, createIfNecessary: true),
           CFArrayGetCount(attachments) > 0 {
            let attachment = unsafeBitCast(CFArrayGetValueAtIndex(attachments, 0), to: CFMutableDictionary.self)
            CFDictionarySetValue(attachment,
                Unmanaged.passUnretained(kCMSampleAttachmentKey_DisplayImmediately).toOpaque(),
                Unmanaged.passUnretained(kCFBooleanTrue).toOpaque())
        }
        if keyframe { calibrationFrames.accept(sampleBuffer, sequence: UInt64(frameCount), received: Date()) }
        for layer in displayLayers {
            if layer.sampleBufferRenderer.status == .failed { layer.sampleBufferRenderer.flush() }
            layer.sampleBufferRenderer.enqueue(sampleBuffer)
        }
        frameCount += 1
    }

    // MARK: - Format description

    private func rebuildFormatDescription() {
        guard let sps, let pps else { return }
        var desc: CMVideoFormatDescription?
        sps.withUnsafeBytes { spsRaw in
            pps.withUnsafeBytes { ppsRaw in
                let spsPtr = spsRaw.baseAddress!.assumingMemoryBound(to: UInt8.self)
                let ppsPtr = ppsRaw.baseAddress!.assumingMemoryBound(to: UInt8.self)
                var params: [UnsafePointer<UInt8>] = [spsPtr, ppsPtr]
                var sizes: [Int] = [sps.count, pps.count]
                CMVideoFormatDescriptionCreateFromH264ParameterSets(
                    allocator: nil, parameterSetCount: 2,
                    parameterSetPointers: &params, parameterSetSizes: &sizes,
                    nalUnitHeaderLength: 4, formatDescriptionOut: &desc
                )
            }
        }
        guard let desc else { return }
        self.formatDescription = desc
    }
}
