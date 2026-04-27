import ArgumentParser
import AVFoundation
import CoreGraphics
import CoreImage
import CoreImage.CIFilterBuiltins
import Foundation
import ImageIO
import MLXLMCommon
import MLXVLM
import UniformTypeIdentifiers

struct AppConfig: Encodable {
    var modelPath: String?
    var prompt: String
    var camera: String?
    var interval: Double
    var fps: Double
    var resolution: Int
    var port: Int
}

private func printFinalConfig(_ config: AppConfig) {
    do {
        let data = try AppJSON.encoder.encode(config)
        if let json = String(data: data, encoding: .utf8) {
            print("Final app config:")
            print(json)
            return
        }
    } catch {
        print("Failed to encode final app config: \(error)")
    }

    print(
        "Final app config: modelPath=\(config.modelPath ?? "nil"), prompt=\(config.prompt), camera=\(config.camera ?? "nil"), interval=\(config.interval), fps=\(config.fps), resolution=\(config.resolution), port=\(config.port)"
    )
}

struct CLIArguments: ParsableCommand {
    static var configuration = CommandConfiguration(commandName: "HelloMLX")

    @Option(name: .long, help: "Name of the camera to use (substring match).")
    var camera: String?

    @Option(name: .long, help: "Local path to a model directory.")
    var modelPath: String?

    @Option(name: .long, help: "Prompt to run against the model and the current camera frames.")
    var prompt: String = ""

    @Option(name: .long, help: "Seconds of camera history to include in each inference pass.")
    var interval: Double = 2

    @Option(name: .long, help: "Frames per second to sample into the buffer.")
    var fps: Double = 1

    @Option(name: .long, help: "Square frame resolution. A value of Y produces YxY frames.")
    var resolution: Int = 512

    @Option(name: .long, help: "Local port to serve the web UI on.")
    var port: Int = 8080

    func validate() throws {
        guard (1...65535).contains(port) else {
            throw ValidationError("--port must be between 1 and 65535.")
        }

        guard interval > 0 else {
            throw ValidationError("--interval must be greater than 0.")
        }

        guard fps > 0 else {
            throw ValidationError("--fps must be greater than 0.")
        }

        guard resolution > 0 else {
            throw ValidationError("--resolution must be greater than 0.")
        }
    }
}

let appConfig: AppConfig = {
    let rawArgs = Array(CommandLine.arguments.dropFirst())
    var filtered: [String] = []
    var skipNext = false

    for arg in rawArgs {
        if skipNext {
            skipNext = false
            continue
        }

        if arg.hasPrefix("-NS") || arg.hasPrefix("-Apple") {
            skipNext = true
            continue
        }

        filtered.append(arg)
    }

    do {
        let parsed = try CLIArguments.parse(filtered)
        let config = AppConfig(
            modelPath: parsed.modelPath,
            prompt: parsed.prompt,
            camera: parsed.camera,
            interval: parsed.interval,
            fps: parsed.fps,
            resolution: parsed.resolution,
            port: parsed.port
        )
        printFinalConfig(config)
        return config
    } catch {
        CLIArguments.exit(withError: error)
    }
}()

@main
struct HelloMLX {
    static func main() async {
        do {
            let dataDirectory = try AppDirectories.makeDataDirectory()
            let runStore = try RunStore(rootURL: dataDirectory)
            let baseURL = makeAdvertisedBaseURL(port: appConfig.port)
            let state = AppState(config: appConfig, baseURL: baseURL, latestRunID: runStore.latestRunID())
            let indexHTML = try loadIndexHTML()
            let webServer = try WebServer(
                port: UInt16(appConfig.port),
                state: state,
                runStore: runStore,
                indexHTML: indexHTML
            )

            let serverTask = Task {
                try await webServer.start()
            }

            do {
                try await webServer.waitUntilListening()
            } catch {
                serverTask.cancel()
                throw error
            }

            print("HELLO_MLX_URL=\(baseURL)")
            print("HELLO_MLX_DATA_DIR=\(dataDirectory.path)")

            let camera = Camera(config: appConfig, state: state, runStore: runStore)
            let cameraTask = Task {
                await camera.start()
            }

            defer {
                cameraTask.cancel()
            }

            try await serverTask.value
        } catch {
            fputs("HelloMLX failed to start: \(error)\n", stderr)
            exit(1)
        }
    }
}

