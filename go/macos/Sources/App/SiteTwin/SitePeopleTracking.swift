import SwiftUI
import Vision
import simd

public struct SitePersonTrack: Identifiable, Equatable, Sendable {
    public let id: UUID
    var position: SIMD3<Double>
    var observedAt: Date
}

struct SitePersonDetection: Sendable {
    var camera: UUID
    var position: SIMD3<Double>
    var observedAt: Date
}

/// Anonymous, short-lived positions. No face recognition or recording.
struct SitePeopleTracker {
    static let lifetime: TimeInterval = 1.25
    private(set) var tracks: [SitePersonTrack] = []

    mutating func update(_ detections: [SitePersonDetection], now: Date) {
        tracks.removeAll { !(0...Self.lifetime).contains(now.timeIntervalSince($0.observedAt)) }
        let fresh = detections.filter {
            (0...Self.lifetime).contains(now.timeIntervalSince($0.observedAt))
        }.filter { detection in (0..<3).allSatisfy { detection.position[$0].isFinite } }
        // Merge only contemporaneous observations from different cameras. A
        // camera's separate people must not collapse into one cluster.
        var groups: [[SitePersonDetection]] = []
        for detection in fresh.sorted(by: { $0.observedAt > $1.observedAt }) {
            if let index = groups.firstIndex(where: { group in
                group.allSatisfy {
                    $0.camera != detection.camera
                        && abs($0.observedAt.timeIntervalSince(detection.observedAt)) <= 0.35
                        && simd_distance($0.position, detection.position) < 0.65
                }
            }) { groups[index].append(detection) }
            else { groups.append([detection]) }
        }
        var available = Set(tracks.map(\.id))
        for group in groups {
            let position = group.reduce(SIMD3<Double>.zero) { $0 + $1.position } / Double(group.count)
            let observedAt = group.map(\.observedAt).max()!
            let candidates = tracks.indices.filter {
                available.contains(tracks[$0].id)
                    && simd_distance(tracks[$0].position, position)
                        < min(2, 0.4 + max(0, observedAt.timeIntervalSince(tracks[$0].observedAt)) * 3)
            }
            if let index = candidates.min(by: {
                simd_distance(tracks[$0].position, position) < simd_distance(tracks[$1].position, position)
            }) {
                available.remove(tracks[index].id)
                let dt = observedAt.timeIntervalSince(tracks[index].observedAt)
                guard dt > 0 else { continue }
                let alpha = 1 - exp(-dt / 0.18)
                tracks[index].position += (position - tracks[index].position) * alpha
                tracks[index].observedAt = observedAt
            } else {
                tracks.append(SitePersonTrack(id: UUID(), position: position, observedAt: observedAt))
            }
        }
    }
}

enum SitePersonProjection {
    /// Vision uses a lower-left origin. Camera calibration uses top-left pixels
    /// and optical right/down/forward coordinates, in metres after rotation.
    static func floorPoint(ankle: CGPoint, geometry: SiteCameraGeometry,
                           revision: UUID, floorY: Double, now: Date) -> SIMD3<Double>? {
        guard floorY.isFinite, ankle.x.isFinite, ankle.y.isFinite,
              (0.01...0.99).contains(ankle.x), (0.01...0.99).contains(ankle.y),
              geometry.pose.isCurrent(revision: revision, now: now),
              let lens = geometry.lens,
              let ray = lens.ray(x: Double(ankle.x) * Double(lens.width),
                                 y: (1 - Double(ankle.y)) * Double(lens.height)) else { return nil }
        let p = geometry.pose.position
        let origin = SIMD3(p.x, p.y, p.z)
        let direction = geometry.pose.quaternion.act(ray)
        // Near-horizontal rays make small keypoint errors produce huge jumps.
        guard origin.y > floorY + 0.2, direction.y < -0.08 else { return nil }
        let distance = (floorY - origin.y) / direction.y
        guard (0.2...12).contains(distance) else { return nil }
        return origin + direction * distance
    }

