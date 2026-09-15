import AVFoundation
import CoreAudio
import Foundation

enum AudioKind: Sendable, Equatable {
    case input
    case output
}

struct AudioDeviceInfo: Sendable, Equatable {
    var id: UInt32
    var name: String
    var kind: AudioKind
    var isDefault: Bool
}

enum AudioError: Error, CustomStringConvertible {
    case deviceNotFound(UInt32)
    case osStatus(String, OSStatus)

    var description: String {
        switch self {
        case .deviceNotFound(let id):
            return "Audio device \(id) not found."
        case .osStatus(let action, let status):
            return "\(action) failed (OSStatus \(status))."
        }
    }
}

/// Manages the host's audio devices and input streaming.
protocol AudioManaging: Sendable {
    func listDevices(typeFilter: AudioKind?) async throws -> [AudioDeviceInfo]
    func setDefault(deviceID: UInt32) async throws
    func levels(
        deviceID: UInt32,
        rateHz: UInt32
    ) -> AsyncThrowingStream<
        (peakDb: Float, rmsDb: Float), any Error
    >
    func audio(
        deviceID: UInt32,
        sampleRate: UInt32,
        channels: UInt32
    ) -> AsyncThrowingStream<
        (pcm: Data, sampleRate: UInt32, channels: UInt32), any Error
    >
}

/// Live CoreAudio HAL + AVAudioEngine implementation.
struct AudioController: AudioManaging {
    // MARK: - Device enumeration

    // The CoreAudio HAL calls below are synchronous and can stall (e.g. while
    // devices are being added or removed), so they run off the cooperative pool.

    func listDevices(typeFilter: AudioKind?) async throws -> [AudioDeviceInfo] {
        try await BlockingExecutor.run {
            let ids = try Self.deviceIDs()
            let defaultInput = try? Self.defaultDevice(
                selector: kAudioHardwarePropertyDefaultInputDevice
            )
            let defaultOutput = try? Self.defaultDevice(
                selector: kAudioHardwarePropertyDefaultOutputDevice
            )

            var devices: [AudioDeviceInfo] = []
            for id in ids {
                let name = Self.deviceName(id)
                if typeFilter != .output,
                    Self.channelCount(id, scope: kAudioObjectPropertyScopeInput) > 0
                {
                    devices.append(
                        AudioDeviceInfo(
                            id: id,
                            name: name,
                            kind: .input,
                            isDefault: id == defaultInput
                        )
                    )
                }
                if typeFilter != .input,
                    Self.channelCount(id, scope: kAudioObjectPropertyScopeOutput) > 0
                {
                    devices.append(
                        AudioDeviceInfo(
                            id: id,
                            name: name,
                            kind: .output,
                            isDefault: id == defaultOutput
                        )
                    )
                }
            }
            return devices
        }
    }

    func setDefault(deviceID: UInt32) async throws {
        try await BlockingExecutor.run {
            let ids = try Self.deviceIDs()
            guard ids.contains(deviceID) else { throw AudioError.deviceNotFound(deviceID) }

            if Self.channelCount(deviceID, scope: kAudioObjectPropertyScopeOutput) > 0 {
                try Self.setDefaultDevice(
                    selector: kAudioHardwarePropertyDefaultOutputDevice,
                    id: deviceID
                )
            }
            if Self.channelCount(deviceID, scope: kAudioObjectPropertyScopeInput) > 0 {
                try Self.setDefaultDevice(
                    selector: kAudioHardwarePropertyDefaultInputDevice,
                    id: deviceID
                )
            }
        }
    }

    // MARK: - Streaming

    func levels(
        deviceID: UInt32,
        rateHz: UInt32
    ) -> AsyncThrowingStream<
        (peakDb: Float, rmsDb: Float), any Error
    > {
        // Bound the buffer: the CoreAudio render thread produces faster than a
        // slow gRPC consumer drains. Keeping only the newest samples caps memory
        // and keeps levels current rather than replaying a stale backlog.
        AsyncThrowingStream(bufferingPolicy: .bufferingNewest(8)) { continuation in
            let session = AudioTapSession()
            let interval = rateHz == 0 ? 0.1 : 1.0 / Double(rateHz)
            do {
                try session.start(deviceID: deviceID, minInterval: interval) { buffer in
                    let (peak, rms) = AudioTapSession.levels(of: buffer)
                    continuation.yield((peakDb: peak, rmsDb: rms))
                }
            } catch {
                continuation.finish(throwing: error)
                return
            }
            continuation.onTermination = { _ in session.stop() }
        }
    }

