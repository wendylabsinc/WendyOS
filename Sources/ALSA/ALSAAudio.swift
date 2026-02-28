#if os(Linux)
    import Foundation
    #if canImport(Glibc)
        import Glibc
    #elseif canImport(Musl)
        import Musl
    #endif
    import NIOCore
    import Subprocess

    /// Represents an ALSA audio device
    public struct ALSAAudioDevice: Sendable {
        public let cardIndex: Int
        public let deviceIndex: Int
        public let name: String
        public let cardName: String
        public let isCapture: Bool
        public let isPlayback: Bool

        /// ALSA device identifier for opening (e.g., "plughw:0,0")
        public var deviceId: String {
            "plughw:\(cardIndex),\(deviceIndex)"
        }

        /// Hardware device identifier (e.g., "hw:0,0")
        public var hwDeviceId: String {
            "hw:\(cardIndex),\(deviceIndex)"
        }
    }

    /// High-level ALSA audio interface using direct /proc/asound access for listing
    /// and arecord subprocess for capture
    public struct ALSAAudio: Sendable {

        public init() throws {
            // Verify /proc/asound exists
            guard FileManager.default.fileExists(atPath: "/proc/asound/cards") else {
                throw ALSAError.notAvailable
            }
        }

        /// Check if ALSA is available on this system
        public static var isAvailable: Bool {
            FileManager.default.fileExists(atPath: "/proc/asound/cards")
        }

        /// Check if arecord is available for audio capture
        public static var isArecordAvailable: Bool {
            FileManager.default.fileExists(atPath: "/usr/bin/arecord")
        }

        /// Get the initialization error if ALSA failed to load (for debugging)
        public static var initializationError: Error? {
            if isAvailable {
                return nil
            }
            return ALSAError.notAvailable
        }

        /// List all available audio devices
        public func listDevices() throws -> [ALSAAudioDevice] {
            var devices: [ALSAAudioDevice] = []

            let cards = try ALSADirect.listCards()

            for card in cards {
                for pcmDevice in card.pcmDevices {
                    // Use card name as the primary name (it usually has the actual device name like "Blue Yeti")
                    // Use PCM device name as secondary info if it differs from card name
                    let displayName: String
                    let description: String

                    if card.pcmDevices.count == 1 {
                        // Single PCM device on this card - just use the card name
                        displayName = card.name
                        description = pcmDevice.name != card.name ? pcmDevice.name : ""
                    } else {
                        // Multiple PCM devices - include device index for clarity
                        displayName = "\(card.name) (\(pcmDevice.name))"
                        description = ""
                    }

                    devices.append(
                        ALSAAudioDevice(
                            cardIndex: card.index,
                            deviceIndex: pcmDevice.index,
                            name: displayName,
                            cardName: description,
                            isCapture: pcmDevice.isCapture,
                            isPlayback: pcmDevice.isPlayback
                        )
                    )
                }
            }

            return devices
        }

        /// Open a PCM device for capture using arecord subprocess
        public func openCapture(
            device: String = "default",
            sampleRate: UInt32 = 48000,
            channels: UInt32 = 1,
            latencyMicroseconds: UInt32 = 100000
        ) throws -> ALSACaptureStream {
            // Parse device string like "plughw:0,0" or "hw:0,0"
            var cardIndex = 0
            var deviceIndex = 0

            if device != "default" {
                // Parse "plughw:X,Y" or "hw:X,Y" format
                let parts = device.components(separatedBy: ":")
                if parts.count == 2 {
                    let indices = parts[1].components(separatedBy: ",")
                    if indices.count >= 1, let card = Int(indices[0]) {
                        cardIndex = card
                    }
                    if indices.count >= 2, let dev = Int(indices[1]) {
                        deviceIndex = dev
                    }
                }
            }

            return try ALSACaptureStream(
                cardIndex: cardIndex,
                deviceIndex: deviceIndex,
                sampleRate: sampleRate,
                channels: channels
            )
        }
    }

    /// ALSA PCM capture stream using arecord subprocess
    public struct ALSACaptureStream: Sendable {
        public let cardIndex: Int
        public let deviceIndex: Int
        public let sampleRate: UInt32
        public let channels: UInt32
        private let bytesPerFrame: Int

        init(
            cardIndex: Int,
            deviceIndex: Int,
            sampleRate: UInt32,
            channels: UInt32
        ) throws {
            self.cardIndex = cardIndex
            self.deviceIndex = deviceIndex
            self.sampleRate = sampleRate
            self.channels = channels
            self.bytesPerFrame = Int(channels) * 2  // 16-bit = 2 bytes per sample

            // Check if arecord is available
            guard ALSAAudio.isArecordAvailable else {
                throw ALSAError.setParamsFailed(
                    "arecord not found. Please install alsa-utils package."
                )
            }
        }

        public func withAudioData(
            framesPerChunk: Int,
            handler: @Sendable @escaping (ByteBuffer) async throws -> Void
        ) async throws {
            let device = "plughw:\(cardIndex),\(deviceIndex)"

            _ = try await Subprocess.run(
                .path("/usr/bin/arecord"),
                arguments: [
                    "-D", device,
                    "-f", "S16_LE",
                    "-r", "\(sampleRate)",
                    "-c", "\(channels)",
                    "-t", "raw",
                    "-q",  // Quiet mode
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
