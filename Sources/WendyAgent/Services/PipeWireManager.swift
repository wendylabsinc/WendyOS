import Foundation
import Logging
import NIOCore
import Subprocess
import _NIOFileSystem

#if os(Linux)
    import ALSA
#endif

/// Represents an audio device
public struct AudioDevice: Sendable {
    public let id: UInt32
    public let name: String
    public let description: String
    public let type: AudioDeviceType
    public let source: AudioSource
    public let isDefault: Bool
    public let cardIndex: Int
    public let deviceIndex: Int

    public enum AudioDeviceType: Sendable {
        case input  // Microphone/source
        case output  // Speaker/sink
    }

    public enum AudioSource: Sendable {
        case pipewire  // PipeWire-managed device (including Bluetooth)
        case alsa  // Direct ALSA device
    }

    /// ALSA device identifier (e.g., "plughw:0,0")
    public var alsaDevice: String {
        "plughw:\(cardIndex),\(deviceIndex)"
    }
}

/// Errors that can occur during audio operations
public enum AudioError: Error, LocalizedError {
    case commandFailed(String)
    case parseError(String)
    case deviceNotFound(UInt32)
    case recordingFailed(String)
    case notAvailable

    public var errorDescription: String? {
        switch self {
        case .commandFailed(let msg): return "Command failed: \(msg)"
        case .parseError(let msg): return "Parse error: \(msg)"
        case .deviceNotFound(let id): return "Device not found: \(id)"
        case .recordingFailed(let msg): return "Recording failed: \(msg)"
        case .notAvailable: return "Audio system not available"
        }
    }
}

// Keep old name for compatibility
public typealias PipeWireError = AudioError