    func audio(
        deviceID: UInt32,
        sampleRate: UInt32,
        channels: UInt32
    ) -> AsyncThrowingStream<
        (pcm: Data, sampleRate: UInt32, channels: UInt32), any Error
    > {
        // Bound the buffer so a stalled network consumer drops old audio chunks
        // instead of accumulating an unbounded backlog. For a live mic, low
        // latency beats completeness: keep only ~4 buffers (~4096 frames each,
        // ~340ms at 48kHz) so a brief consumer stall drops stale audio and
        // stays current rather than adding seconds of lag. 32 buffers (~2.7s)
        // sounded "mega laggy" in testing.
        AsyncThrowingStream(bufferingPolicy: .bufferingNewest(4)) { continuation in
            let session = AudioTapSession()
            do {
                try session.start(deviceID: deviceID, minInterval: 0) { buffer in
                    let data = AudioTapSession.int16PCM(from: buffer)
                    let format = buffer.format
                    continuation.yield(
                        (
                            pcm: data,
                            sampleRate: UInt32(format.sampleRate),
                            channels: UInt32(format.channelCount)
                        )
                    )
                }
            } catch {
                continuation.finish(throwing: error)
                return
            }
            continuation.onTermination = { _ in session.stop() }
        }
    }

    // MARK: - CoreAudio helpers

    private static func deviceIDs() throws -> [AudioDeviceID] {
        var address = AudioObjectPropertyAddress(
            mSelector: kAudioHardwarePropertyDevices,
            mScope: kAudioObjectPropertyScopeGlobal,
            mElement: kAudioObjectPropertyElementMain
        )
        var dataSize: UInt32 = 0
        var status = AudioObjectGetPropertyDataSize(
            AudioObjectID(kAudioObjectSystemObject),
            &address,
            0,
            nil,
            &dataSize
        )
        guard status == noErr else { throw AudioError.osStatus("List audio devices", status) }

        let count = Int(dataSize) / MemoryLayout<AudioDeviceID>.size
        var ids = [AudioDeviceID](repeating: 0, count: count)
        status = AudioObjectGetPropertyData(
            AudioObjectID(kAudioObjectSystemObject),
            &address,
            0,
            nil,
            &dataSize,
            &ids
        )
        guard status == noErr else { throw AudioError.osStatus("List audio devices", status) }
        return ids
    }

    private static func deviceName(_ id: AudioDeviceID) -> String {
        var address = AudioObjectPropertyAddress(
            mSelector: kAudioObjectPropertyName,
            mScope: kAudioObjectPropertyScopeGlobal,
            mElement: kAudioObjectPropertyElementMain
        )
        var name: CFString = "" as CFString
        var dataSize = UInt32(MemoryLayout<CFString>.size)
        let status = withUnsafeMutablePointer(to: &name) {
            AudioObjectGetPropertyData(id, &address, 0, nil, &dataSize, $0)
        }
        guard status == noErr else { return "Audio Device \(id)" }
        return name as String
    }

    private static func channelCount(_ id: AudioDeviceID, scope: AudioObjectPropertyScope) -> Int {
        var address = AudioObjectPropertyAddress(
            mSelector: kAudioDevicePropertyStreamConfiguration,
            mScope: scope,
            mElement: kAudioObjectPropertyElementMain
        )
        var dataSize: UInt32 = 0
        guard AudioObjectGetPropertyDataSize(id, &address, 0, nil, &dataSize) == noErr,
            dataSize > 0
        else { return 0 }

        let bufferList = UnsafeMutableRawPointer.allocate(
            byteCount: Int(dataSize),
            alignment: MemoryLayout<AudioBufferList>.alignment
        )
        defer { bufferList.deallocate() }
        guard AudioObjectGetPropertyData(id, &address, 0, nil, &dataSize, bufferList) == noErr
        else { return 0 }

        let listPointer = UnsafeMutableAudioBufferListPointer(
            bufferList.assumingMemoryBound(to: AudioBufferList.self)
        )
        return listPointer.reduce(0) { $0 + Int($1.mNumberChannels) }
    }

