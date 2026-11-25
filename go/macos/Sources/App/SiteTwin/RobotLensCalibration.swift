import Foundation
import CryptoKit
import simd

struct RobotCameraMode: Codable, Equatable, Sendable {
    var width, height: Int
    var codec: String

    init(_ frame: CameraCalibrationFrame) {
        width = frame.width; height = frame.height; codec = frame.codec
    }
    var label: String { "\(codec) \(width)x\(height)" }
    func matches(_ lens: SiteCameraLens) -> Bool {
        lens.width == width && lens.height == height && lens.captureMode == label
    }
}

struct RobotLensProfile: Codable, Sendable {
    var version = 1
    var robot: String
    var lens: SiteCameraLens
    var measuredAt: Date
}

// Lens measurements belong to a robot and capture mode, not a room revision.
// Room changes must not silently throw away the physical camera calibration.
enum RobotLensStore {
    static func directory() throws -> URL {
        try FileManager.default.url(for: .applicationSupportDirectory, in: .userDomainMask,
                                    appropriateFor: nil, create: true).appendingPathComponent("WendyRobotLenses", isDirectory: true)
    }

    static func url(robot: String, mode: RobotCameraMode, root: URL) -> URL {
        let key = Data("\(robot)\n\(mode.label)".utf8)
        let digest = SHA256.hash(data: key).map { String(format: "%02x", $0) }.joined()
        return root.appendingPathComponent("\(digest).json")
    }

    static func read(robot: String, mode: RobotCameraMode, root: URL? = nil) throws -> RobotLensProfile? {
        let url = url(robot: robot, mode: mode, root: try root ?? directory())
        guard FileManager.default.fileExists(atPath: url.path) else { return nil }
        let attributes = try url.resourceValues(forKeys: [.isRegularFileKey, .isSymbolicLinkKey, .fileSizeKey])
        guard attributes.isRegularFile == true, attributes.isSymbolicLink != true,
              let size = attributes.fileSize, size <= 64 * 1024 else { throw SiteLocalizationError.invalidMap }
        let profile = try JSONDecoder().decode(RobotLensProfile.self, from: Data(contentsOf: url))
        guard profile.version == 1, profile.robot == robot, profile.measuredAt.timeIntervalSince1970.isFinite,
              profile.lens.measured, profile.lens.isValid, profile.lens.boundaryRays != nil,
              mode.matches(profile.lens) else { throw WendyError(message: "Go2's saved lens calibration does not match this camera mode.") }
        return profile
    }

    static func save(_ profile: RobotLensProfile, mode: RobotCameraMode, root: URL? = nil) throws {
        guard !profile.robot.isEmpty, profile.robot.utf8.count <= 256, profile.version == 1,
              profile.measuredAt.timeIntervalSince1970.isFinite, profile.lens.measured,
              profile.lens.isValid, profile.lens.boundaryRays != nil, mode.matches(profile.lens) else {
            throw WendyError(message: "Only a validated lens measurement for this Go2 camera mode can be saved.")
        }
        let root = try root ?? directory()
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        try JSONEncoder().encode(profile).write(to: url(robot: profile.robot, mode: mode, root: root), options: .atomic)
    }
}

// The raw Go2 WebRTC camera is H.264. Do not scale this preset onto a
// different crop, resolution, rectified image, or replacement camera.
// Coefficients: abizovnuralem/go2_ros2_sdk, commit
// b440609591a249e7bdd4bbc88e056a3660575447,
// go2_robot_sdk/calibration/front_camera_720.yaml (BSD-2-Clause).
// See Resources/Notices/go2-camera-LICENSE.txt.
enum Go2CameraProfile {
    static let identifier = "go2-front-720p-v1"

    static func lens(for mode: RobotCameraMode) -> SiteCameraLens? {
        guard mode.width == 1280, mode.height == 720, mode.codec == "H.264" else { return nil }
        return SiteCameraLens(width: 1280, height: 720,
            fx: 864.39938, fy: 863.73849, cx: 639.19798, cy: 373.28118,
            distortionModel: "brown_conrady", distortion: [-0.354630, 0.102054, -0.001614, -0.001249, 0],
            measured: false,
            note: "Go2 720p community preset, \(identifier). Not measured on this robot. Refine calibration if room matching fails.",
            captureMode: mode.label)
    }

    // Unitree's Go2 URDF, commit 157ff950351e5f535e03c5d85a16b7d594b3dbcc:
    // robots/go2_description/urdf/go2_description.urdf, front_camera_joint.
    // The mount is (forward, left, up) = (0.32715, -0.00003, 0.04297) m.
    // Express it in camera optical axes (right, down, forward) before applying
    // the fitted camera rotation, including body pitch and roll.
    static func bodyPosition(camera: SiteCameraPose) -> SIMD3<Double> {
        let offset = camera.quaternion.act(SIMD3<Double>(0.00003, -0.04297, 0.32715))
        return SIMD3(camera.position.x, camera.position.y, camera.position.z) - offset
    }
}

struct RobotLensSelection: Sendable {
    enum Source: Sendable, Equatable { case measured, go2Preset, estimated }
    let lens: SiteCameraLens
    let source: Source

    var label: String {
        switch source {
        case .measured: "Measured lens"
        case .go2Preset: "Go2 preset · refine if needed"
        case .estimated: "Estimated lens"
        }
    }

    static func resolve(mode: RobotCameraMode, measured: SiteCameraLens?, previous: SiteCameraLens? = nil,
                        recalibrate: Bool = false) -> Self? {
        for candidate in [measured, previous] {
            if let lens = candidate, lens.measured, lens.isValid, mode.matches(lens), lens.boundaryRays != nil {
                return Self(lens: lens, source: .measured)
            }
        }
        if let lens = Go2CameraProfile.lens(for: mode) {
            return Self(lens: lens, source: .go2Preset)
        }
        if !recalibrate, let lens = previous, lens.isValid, mode.matches(lens), lens.boundaryRays != nil {
            return Self(lens: lens, source: .estimated)
        }
        return nil
    }
}

struct RobotLensSession: Equatable, Sendable {
    let id = UUID()
    var handle: UInt64
    var robot: String
}

struct RobotLensSamples {
    private(set) var mode: RobotCameraMode?
    private var sequence: UInt64?
    private var received: Date?

    mutating func accept(_ frame: CameraCalibrationFrame, now: Date) throws -> Bool {
        guard frame.isFresh(at: now), frame.sequence != sequence,
              received == nil || frame.received > received! else { return false }
        guard (32...8192).contains(frame.width), (32...8192).contains(frame.height),
              frame.gray.count == frame.width * frame.height, !frame.codec.isEmpty else { throw SiteLocalizationError.invalidImage }
        let next = RobotCameraMode(frame)
        guard mode == nil || mode == next else { throw WendyError(message: "The Go2 camera mode changed. Restart lens calibration.") }
        mode = next; sequence = frame.sequence; received = frame.received
        return true
    }
}
