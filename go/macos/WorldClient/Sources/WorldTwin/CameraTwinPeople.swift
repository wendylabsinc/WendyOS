import Foundation
import CoreGraphics
import Vision
import simd

// Adapted from WendyOS SitePeopleTracking and SiteCameraGeometry. These are
// anonymous app inferences in a selected scan, never navigation clearance.
struct CameraTwinPeopleEvidence: Codable, Equatable, Sendable {
    var cameraID: String
    var receiptID: String
    var sha256: String
    var sourceSession: String
    var sequence: UInt64
    var receivedAt: Date
    var origin: String
    var poseObservedAt: Date
    enum CodingKeys: String, CodingKey {
        case sha256, sequence, origin
        case cameraID = "camera_id", receiptID = "receipt_id", sourceSession = "source_session"
        case receivedAt = "received_at", poseObservedAt = "pose_observed_at"
    }
}

struct CameraTwinPeopleLens: Equatable, Sendable {
    var width: Int, height: Int
    var fx: Double, fy: Double, cx: Double, cy: Double
    var distortion: [Double]

    // Optical right/down/forward ray. Failed inversion remains unavailable.
    func ray(x: Double, y: Double) -> SIMD3<Double>? {
        guard (1...8192).contains(width), (1...8192).contains(height),
              [x,y,fx,fy,cx,cy].allSatisfy(\.isFinite), fx > 0, fy > 0,
              fx <= 1e7, fy <= 1e7, (0...Double(width)).contains(cx), (0...Double(height)).contains(cy),
              distortion.count == 5, distortion.allSatisfy({ $0.isFinite && abs($0) <= 100 }) else { return nil }
        let xd = (x-cx)/fx, yd = (y-cy)/fy
        var u = xd, v = yd
        let k1 = distortion[0], k2 = distortion[1], p1 = distortion[2], p2 = distortion[3], k3 = distortion[4]
        for _ in 0..<40 {
            let r2 = u*u+v*v, radial = 1+r2*(k1+r2*(k2+r2*k3))
            guard radial.isFinite, radial > 0.01 else { return nil }
            let du = 2*p1*u*v+p2*(r2+2*u*u), dv = p1*(r2+2*v*v)+2*p2*u*v
            u = (xd-du)/radial; v = (yd-dv)/radial
        }
        let r2 = u*u+v*v, radial = 1+r2*(k1+r2*(k2+r2*k3))
        let errorX = (u*radial+2*p1*u*v+p2*(r2+2*u*u)-xd)*fx
        let errorY = (v*radial+p1*(r2+2*v*v)+2*p2*u*v-yd)*fy
        guard u.isFinite, v.isFinite, errorX.isFinite, errorY.isFinite, hypot(errorX,errorY) < 0.1 else { return nil }
        return simd_normalize(SIMD3(u,v,1))
    }
}

struct CameraTwinPeopleCalibration: Equatable, Sendable {
    var coordinateFrame: String
    var siteID: String
    var modelRevision: String
    var modelSHA256: String
    var worldFromCamera: [Double]
    var lens: CameraTwinPeopleLens
    var poseObservedAt: Date
    var poseValidUntil: Date

    func floorPoint(ankle: CGPoint, floorY: Double, now: Date) -> SIMD3<Double>? {
        guard floorY.isFinite, ankle.x.isFinite, ankle.y.isFinite,
              (0.01...0.99).contains(ankle.x), (0.01...0.99).contains(ankle.y),
              poseObservedAt <= now, poseValidUntil > now,
              worldFromCamera.count == 16, worldFromCamera.allSatisfy(\.isFinite),
              abs(worldFromCamera[3]) < 1e-6, abs(worldFromCamera[7]) < 1e-6,
              abs(worldFromCamera[11]) < 1e-6, abs(worldFromCamera[15]-1) < 1e-6,
              let ray = lens.ray(x: Double(ankle.x)*Double(lens.width), y: (1-Double(ankle.y))*Double(lens.height)) else { return nil }
        let x = SIMD3(worldFromCamera[0],worldFromCamera[1],worldFromCamera[2])
        let y = SIMD3(worldFromCamera[4],worldFromCamera[5],worldFromCamera[6])
        let z = SIMD3(worldFromCamera[8],worldFromCamera[9],worldFromCamera[10])
        guard [x,y,z].allSatisfy({ abs(simd_length_squared($0)-1) < 0.001 }),
              abs(simd_dot(x,y)) < 0.001, abs(simd_dot(x,z)) < 0.001,
              abs(simd_dot(y,z)) < 0.001, simd_dot(simd_cross(x,y),z) > 0.999 else { return nil }
        let origin = SIMD3(worldFromCamera[12],worldFromCamera[13],worldFromCamera[14])
        let direction = x*ray.x+y*ray.y+z*ray.z
        guard origin.y > floorY+0.2, direction.y < -0.08 else { return nil }
        let distance = (floorY-origin.y)/direction.y
        guard (0.2...12).contains(distance) else { return nil }
        return origin+direction*distance
    }
}

