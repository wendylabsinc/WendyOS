import Foundation
import simd

/// Capture and display adapter. The Linux twin owns solving, lens selection,
/// verification and compare-and-swap persistence for both Apple and web clients.
@MainActor
enum SiteCameraAlignment {
    struct Frame: Encodable {
        var width: Int, height: Int
        var codec: String
        var sequence: UInt64
        var ageMS: Double
        var gray: Data
        init(_ frame: CameraCalibrationFrame) {
            width = frame.width; height = frame.height; codec = frame.codec
            sequence = frame.sequence; gray = frame.gray
            ageMS = max(0, Date().timeIntervalSince(frame.received) * 1000)
        }
    }
    struct Command: Encodable {
        var operation: String
        var token: String? = nil
        var roomFrame: String? = nil
        var camera: String? = nil
        var automatic: Bool? = nil
        var frame: Frame? = nil
    }
    struct Result: Decodable {
        var success: Int, inliers: Int, fittedLens: Int
        var fx: Double, fy: Double, cx: Double, cy: Double, rms: Double
        var distortion: [Double], transform: [Double]
        func display(revision: UUID, frame: CameraCalibrationFrame) throws -> SiteLocalizationResult {
            guard success == 1, transform.count == 16, distortion.count == 5 else {
                throw SiteLocalizationError.invalidResult
            }
            let t = transform
            let rotation = simd_double3x3(columns: (SIMD3(t[0],t[1],t[2]), SIMD3(t[4],t[5],t[6]), SIMD3(t[8],t[9],t[10])))
            let now = Date()
            return SiteLocalizationResult(lens: SiteCameraLens(width: frame.width, height: frame.height,
                fx: fx, fy: fy, cx: cx, cy: cy, distortionModel: "brown_conrady", distortion: distortion,
                measured: fittedLens != 2, note: fittedLens == 2 ? "Estimated lens; not measured." : "Validated by the Linux digital twin.",
                captureMode: "\(frame.codec) \(frame.width)x\(frame.height)"),
                pose: SiteCameraPose(modelRevision: revision, position: SitePoint(x: t[12], y: t[13], z: t[14]),
                    rotation: simd_quatd(rotation).normalized.vector, method: 2,
                    provenance: "Linux twin camera alignment; verified on two fresh frames.",
                    observedAt: now, validUntil: now.addingTimeInterval(7*24*60*60)),
                inliers: inliers, residual: rms, fittedLens: fittedLens != 0)
        }
    }
    struct Reply: Decodable {
        var policyVersion: Int
        var token: String?
        var result: Result
    }
    static func send(_ command: Command, to endpoint: URL) async throws -> Reply {
        var request = URLRequest(url: endpoint.appendingPathComponent("v1/camera-alignment"))
        request.httpMethod = "POST"; request.timeoutInterval = 60
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = try JSONEncoder().encode(command)
        let (data, response) = try await URLSession.shared.data(for: request)
        guard (response as? HTTPURLResponse)?.statusCode == 200 else {
            throw WendyError(message: String(decoding: data.prefix(2048), as: UTF8.self))
        }
        let reply = try JSONDecoder().decode(Reply.self, from: data)
        guard reply.policyVersion == 1 else { throw WendyError(message: "Update the digital twin client to use this camera alignment service.") }
        return reply
    }
    static func align(model: AppModel, request: SiteAlignmentRequest, feed: CameraFeed,
                      automatic: Bool = false, isCurrent: () -> Bool = { true }) async throws -> SiteLocalizationResult {
        let session = request.session, stream = feed.stream, source = request.camera.sourceID
        func check() throws {
            try Task.checkCancellation()
            guard isCurrent(), request.isCurrent(session: model.deviceSession),
                  model.cameraFeeds.filter({ $0.camera.stableID == source }).count == 1,
                  model.cameraFeeds.contains(where: { $0 === feed }),
                  feed.camera.stableID == source, feed.running, stream != 0, feed.stream == stream else {
                throw CancellationError()
            }
        }
        try check()
        let endpoint = try await GoClient.shared.openApp("dev.wendy.digital-twin", session: session)
        try check()
        let first = try await freshFrame(feed, check: check)
        let begin = try await send(Command(operation: "begin",
            roomFrame: request.site.id.uuidString + "/" + request.site.site.modelRevision.uuidString,
            camera: source, automatic: automatic, frame: Frame(first)), to: endpoint)
        guard let token = begin.token else { throw SiteLocalizationError.invalidResult }
        do {
            try check()
            // The service requires a newly received frame after the first solve.
            let earliest = Date().addingTimeInterval(0.85)
            try await Task.sleep(for: .milliseconds(850))
            let second = try await freshFrame(feed, after: first, earliest: earliest, check: check)
            let verified = try await send(Command(operation: "verify", token: token, frame: Frame(second)), to: endpoint)
            try check()
            let result = try verified.result.display(revision: request.site.site.modelRevision, frame: second)
            _ = try await send(Command(operation: "commit", token: token), to: endpoint)
            try check()
            return result
        } catch {
            // A cancelled view must not retain a pending calibration lease.
            await cancel(token, at: endpoint)
            throw error
        }
    }
    // Cleanup must reach the service even when this caller's task is cancelled.
    // A bounded callback request does not create an unstructured Swift task.
    private static func cancel(_ token: String, at endpoint: URL) async {
        var request = URLRequest(url: endpoint.appendingPathComponent("v1/camera-alignment"))
        request.httpMethod = "POST"; request.timeoutInterval = 5
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = try? JSONEncoder().encode(Command(operation: "cancel", token: token))
        await withCheckedContinuation { (continuation: CheckedContinuation<Void, Never>) in
            URLSession.shared.dataTask(with: request) { _, _, _ in continuation.resume() }.resume()
        }
    }
    private static func freshFrame(_ feed: CameraFeed, after previous: CameraCalibrationFrame? = nil,
                                   earliest: Date = .distantPast, check: () throws -> Void) async throws -> CameraCalibrationFrame {
        let deadline = ContinuousClock.now.advanced(by: .seconds(8))
        while ContinuousClock.now < deadline {
            try check()
            if let frame = feed.calibrationFrame(), (0...1.5).contains(Date().timeIntervalSince(frame.received)),
               frame.received >= earliest,
               previous == nil || (frame.sequence > previous!.sequence && frame.received > previous!.received) {
                return frame
            }
            try await Task.sleep(for: .milliseconds(150))
        }
        throw WendyError(message: "Waiting for fresh camera video.")
    }
}
