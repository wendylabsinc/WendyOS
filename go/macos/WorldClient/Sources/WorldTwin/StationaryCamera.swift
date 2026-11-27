import SwiftUI
import Combine
import AppKit
import CryptoKit
import ImageIO
import Darwin

enum StationaryCameraError: Error { case profile, response, size, evidence, http(Int) }
private let cameraInk = Color(red: 0.11, green: 0.19, blue: 0.20)
private let cameraTeal = Color(red: 0.10, green: 0.43, blue: 0.39)
private let cameraPaper = Color(red: 0.96, green: 0.97, blue: 0.95)

struct StationaryCameraReceipt: Decodable, Equatable, Identifiable {
    let id: String
    let sha256: String
    let width: Int
    let height: Int
    let size: Int
    let contentType: String
    let representation: String
    let sourceCodec: String
    let receivedAt: Date
    let retainedUntil: Date
    let sourceSession: String
    let sequence: UInt64
    let origin: String
    let exposureTimeKnown: Bool
    let calibrationStatus: String

    enum CodingKeys: String, CodingKey {
        case id, sha256, width, height, size, representation, sequence, origin
        case contentType = "content_type", sourceCodec = "source_codec", receivedAt = "received_at"
        case retainedUntil = "retained_until", sourceSession = "source_session"
        case exposureTimeKnown = "exposure_time_known", calibrationStatus = "calibration_status"
    }
    func validate() throws {
        guard !id.isEmpty, id.utf8.count <= 256, Self.validDigest(sha256), width > 0, height > 0,
              width <= 8192, height <= 8192, width * height <= 32_000_000,
              size > 0, size <= 8 << 20, contentType == "image/jpeg",
              ["original_jpeg", "decoded_jpeg"].contains(representation), !sourceCodec.isEmpty, sourceCodec.utf8.count <= 64,
              !sourceSession.isEmpty, sourceSession.utf8.count <= 256, sequence > 0,
              ["measured", "simulated"].contains(origin), !exposureTimeKnown, calibrationStatus == "unknown",
              retainedUntil > receivedAt else { throw StationaryCameraError.response }
    }
    static func validDigest(_ value: String) -> Bool {
        value.utf8.count == 64 && value.utf8.allSatisfy { (48...57).contains($0) || (97...102).contains($0) }
    }
    var originLabel: String { origin == "measured" ? "Physical camera" : "Simulated camera" }
    var imageLabel: String { representation == "original_jpeg" ? "Original JPEG" : "Decoded camera frame" }
}

struct StationaryCameraState: Decodable {
    struct Camera: Decodable { let device: String; let id: String; let label: String }
    let schema: String
    let camera: Camera
    let connection: String
    let detail: String
    let fresh: Bool
    let freshness: String
    let exposureTimeKnown: Bool
    let calibrationStatus: String
    let latest: StationaryCameraReceipt?
    let recent: [StationaryCameraReceipt]
    enum CodingKeys: String, CodingKey {
        case schema, camera, connection, detail, fresh, freshness, latest, recent
        case exposureTimeKnown = "exposure_time_known", calibrationStatus = "calibration_status"
    }
    static func decode(_ data: Data) throws -> Self {
        guard data.count <= 512 << 10 else { throw StationaryCameraError.size }
        let decoder = JSONDecoder()
        let fractional = Date.ISO8601FormatStyle(includingFractionalSeconds: true)
        let wholeSeconds = Date.ISO8601FormatStyle(includingFractionalSeconds: false)
        decoder.dateDecodingStrategy = .custom { decoder in
            let raw = try decoder.singleValueContainer().decode(String.self)
            guard raw.utf8.count <= 64 else { throw StationaryCameraError.response }
            if let date = (try? fractional.parse(raw)) { return date }
            guard let date = (try? wholeSeconds.parse(raw)) else { throw StationaryCameraError.response }
            return date
        }
        let value = try decoder.decode(Self.self, from: data)
        guard value.schema == "wendy.stationary-camera.state.v1",
              ["connecting", "streaming", "disconnected", "error", "stopped"].contains(value.connection),
              ["waiting", "receiving", "stale", "disconnected", "error", "stopped"].contains(value.freshness),
              !value.exposureTimeKnown, value.calibrationStatus == "unknown", value.recent.count <= 64,
              Set(value.recent.map(\.id)).count == value.recent.count, value.detail.utf8.count <= 2048,
              [value.camera.device, value.camera.id, value.camera.label].allSatisfy({ !$0.isEmpty && $0.utf8.count <= 1024 }),
              !value.fresh || (value.connection == "streaming" && value.freshness == "receiving" && value.latest != nil)
        else { throw StationaryCameraError.response }
        try value.latest?.validate()
        try value.recent.forEach { try $0.validate() }
        return value
    }
}