// CGImage is immutable. Sending a retained image to the inference actor does
// not expose mutable pixel storage or UI objects.
struct CameraTwinPeopleFrame: @unchecked Sendable {
    var image: CGImage
    var receipt: CameraTwinPeopleEvidence
    var calibration: CameraTwinPeopleCalibration
}

struct CameraTwinPersonObservation: Codable, Equatable, Identifiable, Sendable {
    var id: String
    var position: [Double]
    var observedAt: Date
    var validUntil: Date
    var provenance = "inferred"
    var inputOrigin: String
    var inputs: [CameraTwinPeopleEvidence]
    enum CodingKeys: String, CodingKey {
        case id, position, provenance, inputs
        case observedAt = "observed_at", validUntil = "valid_until", inputOrigin = "input_origin"
    }
}

struct CameraTwinPeopleBatch: Codable, Sendable {
    var schema = "wendy.camera-twin.observations.v1"
    var coordinateFrame: String
    var modelSHA256: String
    var floorY: Double
    var observations: [CameraTwinPersonObservation]
    enum CodingKeys: String, CodingKey {
        case schema, observations
        case coordinateFrame = "coordinate_frame", modelSHA256 = "model_sha256", floorY = "floor_y"
    }
    func encoded() throws -> Data {
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .custom { value, encoder in
            var container = encoder.singleValueContainer()
            let formatter = ISO8601DateFormatter()
            formatter.formatOptions = [.withInternetDateTime,.withFractionalSeconds]
            try container.encode(formatter.string(from: value))
        }
        return try encoder.encode(self)
    }
}

struct CameraTwinPeopleResult: Sendable {
    var tracks: [CameraTwinPersonObservation]
    var statuses: [String: String]
    var observations: CameraTwinPeopleBatch?
}