final class Camera: NSObject {
    private let config: AppConfig
    private let state: AppState
    private let runStore: RunStore

    private let session = AVCaptureSession()
    private let captureQueue = DispatchQueue(label: "HelloMLX.Camera.Capture")
    private let frameBufferLock = NSLock()
    private let ciContext = CIContext()

    private var frameBuffer: [FrameCapture] = []
    private var lastSampledAt: Date?
    private var model: ModelContainer?
    private var modelName: String?
    private var cameraName: String?

    init(config: AppConfig, state: AppState, runStore: RunStore) {
        self.config = config
        self.state = state
        self.runStore = runStore
    }

    func start() async {
        await state.setCameraStarting()
        await configureCamera()
        await loadModelIfNeeded()
        if model != nil {
            await runInferenceLoop()
        }
    }

    private func configureCamera() async {
        let available = AVCaptureDevice.DiscoverySession(
            deviceTypes: [.builtInWideAngleCamera, .external],
            mediaType: .video,
            position: .unspecified
        ).devices

        print("Available cameras:")
        for device in available {
            print("  - \(device.localizedName) [\(device.uniqueID)]")
        }

        let device: AVCaptureDevice?
        if let name = config.camera, !name.isEmpty {
            device = available.first { $0.localizedName.localizedCaseInsensitiveContains(name) }
            if let device {
                print("Matched: \(device.localizedName) [\(device.uniqueID)]")
            } else {
                print("No camera matching \"\(name)\"")
            }
        } else {
            device = AVCaptureDevice.default(for: .video)
        }

        guard let device else {
            await state.setCameraFailed(message: "No webcam found.")
            return
        }

        do {
            let input = try AVCaptureDeviceInput(device: device)
            let output = AVCaptureVideoDataOutput()
            output.alwaysDiscardsLateVideoFrames = true
            output.setSampleBufferDelegate(self, queue: captureQueue)

            session.beginConfiguration()
            if session.canAddInput(input) {
                session.addInput(input)
            }
            if session.canAddOutput(output) {
                session.addOutput(output)
            }
            session.commitConfiguration()
            session.startRunning()

            cameraName = device.localizedName
            print("Using: \(device.localizedName) [\(device.uniqueID)]")
            await state.setCameraReady(name: device.localizedName)
        } catch {
            await state.setCameraFailed(message: "Failed to configure camera: \(error.localizedDescription)")
        }
    }

    private func loadModelIfNeeded() async {
        guard let modelPath = config.modelPath, !modelPath.isEmpty else {
            print("Model not given, no AI then.")
            return
        }

        let modelDirectory = URL(fileURLWithPath: (modelPath as NSString).expandingTildeInPath)
        let configuredName = modelDirectory.lastPathComponent
        await state.setModelLoading(name: configuredName)
        print("Loading model: \(modelDirectory.path) …")

        do {
            let container = try await VLMModelFactory.shared.loadContainer(
                configuration: ModelConfiguration(directory: modelDirectory)
            ) { progress in
                let percent = Int(progress.fractionCompleted * 100)
                print("  Loading: \(percent)%")
            }
            let loadedName = await container.perform { context in context.configuration.name }
            model = container
            modelName = loadedName
            print("Model loaded successfully: \(loadedName)")
            await state.setModelReady(name: loadedName)
        } catch {
            print("Failed to load model: \(error)")
            await state.setModelFailed(message: "Failed to load model: \(error.localizedDescription)", name: configuredName)
        }
    }

