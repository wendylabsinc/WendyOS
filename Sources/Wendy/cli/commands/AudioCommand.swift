import ArgumentParser
import CLIOutput
import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import Logging
import Noora
import WendyAgentGRPC

struct AudioCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "audio",
        abstract: "Manage audio devices and stream audio.",
        subcommands: [
            ListCommand.self,
            SetDefaultCommand.self,
            MonitorCommand.self,
            ListenCommand.self,
        ]
    )

    // MARK: - List Command

    struct ListCommand: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            commandName: "list",
            abstract: "List available audio devices."
        )

        @Option(name: .shortAndLong, help: "Filter by device type (input or output)")
        var type: String?

        @Flag(name: [.customShort("j"), .long], help: "Output in JSON format")
        var json: Bool = false

        @OptionGroup var agentConnectionOptions: AgentConnectionOptions

        func run() async throws {
            let devices = try await withAgentGRPCClient(
                agentConnectionOptions,
                title: "For which device do you want to list audio devices?"
            ) { client in
                let audio = Wendy_Agent_Services_V1_WendyAudioService.Client(wrapping: client)

                var request = Wendy_Agent_Services_V1_ListAudioDevicesRequest()
                if let typeFilter = type?.lowercased() {
                    switch typeFilter {
                    case "input", "mic", "microphone", "source":
                        request.typeFilter = .input
                    case "output", "speaker", "sink":
                        request.typeFilter = .output
                    default:
                        break
                    }
                }

                if json {
                    return try await audio.listAudioDevices(request).devices
                } else {
                    return try await cliOutput.withProgress(
                        message: "Discovering audio devices",
                        successMessage: "Audio devices discovered",
                        errorMessage: "Failed to discover audio devices"
                    ) { [request] in
                        try await audio.listAudioDevices(request)
                    }.devices
                }
            }

            if devices.isEmpty {
                cliOutput.info("No audio devices found.")
            } else if json {
                try outputJSON(devices)
            } else {
                outputText(devices)
            }
        }

        private func outputJSON(_ devices: [Wendy_Agent_Services_V1_AudioDevice]) throws {
            struct DeviceInfo: Codable {
                let id: UInt32
                let name: String
                let description: String
                let type: String
                let isDefault: Bool
            }

            let deviceInfos = devices.map { device in
                DeviceInfo(
                    id: device.id,
                    name: device.name,
                    description: device.description_p,
                    type: device.type == .input ? "input" : "output",
                    isDefault: device.isDefault
                )
            }

            let encoder = JSONEncoder()
            encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
            let jsonData = try encoder.encode(deviceInfos)
            print(String(data: jsonData, encoding: .utf8)!)
        }

        private func outputText(_ devices: [Wendy_Agent_Services_V1_AudioDevice]) {
            // Group by type
            let inputs = devices.filter { $0.type == .input }
            let outputs = devices.filter { $0.type == .output }

            print("Audio Devices:")
            print("==============")
            print()

            if !inputs.isEmpty {
                print("Input Devices (Microphones):")
                for device in inputs {
                    let defaultMarker = device.isDefault ? " [DEFAULT]" : ""
                    print("  \(device.id). \(device.name)\(defaultMarker)")
                }
                print()
            }

            if !outputs.isEmpty {
                print("Output Devices (Speakers):")
                for device in outputs {
                    let defaultMarker = device.isDefault ? " [DEFAULT]" : ""
                    print("  \(device.id). \(device.name)\(defaultMarker)")
                }
                print()
            }

            print("Total: \(devices.count) device\(devices.count == 1 ? "" : "s")")
            print("\nTip: Use 'wendy audio set-default <id>' to change the default device")
        }
    }

    // MARK: - Set Default Command

    struct SetDefaultCommand: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            commandName: "set-default",
            abstract: "Set the default audio device."
        )

        @Argument(help: "Device ID to set as default")
        var deviceId: UInt32

        @OptionGroup var agentConnectionOptions: AgentConnectionOptions

        func run() async throws {
            try await withAgentGRPCClient(
                agentConnectionOptions,
                title: "Which device do you want to configure?"
            ) { client in
                let audio = Wendy_Agent_Services_V1_WendyAudioService.Client(wrapping: client)

                var request = Wendy_Agent_Services_V1_SetDefaultAudioDeviceRequest()
                request.deviceID = deviceId

                let response = try await cliOutput.withProgress(
                    message: "Setting default audio device to \(deviceId)",
                    successMessage: "Default audio device set to \(deviceId)",
                    errorMessage: "Failed to set default audio device"
                ) { [request] in
                    try await audio.setDefaultAudioDevice(request)
                }

                if !response.success {
                    let errorMessage =
                        response.hasErrorMessage ? response.errorMessage : "Unknown error"
                    print("Failed to set default device: \(errorMessage)")
                    throw ExitCode.failure
                }
            }
        }
    }

    // MARK: - Monitor Command (VU Meter)

    struct MonitorCommand: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            commandName: "monitor",
            abstract: "Real-time audio level visualizer (VU meter)."
        )

        @Option(
            name: [.customShort("i"), .customLong("audio-device")],
            help: "Audio device ID to monitor (skips interactive selection)"
        )
        var audioDevice: UInt32?

        @Option(name: .shortAndLong, help: "Update rate in Hz (1-60, default: 20)")
        var rate: UInt32 = 20

        @OptionGroup var agentConnectionOptions: AgentConnectionOptions

        func run() async throws {
            try await withAgentGRPCClient(
                agentConnectionOptions,
                title: "Which Wendy device do you want to monitor?"
            ) { client in
                let audio = Wendy_Agent_Services_V1_WendyAudioService.Client(wrapping: client)

                // Get device ID - either from command line or interactive selection
                let deviceId = try await selectAudioDevice(
                    audio: audio,
                    preselectedId: audioDevice,
                    deviceType: .input,
                    prompt: "Select an input device to monitor"
                )

                print("Monitoring audio levels (Ctrl+C to stop)...")
                print()

                try await self.monitorAudioLevels(audio: audio, deviceId: deviceId, rate: self.rate)
            }
        }

        private func monitorAudioLevels<T: GRPCCore.ClientTransport>(
            audio: Wendy_Agent_Services_V1_WendyAudioService.Client<T>,
            deviceId: UInt32,
            rate: UInt32
        ) async throws {
            var request = Wendy_Agent_Services_V1_StreamAudioLevelsRequest()
            request.deviceID = deviceId
            request.updateRateHz = rate

            try await streamLevels(audio: audio, request: request) { update in
                self.renderVUMeter(peakDb: update.peakDb, rmsDb: update.rmsDb)
            }
        }

        private func streamLevels<T: GRPCCore.ClientTransport>(
            audio: Wendy_Agent_Services_V1_WendyAudioService.Client<T>,
            request: Wendy_Agent_Services_V1_StreamAudioLevelsRequest,
            handler: @Sendable @escaping (Wendy_Agent_Services_V1_AudioLevelUpdate) -> Void
        ) async throws {
            @Sendable func handleResponse(
                response: GRPCCore.StreamingClientResponse<Wendy_Agent_Services_V1_AudioLevelUpdate>
            ) async throws -> Bool {
                for try await update in response.messages {
                    handler(update)
                }
                return true
            }
            _ = try await audio.streamAudioLevels(request, onResponse: handleResponse)
        }

        private func renderVUMeter(peakDb: Float, rmsDb: Float) {
            // VU meter: 40 characters wide, from -60dB to 0dB
            let minDb: Float = -60.0
            let maxDb: Float = 0.0
            let meterWidth = 40

            // Clamp and normalize
            let normalizedPeak = max(0, min(1, (peakDb - minDb) / (maxDb - minDb)))
            let normalizedRms = max(0, min(1, (rmsDb - minDb) / (maxDb - minDb)))

            let peakPos = Int(normalizedPeak * Float(meterWidth))
            let rmsPos = Int(normalizedRms * Float(meterWidth))

            // Build the meter
            var meter = ""
            for i in 0..<meterWidth {
                if i < rmsPos {
                    // Filled portion (RMS level)
                    if i < meterWidth * 6 / 10 {
                        meter += "\u{2588}"  // Full block (green zone)
                    } else if i < meterWidth * 8 / 10 {
                        meter += "\u{2588}"  // Full block (yellow zone)
                    } else {
                        meter += "\u{2588}"  // Full block (red zone)
                    }
                } else if i == peakPos && peakPos > 0 {
                    meter += "|"  // Peak indicator
                } else {
                    meter += "\u{2591}"  // Light shade (empty)
                }
            }

            // Clear line and print
            print(
                "\r\u{001B}[K[\(meter)] Peak: \(String(format: "%6.1f", peakDb)) dB  RMS: \(String(format: "%6.1f", rmsDb)) dB",
                terminator: ""
            )
            #if os(Linux)
                fflush(nil)
            #else
                fflush(stdout)
            #endif
        }
    }

    // MARK: - Listen Command (Audio Streaming)

    struct ListenCommand: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            commandName: "listen",
            abstract: "Stream audio from device to Mac speakers."
        )

        @Option(
            name: [.customShort("i"), .customLong("audio-device")],
            help: "Audio device ID to stream from (skips interactive selection)"
        )
        var audioDevice: UInt32?

        @Option(name: .shortAndLong, help: "Sample rate in Hz (default: 48000)")
        var sampleRate: UInt32 = 48000

        @Option(name: .shortAndLong, help: "Number of channels (default: 1)")
        var channels: UInt32 = 1

        @OptionGroup var agentConnectionOptions: AgentConnectionOptions

        func run() async throws {
            #if os(macOS)
                try await withAgentGRPCClient(
                    agentConnectionOptions,
                    title: "Which Wendy device do you want to stream audio from?"
                ) { client in
                    let audio = Wendy_Agent_Services_V1_WendyAudioService.Client(wrapping: client)

                    // Get device ID - either from command line or interactive selection
                    let deviceId = try await selectAudioDevice(
                        audio: audio,
                        preselectedId: audioDevice,
                        deviceType: .input,
                        prompt: "Select an input device to listen to"
                    )

                    print("Streaming audio from device (Ctrl+C to stop)...")
                    print("Sample rate: \(sampleRate) Hz, Channels: \(channels)")
                    print()

                    let player = try AudioPlayer(
                        sampleRate: Double(sampleRate),
                        channels: Int(channels)
                    )

                    try await self.streamAudioFromDevice(
                        audio: audio,
                        deviceId: deviceId,
                        rate: self.sampleRate,
                        channels: self.channels,
                        player: player
                    )
                }
            #else
                print("Audio playback is only supported on macOS.")
                throw ExitCode.failure
            #endif
        }

        #if os(macOS)
            private func streamAudioFromDevice<T: GRPCCore.ClientTransport>(
                audio: Wendy_Agent_Services_V1_WendyAudioService.Client<T>,
                deviceId: UInt32,
                rate: UInt32,
                channels: UInt32,
                player: AudioPlayer
            ) async throws {
                var request = Wendy_Agent_Services_V1_StreamAudioRequest()
                request.deviceID = deviceId
                request.sampleRate = rate
                request.channels = channels

                try await streamAudioChunks(audio: audio, request: request, player: player)
            }

            private func streamAudioChunks<T: GRPCCore.ClientTransport>(
                audio: Wendy_Agent_Services_V1_WendyAudioService.Client<T>,
                request: Wendy_Agent_Services_V1_StreamAudioRequest,
                player: AudioPlayer
            ) async throws {
                @Sendable func handleResponse(
                    response: GRPCCore.StreamingClientResponse<Wendy_Agent_Services_V1_AudioChunk>
                ) async throws -> Bool {
                    try player.start()
                    for try await chunk in response.messages {
                        player.enqueue(pcmData: chunk.pcmData)
                    }
                    player.stop()
                    return true
                }
                _ = try await audio.streamAudio(request, onResponse: handleResponse)
            }
        #endif
    }
}

