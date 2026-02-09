#if os(macOS)
    import AVFoundation
    import Foundation
    import os

    /// Audio player for streaming PCM audio to Mac speakers
    final class AudioPlayer: @unchecked Sendable {
        private let engine: AVAudioEngine
        private let playerNode: AVAudioPlayerNode
        private let format: AVAudioFormat
        private let bufferQueue = DispatchQueue(label: "audio.buffer.queue")
        private var isRunning = false
        private let logger = Logger(subsystem: "wendy.audio", category: "AudioPlayer")

        /// Initialize the audio player with specified format
        /// - Parameters:
        ///   - sampleRate: Sample rate in Hz (e.g., 48000)
        ///   - channels: Number of audio channels (1 for mono, 2 for stereo)
        init(sampleRate: Double, channels: Int) throws {
            engine = AVAudioEngine()
            playerNode = AVAudioPlayerNode()

            // Create format for signed 16-bit little-endian PCM
            guard
                let audioFormat = AVAudioFormat(
                    commonFormat: .pcmFormatInt16,
                    sampleRate: sampleRate,
                    channels: AVAudioChannelCount(channels),
                    interleaved: true
                )
            else {
                throw AudioPlayerError.invalidFormat
            }

            format = audioFormat

            // Set up the audio engine
            engine.attach(playerNode)
            engine.connect(playerNode, to: engine.mainMixerNode, format: format)
        }

        /// Start the audio engine and player
        func start() throws {
            guard !isRunning else { return }

            do {
                try engine.start()
                playerNode.play()
                isRunning = true
            } catch {
                throw AudioPlayerError.engineStartFailed(error)
            }
        }

        /// Stop the audio engine and player
        func stop() {
            guard isRunning else { return }

            playerNode.stop()
            engine.stop()
            isRunning = false
        }

        /// Enqueue PCM audio data for playback
        /// - Parameter pcmData: Raw PCM data in s16le format
        func enqueue(pcmData: Data) {
            guard isRunning, !pcmData.isEmpty else { return }

            bufferQueue.async { [weak self] in
                guard let self = self else { return }

                // Calculate number of frames
                let bytesPerFrame = Int(self.format.streamDescription.pointee.mBytesPerFrame)
                let frameCount = pcmData.count / bytesPerFrame

                guard frameCount > 0 else {
                    self.logger.debug("Skipping audio chunk with no complete frames")
                    return
                }

                // Create audio buffer
                guard
                    let buffer = AVAudioPCMBuffer(
                        pcmFormat: self.format,
                        frameCapacity: AVAudioFrameCount(frameCount)
                    )
                else {
                    self.logger.error("Failed to create AVAudioPCMBuffer")
                    return
                }

                buffer.frameLength = AVAudioFrameCount(frameCount)

                // Copy PCM data to buffer - use audioBufferList for interleaved format
                pcmData.withUnsafeBytes { rawBuffer in
                    guard let baseAddress = rawBuffer.baseAddress else {
                        self.logger.error("Failed to get base address from PCM data")
                        return
                    }
                    
                    // For interleaved format, there should be exactly one buffer
                    let bufferList = buffer.audioBufferList.pointee
                    guard bufferList.mNumberBuffers == 1,
                          let audioData = bufferList.mBuffers.mData else {
                        self.logger.error("Invalid audio buffer configuration: expected 1 buffer for interleaved format, got \(bufferList.mNumberBuffers)")
                        return
                    }
                    
                    memcpy(audioData, baseAddress, pcmData.count)
                }

                // Schedule the buffer for playback
                self.playerNode.scheduleBuffer(buffer, completionHandler: nil)
            }
        }

        deinit {
            stop()
        }
    }

    /// Errors that can occur during audio playback
    enum AudioPlayerError: Error, LocalizedError {
        case invalidFormat
        case engineStartFailed(Error)

        var errorDescription: String? {
            switch self {
            case .invalidFormat:
                return "Failed to create audio format"
            case .engineStartFailed(let error):
                return "Failed to start audio engine: \(error.localizedDescription)"
            }
        }
    }
#endif