struct StationaryCameraPoseState: Decodable {
    struct Site: Decodable {
        let id: String; let name: String; let modelRevision: String; let modelSHA256: String
        enum CodingKeys: String, CodingKey { case id, name; case modelRevision = "model_revision", modelSHA256 = "model_sha256" }
    }
    struct CalibrationProfile: Decodable, Equatable {
        let id: String; let name: String; let scope: String; let origin: String; let sha256: String
        let referenceDeviceCount: Int; let bindingSource: String
        enum CodingKeys: String, CodingKey {
            case id, name, scope, origin, sha256
            case referenceDeviceCount = "reference_device_count", bindingSource = "binding_source"
        }
        func validate() throws {
            guard !id.isEmpty, id.utf8.count <= 256, !name.isEmpty, name.utf8.count <= 256,
                  !id.unicodeScalars.contains(where: { CharacterSet.controlCharacters.contains($0) }),
                  !name.unicodeScalars.contains(where: { CharacterSet.controlCharacters.contains($0) }),
                  ["model", "device"].contains(scope), ["measured", "simulated"].contains(origin),
                  StationaryCameraReceipt.validDigest(sha256), (1...64).contains(referenceDeviceCount),
                  bindingSource == "operator" else { throw StationaryCameraError.response }
        }
        var label: String {
            if origin == "simulated" { return scope == "model" ? "Simulated model calibration" : "Simulated device calibration" }
            return scope == "model" ? "Model calibration" : "Per-device calibration"
        }
        var explanation: String {
            if origin == "simulated" { return "Simulation evidence only. Camera variant selected by operator." }
            if scope == "model" {
                return "Reference measurements from \(referenceDeviceCount) \(referenceDeviceCount == 1 ? "device" : "devices"). This unit was not individually calibrated."
            }
            return "Calibration for this device. Camera variant selected by operator."
        }
    }
    struct Lens: Decodable {
        let quality: String; let fx: Double; let fy: Double; let cx: Double; let cy: Double; let distortion: [Double]
        var profile: CalibrationProfile? = nil
        var qualityLabel: String {
            if let profile { return profile.label }
            return quality == "approximate" ? "Assumed lens · approximate pose" : quality == "scene_fit" ? "Lens fitted to room references" : "Measured lens"
        }
        var usesSharedOrAssumedLens: Bool { quality == "approximate" || quality == "model_calibration" }
    }
    struct Pose: Decodable {
        let worldFromCamera: [Double]; let position: [Double]; let lens: Lens
        let matches: Int; let inliers: Int; let rmsPixels: Double; let observedAt: Date; let validUntil: Date
        enum CodingKeys: String, CodingKey {
            case position, lens, matches, inliers
            case worldFromCamera = "world_from_camera", rmsPixels = "rms_pixels", observedAt = "observed_at", validUntil = "valid_until"
        }
    }
    struct Evidence: Decodable {
        let receiptID: String; let sha256: String; let sourceSession: String; let sequence: UInt64; let receivedAt: Date; let origin: String
        enum CodingKeys: String, CodingKey {
            case sha256, sequence, origin
            case receiptID = "receipt_id", sourceSession = "source_session", receivedAt = "received_at"
        }
    }
    let schema: String; let status: String; let detail: String; let fresh: Bool
    let provenance: String; let inputOrigin: String?; let site: Site
    let coordinateFrame: String; let units: String; let geographicAnchorKnown: Bool; let uncertaintyKnown: Bool
    let pose: Pose?; let evidence: [Evidence]?; let calibration: CalibrationProfile?
    enum CodingKeys: String, CodingKey {
        case schema, status, detail, fresh, provenance, site, units, pose, evidence, calibration
        case inputOrigin = "input_origin", coordinateFrame = "coordinate_frame"
        case geographicAnchorKnown = "geographic_anchor_known", uncertaintyKnown = "uncertainty_known"
    }
    static func decode(_ data: Data) throws -> Self {
        guard data.count <= 64 << 10 else { throw StationaryCameraError.size }
        let decoder = JSONDecoder()
        let fractional = Date.ISO8601FormatStyle(includingFractionalSeconds: true)
        let wholeSeconds = Date.ISO8601FormatStyle(includingFractionalSeconds: false)
        decoder.dateDecodingStrategy = .custom { decoder in
            let value = try decoder.singleValueContainer().decode(String.self)
            guard value.utf8.count <= 64 else { throw StationaryCameraError.response }
            if let date = (try? fractional.parse(value)) { return date }
            guard let date = (try? wholeSeconds.parse(value)) else { throw StationaryCameraError.response }; return date
        }
        let value = try decoder.decode(Self.self, from: data)
        guard value.schema == "wendy.camera-pose.state.v1",
              ["disabled", "idle", "estimating", "located", "no_match", "unavailable"].contains(value.status),
              value.provenance == "inferred", ["", "measured", "simulated"].contains(value.inputOrigin ?? ""),
              value.units == "m", !value.geographicAnchorKnown, !value.uncertaintyKnown,
              [value.detail, value.coordinateFrame, value.site.id, value.site.name, value.site.modelRevision].allSatisfy({ $0.utf8.count <= 2048 }),
              (value.evidence?.count ?? 0) <= 2,
              !value.fresh || (value.status == "located" && value.pose != nil)
        else { throw StationaryCameraError.response }
        try value.calibration?.validate()
        for item in value.evidence ?? [] {
            guard StationaryCameraReceipt.validDigest(item.sha256), !item.receiptID.isEmpty, item.receiptID.utf8.count <= 256,
                  !item.sourceSession.isEmpty, item.sourceSession.utf8.count <= 256, item.sequence > 0,
                  ["measured", "simulated"].contains(item.origin) else { throw StationaryCameraError.response }
        }
        if let pose = value.pose {
            guard value.status == "located", value.fresh, !value.site.id.isEmpty, !value.site.modelRevision.isEmpty,
                  StationaryCameraReceipt.validDigest(value.site.modelSHA256), !value.coordinateFrame.isEmpty,
                  pose.worldFromCamera.count == 16, pose.worldFromCamera.allSatisfy(\.isFinite),
                  pose.position.count == 3, pose.position.allSatisfy({ $0.isFinite && abs($0) <= 1000 }),
                  zip(pose.position, pose.worldFromCamera[12...14]).allSatisfy({ abs($0 - $1) < 0.000001 }),
                  ["approximate", "scene_fit", "measured", "model_calibration"].contains(pose.lens.quality),
                  [pose.lens.fx, pose.lens.fy, pose.lens.cx, pose.lens.cy].allSatisfy(\.isFinite),
                  pose.lens.fx > 0, pose.lens.fy > 0, pose.lens.distortion.count == 5, pose.lens.distortion.allSatisfy(\.isFinite),
                  pose.inliers >= 8, pose.matches >= pose.inliers, pose.rmsPixels.isFinite, pose.rmsPixels >= 0,
                  pose.validUntil > pose.observedAt, let inputs = value.evidence, inputs.count == 2,
                  inputs[0].sourceSession == inputs[1].sourceSession, inputs[0].sequence < inputs[1].sequence,
                  inputs[0].receivedAt < inputs[1].receivedAt,
                  inputs.allSatisfy({ $0.origin == value.inputOrigin })
            else { throw StationaryCameraError.response }
            try pose.lens.profile?.validate()
            if let profile = pose.lens.profile {
                guard (profile.scope == "model" && pose.lens.quality == "model_calibration") ||
                      (profile.scope == "device" && pose.lens.quality == "measured"),
                      profile.origin != "simulated" || value.inputOrigin == "simulated",
                      value.calibration == nil || value.calibration == profile
                else { throw StationaryCameraError.response }
            } else if pose.lens.quality == "model_calibration" || value.calibration != nil {
                throw StationaryCameraError.response
            }
        }
        return value
    }
}