// MARK: - Audio Device Selection Helper

/// Wrapper for audio device selection in Noora picker
private struct AudioDeviceOption: CustomStringConvertible, Hashable {
    let device: Wendy_Agent_Services_V1_AudioDevice

    var description: String {
        let defaultMarker = device.isDefault ? " (Default)" : ""
        let cardInfo = device.description_p.isEmpty ? "" : " - \(device.description_p)"
        return "\(device.name)\(cardInfo)\(defaultMarker)"
    }

    func hash(into hasher: inout Hasher) {
        hasher.combine(device.id)
    }

    static func == (lhs: AudioDeviceOption, rhs: AudioDeviceOption) -> Bool {
        lhs.device.id == rhs.device.id
    }
}

/// Interactively select an audio device, or use the preselected ID if provided
private func selectAudioDevice<T: GRPCCore.ClientTransport>(
    audio: Wendy_Agent_Services_V1_WendyAudioService.Client<T>,
    preselectedId: UInt32?,
    deviceType: Wendy_Agent_Services_V1_AudioDeviceType,
    prompt: String
) async throws -> UInt32 {
    // If a device ID was provided on command line, use it directly
    if let id = preselectedId {
        return id
    }

    // Fetch available devices
    var request = Wendy_Agent_Services_V1_ListAudioDevicesRequest()
    request.typeFilter = deviceType

    let response = try await cliOutput.withProgress(
        message: "Discovering audio devices",
        successMessage: "Audio devices discovered",
        errorMessage: "Failed to discover audio devices"
    ) { [request] in
        try await audio.listAudioDevices(request)
    }

    let devices = response.devices

    guard !devices.isEmpty else {
        let typeStr = deviceType == .input ? "input" : "output"
        throw AudioSelectionError.noDevices(type: typeStr)
    }

    // If only one device, use it automatically
    if devices.count == 1 {
        let device = devices[0]
        print("Using \(device.name)")
        return device.id
    }

    // Sort devices: default first, then by name
    let sortedDevices = devices.sorted { a, b in
        if a.isDefault != b.isDefault {
            return a.isDefault
        }
        return a.name < b.name
    }

    // Create options for picker
    let options = sortedDevices.map { AudioDeviceOption(device: $0) }

    // Show interactive picker
    let selected = try await cliOutput.singleChoicePrompt(
        title: .init(stringLiteral: prompt),
        question: "Select a device",
        options: options
    )

    return selected.device.id
}

private enum AudioSelectionError: Error, LocalizedError {
    case noDevices(type: String)

    var errorDescription: String? {
        switch self {
        case .noDevices(let type):
            return "No \(type) audio devices found on the device"
        }
    }
}
