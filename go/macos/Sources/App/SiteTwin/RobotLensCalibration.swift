import Foundation
import CryptoKit

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