struct StationaryCameraProfile: Decodable {
    let url: URL
    let token: String
    static func read(_ fileURL: URL) throws -> Self {
        let fd = Darwin.open(fileURL.path, O_RDONLY | O_NOFOLLOW | O_CLOEXEC)
        guard fd >= 0 else { throw StationaryCameraError.profile }
        let handle = FileHandle(fileDescriptor: fd, closeOnDealloc: true)
        defer { try? handle.close() }
        var info = stat()
        guard fstat(fd, &info) == 0, info.st_mode & S_IFMT == S_IFREG,
              info.st_mode & 0o077 == 0, info.st_uid == getuid(), info.st_nlink == 1,
              info.st_size > 0, info.st_size <= 16 << 10,
              let data = try handle.read(upToCount: (16 << 10) + 1), data.count <= 16 << 10
        else { throw StationaryCameraError.profile }
        let value = try JSONDecoder().decode(Self.self, from: data)
        try value.validate()
        return value
    }
    func validate() throws {
        guard url.scheme == "http", ["127.0.0.1", "[::1]"].contains(url.host ?? ""),
              let port = url.port, port > 0, port <= 65535, url.user == nil, url.password == nil,
              url.query == nil, url.fragment == nil, ["", "/"].contains(url.path),
              token.utf8.count >= 32, token.utf8.count <= 256,
              token.utf8.allSatisfy({ $0 >= 33 && $0 <= 126 })
        else { throw StationaryCameraError.profile }
    }
}

private final class StationaryCameraRedirects: NSObject, URLSessionTaskDelegate, @unchecked Sendable {
    func urlSession(_ session: URLSession, task: URLSessionTask, willPerformHTTPRedirection response: HTTPURLResponse,
                    newRequest request: URLRequest, completionHandler: @escaping @Sendable (URLRequest?) -> Void) {
        completionHandler(nil)
    }
}

@MainActor final class StationaryCameraModel: ObservableObject {
    @Published private(set) var state: StationaryCameraState?
    @Published private(set) var liveImage: NSImage?
    @Published private(set) var selected: StationaryCameraReceipt?
    @Published private(set) var selectedImage: NSImage?
    @Published private(set) var message: String?
    @Published private(set) var reachable = false
    @Published private(set) var loadingEvidence = false
    @Published private(set) var clockTick: UInt64 = 0
    @Published private(set) var controlPending = false
    @Published private(set) var controlMessage: String?
    @Published private(set) var poseState: StationaryCameraPoseState?
    @Published private(set) var poseMessage: String?
    @Published private(set) var posePending = false
    @Published var twin: CameraTwinSnapshot?
    @Published var selectedCameraID: String?
    @Published var roomURL: URL?
    var roomCapture: NSImage?
    @Published var twinMessage: String?
    @Published var showRoom = true
    @Published var showCameraColor = false
    @Published var showPeople = false
    @Published var floorY: Double?
    @Published var choosingFloor = false
    @Published var twinImages: [String: NSImage] = [:]
    @Published var peopleTracks: [CameraTwinPersonDisplay] = []
    @Published var peopleMessage = "Choose a floor to place anonymous people."
    var twinGeneration: UInt64 = 0
    var refreshGeneration: UInt64 = 0
    var poseRefreshGeneration: UInt64 = 0
    var twinUpdatedUptime: TimeInterval = -.infinity
    var roomDigest: String?
    var peopleEngine = CameraTwinPeopleEngine()
    var lastPeopleContext: String?
    var peopleProcessing = false
    var lastPeopleBatch: CameraTwinPeopleBatch?
    var lastColorUpdate: TimeInterval = -.infinity
    private var poseReachable = false
    private var lastPoseUptime: TimeInterval = -.infinity
    let profile: StationaryCameraProfile?
    private let session: URLSession
    private var lastStateUptime: TimeInterval = -.infinity
    private var liveDigest: String?
    private var selectedGeneration = 0
    private var imageCache: [String: (NSImage, Date, Int)] = [:]
    private var cacheOrder: [String] = []