    private static func defaultDevice(selector: AudioObjectPropertySelector) throws -> AudioDeviceID
    {
        var address = AudioObjectPropertyAddress(
            mSelector: selector,
            mScope: kAudioObjectPropertyScopeGlobal,
            mElement: kAudioObjectPropertyElementMain
        )
        var deviceID = AudioDeviceID(0)
        var dataSize = UInt32(MemoryLayout<AudioDeviceID>.size)
        let status = AudioObjectGetPropertyData(
            AudioObjectID(kAudioObjectSystemObject),
            &address,
            0,
            nil,
            &dataSize,
            &deviceID
        )
        guard status == noErr else { throw AudioError.osStatus("Get default device", status) }
        return deviceID
    }

    private static func setDefaultDevice(
        selector: AudioObjectPropertySelector,
        id: AudioDeviceID
    )
        throws
    {
        var address = AudioObjectPropertyAddress(
            mSelector: selector,
            mScope: kAudioObjectPropertyScopeGlobal,
            mElement: kAudioObjectPropertyElementMain
        )
        var deviceID = id
        let status = AudioObjectSetPropertyData(
            AudioObjectID(kAudioObjectSystemObject),
            &address,
            0,
            nil,
            UInt32(MemoryLayout<AudioDeviceID>.size),
            &deviceID
        )
        guard status == noErr else { throw AudioError.osStatus("Set default device", status) }
    }
}

/// Holds a live `AVAudioEngine` input tap for the duration of a stream.
///
/// `@unchecked Sendable` safety invariant: after `start(...)` installs the tap,
/// `lastEmit` is read and written only from the single serialized audio render
/// thread that drives the tap callback, so those mutations never race. `start`
/// and `stop` are each called once from the owning `AsyncThrowingStream` build /
/// termination closures and do not touch `lastEmit`.
final class AudioTapSession: @unchecked Sendable {
    private let engine = AVAudioEngine()
    private var lastEmit = Date.distantPast

    /// Installs an input tap. `minInterval` throttles callbacks (0 = every buffer).
    func start(
        deviceID: UInt32,
        minInterval: TimeInterval,
        onBuffer: @escaping @Sendable (AVAudioPCMBuffer) -> Void
    ) throws {
        let input = engine.inputNode
        let format = input.inputFormat(forBus: 0)
        input.installTap(onBus: 0, bufferSize: 4096, format: format) { [self] buffer, _ in
            let now = Date()
            if minInterval > 0, now.timeIntervalSince(lastEmit) < minInterval { return }
            lastEmit = now
            onBuffer(buffer)
        }
        engine.prepare()
        try engine.start()
    }

    func stop() {
        engine.inputNode.removeTap(onBus: 0)
        engine.stop()
    }

    // MARK: - Buffer math (pure)

    static func levels(of buffer: AVAudioPCMBuffer) -> (peakDb: Float, rmsDb: Float) {
        guard let channelData = buffer.floatChannelData, buffer.frameLength > 0 else {
            return (-160, -160)
        }
        let frameCount = Int(buffer.frameLength)
        let channelCount = Int(buffer.format.channelCount)
        var peak: Float = 0
        var sumSquares: Float = 0
        for channel in 0..<channelCount {
            let samples = channelData[channel]
            for frame in 0..<frameCount {
                let value = abs(samples[frame])
                peak = max(peak, value)
                sumSquares += value * value
            }
        }
        let rms = (sumSquares / Float(max(1, frameCount * channelCount))).squareRoot()
        return (linearToDb(peak), linearToDb(rms))
    }

    static func linearToDb(_ value: Float) -> Float {
        value <= 0 ? -160 : max(-160, 20 * log10(value))
    }

    /// Converts a float buffer to interleaved 16-bit little-endian PCM.
    static func int16PCM(from buffer: AVAudioPCMBuffer) -> Data {
        guard let channelData = buffer.floatChannelData, buffer.frameLength > 0 else {
            return Data()
        }
        let frameCount = Int(buffer.frameLength)
        let channelCount = Int(buffer.format.channelCount)
        var samples = [Int16]()
        samples.reserveCapacity(frameCount * channelCount)
        for frame in 0..<frameCount {
            for channel in 0..<channelCount {
                let clamped = max(-1, min(1, channelData[channel][frame]))
                samples.append(Int16(clamped * Float(Int16.max)))
            }
        }
        return samples.withUnsafeBytes { Data($0) }
    }
}