    private func runInferenceLoop() async {
        guard let container = model else { return }

        let interval = config.interval
        print(
            "Sampling at \(config.fps) fps, evaluating last \(config.interval)s of frames at \(config.resolution)x\(config.resolution)."
        )

        while !Task.isCancelled {
            let prompt = await state.currentPrompt().trimmingCharacters(in: .whitespacesAndNewlines)
            guard !prompt.isEmpty else {
                try? await Task.sleep(for: .seconds(1))
                continue
            }

            while !Task.isCancelled && windowFrames(within: interval).count < 1 {
                try? await Task.sleep(for: .seconds(1))
            }
            if Task.isCancelled { return }

            let frames = windowFrames(within: interval)
            guard !frames.isEmpty else { continue }

            print("Prompt: \(prompt)")
            print("Running inference on \(frames.count) frame(s)…")
            await state.setInferenceRunning(true)

            var userInput = UserInput(
                chat: [.user(prompt, images: frames.map { .ciImage(CIImage(cgImage: $0.image)) })]
            )
            let resolution = CGFloat(config.resolution)
            userInput.processing.resize = CGSize(width: resolution, height: resolution)

            var response = ""
            do {
                let lmInput = try await container.prepare(input: userInput)
                let stream = try await container.generate(input: lmInput, parameters: .init())
                for await generation in stream {
                    switch generation {
                    case .chunk(let text):
                        print(text, terminator: "")
                        fflush(stdout)
                        response += text
                    case .info(let info):
                        print("\n")
                        print(info.summary())
                    default:
                        break
                    }
                }
            } catch {
                print("Generation failed: \(error)")
                await state.setError("Generation failed: \(error.localizedDescription)")
            }
            await state.setInferenceRunning(false)

            let cleanedResponse = response.trimmingCharacters(in: .whitespacesAndNewlines)
            guard !cleanedResponse.isEmpty else { continue }

            do {
                let run = try runStore.persistRun(
                    prompt: prompt,
                    response: cleanedResponse,
                    frames: frames,
                    cameraName: cameraName,
                    modelName: modelName,
                    interval: config.interval,
                    fps: config.fps,
                    resolution: config.resolution
                )
                await state.recordRun(id: run.id, at: run.timestamp)
                await state.setError(nil)
            } catch {
                await state.setError("Failed to persist run: \(error.localizedDescription)")
            }
        }
    }

    private func windowFrames(within seconds: TimeInterval) -> [FrameCapture] {
        let cutoff = Date().addingTimeInterval(-seconds)
        frameBufferLock.lock()
        defer { frameBufferLock.unlock() }
        return frameBuffer.filter { $0.capturedAt >= cutoff }
    }

    private func appendFrame(_ frame: FrameCapture, jpeg: Data) {
        frameBufferLock.lock()
        frameBuffer.append(frame)
        let cutoff = Date().addingTimeInterval(-600)
        frameBuffer.removeAll { $0.capturedAt < cutoff }
        frameBufferLock.unlock()

        Task {
            await state.setLiveFrame(jpeg: jpeg, at: frame.capturedAt)
        }
    }

    private func makeSquareFrameImage(from ciImage: CIImage) -> CGImage? {
        let extent = ciImage.extent.integral
        let cropLength = min(extent.width, extent.height)
        guard cropLength > 0 else { return nil }

        let cropped = ciImage.cropped(
            to: CGRect(
                x: extent.origin.x + (extent.width - cropLength) / 2,
                y: extent.origin.y + (extent.height - cropLength) / 2,
                width: cropLength,
                height: cropLength
            )
        )

        let normalized = cropped.transformed(
            by: CGAffineTransform(
                translationX: -cropped.extent.origin.x,
                y: -cropped.extent.origin.y
            )
        )

        let resolution = CGFloat(config.resolution)
        let scale = resolution / cropLength
        let resized = normalized.transformed(by: CGAffineTransform(scaleX: scale, y: scale))
        let outputRect = CGRect(origin: .zero, size: CGSize(width: resolution, height: resolution))
        return ciContext.createCGImage(resized, from: outputRect)
    }
}

extension Camera: AVCaptureVideoDataOutputSampleBufferDelegate {
    func captureOutput(
        _ output: AVCaptureOutput,
        didOutput sampleBuffer: CMSampleBuffer,
        from connection: AVCaptureConnection
    ) {
        guard let pixelBuffer = sampleBuffer.imageBuffer else { return }

        let now = Date()
        let minGap = 1.0 / config.fps

        frameBufferLock.lock()
        let shouldSample = lastSampledAt == nil || now.timeIntervalSince(lastSampledAt!) >= minGap
        if shouldSample {
            lastSampledAt = now
        }
        frameBufferLock.unlock()

        guard shouldSample else { return }

        let ciImage = CIImage(cvPixelBuffer: pixelBuffer)

        guard let image = makeSquareFrameImage(from: ciImage),
              let jpeg = jpegData(from: image)
        else {
            return
        }

        appendFrame(FrameCapture(capturedAt: now, image: image), jpeg: jpeg)
    }
}

