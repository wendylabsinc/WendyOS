import CoreGraphics
import Foundation

struct PersistedFrame: Codable, Sendable {
    let capturedAt: Date
    let url: String
}

struct PersistedRun: Codable, Sendable {
    let id: String
    let timestamp: Date
    let prompt: String
    let response: String
    let cameraName: String?
    let modelName: String?
    let interval: Double
    let fps: Double
    let resolution: Int?
    let frameCount: Int
    let frames: [PersistedFrame]
}

struct RunsResponse: Codable {
    let items: [PersistedRun]
    let nextCursor: String?
}

struct FrameCapture {
    let capturedAt: Date
    let image: CGImage
}

struct RunStore {
    let rootURL: URL
    private let runsURL: URL
    private let fileManager = FileManager.default
    private let decoder = AppJSON.decoder
    private let encoder = AppJSON.encoder

    init(rootURL: URL) throws {
        self.rootURL = rootURL
        self.runsURL = rootURL.appendingPathComponent("runs", isDirectory: true)
        try fileManager.createDirectory(at: runsURL, withIntermediateDirectories: true)
    }

    func latestRunID() -> String? {
        try? listRuns(limit: 1, before: nil).items.first?.id
    }

    func listRuns(limit: Int, before cursor: String?) throws -> RunsResponse {
        let runs = try allRuns()
        let startIndex: Int
        if let cursor, let index = runs.firstIndex(where: { $0.id == cursor }) {
            startIndex = index + 1
        } else {
            startIndex = 0
        }

        let page = Array(runs.dropFirst(startIndex).prefix(limit))
        let nextCursor = startIndex + page.count < runs.count ? page.last?.id : nil
        return RunsResponse(items: page, nextCursor: nextCursor)
    }

    func loadRun(id: String) throws -> PersistedRun {
        let url = runsURL.appendingPathComponent(id, isDirectory: true).appendingPathComponent("result.json")
        let data = try Data(contentsOf: url)
        return try decoder.decode(PersistedRun.self, from: data)
    }

    func persistRun(
        prompt: String,
        response: String,
        frames: [FrameCapture],
        cameraName: String?,
        modelName: String?,
        interval: Double,
        fps: Double,
        resolution: Int
    ) throws -> PersistedRun {
        let id = RunID.make()
        let directoryURL = runsURL.appendingPathComponent(id, isDirectory: true)
        try fileManager.createDirectory(at: directoryURL, withIntermediateDirectories: true)

        var persistedFrames: [PersistedFrame] = []
        persistedFrames.reserveCapacity(frames.count)

        for (index, frame) in frames.enumerated() {
            let filename = String(format: "frame-%03d.jpg", index)
            let fileURL = directoryURL.appendingPathComponent(filename)
            guard let jpeg = jpegData(from: frame.image) else {
                throw RunStoreError.failedToEncodeFrame(index)
            }
            try jpeg.write(to: fileURL, options: .atomic)
            persistedFrames.append(
                PersistedFrame(
                    capturedAt: frame.capturedAt,
                    url: "/artifacts/runs/\(id)/\(filename)"
                )
            )
        }

        let run = PersistedRun(
            id: id,
            timestamp: Date(),
            prompt: prompt,
            response: response,
            cameraName: cameraName,
            modelName: modelName,
            interval: interval,
            fps: fps,
            resolution: resolution,
            frameCount: persistedFrames.count,
            frames: persistedFrames
        )

        let temporaryURL = directoryURL.appendingPathComponent("result.json.tmp")
        let finalURL = directoryURL.appendingPathComponent("result.json")
        let data = try encoder.encode(run)
        try data.write(to: temporaryURL, options: .atomic)
        if fileManager.fileExists(atPath: finalURL.path) {
            try fileManager.removeItem(at: finalURL)
        }
        try fileManager.moveItem(at: temporaryURL, to: finalURL)
        return run
    }

    private func allRuns() throws -> [PersistedRun] {
        let directories = try fileManager.contentsOfDirectory(
            at: runsURL,
            includingPropertiesForKeys: nil,
            options: [.skipsHiddenFiles]
        )

        return try directories.compactMap { directoryURL in
            let resultURL = directoryURL.appendingPathComponent("result.json")
            guard fileManager.fileExists(atPath: resultURL.path) else { return nil }
            let data = try Data(contentsOf: resultURL)
            return try decoder.decode(PersistedRun.self, from: data)
        }
        .sorted { $0.timestamp > $1.timestamp }
    }
}

enum RunStoreError: LocalizedError {
    case failedToEncodeFrame(Int)

    var errorDescription: String? {
        switch self {
        case .failedToEncodeFrame(let index):
            return "Failed to encode frame \(index) as JPEG."
        }
    }
}