    init(profileURL: URL) {
        profile = try? StationaryCameraProfile.read(profileURL)
        let config = URLSessionConfiguration.ephemeral
        config.timeoutIntervalForRequest = 2
        config.timeoutIntervalForResource = 3
        config.urlCache = nil
        config.httpCookieStorage = nil
        config.requestCachePolicy = .reloadIgnoringLocalCacheData
        session = URLSession(configuration: config, delegate: StationaryCameraRedirects(), delegateQueue: nil)
        if profile == nil { message = "The private camera connection could not be opened." }
    }
    // Used by native checks and explicit recorded-state rendering. No camera opens here.
    init(previewState: StationaryCameraState, image: NSImage?) {
        profile = nil
        session = URLSession(configuration: .ephemeral)
        state = previewState
        liveImage = image
        liveDigest = previewState.latest?.sha256
        reachable = true
        lastStateUptime = ProcessInfo.processInfo.systemUptime
    }
    var isReceiving: Bool {
        reachable && state?.fresh == true && state?.connection == "streaming"
            && ProcessInfo.processInfo.systemUptime - lastStateUptime < 3
    }
    var poseSelectionMismatch: Bool {
        guard let selected, let inputs = poseState?.evidence, !inputs.isEmpty else { return false }
        return inputs.contains { $0.sourceSession != selected.sourceSession || $0.origin != selected.origin }
    }
    var currentPose: StationaryCameraPoseState.Pose? {
        guard !poseSelectionMismatch, poseReachable, ProcessInfo.processInfo.systemUptime - lastPoseUptime < 3,
              isReceiving, let value = poseState, value.status == "located", value.fresh,
              let pose = value.pose, pose.validUntil > Date(), let latest = state?.latest,
              let inputs = value.evidence, inputs.count == 2,
              inputs.allSatisfy({ $0.sourceSession == latest.sourceSession && $0.origin == latest.origin }),
              value.inputOrigin == latest.origin else { return nil }
        return pose
    }
    var canEstimatePose: Bool {
        isReceiving && poseReachable && poseState?.status != "disabled" && poseState?.status != "estimating" && !posePending
    }
    var poseStatusLabel: String {
        if poseSelectionMismatch { return "No position for this retained image" }
        if currentPose != nil { return "Position estimated" }
        if posePending || poseState?.status == "estimating" { return "Estimating position" }
        if !poseReachable && poseState != nil { return "Position service unavailable" }
        switch poseState?.status {
        case "no_match": return "No position found"
        case "disabled": return "Room references needed"
        case "unavailable": return "Position unavailable"
        case "located": return "Position needs a fresh check"
        default: return "Position not estimated"
        }
    }
    var displayReceipt: StationaryCameraReceipt? { selected ?? state?.latest }
    var displayImage: NSImage? { selected == nil ? liveImage : selectedImage }
    var statusLabel: String {
        if profile == nil && state == nil { return "Connection unavailable" }
        if !reachable || ProcessInfo.processInfo.systemUptime - lastStateUptime >= 3 { return "Viewer disconnected" }
        if isReceiving { return "Receiving frames" }
        switch state?.freshness {
        case "waiting": return "Waiting for camera"
        case "disconnected": return "Camera disconnected"
        case "stopped": return "Capture stopped"
        case "error": return "Capture unavailable"
        default: return "No fresh frames"
        }
    }
    func run() async {
        guard profile != nil else { return }
        defer { stop() }
        while !Task.isCancelled {
            await refresh()
            do { try await Task.sleep(for: .milliseconds(500)) } catch { return }
        }
    }