enum AppDirectories {
    static func makeDataDirectory() throws -> URL {
        let url = URL(fileURLWithPath: FileManager.default.currentDirectoryPath, isDirectory: true)
            .appendingPathComponent("Runs", isDirectory: true)
        try FileManager.default.createDirectory(at: url, withIntermediateDirectories: true)
        return url
    }
}

enum AppJSON {
    static let encoder: JSONEncoder = {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        encoder.dateEncodingStrategy = .custom { date, encoder in
            var container = encoder.singleValueContainer()
            try container.encode(ISO8601.dateString(from: date))
        }
        return encoder
    }()

    static let decoder: JSONDecoder = {
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .custom { decoder in
            let container = try decoder.singleValueContainer()
            let value = try container.decode(String.self)
            guard let date = ISO8601.date(from: value) else {
                throw DecodingError.dataCorruptedError(in: container, debugDescription: "Invalid ISO-8601 date: \(value)")
            }
            return date
        }
        return decoder
    }()
}

enum ISO8601 {
    private static let formatter: ISO8601DateFormatter = {
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        formatter.timeZone = TimeZone(secondsFromGMT: 0)
        return formatter
    }()

    static func dateString(from date: Date) -> String {
        formatter.string(from: date)
    }

    static func date(from string: String) -> Date? {
        formatter.date(from: string)
    }
}

enum RunID {
    private static let formatter: DateFormatter = {
        let formatter = DateFormatter()
        formatter.calendar = Calendar(identifier: .iso8601)
        formatter.locale = Locale(identifier: "en_US_POSIX")
        formatter.timeZone = TimeZone(secondsFromGMT: 0)
        formatter.dateFormat = "yyyy-MM-dd'T'HH-mm-ss.SSS'Z'"
        return formatter
    }()

    static func make(date: Date = Date()) -> String {
        formatter.string(from: date)
    }
}

func jpegData(from image: CGImage, compressionQuality: Double = 0.8) -> Data? {
    let data = NSMutableData()
    guard let destination = CGImageDestinationCreateWithData(data, UTType.jpeg.identifier as CFString, 1, nil) else {
        return nil
    }

    let properties: [CFString: Any] = [
        kCGImageDestinationLossyCompressionQuality: compressionQuality
    ]
    CGImageDestinationAddImage(destination, image, properties as CFDictionary)
    guard CGImageDestinationFinalize(destination) else {
        return nil
    }
    return data as Data
}

func makeAdvertisedBaseURL(port: Int) -> String {
    let hostname = ProcessInfo.processInfo.hostName.trimmingCharacters(in: .whitespacesAndNewlines)
    let host = hostname.isEmpty ? "localhost" : hostname
    return "http://\(host):\(port)/"
}

func loadIndexHTML() throws -> String {
    let fileManager = FileManager.default
    let executableURL = URL(fileURLWithPath: CommandLine.arguments[0]).standardizedFileURL
    let executableDirectory = executableURL.deletingLastPathComponent()
    let executableName = executableURL.lastPathComponent
    let siblingBundleName = "\(executableName).bundle"
    let sourceDirectory = URL(fileURLWithPath: #filePath).deletingLastPathComponent()
    let projectDirectory = sourceDirectory.deletingLastPathComponent()
    let workingDirectory = URL(fileURLWithPath: fileManager.currentDirectoryPath)

    let candidates = [
        projectDirectory.appendingPathComponent("Resources/index.html"),
        workingDirectory.appendingPathComponent("Resources/index.html"),
        workingDirectory.appendingPathComponent("\(siblingBundleName)/index.html"),
        workingDirectory.appendingPathComponent("\(executableName)/index.html"),
        workingDirectory.appendingPathComponent("index.html"),
        executableDirectory.appendingPathComponent("\(siblingBundleName)/index.html"),
        executableDirectory.appendingPathComponent("\(executableName)/index.html"),
        executableDirectory.appendingPathComponent("index.html")
    ]

    for candidate in candidates where fileManager.fileExists(atPath: candidate.path) {
        return try String(contentsOf: candidate, encoding: .utf8)
    }

    let searchedPaths = candidates.map(\.path).joined(separator: ", ")
    throw NSError(
        domain: "HelloMLX",
        code: 1,
        userInfo: [NSLocalizedDescriptionKey: "Could not locate index.html. Searched: \(searchedPaths)"]
    )
}