    static func ankles(in image: CGImage) throws -> [CGPoint] {
        let request = VNDetectHumanBodyPoseRequest()
        try VNImageRequestHandler(cgImage: image, orientation: .up).perform([request])
        return try (request.results ?? []).compactMap { observation in
            let left = try observation.recognizedPoint(.leftAnkle)
            let right = try observation.recognizedPoint(.rightAnkle)
            // Both ankles must be visible. A bounding-box bottom can be a knee
            // or an image crop and gives a misleading floor position.
            guard left.confidence >= 0.5, right.confidence >= 0.5 else { return nil }
            return CGPoint(x: (left.location.x + right.location.x) / 2,
                           y: (left.location.y + right.location.y) / 2)
        }
    }
}

/// SwiftUI owns the inference loop and its decoding leases. Work is sequential
/// on a utility queue, so slow inference never queues a backlog of video frames.
@MainActor struct SitePeopleCapture: View {
    let site: Site
    let sources: [UUID: SiteCameraColorSource]
    let geometry: [UUID: SiteCameraGeometry]
    let floorY: Double
    let publish: ([SitePersonTrack], [UUID: String]) -> Void
    @Environment(\.scenePhase) private var scenePhase

    struct Context: Equatable {
        var site: Site
        var sources: [UUID: UUID]
        var geometry: [UUID: SiteCameraGeometry]
        var floorY: Double
        var active: Bool
    }
    private var context: Context {
        Context(site: site, sources: sources.mapValues(\.id), geometry: geometry,
                floorY: floorY, active: scenePhase == .active)
    }

    var body: some View {
        Color.clear.frame(width: 0, height: 0).task(id: context) {
            publish([], [:])
            guard scenePhase == .active else { return }
            let leases = sources.values.map { ($0, $0.acquire()) }
            defer { for (source, lease) in leases { source.release(lease) } }
            var tracker = SitePeopleTracker()
            var processed: [UUID: Date] = [:]
            var statuses: [UUID: String] = [:]
            while !Task.isCancelled {
                var detections: [SitePersonDetection] = []
                for id in sources.keys.sorted(by: { $0.uuidString < $1.uuidString }) {
                    guard !Task.isCancelled, let source = sources[id] else { return }
                    let now = Date()
                    guard let camera = geometry[id], camera.pose.isCurrent(revision: site.modelRevision, now: now),
                          let lens = camera.lens, lens.isValid else {
                        statuses[id] = "Calibrate and align this camera."; continue
                    }
                    guard let frame = source.frame(), frame.matches(lens, at: now),
                          now.timeIntervalSince(frame.received) <= SitePeopleTracker.lifetime else {
                        statuses[id] = "Waiting for fresh video at the calibrated resolution."; continue
                    }
                    guard processed[id] != frame.received else { continue }
                    processed[id] = frame.received
                    do {
                        let ankles = try await BlockingWork.run(on: .global(qos: .utility)) {
                            try SitePersonProjection.ankles(in: frame.image)
                        }
                        try Task.checkCancellation()
                        let projected: [SitePersonDetection] = ankles.compactMap { ankle in
                            guard let point = SitePersonProjection.floorPoint(ankle: ankle, geometry: camera,
                                revision: site.modelRevision, floorY: floorY, now: Date()) else { return nil }
                            return SitePersonDetection(camera: id, position: point, observedAt: frame.received)
                        }
                        detections.append(contentsOf: projected)
                        statuses[id] = !projected.isEmpty
                            ? "Tracking visible feet" : "No visible feet detected"
                    } catch is CancellationError { return }
                    catch { statuses[id] = "Person detection failed. Retrying…" }
                }
                tracker.update(detections, now: Date())
                publish(tracker.tracks, statuses)
                do { try await Task.sleep(for: .milliseconds(200)) } catch { return }
            }
        }
    }
}
