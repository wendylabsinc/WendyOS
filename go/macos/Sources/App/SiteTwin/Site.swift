import Foundation
import simd

/// Coordinates are metres, right-handed, Y up. A marker is a manually placed
/// sensor location, not a camera calibration or a measured object position.
public struct SitePoint: Codable, Equatable, Sendable {
    public var x: Double
    public var y: Double
    public var z: Double

    public init(x: Double, y: Double, z: Double) {
        self.x = x
        self.y = y
        self.z = z
    }

    public var isValid: Bool {
        [x, y, z].allSatisfy { $0.isFinite && abs($0) <= 1_000_000 }
    }
}

public struct SiteCamera: Codable, Identifiable, Equatable, Sendable {
    public var id: UUID
    /// A scoped, stable device principal supplied by the authenticated app.
    public var gatewayID: String
    public var sourceID: String
    public var name: String
    public var position: SitePoint?
    var manualAlignment: SiteManualAlignment?

    public init(
        id: UUID = UUID(),
        gatewayID: String,
        sourceID: String,
        name: String,
        position: SitePoint? = nil
    ) {
        self.id = id
        self.gatewayID = gatewayID
        self.sourceID = sourceID
        self.name = name
        self.position = position
    }
}

public struct Site: Codable, Identifiable, Equatable, Sendable {
    public static let currentVersion = 1
    public var version = currentVersion
    public var id: UUID
    public var name: String
    public var modelRevision: UUID
    public var metresPerModelUnit: Double
    public var cameras: [SiteCamera]

    public init(
        id: UUID = UUID(),
        name: String,
        metresPerModelUnit: Double = 1,
        cameras: [SiteCamera] = []
    ) {
        self.id = id
        self.name = name
        self.modelRevision = UUID()
        self.metresPerModelUnit = metresPerModelUnit
        self.cameras = cameras
    }

    public func validate() throws {
        guard version == Self.currentVersion else { throw SiteError.unsupportedVersion }
        guard !name.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty, name.count <= 200,
            metresPerModelUnit.isFinite, metresPerModelUnit > 0, metresPerModelUnit <= 1_000,
            cameras.count <= 256
        else { throw SiteError.invalidSite }
        var ids = Set<UUID>()
        var sources = Set<String>()
        for camera in cameras {
            guard ids.insert(camera.id).inserted,
                !camera.gatewayID.isEmpty, camera.gatewayID.count <= 1024,
                !camera.sourceID.isEmpty, camera.sourceID.count <= 1024,
                camera.name.count <= 200,
                sources.insert("\(camera.gatewayID.count):\(camera.gatewayID)\(camera.sourceID)")
                    .inserted,
                camera.position?.isValid != false
            else { throw SiteError.invalidSite }
            if let manual = camera.manualAlignment {
                guard let position = camera.position, manual.geometry(position: position, revision: modelRevision, now: Date()) != nil else { throw SiteError.invalidSite }
            }
        }
    }

    /// Replacing geometry invalidates all placements; old coordinates must never
    /// silently become positions in the new scan.
    public mutating func replaceModel(metresPerUnit: Double = 1) {
        modelRevision = UUID()
        metresPerModelUnit = metresPerUnit
        for index in cameras.indices { cameras[index].position = nil; cameras[index].manualAlignment = nil }
    }
}

public enum SiteError: Error, LocalizedError, Sendable {
    case unsupportedVersion, invalidSite, invalidModel, tooLarge, missingSite

    public var errorDescription: String? {
        switch self {
        case .unsupportedVersion:
            "This site uses a newer file format. Update Wendy and try again."
        case .invalidSite:
            "The site contains invalid settings. Choose a valid site file and try again."
        case .invalidModel: "Choose a self-contained USDZ model of the site."
        case .tooLarge: "This model is too large. Export a USDZ model smaller than 128 MB."
        case .missingSite: "The saved site is unavailable. Import the site again."
        }
    }
}

/// Received observations are deliberately separate from persisted site setup.
/// Reloading the app never restores a previously live reading as current.
public struct SiteObservation: Codable, Equatable, Identifiable, Sendable {
    public var id: UUID
    public var sensorID: UUID
    public var modelRevision: UUID
    public var capturedAt: Date
    public var receivedAt: Date
    public var value: Double
    public var unit: String
    public var position: SitePoint?
    public var confidence: Double?

    public init(
        id: UUID = UUID(),
        sensorID: UUID,
        modelRevision: UUID,
        capturedAt: Date,
        receivedAt: Date,
        value: Double,
        unit: String,
        position: SitePoint? = nil,
        confidence: Double? = nil
    ) {
        self.id = id
        self.sensorID = sensorID
        self.modelRevision = modelRevision
        self.capturedAt = capturedAt
        self.receivedAt = receivedAt
        self.value = value
        self.unit = unit
        self.position = position
        self.confidence = confidence
    }

    public func isCurrent(in site: Site, now: Date, maximumAge: TimeInterval = 5) -> Bool {
        guard maximumAge.isFinite, maximumAge > 0, modelRevision == site.modelRevision,
            value.isFinite, position?.isValid != false,
            confidence.map({ $0.isFinite && (0...1).contains($0) }) != false,
            site.cameras.contains(where: { $0.id == sensorID }), !unit.isEmpty
        else { return false }
        // A future or unsynchronised capture time is unknown, not fresh.
        let captureAge = now.timeIntervalSince(capturedAt)
        let receiveAge = now.timeIntervalSince(receivedAt)
        return captureAge >= 0 && captureAge <= maximumAge && receiveAge >= 0
            && receiveAge <= maximumAge
    }
}

/// User-authored visualization, kept with the model rather than reported as a measured device calibration.
struct SiteManualAlignment: Codable, Equatable, Sendable {
    var modelRevision: UUID
    var yaw: Double
    var pitch: Double
    var roll: Double
    var horizontalFOV: Double
    var width: Int
    var height: Int

    func geometry(position: SitePoint, revision: UUID, now: Date) -> SiteCameraGeometry? {
        guard revision == modelRevision, position.isValid,
              [yaw, pitch, roll, horizontalFOV].allSatisfy(\.isFinite),
              (20...140).contains(horizontalFOV), (1...8192).contains(width), (1...8192).contains(height) else { return nil }
        let rotation = simd_quatd(angle: yaw * .pi / 180, axis: SIMD3(0, 1, 0))
            * simd_quatd(angle: pitch * .pi / 180, axis: SIMD3(1, 0, 0))
            * simd_quatd(angle: roll * .pi / 180, axis: SIMD3(0, 0, 1))
        let focal = Double(width) / (2 * tan(horizontalFOV * .pi / 360))
        let lens = SiteCameraLens(width: width, height: height, fx: focal, fy: focal,
            cx: Double(width)/2, cy: Double(height)/2, distortionModel: "none", distortion: [],
            measured: false, note: "Manually aligned", captureMode: "Manual \(width)x\(height)")
        let pose = SiteCameraPose(modelRevision: revision, position: position, rotation: rotation.vector,
            method: 1, provenance: "Manual visualization", observedAt: now, validUntil: now.addingTimeInterval(86400))
        return SiteCameraGeometry(pose: pose, lens: lens)
    }
}