/// Manager for interacting with ALSA audio system
public actor PipeWireManager {
    private nonisolated let logger = Logger(label: "AudioManager")

    // Track which device is currently selected as "default" (by ID)
    private var defaultInputId: UInt32?
    private var defaultOutputId: UInt32?

    // Cache of devices for quick lookup
    private var cachedDevices: [AudioDevice]?

    public init() {}

    /// List all available audio devices using PipeWire (wpctl)
    /// This includes Bluetooth devices managed by WirePlumber
    public func listDevices(
        typeFilter: AudioDevice.AudioDeviceType? = nil
    ) async throws -> [AudioDevice] {
        logger.debug(
            "Listing audio devices",
            metadata: ["typeFilter": "\(String(describing: typeFilter))"]
        )

        #if os(Linux)
            // Use wpctl to list devices - this includes PipeWire-managed Bluetooth devices
            let devices = try await listDevicesWithWpctl(typeFilter: typeFilter)

            // Fall back to ALSA if wpctl returns nothing (PipeWire not running)
            if devices.isEmpty {
                logger.debug("wpctl returned no devices, falling back to ALSA")
                return try listDevicesWithALSA(typeFilter: typeFilter)
            }

            cachedDevices = devices
            logger.info("Found \(devices.count) audio devices via PipeWire")
            return devices
        #else
            // macOS - return empty for now (audio handled differently)
            return []
        #endif
    }

    #if os(Linux)
        /// List devices using wpctl (PipeWire)
        private func listDevicesWithWpctl(
            typeFilter: AudioDevice.AudioDeviceType? = nil
        ) async throws -> [AudioDevice] {
            do {
                // Quick check if PipeWire is running
                let pidofResult = try await Subprocess.run(
                    .name("pidof"),
                    arguments: ["pipewire"],
                    output: .string(limit: 1024)
                )
                guard pidofResult.terminationStatus.isSuccess else {
                    logger.debug("PipeWire not running, skipping wpctl")
                    return []
                }

                let result = try await Subprocess.run(
                    .name("wpctl"),
                    arguments: ["status"],
                    output: .string(limit: .max)
                )

                guard result.terminationStatus.isSuccess else {
                    logger.warning("wpctl exited with non-zero status")
                    return []
                }

                guard let output = result.standardOutput else {
                    return []
                }

                return parseWpctlOutput(output, typeFilter: typeFilter)
            } catch {
                logger.warning("Failed to run wpctl: \(error)")
                return []
            }
        }

        /// Parse wpctl status output to extract audio devices
        private func
            parseWpctlOutput(
                _ output: String,
                typeFilter: AudioDevice.AudioDeviceType?
            ) -> [AudioDevice]
        {
            var devices: [AudioDevice] = []
            let lines = output.components(separatedBy: "\n")

            var inAudioSection = false
            var currentSection: String? = nil  // "Sinks", "Sources", or "Filters"

            for line in lines {
                // Detect Audio section
                if line.hasPrefix("Audio") {
                    inAudioSection = true
                    continue
                }

                // Detect end of Audio section (Video or Settings)
                if inAudioSection && (line.hasPrefix("Video") || line.hasPrefix("Settings")) {
                    break
                }

                guard inAudioSection else { continue }

                // Detect subsections
                if line.contains("Sinks:") {
                    currentSection = "Sinks"
                    continue
                } else if line.contains("Sources:") {
                    currentSection = "Sources"
                    continue
                } else if line.contains("Filters:") {
                    currentSection = "Filters"
                    continue
                } else if line.contains("Devices:") || line.contains("Streams:") {
                    currentSection = nil
                    continue
                }

                // Must be in a relevant section
                guard
                    currentSection == "Sinks" || currentSection == "Sources"
                        || currentSection == "Filters"
                else {
                    continue
                }

                // Extract device info using regex
                // Pattern: optional *, then ID, then name, then optional [vol: X.XX] or [Audio/Source] etc
                // Capture the bracket content to determine device type for Filters
                let pattern = #"^\s*[│├└─\s]*(\*?)\s*(\d+)\.\s+(.+?)(?:\s+\[([^\]]*)\])?\s*$"#
                guard let regex = try? NSRegularExpression(pattern: pattern, options: []),
                    let match = regex.firstMatch(
                        in: line,
                        options: [],
                        range: NSRange(line.startIndex..., in: line)
                    )
                else {
                    continue
                }

                let isDefaultStr =
                    match.range(at: 1).location != NSNotFound
                    ? String(line[Range(match.range(at: 1), in: line)!]) : ""
                let isDefault = isDefaultStr.contains("*")

                guard let idRange = Range(match.range(at: 2), in: line),
                    let id = UInt32(line[idRange])
                else { continue }

                guard let nameRange = Range(match.range(at: 3), in: line) else { continue }
                let name = String(line[nameRange]).trimmingCharacters(in: .whitespaces)

                // Get bracket content if present (e.g., "vol: 0.69" or "Audio/Source")
                var bracketContent = ""
                if match.range(at: 4).location != NSNotFound,
                    let bracketRange = Range(match.range(at: 4), in: line)
                {
                    bracketContent = String(line[bracketRange])
                }

                // Skip internal/loopback devices
                if name.contains("bluez_capture_internal") || name.contains("loopback-") {
                    continue
                }

                // Determine device type
                let deviceType: AudioDevice.AudioDeviceType
                if currentSection == "Sinks" {
                    deviceType = .output
                } else if currentSection == "Sources" {
                    deviceType = .input
                } else if currentSection == "Filters" {
                    // In Filters section, check bracket content to determine type
                    // [Audio/Source] = input, [Audio/Sink] = output
                    if bracketContent.contains("Source") || name.contains("_input") {
                        deviceType = .input
                    } else if bracketContent.contains("Sink") || name.contains("_output") {
                        deviceType = .output
                    } else {
                        // Skip unknown filter types
                        continue
                    }
                } else {
                    continue
                }

                // Skip if we're filtering and this isn't the right type
                if let filter = typeFilter, filter != deviceType {
                    continue
                }

                // Determine description based on device type hints in name
                var description = ""
                if name.lowercased().contains("bluetooth") || name.lowercased().contains("bluez") {
                    description = "Bluetooth"
                } else if name.lowercased().contains("webcam") || name.lowercased().contains("c920")
                {
                    description = "USB Webcam"
                } else if name.lowercased().contains("built-in")
                    || name.lowercased().contains("analog")
                {
                    description = "Built-in"
                }

                // Create a user-friendly name for bluez devices
                var displayName = name
                if name.hasPrefix("bluez_input.") || name.hasPrefix("bluez_output.") {
                    // Try to find a friendlier name - for now just indicate it's Bluetooth audio
                    let address = name.components(separatedBy: ".").last ?? ""
                    displayName = "Bluetooth Audio (\(address))"
                    description = "Bluetooth"
                }

                devices.append(
                    AudioDevice(
                        id: id,
                        name: displayName,
                        description: description,
                        type: deviceType,
                        source: .pipewire,
                        isDefault: isDefault,
                        cardIndex: 0,  // Not applicable for PipeWire devices
                        deviceIndex: 0
                    )
                )
            }

            return devices
        }

        /// Fallback: List devices using direct ALSA access
        private func listDevicesWithALSA(
            typeFilter: AudioDevice.AudioDeviceType? = nil
        ) throws -> [AudioDevice] {
            guard ALSAAudio.isAvailable else {
                if let initError = ALSAAudio.initializationError {
                    throw AudioError.commandFailed("ALSA not available: \(initError)")
                }
                throw AudioError.notAvailable
            }

            let alsa = try ALSAAudio()
            let alsaDevices = try alsa.listDevices()

            var devices: [AudioDevice] = []
            var nextId: UInt32 = 1

            for alsaDevice in alsaDevices {
                if alsaDevice.isCapture && (typeFilter == nil || typeFilter == .input) {
                    let isDefault =
                        defaultInputId == nextId || (defaultInputId == nil && nextId == 1)
                    devices.append(
                        AudioDevice(
                            id: nextId,
                            name: alsaDevice.name,
                            description: "\(alsaDevice.cardName) - Input",
                            type: .input,
                            source: .alsa,
                            isDefault: isDefault,
                            cardIndex: alsaDevice.cardIndex,
                            deviceIndex: alsaDevice.deviceIndex
                        )
                    )
                    nextId += 1
                }

                if alsaDevice.isPlayback && (typeFilter == nil || typeFilter == .output) {
                    let firstOutputId = devices.first { $0.type == .output }?.id ?? nextId
                    let isDefault =
                        defaultOutputId == nextId
                        || (defaultOutputId == nil && nextId == firstOutputId)
                    devices.append(
                        AudioDevice(
                            id: nextId,
                            name: alsaDevice.name,
                            description: "\(alsaDevice.cardName) - Output",
                            type: .output,
                            source: .alsa,
                            isDefault: isDefault,
                            cardIndex: alsaDevice.cardIndex,
                            deviceIndex: alsaDevice.deviceIndex
                        )
                    )
                    nextId += 1
                }
            }

            logger.info("Found \(devices.count) audio devices via ALSA fallback")
            return devices
        }
    #endif

    /// Set the default audio device (stored in memory for this session)
    public func setDefaultDevice(id: UInt32) async throws {
        logger.info("Setting default device", metadata: ["id": "\(id)"])

        let allDevices = try await listDevices()
        guard let device = allDevices.first(where: { $0.id == id }) else {
            throw AudioError.deviceNotFound(id)
        }

        if device.type == .input {
            defaultInputId = id
        } else {
            defaultOutputId = id
        }

        logger.info(
            "Successfully set default device",
            metadata: ["id": "\(id)", "type": "\(device.type)"]
        )
    }

    /// Get the device identifier for a device ID (or default)
    /// Returns either an ALSA device string (plughw:X,Y) or a PipeWire node ID
    public func getALSADevice(
        forId id: UInt32?,
        type: AudioDevice.AudioDeviceType
    ) async throws -> String {
        let devices = try await listDevices(typeFilter: type)

        if let id, id != 0 {
            guard let device = devices.first(where: { $0.id == id }) else {
                throw AudioError.deviceNotFound(id)
            }
            // For PipeWire devices, return the PipeWire node ID
            if device.source == .pipewire {
                return "pipewire:\(device.id)"
            }
            return device.alsaDevice
        }

        // Use default
        let defaultId = type == .input ? defaultInputId : defaultOutputId
        if let defaultId = defaultId, let device = devices.first(where: { $0.id == defaultId }) {
            if device.source == .pipewire {
                return "pipewire:\(device.id)"
            }
            return device.alsaDevice
        }

        // Fall back to first device
        guard let first = devices.first else {
            throw AudioError.deviceNotFound(0)
        }
        if first.source == .pipewire {
            return "pipewire:\(first.id)"
        }
        return first.alsaDevice
    }

    /// Stream audio levels from a device using structured concurrency
    ///
    /// - Parameters:
    ///   - deviceId: Device ID to stream from (nil for default)
    ///   - updateRateHz: Update rate in Hz (1-60)
    ///   - handler: Called for each audio level update
    ///
    /// Note: This method is nonisolated to avoid blocking the actor during I/O.
    nonisolated public func withAudioLevels(
        deviceId: UInt32?,
        updateRateHz: UInt32,
        handler: @Sendable @escaping (Float, Float) async throws -> Void
    ) async throws {
        #if os(Linux)
            // Get device string while isolated, then release the actor
            let deviceString = try await getALSADevice(forId: deviceId, type: .input)
            let rate = max(1, min(60, updateRateHz))
            let sampleRate: UInt32 = 48000
            let samplesPerUpdate = Int(sampleRate / rate)

            logger.debug("Starting audio level monitoring", metadata: ["device": "\(deviceString)"])

            // Check if this is a PipeWire device
            if deviceString.hasPrefix("pipewire:") {
                let nodeId = String(deviceString.dropFirst("pipewire:".count))
                try await streamFromPipeWire(
                    nodeId: nodeId,
                    sampleRate: sampleRate,
                    channels: 1,
                    framesPerChunk: samplesPerUpdate
                ) { buffer in
                    let levels = Self.calculateLevels(from: buffer)
                    try await handler(levels.peakDb, levels.rmsDb)
                }
            } else {
                // Use ALSA for hardware devices
                let alsa = try ALSAAudio()
                let stream = try alsa.openCapture(
                    device: deviceString,
                    sampleRate: sampleRate,
                    channels: 1,
                    latencyMicroseconds: 50000
                )

                try await stream.withAudioData(framesPerChunk: samplesPerUpdate) { buffer in
                    let levels = Self.calculateLevels(from: buffer)
                    try await handler(levels.peakDb, levels.rmsDb)
                }
            }
        #else
            throw AudioError.notAvailable
        #endif
    }

    /// Stream raw PCM audio from a device using structured concurrency
    ///
    /// - Parameters:
    ///   - deviceId: Device ID to stream from (nil for default)
    ///   - sampleRate: Sample rate in Hz
    ///   - channels: Number of channels
    ///   - handler: Called for each audio chunk with (data, timestampNs)
    ///
    /// Note: This method is nonisolated to avoid blocking the actor during I/O.
    nonisolated public func withAudioStream(
        deviceId: UInt32?,
        sampleRate: UInt32,
        channels: UInt32,
        handler: @Sendable @escaping (ByteBuffer, UInt64) async throws -> Void
    ) async throws {
        #if os(Linux)
            let rate = sampleRate == 0 ? 48000 : sampleRate
            let chans = channels == 0 ? 1 : channels

            // Get device string while isolated, then release the actor
            let deviceString = try await getALSADevice(forId: deviceId, type: .input)
            logger.debug(
                "Starting audio stream",
                metadata: [
                    "device": "\(deviceString)",
                    "rate": "\(rate)",
                    "channels": "\(chans)",
                ]
            )

            let startTime = DispatchTime.now().uptimeNanoseconds
            // Stream audio in ~20ms chunks
            let framesPerChunk = Int(rate / 50)

            // Check if this is a PipeWire device
            if deviceString.hasPrefix("pipewire:") {
                let nodeId = String(deviceString.dropFirst("pipewire:".count))
                try await streamFromPipeWire(
                    nodeId: nodeId,
                    sampleRate: rate,
                    channels: chans,
                    framesPerChunk: framesPerChunk
                ) { buffer in
                    let timestampNs = DispatchTime.now().uptimeNanoseconds - startTime
                    try await handler(buffer, timestampNs)
                }
            } else {
                // Use ALSA for hardware devices
                let alsa = try ALSAAudio()
                let stream = try alsa.openCapture(
                    device: deviceString,
                    sampleRate: rate,
                    channels: chans,
                    latencyMicroseconds: 50000
                )

                try await stream.withAudioData(framesPerChunk: framesPerChunk) { buffer in
                    let timestampNs = DispatchTime.now().uptimeNanoseconds - startTime
                    try await handler(buffer, timestampNs)
                }
            }
        #else
            throw AudioError.notAvailable
        #endif
    }

    // MARK: - Legacy AsyncThrowingStream API (for gRPC compatibility)

    /// Stream audio levels from a device
    /// Returns an AsyncStream of (peakDb, rmsDb) tuples
    ///
    /// Note: This uses AsyncStream.makeStream() pattern for proper structured concurrency
    public func streamAudioLevels(
        deviceId: UInt32?,
        updateRateHz: UInt32
    ) -> AsyncThrowingStream<(peakDb: Float, rmsDb: Float), Error> {
        let (stream, continuation) = AsyncThrowingStream<(peakDb: Float, rmsDb: Float), Error>
            .makeStream()

        let task = Task { [self] in
            do {
                try await withAudioLevels(deviceId: deviceId, updateRateHz: updateRateHz) {
                    peakDb,
                    rmsDb in
                    continuation.yield((peakDb: peakDb, rmsDb: rmsDb))
                }
                continuation.finish()
            } catch {
                continuation.finish(throwing: error)
            }
        }

        continuation.onTermination = { _ in
            task.cancel()
        }

        return stream
    }

    // MARK: - Private Helpers

    /// Calculate peak and RMS levels from PCM data
    private static func calculateLevels(from buffer: ByteBuffer) -> (peakDb: Float, rmsDb: Float) {
        guard buffer.readableBytes > 0 else {
            return (peakDb: -96.0, rmsDb: -96.0)
        }

        var peak: Int16 = 0
        var sumSquares: Float = 0

        buffer.readableBytesSpan.withUnsafeBytes { buffer in
            let samples = buffer.bindMemory(to: Int16.self)
            for sample in samples {
                let absSample = abs(sample)
                if absSample > peak {
                    peak = absSample
                }
                let normalized = Float(sample) / Float(Int16.max)
                sumSquares += normalized * normalized
            }
        }

        let sampleCount = Int(buffer.readableBytes / 2)
        let rms = sqrt(sumSquares / Float(max(1, sampleCount)))

        // Convert to dB (0 dB = max amplitude)
        let peakDb = peak > 0 ? 20 * log10(Float(peak) / Float(Int16.max)) : -96.0
        let rmsDb = rms > 0 ? 20 * log10(rms) : -96.0

        return (peakDb: peakDb, rmsDb: rmsDb)
    }
}