    func stop() { reachable = false; twinUpdatedUptime = -.infinity; peopleTracks = [] }
    func tick(now: Date = Date()) {
        clockTick &+= 1
        imageCache = imageCache.filter { $0.value.1 > now }
        cacheOrder.removeAll { imageCache[$0] == nil }
        if let item = selected, item.retainedUntil <= now { selectedImage = nil; message = "This retained image has expired." }
        if let item = state?.latest, item.retainedUntil <= now { liveImage = nil; liveDigest = nil }
    }
    func refresh() async {
        guard profile != nil else { return }
        refreshGeneration &+= 1
        let refreshAtStart = refreshGeneration
        await refreshTwin()
        guard refreshAtStart == refreshGeneration else { return }
        let cameraAtStart = selectedCameraID
        if twinFresh, let camera = twin?.cameras.first(where: { $0.id == cameraAtStart }) {
            apply(camera.state); applyPose(camera.pose)
            liveImage = twinImages[camera.id]
            liveDigest = liveImage == nil ? nil : camera.state.latest?.sha256
            await refreshPeople(); tick(); return
        }
        do {
            let data = try await get(path: "/v1/state", limit: 512 << 10, contentType: "application/json")
            guard !Task.isCancelled, refreshAtStart == refreshGeneration, cameraAtStart == selectedCameraID else { return }
            let next = try StationaryCameraState.decode(data)
            apply(next)
            if let latest = next.latest, latest.retainedUntil > Date(), latest.sha256 != liveDigest {
                let image = try await load(latest)
                guard !Task.isCancelled, state?.latest?.id == latest.id else { return }
                liveImage = image; liveDigest = latest.sha256
            }
        } catch {
            if Task.isCancelled { return }
            reachable = false
            if selected == nil { message = "The camera connection is unavailable. Retained evidence remains listed." }
        }
        guard refreshAtStart == refreshGeneration, cameraAtStart == selectedCameraID else { return }
        await refreshPose()
        await refreshPeople()
        tick()
    }
    func apply(_ next: StationaryCameraState) {
        // Never present prior pixels beside a new receipt while its image is unavailable.
        if next.latest?.sha256 != liveDigest { liveImage = nil; liveDigest = nil }
        state = next; reachable = true; lastStateUptime = ProcessInfo.processInfo.systemUptime
        if selected == nil { message = nil }
        // An exact selected receipt remains independent of subsequent snapshots.
    }
    func select(_ receipt: StationaryCameraReceipt) async {
        selectedGeneration &+= 1
        let generation = selectedGeneration
        selected = receipt; selectedImage = nil; loadingEvidence = true; message = nil
        defer { if selectedGeneration == generation { loadingEvidence = false } }
        do {
            try receipt.validate()
            let image = try await load(receipt)
            guard generation == selectedGeneration else { return }
            selectedImage = image
        } catch {
            guard generation == selectedGeneration else { return }
            message = receipt.retainedUntil <= Date() ? "This retained image has expired." : "This image is unavailable. Its receipt remains inspectable."
        }
    }
    func showLive() {
        selectedGeneration &+= 1
        selected = nil; selectedImage = nil; loadingEvidence = false; message = nil
    }
    func control(_ action: String) async {
        guard ["pause", "resume"].contains(action), !controlPending, profile != nil else { return }
        controlPending = true; controlMessage = nil
        defer { controlPending = false }
        do {
            let body = try JSONSerialization.data(withJSONObject: ["action": action])
            _ = try await get(path: "/v1/control", limit: 4096, contentType: "application/json", method: "POST", body: body)
            await refresh()
        } catch {
            controlMessage = "The control outcome is unknown. Check the feed status before trying again."
        }
    }
    func applyPose(_ value: StationaryCameraPoseState) {
        poseState = value; poseReachable = true; lastPoseUptime = ProcessInfo.processInfo.systemUptime
        poseMessage = nil
    }
    func refreshPose() async {
        guard profile != nil else { return }
        poseRefreshGeneration &+= 1
        let generation = poseRefreshGeneration, cameraID = selectedCameraID
        do {
            let data = try await get(path: "/v1/pose", limit: 64 << 10, contentType: "application/json")
            guard !Task.isCancelled, generation == poseRefreshGeneration, cameraID == selectedCameraID else { return }
            applyPose(try StationaryCameraPoseState.decode(data))
        } catch StationaryCameraError.http(404) {
            guard generation == poseRefreshGeneration, cameraID == selectedCameraID else { return }
            poseState = nil; poseReachable = false; poseMessage = nil
        } catch {
            if Task.isCancelled || generation != poseRefreshGeneration || cameraID != selectedCameraID { return }
            poseReachable = false
            if poseState != nil { poseMessage = "Position is unavailable until the connection returns." }
        }
    }
    func estimatePosition() async {
        guard canEstimatePose, profile != nil else { return }
        posePending = true; poseMessage = nil
        defer { posePending = false }
        do {
            _ = try await get(path: "/v1/pose/estimate", limit: 64 << 10, contentType: "application/json", method: "POST", body: Data("{}".utf8), statuses: [200, 202])
            await refreshPose()
        } catch {
            poseMessage = "The estimate could not be started. Check the camera and room references, then retry."
        }
    }
    func load(_ receipt: StationaryCameraReceipt, cameraID: String? = nil) async throws -> NSImage {
        guard receipt.retainedUntil > Date() else { throw StationaryCameraError.evidence }
        if let cached = imageCache[receipt.sha256], cached.1 > Date(), cached.2 == receipt.size,
           cached.0.size == NSSize(width: receipt.width, height: receipt.height) { return cached.0 }
        let data = try await get(path: "/v1/evidence/" + receipt.sha256, limit: receipt.size, contentType: "image/jpeg", cameraID: cameraID)
        let image = try Self.verifyImage(data, receipt: receipt)
        guard receipt.retainedUntil > Date() else { throw StationaryCameraError.evidence }
        imageCache[receipt.sha256] = (image, receipt.retainedUntil, data.count)
        cacheOrder.removeAll { $0 == receipt.sha256 }; cacheOrder.append(receipt.sha256)
        while cacheOrder.count > 12 || imageCache.values.reduce(0, { $0 + $1.2 }) > 24 << 20 {
            imageCache.removeValue(forKey: cacheOrder.removeFirst())
        }
        return image
    }
    static func verifyImage(_ data: Data, receipt: StationaryCameraReceipt) throws -> NSImage {
        try receipt.validate()
        guard data.count == receipt.size,
              SHA256.hash(data: data).map({ String(format: "%02x", $0) }).joined() == receipt.sha256,
              let source = CGImageSourceCreateWithData(data as CFData, [kCGImageSourceShouldCache: false] as CFDictionary),
              CGImageSourceGetCount(source) == 1,
              let type = CGImageSourceGetType(source), type as String == "public.jpeg",
              let properties = CGImageSourceCopyPropertiesAtIndex(source, 0, nil) as? [String: Any],
              properties[kCGImagePropertyPixelWidth as String] as? Int == receipt.width,
              properties[kCGImagePropertyPixelHeight as String] as? Int == receipt.height,
              let image = CGImageSourceCreateImageAtIndex(source, 0, nil)
        else { throw StationaryCameraError.evidence }
        return NSImage(cgImage: image, size: NSSize(width: receipt.width, height: receipt.height))
    }
    func get(path: String, limit: Int, contentType: String, method: String = "GET", body: Data? = nil, statuses: Set<Int> = [200], cameraID: String? = nil) async throws -> Data {
        guard let profile, let base = URL(string: path, relativeTo: profile.url)?.absoluteURL,
              var components = URLComponents(url: base, resolvingAgainstBaseURL: false) else { throw StationaryCameraError.profile }
        if !path.hasPrefix("/v1/twin"), !path.hasPrefix("/v1/site"), let id = cameraID ?? selectedCameraID {
            components.queryItems = [URLQueryItem(name: "camera", value: id)]
        }
        guard let url = components.url else { throw StationaryCameraError.profile }
        var request = URLRequest(url: url)
        request.httpMethod = method; request.httpBody = body
        if body != nil { request.setValue("application/json", forHTTPHeaderField: "Content-Type") }
        request.setValue("Bearer " + profile.token, forHTTPHeaderField: "Authorization")
        request.setValue(contentType, forHTTPHeaderField: "Accept")
        return try await Self.receive(session: session, request: request, limit: limit, contentType: contentType, statuses: statuses)
    }
    // Consume bounded response bytes away from the UI actor. Awaiting every byte
    // on MainActor adds one executor hop per byte and ages short-lived evidence.
    nonisolated private static func receive(session: URLSession, request: URLRequest, limit: Int, contentType: String, statuses: Set<Int>) async throws -> Data {
        let (bytes, response) = try await session.bytes(for: request)
        guard let response = response as? HTTPURLResponse else { throw StationaryCameraError.response }
        guard statuses.contains(response.statusCode) else { throw StationaryCameraError.http(response.statusCode) }
        guard response.mimeType == contentType, response.expectedContentLength <= limit else { throw StationaryCameraError.response }
        var result = Data(); result.reserveCapacity(min(max(0, Int(response.expectedContentLength)), limit))
        for try await byte in bytes {
            guard result.count < limit else { throw StationaryCameraError.size }
            result.append(byte)
        }
        return result
    }
    func capture(to destination: URL, caption: String? = nil) throws {
        if showRoom, let url = roomURL {
            roomCapture = CameraTwinScene.capture(url: url, scale: twin?.site?.metresPerModelUnit ?? 1,
                cameras: visibleTwinCameras, images: twinImages, selected: selectedCameraID,
                color: showCameraColor, people: showPeople && twinFresh ? peopleTracks : [])
        }
        defer { roomCapture = nil }
        let content = StationaryCameraContent(model: self, managesConnection: false)
        let view: AnyView
        if let caption {
            view = AnyView(VStack(spacing: 0) {
                Text((displayReceipt?.origin == "measured" ? "PHYSICAL CAMERA" : displayReceipt?.origin == "simulated" ? "SIMULATED INPUT" : "UNKNOWN SOURCE") + "  /  RECORDED NATIVE CONTROLS")
                    .font(.system(size: 10, weight: .semibold)).tracking(1.5)
                    .foregroundStyle(.white).frame(maxWidth: .infinity).frame(height: 28)
                    .background(cameraInk)
                content.frame(width: 1440, height: 832)
                HStack {
                    Text(caption).font(.system(size: 14, weight: .semibold)).foregroundStyle(.white)
                    Spacer()
                }.padding(.horizontal, 28).frame(height: 40).background(cameraTeal)
            }.frame(width: 1440, height: 900).environment(\.colorScheme, .light))
        } else {
            view = AnyView(content.frame(width: 1440, height: 900).environment(\.colorScheme, .light))
        }
        let hosting = NSHostingView(rootView: view)
        hosting.frame = NSRect(x: 0, y: 0, width: 1440, height: 900)
        let window = NSWindow(contentRect: hosting.frame, styleMask: [.borderless], backing: .buffered, defer: false)
        window.isReleasedWhenClosed = false
        window.contentView = hosting
        defer { window.close() }
        hosting.layoutSubtreeIfNeeded()
        window.displayIfNeeded()
        guard let bitmap = hosting.bitmapImageRepForCachingDisplay(in: hosting.bounds) else { throw StationaryCameraError.evidence }
        hosting.cacheDisplay(in: hosting.bounds, to: bitmap)
        guard let png = bitmap.representation(using: .png, properties: [:]) else { throw StationaryCameraError.evidence }
        try png.write(to: destination, options: .atomic)
    }
}

