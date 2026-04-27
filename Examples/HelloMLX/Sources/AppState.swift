import Foundation

enum CameraStatus: String, Codable {
    case starting
    case ready
    case failed
}

enum ModelStatus: String, Codable {
    case notConfigured
    case loading
    case ready
    case failed
}

struct AppInfo: Codable {
    let startedAt: Date
    let url: String
}

struct CameraInfo: Codable {
    let status: CameraStatus
    let name: String?
    let lastFrameAt: Date?
    let frameURL: String?
}

struct ModelInfo: Codable {
    let status: ModelStatus
    let name: String?
}

struct PromptInfo: Codable {
    let text: String
    let updatedAt: Date
}

struct RunInfo: Codable {
    let interval: Int
    let fps: Int
    let isRunningInference: Bool
    let latestRunID: String?
    let lastInferenceAt: Date?
}

struct StateResponse: Codable {
    let app: AppInfo
    let camera: CameraInfo
    let model: ModelInfo
    let prompt: PromptInfo
    let run: RunInfo
    let error: String?
}

struct PromptUpdateRequest: Decodable {
    let text: String
}

struct PromptUpdateResponse: Codable {
    let ok: Bool
    let prompt: PromptInfo
}

actor AppState {
    private let startedAt = Date()
    private let baseURL: String
    private let interval: Int
    private let fps: Int

    private var cameraStatus: CameraStatus = .starting
    private var cameraName: String?
    private var lastFrameAt: Date?
    private var lastFrameJPEG: Data?

    private var modelStatus: ModelStatus
    private var modelName: String?

    private var promptText: String
    private var promptUpdatedAt = Date()

    private var isRunningInference = false
    private var latestRunID: String?
    private var lastInferenceAt: Date?
    private var lastError: String?

    init(config: AppConfig, baseURL: String, latestRunID: String?) {
        self.baseURL = baseURL
        self.interval = config.interval
        self.fps = config.fps
        self.modelStatus = config.modelPath == nil ? .notConfigured : .loading
        self.modelName = config.modelPath.map { URL(fileURLWithPath: $0).lastPathComponent }
        self.promptText = config.prompt
        self.latestRunID = latestRunID
    }

    func snapshot() -> StateResponse {
        StateResponse(
            app: AppInfo(startedAt: startedAt, url: baseURL),
            camera: CameraInfo(
                status: cameraStatus,
                name: cameraName,
                lastFrameAt: lastFrameAt,
                frameURL: lastFrameAt.map {
                    let encoded = ISO8601.dateString(from: $0)
                    return "/frame.jpg?t=\(encoded.addingPercentEncoding(withAllowedCharacters: .urlQueryAllowed) ?? encoded)"
                }
            ),
            model: ModelInfo(status: modelStatus, name: modelName),
            prompt: PromptInfo(text: promptText, updatedAt: promptUpdatedAt),
            run: RunInfo(
                interval: interval,
                fps: fps,
                isRunningInference: isRunningInference,
                latestRunID: latestRunID,
                lastInferenceAt: lastInferenceAt
            ),
            error: lastError
        )
    }

    func liveFrameJPEG() -> Data? {
        lastFrameJPEG
    }

    func currentPrompt() -> String {
        promptText
    }

    func savePrompt(_ text: String) -> PromptUpdateResponse {
        promptText = text
        promptUpdatedAt = Date()
        return PromptUpdateResponse(ok: true, prompt: PromptInfo(text: promptText, updatedAt: promptUpdatedAt))
    }

    func setCameraStarting() {
        cameraStatus = .starting
        lastError = nil
    }

    func setCameraReady(name: String) {
        cameraStatus = .ready
        cameraName = name
        if lastError == "No webcam found." {
            lastError = nil
        }
    }

    func setCameraFailed(message: String) {
        cameraStatus = .failed
        lastError = message
    }

    func setLiveFrame(jpeg: Data, at date: Date) {
        lastFrameJPEG = jpeg
        lastFrameAt = date
    }

    func setModelLoading(name: String?) {
        modelStatus = .loading
        modelName = name
        lastError = nil
    }

    func setModelReady(name: String?) {
        modelStatus = .ready
        modelName = name
    }

    func setModelFailed(message: String, name: String?) {
        modelStatus = .failed
        modelName = name
        lastError = message
    }

    func setInferenceRunning(_ isRunning: Bool) {
        isRunningInference = isRunning
    }

    func recordRun(id: String, at date: Date) {
        latestRunID = id
        lastInferenceAt = date
    }

    func setError(_ message: String?) {
        lastError = message
    }
}