// MARK: - PipeWire Capture Helpers

#if os(Linux)
    extension PipeWireManager {
        /// Stream audio from a PipeWire node using pw-record with async I/O
        ///
        /// - Parameters:
        ///   - nodeId: PipeWire node ID to capture from
        ///   - sampleRate: Sample rate in Hz
        ///   - channels: Number of channels
        ///   - framesPerChunk: Number of frames per chunk to yield
        ///   - handler: Called for each audio chunk
        private func streamFromPipeWire(
            nodeId: String,
            sampleRate: UInt32,
            channels: UInt32,
            framesPerChunk: Int,
            handler: @Sendable @escaping (ByteBuffer) async throws -> Void
        ) async throws {
            let bytesPerFrame = Int(channels) * 2  // 16-bit = 2 bytes per sample
            let bytesPerChunk = framesPerChunk * bytesPerFrame

            _ = try await Subprocess.run(
                .path("/usr/bin/pw-record"),
                arguments: [
                    "--target", nodeId,
                    "--format", "s16",
                    "--rate", "\(sampleRate)",
                    "--channels", "\(channels)",
                    "-",  // Output to stdout
                ]
            ) { execution, stdin, stdout, _ in
                do {
                    for try await chunk in stdout {
                        try Task.checkCancellation()

                        var buffer = ByteBuffer()
                        chunk.withUnsafeBytes { bytes in
                            _ = buffer.writeBytes(bytes)
                        }
                        try await handler(buffer)
                    }
                    try await stdin.finish()
                    try execution.send(signal: .interrupt)
                } catch {
                    try execution.send(signal: .kill)
                    throw error
                }
            }
        }
    }
#endif