struct StationaryCameraContent: View {
    @State private var actions = TwinActions()

    @StateObject private var model: StationaryCameraModel
    @State private var showingEvidence = false
    private let managesConnection: Bool
    private let clock = Timer.publish(every: 0.5, on: .main, in: .common).autoconnect()
    init(profileURL: URL) { _model = StateObject(wrappedValue: StationaryCameraModel(profileURL: profileURL)); managesConnection = true }
    init(model: StationaryCameraModel, managesConnection: Bool = true) { _model = StateObject(wrappedValue: model); self.managesConnection = managesConnection }
    var body: some View {
        Group {

        VStack(spacing: 0) {
            header
            Divider()
            HStack(spacing: 0) {
                VStack(alignment: .leading, spacing: model.showRoom && model.twin != nil ? 12 : 20) {
                    HStack {
                        VStack(alignment: .leading, spacing: 5) {
                            Text(model.selected == nil ? (model.twin == nil ? "Camera observation" : "Your room, live") : "Retained evidence").font(.system(size: 25, weight: .semibold))
                            Text(model.selected == nil ? (model.twin?.site?.name ?? "One stationary camera, received by World.") : "This receipt stays selected as new frames arrive.")
                                .font(.system(size: 13)).foregroundStyle(.secondary)
                        }
                        Spacer()
                        if model.selected != nil { Button("Return to live") { model.showLive() }.buttonStyle(.borderedProminent).tint(cameraTeal) }
                    }
                    if model.twin != nil {
                        CameraTwinToolbar(model: model)
                    }
                    if model.showRoom, model.twin != nil {
                        CameraTwinRoomPanel(model: model).frame(maxHeight: .infinity)
                        imagePanel.frame(height: 110)
                    } else {
                        imagePanel
                    }
                    receiptStrip
                    if let message = model.controlMessage ?? model.message { Text(message).font(.system(size: 12)).foregroundStyle(.secondary) }
                    if !model.showRoom || model.twin == nil { history }
                    else { Button("Inspect recent evidence") { showingEvidence = true }.font(.system(size: 11)) }
                }.padding(model.showRoom && model.twin != nil ? 20 : 28)
                Divider()
                details.frame(width: 276)
            }
            footer
        }.background(cameraPaper).foregroundStyle(cameraInk)
            .frame(minWidth: 1080, minHeight: 700)
            .task { if managesConnection { await model.run() } }.onDisappear { if managesConnection { model.stop() } }
            .onReceive(clock) { model.tick(now: $0) }
            .sheet(isPresented: $showingEvidence) {
                VStack(alignment: .leading, spacing: 16) {
                    HStack { Text("Retained camera evidence").font(.title2); Spacer(); Button("Done") { showingEvidence = false } }
                    history
                    imagePanel.frame(height: 300)
                    receiptStrip
                }.padding(24).frame(width: 820, height: 540).background(cameraPaper)
            }

        }.task { await actions.run() }
}
    private var header: some View {
        HStack(spacing: 14) {
            Image(systemName: "camera.aperture").font(.system(size: 27, weight: .medium)).foregroundStyle(cameraTeal)
            VStack(alignment: .leading, spacing: 3) {
                Text("Wendy World").font(.system(size: 20, weight: .semibold, design: .rounded))
                Text(model.twin == nil ? "STATIONARY CAMERA" : "ROOM TWIN").font(.system(size: 9, weight: .semibold)).tracking(1.8).foregroundStyle(.secondary)
            }
            Spacer()
            if let twin = model.twin, twin.cameras.count > 1 {
                Picker("Camera", selection: Binding(get: { model.selectedCameraID ?? "" }, set: { id in actions.submit { [self] in  await model.chooseCamera(id) } })) {
                    ForEach(twin.cameras) { camera in Text(camera.name).tag(camera.id) }
                }.frame(maxWidth: 190)
            }
            if let receipt = model.state?.latest {
                Text(receipt.originLabel).font(.system(size: 12, weight: .medium)).padding(.horizontal, 12).padding(.vertical, 7)
                    .background(Color.white.opacity(0.8), in: Capsule())
            }
            Button {
                actions.submit { [self] in  await model.control(model.state?.connection == "stopped" ? "resume" : "pause") }
            } label: {
                Label(model.state?.connection == "stopped" ? "Resume feed" : "Pause feed",
                      systemImage: model.state?.connection == "stopped" ? "play.fill" : "pause.fill")
            }.buttonStyle(.bordered).disabled(model.controlPending || !model.reachable)
            Label(model.statusLabel, systemImage: model.isReceiving ? "circle.fill" : "circle.dotted")
                .font(.system(size: 12, weight: .semibold)).foregroundStyle(model.isReceiving ? cameraTeal : .orange)
                .padding(.horizontal, 14).padding(.vertical, 8).background((model.isReceiving ? cameraTeal : Color.orange).opacity(0.09), in: Capsule())
        }.padding(.leading, 82).padding(.trailing, 28).frame(height: 80)
    }
    private var imagePanel: some View {
        ZStack(alignment: .topLeading) {
            RoundedRectangle(cornerRadius: 16).fill(Color(red: 0.075, green: 0.12, blue: 0.13))
            if let image = model.displayImage {
                Image(nsImage: image).resizable().scaledToFit().padding(8).frame(maxWidth: .infinity, maxHeight: .infinity)
            } else {
                VStack(spacing: 14) {
                    Image(systemName: model.loadingEvidence ? "arrow.down.circle" : "video.slash").font(.system(size: 35, weight: .light))
                    Text(model.loadingEvidence ? "Opening verified evidence" : "Waiting for a camera image").font(.system(size: 15, weight: .medium))
                    Text("Frames appear here when the selected source is available.").font(.system(size: 12)).foregroundStyle(.white.opacity(0.55))
                }.foregroundStyle(.white.opacity(0.82)).frame(maxWidth: .infinity, maxHeight: .infinity)
            }
            HStack(spacing: 7) {
                Circle().fill(model.selected != nil ? Color.white : model.isReceiving ? Color.green : Color.orange).frame(width: 6, height: 6)
                Text(model.selected != nil ? "RETAINED IMAGE" : model.isReceiving ? "RECEIVING" : model.displayReceipt == nil ? "WAITING FOR CAMERA" : "LAST RECEIVED IMAGE")
                    .font(.system(size: 9, weight: .semibold)).tracking(1)
            }.foregroundStyle(.white).padding(.horizontal, 11).padding(.vertical, 8)
                .background(.black.opacity(0.6), in: Capsule()).padding(18)
        }.frame(maxWidth: .infinity, maxHeight: .infinity).frame(minHeight: model.showRoom && model.twin != nil ? 110 : 270).clipShape(RoundedRectangle(cornerRadius: 16))
    }
    private var receiptStrip: some View {
        HStack(spacing: 26) {
            metric("SOURCE", model.state?.camera.label ?? "Waiting for source")
            if let receipt = model.displayReceipt {
                metric("RECEIVED", receipt.receivedAt.formatted(date: .omitted, time: .standard))
                metric("IMAGE", "\(receipt.width) × \(receipt.height)")
                metric("FRAME", String(receipt.sequence))
            }
            Spacer(minLength: 0)
        }
    }
    private func metric(_ title: String, _ value: String) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            Text(title).font(.system(size: 9, weight: .semibold)).tracking(1.2).foregroundStyle(.secondary)
            Text(value).font(.system(size: 12, weight: .medium)).lineLimit(1)
        }
    }
    private var history: some View {
        VStack(alignment: .leading, spacing: 12) {
            HStack {
                Text("Recent evidence").font(.system(size: 14, weight: .semibold))
                Spacer()
                Text("Select a receipt to inspect its image").font(.system(size: 11)).foregroundStyle(.secondary)
            }
            ScrollView(.horizontal) {
                HStack(spacing: 10) {
                    ForEach(model.state?.recent ?? []) { item in
                        Button { actions.submit { [self] in  await model.select(item) } } label: {
                            VStack(alignment: .leading, spacing: 7) {
                                HStack {
                                    Image(systemName: "photo").foregroundStyle(cameraTeal)
                                    Spacer()
                                    Text("#\(item.sequence)").foregroundStyle(.secondary)
                                }
                                Text(item.receivedAt.formatted(date: .omitted, time: .standard)).fontWeight(.medium)
                                Text(String(item.sha256.prefix(12))).font(.system(size: 9, design: .monospaced)).foregroundStyle(.secondary)
                            }.font(.system(size: 11)).frame(width: 118, alignment: .leading).padding(12)
                                .background(model.selected?.id == item.id ? cameraTeal.opacity(0.10) : Color.white, in: RoundedRectangle(cornerRadius: 10))
                                .overlay(RoundedRectangle(cornerRadius: 10).stroke(model.selected?.id == item.id ? cameraTeal : cameraInk.opacity(0.08), lineWidth: 1))
                        }.buttonStyle(.plain).accessibilityLabel("Inspect frame \(item.sequence)")
                    }
                    if model.state?.recent.isEmpty != false { Text("The first received image will appear here.").font(.system(size: 12)).foregroundStyle(.secondary).frame(height: 78) }
                }.padding(.bottom, 3)
            }.scrollIndicators(.hidden).frame(height: 94)
        }
    }
    private var details: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 24) {
                VStack(alignment: .leading, spacing: 8) {
                    Image(systemName: "sensor.fill").font(.system(size: 23)).foregroundStyle(cameraTeal)
                    Text("Camera details").font(.system(size: 21, weight: .semibold))
                    Text("World retains the received image with its source and receipt time.").font(.system(size: 12)).foregroundStyle(.secondary).lineSpacing(3)
                }
                if model.poseState != nil {
                    Divider()
                    positionPanel
                }
                Divider()
                detail("Device", model.state?.camera.device ?? "Not connected")
                detail("Camera", model.state?.camera.id ?? "Not selected")
                if let receipt = model.displayReceipt {
                    detail("Provenance", receipt.origin == "measured" ? "Measured camera input" : "Simulated input")
                    detail("Image representation", receipt.imageLabel)
                    detail("Receipt time", receipt.receivedAt.formatted(date: .abbreviated, time: .standard))
                    detail("Evidence expires", receipt.retainedUntil.formatted(date: .abbreviated, time: .standard))
                    VStack(alignment: .leading, spacing: 6) {
                        Text("SHA-256").font(.system(size: 10, weight: .semibold)).foregroundStyle(.secondary)
                        Text(receipt.sha256).font(.system(size: 9, design: .monospaced)).textSelection(.enabled).lineSpacing(3)
                    }
                }
                Divider()
                detail("Exposure time", "Unknown")
                detail("Spatial calibration", model.currentPose == nil ? "Not configured" : "Estimated against saved scan")
                Text("Receiving means frames are reaching this viewer. It does not establish their exposure time or a position in the room.")
                    .font(.system(size: 11)).foregroundStyle(.secondary).lineSpacing(3)
                if let detail = model.state?.detail, !detail.isEmpty { Text(detail).font(.system(size: 11)).foregroundStyle(.secondary).lineSpacing(3) }
            }.padding(24)
        }.background(Color.white.opacity(0.45))
    }
    private var positionPanel: some View {
        VStack(alignment: .leading, spacing: 12) {
            HStack(spacing: 7) {
                Image(systemName: "viewfinder").foregroundStyle(cameraTeal)
                Text("Camera position").font(.system(size: 15, weight: .semibold))
            }
            Text(model.poseStatusLabel).font(.system(size: 12, weight: .medium))
                .foregroundStyle(model.currentPose == nil ? Color.secondary : cameraTeal)
            if let site = model.poseState?.site, !site.name.isEmpty {
                Text("Reference scan: " + site.name).font(.system(size: 11)).foregroundStyle(.secondary)
            }
            if let calibration = model.poseState?.calibration ?? model.currentPose?.lens.profile {
                VStack(alignment: .leading, spacing: 4) {
                    Text(calibration.label).font(.system(size: 11, weight: .semibold))
                    Text(calibration.name).font(.system(size: 11)).textSelection(.enabled)
                    Text(calibration.explanation).font(.system(size: 10)).foregroundStyle(.secondary).lineSpacing(2)
                }
                .padding(9)
                .frame(maxWidth: .infinity, alignment: .leading)
                .background((calibration.scope == "model" || calibration.origin == "simulated" ? Color.orange : cameraTeal).opacity(0.08), in: RoundedRectangle(cornerRadius: 7))
            }
            Button { actions.submit { [self] in  await model.estimatePosition() } } label: {
                HStack(spacing: 7) {
                    if model.posePending || model.poseState?.status == "estimating" { ProgressView().controlSize(.small) }
                    Text(model.poseState?.status == "estimating" ? "Matching room features…" : "Estimate position")
                }
            }.buttonStyle(.borderedProminent).tint(cameraTeal).disabled(!model.canEstimatePose)
            if let camera = model.twin?.cameras.first(where: { $0.id == model.selectedCameraID }) {
                Toggle("Align automatically", isOn: Binding(get: { camera.automaticAlignment }, set: { enabled in actions.submit { [self] in  await model.setAutomaticAlignment(enabled) } }))
                    .font(.system(size: 11)).toggleStyle(.switch).controlSize(.small)
            }
            if let pose = model.currentPose {
                HStack(spacing: 12) {
                    ForEach(0..<3) { index in
                        VStack(alignment: .leading, spacing: 4) {
                            Text(["X", "Y", "Z"][index]).font(.system(size: 9, weight: .semibold)).foregroundStyle(.secondary)
                            Text(String(format: "%.2f m", pose.position[index])).font(.system(size: 12, weight: .medium, design: .monospaced))
                        }
                    }
                }.padding(10).background(cameraTeal.opacity(0.07), in: RoundedRectangle(cornerRadius: 8))
                detail("View direction in scan", String(format: "[%.2f, %.2f, %.2f]", pose.worldFromCamera[8], pose.worldFromCamera[9], pose.worldFromCamera[10]))
                detail("Lens", pose.lens.qualityLabel)
                detail("Fit support", "\(pose.inliers) geometric matches · \(String(format: "%.2f", pose.rmsPixels)) px RMS")
                Text("Inferred position in this scan’s metre coordinates. No geographic anchor. Position uncertainty is unknown.")
                    .font(.system(size: 10)).foregroundStyle(.secondary).lineSpacing(2)
            } else if model.poseSelectionMismatch {
                Text("The current estimate belongs to another camera session. Return to live to view its position.")
                    .font(.system(size: 11)).foregroundStyle(.secondary).lineSpacing(2)
            } else if let detail = model.poseState?.detail, !detail.isEmpty {
                Text(detail).font(.system(size: 11)).foregroundStyle(.secondary).lineSpacing(2)
            }
            if let message = model.poseMessage { Text(message).font(.system(size: 11)).foregroundStyle(.secondary) }
            if model.currentPose == nil && !model.poseSelectionMismatch {
                Text("Uses room features without markers. No location is shown until two fresh images agree.")
                    .font(.system(size: 10)).foregroundStyle(.secondary).lineSpacing(2)
            }
        }
    }
    private func detail(_ title: String, _ value: String) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            Text(title).font(.system(size: 10, weight: .semibold)).foregroundStyle(.secondary)
            Text(value).font(.system(size: 12)).textSelection(.enabled).fixedSize(horizontal: false, vertical: true)
        }
    }
    private var footer: some View {
        HStack {
            Label("Private camera test", systemImage: "lock").font(.system(size: 10, weight: .medium))
            Spacer()
            Text("Receipt timestamps describe arrival at World. Camera exposure time is unknown.").font(.system(size: 10)).foregroundStyle(.secondary)
        }.padding(.horizontal, 28).frame(height: 35).background(cameraInk.opacity(0.025))
    }
}