actor CameraTwinPeopleEngine {
    typealias Detector = @Sendable (CGImage) throws -> [CGPoint]
    static let lifetime: TimeInterval = 1.25
    private let detector: Detector
    private var processed: [String: CameraTwinPeopleEvidence] = [:]
    private var calibrations: [String: CameraTwinPeopleCalibration] = [:]
    private var detections: [String: [Detection]] = [:]
    private var tracks: [CameraTwinPersonObservation] = []
    private var floor: Double?
    private var context: String?

    private struct Detection {
        var position: SIMD3<Double>
        var input: CameraTwinPeopleEvidence
        var validUntil: Date
    }

    init(detector: @escaping Detector = CameraTwinPeopleEngine.ankles) { self.detector = detector }

    func reset() {
        processed.removeAll(); calibrations.removeAll(); detections.removeAll(); tracks.removeAll()
        floor = nil; context = nil
    }

    nonisolated static func ankles(in image: CGImage) throws -> [CGPoint] {
        let request = VNDetectHumanBodyPoseRequest()
        request.revision = VNDetectHumanBodyPoseRequestRevision1
        try VNImageRequestHandler(cgImage: image, orientation: .up).perform([request])
        return try (request.results ?? []).prefix(32).compactMap { observation in
            let left = try observation.recognizedPoint(.leftAnkle), right = try observation.recognizedPoint(.rightAnkle)
            guard left.confidence >= 0.5, right.confidence >= 0.5 else { return nil }
            return CGPoint(x: (left.location.x+right.location.x)/2, y: (left.location.y+right.location.y)/2)
        }
    }

    // The caller awaits each batch before submitting the next. The actor does
    // sequential Vision inference with no internally queued tasks or frames.
    func process(frames: [CameraTwinPeopleFrame], floorY: Double, now: Date = Date()) -> CameraTwinPeopleResult {
        let started = ProcessInfo.processInfo.systemUptime
        guard floorY.isFinite, abs(floorY) <= 1000, frames.count <= 8,
              Set(frames.map { $0.receipt.cameraID }).count == frames.count else {
            reset(); return CameraTwinPeopleResult(tracks: [], statuses: [:], observations: nil)
        }
        guard let first = frames.first else {
            reset(); return CameraTwinPeopleResult(tracks: [], statuses: [:], observations: nil)
        }
        let key = first.calibration.coordinateFrame+":"+first.calibration.modelSHA256
        guard frames.allSatisfy({ $0.calibration.coordinateFrame+":"+$0.calibration.modelSHA256 == key }) else {
            reset(); return CameraTwinPeopleResult(tracks: [], statuses: [:], observations: nil)
        }
        if floor != floorY || context != key { reset(); floor = floorY; context = key }
        let active = Set(frames.map { $0.receipt.cameraID })
        for id in processed.keys.filter({ !active.contains($0) }) { invalidate(cameraID: id) }
        var statuses: [String: String] = [:]
        for frame in frames.sorted(by: { $0.receipt.cameraID < $1.receipt.cameraID }) {
            let input = frame.receipt, calibration = frame.calibration, id = input.cameraID
            guard !id.isEmpty, !input.receiptID.isEmpty, !input.sourceSession.isEmpty, input.sequence > 0,
                  ["measured","simulated"].contains(input.origin),
                  input.sha256.count == 64, input.sha256.allSatisfy({ $0.isHexDigit && !$0.isUppercase }),
                  input.receivedAt <= now, now.timeIntervalSince(input.receivedAt) < Self.lifetime,
                  input.poseObservedAt == calibration.poseObservedAt, calibration.poseValidUntil > now,
                  frame.image.width == calibration.lens.width, frame.image.height == calibration.lens.height else {
                invalidate(cameraID: id); statuses[id] = "Waiting for fresh images and matching calibration."; continue
            }
            if calibrations[id] != calibration || processed[id].map({ $0.sourceSession != input.sourceSession || $0.origin != input.origin }) == true {
                invalidate(cameraID: id)
            }
            calibrations[id] = calibration
            if let previous = processed[id] {
                guard input.sequence > previous.sequence else {
                    if input == previous { statuses[id] = "Waiting for the next image." }
                    else if input.sequence == previous.sequence {
                        invalidate(cameraID: id); statuses[id] = "Conflicting image receipt rejected."
                    }
                    else { statuses[id] = "Out-of-order image ignored." }
                    continue
                }
                guard input.receivedAt > previous.receivedAt else { statuses[id] = "Out-of-order receipt time ignored."; continue }
            }
            processed[id] = input
            do {
                let points = try detector(frame.image).prefix(32)
                detections[id] = points.compactMap { ankle in
                    guard let position = calibration.floorPoint(ankle: ankle, floorY: floorY, now: now) else { return nil }
                    return Detection(position: position, input: input, validUntil: min(input.receivedAt.addingTimeInterval(Self.lifetime), calibration.poseValidUntil))
                }
                statuses[id] = detections[id]?.isEmpty == false ? "Anonymous positions estimated from visible feet." : "No visible feet projected onto this floor."
            } catch {
                detections[id] = []; statuses[id] = "Person detection is unavailable for this image."
            }
        }
        let finished = now.addingTimeInterval(ProcessInfo.processInfo.systemUptime-started)
        tracks.removeAll { $0.validUntil <= finished }
        let flattened: [Detection] = detections.values.flatMap { $0 }
        var fresh: [Detection] = flattened.filter { $0.validUntil > finished }
        fresh.sort { a,b in
            if a.input.receivedAt == b.input.receivedAt { return a.input.cameraID < b.input.cameraID }
            return a.input.receivedAt > b.input.receivedAt
        }
        var groups: [[Detection]] = []
        for detection in fresh {
            if let index = groups.firstIndex(where: { group in group.allSatisfy {
                $0.input.cameraID != detection.input.cameraID && $0.input.origin == detection.input.origin
                    && abs($0.input.receivedAt.timeIntervalSince(detection.input.receivedAt)) <= 0.35
                    && simd_distance($0.position,detection.position) < 0.65
            } }) { groups[index].append(detection) }
            else { groups.append([detection]) }
        }
        var available = Set(tracks.map(\.id)), next: [CameraTwinPersonObservation] = []
        for group in groups.prefix(64) {
            let position = group.reduce(SIMD3<Double>.zero) { $0+$1.position }/Double(group.count)
            let observedAt = group.map { $0.input.receivedAt }.max()!
            let validUntil = group.map(\.validUntil).min()!
            let inputs = group.map(\.input).sorted { $0.cameraID < $1.cameraID }
            let previous = tracks.filter { track in
                guard available.contains(track.id), track.inputOrigin == inputs[0].origin else { return false }
                let dt = observedAt.timeIntervalSince(track.observedAt)
                return dt >= 0 && simd_distance(SIMD3(track.position[0],track.position[1],track.position[2]),position) < min(2,0.4+dt*3)
            }.min { a,b in simd_distance(SIMD3(a.position[0],a.position[1],a.position[2]),position) < simd_distance(SIMD3(b.position[0],b.position[1],b.position[2]),position) }
            let id = previous?.id ?? UUID().uuidString
            available.remove(id)
            // Keep the published point at the supported measurement. Animation
            // may smooth presentation, but never changes its evidence or expiry.
            next.append(CameraTwinPersonObservation(id: id, position: [position.x,position.y,position.z], observedAt: observedAt,
                                                   validUntil: validUntil, inputOrigin: inputs[0].origin, inputs: inputs))
        }
        tracks = next
        let batch = CameraTwinPeopleBatch(coordinateFrame: first.calibration.coordinateFrame, modelSHA256: first.calibration.modelSHA256, floorY: floorY, observations: tracks)
        return CameraTwinPeopleResult(tracks: tracks, statuses: statuses, observations: batch)
    }

    private func invalidate(cameraID: String) {
        processed.removeValue(forKey: cameraID); calibrations.removeValue(forKey: cameraID); detections.removeValue(forKey: cameraID)
        tracks.removeAll { $0.inputs.contains(where: { $0.cameraID == cameraID }) }
    }
}
