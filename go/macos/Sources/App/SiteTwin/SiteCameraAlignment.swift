import Foundation
import simd

enum SiteAlignmentValidation {
    static func verified(_ solution: SiteLocalizationResult, by verification: SiteLocalizationResult,
                         first: CameraCalibrationFrame, second: CameraCalibrationFrame) throws -> SiteLocalizationResult {
        guard second.received > first.received, second.sequence != first.sequence,
              first.width == second.width, first.height == second.height, first.codec == second.codec else {
            throw SiteLocalizationError.invalidImage
        }
        let a = solution.pose.position, b = verification.pose.position
        let distance = simd_distance(SIMD3(a.x, a.y, a.z), SIMD3(b.x, b.y, b.z))
        let angle = 2 * acos(min(1, abs(simd_dot(solution.pose.rotation, verification.pose.rotation))))
        guard distance <= (solution.lens.measured ? 0.08 : 0.30),
              angle <= (solution.lens.measured ? 3 : 10) * .pi / 180 else {
            throw WendyError(message: "The camera position changed between frames. Waiting for a stable view.")
        }
        var result = solution
        result.pose = verification.pose
        result.inliers = min(solution.inliers, verification.inliers)
        result.residual = max(solution.residual, verification.residual)
        return result
    }
}

/// Manual and automatic alignment use exactly the same solve and save checks.
@MainActor
enum SiteCameraAlignment {
    static func load(_ site: StoredSite, into engine: SiteLocalizationEngine) async throws {
        guard let url = site.visualMapURL else { throw SiteLocalizationError.insufficientMap }
        let (map, bytes) = try await BlockingWork.run(on: .global(qos: .utility)) {
            try SiteStore.validateModel(site.modelURL)
            let bytes = try Data(contentsOf: site.modelURL)
            return (try SiteVisualMap.read(url, revision: site.site.modelRevision, model: bytes), bytes)
        }
        try await engine.load(map, revision: site.site.modelRevision, model: bytes)
    }

    static func align(model: AppModel, request: SiteAlignmentRequest, feed: CameraFeed,
                      engine: SiteLocalizationEngine, automatic: Bool = false,
                      isCurrent: () -> Bool = { true }) async throws -> SiteLocalizationResult {
        let session = request.session, stream = feed.stream, source = request.camera.sourceID
        func check() throws {
            try Task.checkCancellation()
            guard isCurrent(), session != 0, model.deviceSession == session,
                  request.gatewayID == request.camera.gatewayID, !request.gatewayID.isEmpty,
                  model.cameraFeeds.filter({ $0.camera.stableID == source }).count == 1,
                  model.cameraFeeds.contains(where: { $0 === feed }),
                  feed.camera.stableID == source, feed.running, stream != 0, feed.stream == stream else {
                throw CancellationError()
            }
        }
        try check()
        let records = try await GoClient.shared.cameraMetadata(session: session).filter { $0.sourceID == source }
        try check()
        guard records.count <= 1 else { throw SiteLocalizationError.invalidResult }
        let record = records.first
        // An earlier estimate is not a lens measurement. Reusing it here can
        // lock every retry to the same incorrect focal length and distance.
        // The second frame still verifies the newly selected lens below.
        let measuredLens = record?.lens.flatMap { $0.measured ? $0 : nil }
        if automatic, request.camera.position != nil || record?.pose(in: request.site.site, now: Date())?.method == 1 {
            throw WendyError(message: "Manual placement retained.")
        }
        let first = try await freshFrame(feed, check: check)
        var solution: SiteLocalizationResult
        do {
            solution = try await engine.localize(gray: first.gray, width: first.width, height: first.height,
                revision: request.site.site.modelRevision, lens: measuredLens)
        } catch SiteLocalizationError.noSolution(_, _, _) {
            try check()
            solution = try await engine.localize(gray: first.gray, width: first.width, height: first.height,
                revision: request.site.site.modelRevision, lens: measuredLens, estimated: true)
        }
        if solution.fittedLens { solution.lens.captureMode = "\(first.codec) \(first.width)x\(first.height)" }
        try check()
        try await Task.sleep(for: .milliseconds(800))
        let second = try await freshFrame(feed, after: first, check: check)
        guard first.width == second.width, first.height == second.height, first.codec == second.codec else {
            throw SiteLocalizationError.invalidImage
        }
        let verification = try await engine.localize(gray: second.gray, width: second.width, height: second.height,
            revision: request.site.site.modelRevision, lens: solution.lens, estimated: !solution.lens.measured)
        solution = try SiteAlignmentValidation.verified(solution, by: verification, first: first, second: second)
        try check()
        guard (feed.jpeg?.width ?? feed.decoder.imageSize?.width) == first.width,
              (feed.jpeg?.height ?? feed.decoder.imageSize?.height) == first.height else {
            throw SiteLocalizationError.invalidImage
        }
        try await GoClient.shared.saveCameraAlignment(session: session, sourceID: source,
            expectedRevision: record?.revision ?? 0, result: solution)
        try check()
        return solution
    }

    private static func freshFrame(_ feed: CameraFeed, after previous: CameraCalibrationFrame? = nil,
                                   check: () throws -> Void) async throws -> CameraCalibrationFrame {
        let deadline = ContinuousClock.now.advanced(by: .seconds(8))
        while ContinuousClock.now < deadline {
            try check()
            if let frame = feed.calibrationFrame(), frame.isFresh(at: Date()),
               previous == nil || (frame.sequence != previous?.sequence && frame.received > previous!.received) {
                return frame
            }
            try await Task.sleep(for: .milliseconds(150))
        }
        throw WendyError(message: "Waiting for fresh camera video.")
    }
}
