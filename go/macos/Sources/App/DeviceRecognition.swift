import Foundation
import CoreGraphics
import ImageIO

/// A match supplies visual evidence, not a hardware identity or an optical pose.
struct DeviceVisualMatch: Identifiable, Sendable {
    let id: String
    let label: String
    let deviceID: String?
    let corners: [CGPoint]
    let inliers: Int
    let residual: Double
}

struct DeviceVisualReference: Codable, Identifiable, Sendable {
    var id = UUID()
    var label: String
    var deviceID: String?
    var width: Int
    var height: Int
    var gray: Data
    var quad: [Double]

    var valid: Bool {
        (16...1920).contains(width) && (16...1920).contains(height)
            && gray.count == width * height && gray.count <= 3_686_400
            && !label.isEmpty && label.count <= 256 && (deviceID?.count ?? 0) <= 1024
            && quad.count == 8 && quad.enumerated().allSatisfy {
                $0.element.isFinite && $0.element >= 0 && $0.element < Double($0.offset.isMultiple(of: 2) ? width : height)
            }
    }
}

/// Actor confinement is the sole access path to this C++ object's mutable state.
private final class DeviceRecognizerHandle: @unchecked Sendable {
    let pointer = WVRecognizerCreate()
    deinit { WVRecognizerDestroy(pointer) }
}

actor DeviceRecognitionEngine {
    private var handle = DeviceRecognizerHandle()
    private var references: [DeviceVisualReference] = []
    private var saved: [DeviceVisualReference] = []
    private let registry: URL
    private var loaded = false

    init(registry: URL = URL.applicationSupportDirectory.appendingPathComponent("WendyDeviceRecognition/references.json")) {
        self.registry = registry
    }

    func load() throws {
        guard !loaded else { return }
        var records: [DeviceVisualReference] = []
        if FileManager.default.fileExists(atPath: registry.path) {
            let size = try registry.resourceValues(forKeys: [.fileSizeKey]).fileSize ?? Int.max
            guard size <= 32 * 1024 * 1024 else { throw RecognitionError.invalidLibrary }
            records = try JSONDecoder().decode([DeviceVisualReference].self, from: Data(contentsOf: registry))
            guard records.count <= 12, records.allSatisfy(\.valid), Set(records.map(\.id)).count == records.count else {
                throw RecognitionError.invalidLibrary
            }
        }
        try rebuild(records)
        saved = records
        loaded = true
    }

    func registered() -> [DeviceVisualReference] { saved }

    func remember(_ reference: DeviceVisualReference) throws {
        guard reference.valid, reference.deviceID != nil else { throw RecognitionError.invalidReference }
        var next = saved.filter { $0.deviceID != reference.deviceID }
        next.append(reference)
        guard next.count <= 12 else { throw RecognitionError.libraryFull }
        // Validate extraction before changing the existing library or writing anything.
        let candidate = DeviceRecognizerHandle()
        guard add(reference, to: candidate) >= 0 else { throw RecognitionError.notEnoughDetail }
        let data = try JSONEncoder().encode(next)
        guard data.count <= 32 * 1024 * 1024 else { throw RecognitionError.libraryFull }
        try FileManager.default.createDirectory(at: registry.deletingLastPathComponent(), withIntermediateDirectories: true)
        try data.write(to: registry, options: .atomic)
        try rebuild(next)
        saved = next
    }

    func forget(_ id: UUID) throws {
        let next = saved.filter { $0.id != id }
        try JSONEncoder().encode(next).write(to: registry, options: .atomic)
        try rebuild(next)
        saved = next
    }

    func detect(gray: Data, width: Int, height: Int) throws -> [DeviceVisualMatch] {
        try inspect(gray: gray, width: width, height: height).matches
    }

    func inspect(gray: Data, width: Int, height: Int) throws -> DeviceRecognitionReport {
        let full = try detectRegion(gray: gray, width: width, height: height)
        guard full.matches.isEmpty, max(width, height) > 960 else { return full }
        // Keep original pixels near the centre instead of shrinking a tiny board
        // with the whole camera image. Do not relax geometric acceptance checks.
        let side = min(960, min(width, height))
        let left = (width-side)/2, top = (height-side)/2
        var cropped = Data(count: side*side)
        cropped.withUnsafeMutableBytes { target in
            gray.withUnsafeBytes { source in
                for row in 0..<side {
                    target.baseAddress!.advanced(by: row*side).copyMemory(from: source.baseAddress!.advanced(by: (top+row)*width+left), byteCount: side)
                }
            }
        }
        var zoom = try detectRegion(gray: cropped, width: side, height: side)
        zoom.usedCenterCrop = true
        zoom.matches = zoom.matches.map { match in
            DeviceVisualMatch(id: match.id, label: match.label, deviceID: match.deviceID,
                corners: match.corners.map { CGPoint(x: ($0.x*Double(side)+Double(left))/Double(width), y: ($0.y*Double(side)+Double(top))/Double(height)) },
                inliers: match.inliers, residual: match.residual)
        }
        return zoom.stage >= full.stage ? zoom : full
    }

    private func detectRegion(gray: Data, width: Int, height: Int) throws -> DeviceRecognitionReport {
        guard width >= 16, height >= 16, width <= 1920, height <= 1920, gray.count == width * height else {
            throw RecognitionError.invalidReference
        }
        var result = gray.withUnsafeBytes { bytes in
            WVRecognizerDetect(handle.pointer, bytes.bindMemory(to: UInt8.self).baseAddress, Int32(width), Int32(height), 0, 0, 0, 0)
        }
        guard result.failed == 0 else { throw RecognitionError.detectionFailed }
        let items = withUnsafeBytes(of: &result.items) { Array($0.bindMemory(to: WVDetection.self).prefix(Int(result.count))) }
        let matches: [DeviceVisualMatch] = items.compactMap { detection in
            guard references.indices.contains(Int(detection.reference_index)) else { return nil }
            let ref = references[Int(detection.reference_index)]
            var raw = detection.corners
            let corners = withUnsafeBytes(of: &raw) { Array($0.bindMemory(to: Double.self)) }
            return DeviceVisualMatch(id: ref.id.uuidString, label: ref.label, deviceID: ref.deviceID,
                corners: stride(from: 0, to: 8, by: 2).map { CGPoint(x: corners[$0], y: corners[$0 + 1]) },
                inliers: Int(detection.inliers), residual: detection.rms_pixels)
        }
        return DeviceRecognitionReport(matches: matches, referenceCount: Int(result.reference_count),
            keypoints: Int(result.frame_keypoints), bestMatches: Int(result.best_matches), bestInliers: Int(result.best_inliers), stage: Int(result.stage))
    }

    func saveDiagnostic(gray: Data, width: Int, height: Int, report: String) throws {
        guard width > 0, height > 0, width <= 1920, height <= 1920, gray.count == width*height else { throw RecognitionError.invalidReference }
        struct Capture: Encodable {
            let capturedAt: Date
            let width, height: Int
            let gray: Data
            let report: String
        }
        let folder = URL.documentsDirectory
        try FileManager.default.createDirectory(at: folder, withIntermediateDirectories: true)
        let capture = Capture(capturedAt: Date(), width: width, height: height, gray: gray, report: report)
        try JSONEncoder().encode(capture).write(to: folder.appendingPathComponent("RecognitionDiagnostic.json"), options: .atomic)
    }

    private func rebuild(_ records: [DeviceVisualReference]) throws {
        let next = DeviceRecognizerHandle()
        guard next.pointer != nil else { throw RecognitionError.detectionFailed }
        var accepted: [DeviceVisualReference] = []
        for ref in Self.bundled() + records {
            guard add(ref, to: next) == accepted.count else { throw RecognitionError.notEnoughDetail }
            accepted.append(ref)
        }
        handle = next
        references = accepted
    }

    private func add(_ ref: DeviceVisualReference, to target: DeviceRecognizerHandle) -> Int {
        guard ref.valid else { return -1 }
        return ref.gray.withUnsafeBytes { bytes in
            ref.quad.withUnsafeBufferPointer { quad in
                Int(WVRecognizerAdd(target.pointer, bytes.bindMemory(to: UInt8.self).baseAddress,
                    Int32(ref.width), Int32(ref.height), quad.baseAddress, 0, 0))
            }
        }
    }

    private static func bundled() -> [DeviceVisualReference] {
        // Photographs identify appearance only. They are not calibrated metric planes.
        let specs: [(String, String, [Double])] = [
            ("seeed-sense", "Seeed XIAO ESP32S3 Sense", [730, 461, 1121, 616, 1008, 880, 679, 665]),
            ("seeed-xiao", "Seeed XIAO ESP32S3", [480, 280, 1190, 280, 1190, 1100, 480, 1100])
        ]
        return specs.compactMap { name, label, quad in
            guard let url = Bundle.main.url(forResource: name, withExtension: "jpg", subdirectory: "DeviceRecognition"),
                  let source = CGImageSourceCreateWithURL(url as CFURL, nil),
                  let image = CGImageSourceCreateImageAtIndex(source, 0, nil) else { return nil }
            let scale = min(1, 960 / Double(max(image.width, image.height)))
            let width = Int(Double(image.width) * scale), height = Int(Double(image.height) * scale)
            var gray = Data(count: width * height)
            let ok = gray.withUnsafeMutableBytes { bytes -> Bool in
                guard let context = CGContext(data: bytes.baseAddress, width: width, height: height, bitsPerComponent: 8,
                    bytesPerRow: width, space: CGColorSpaceCreateDeviceGray(), bitmapInfo: CGImageAlphaInfo.none.rawValue) else { return false }
                context.interpolationQuality = .high
                context.draw(image, in: CGRect(x: 0, y: 0, width: width, height: height))
                return true
            }
            guard ok else { return nil }
            return DeviceVisualReference(label: label, deviceID: nil, width: width, height: height, gray: gray, quad: quad.map { $0 * scale })
        }
    }
}

enum RecognitionError: LocalizedError {
    case invalidLibrary, invalidReference, libraryFull, notEnoughDetail, detectionFailed
    var errorDescription: String? {
        switch self {
        case .invalidLibrary: "The saved appearance library could not be read."
        case .invalidReference: "The reference image is invalid."
        case .libraryFull: "The appearance library is full. Remove an old reference first."
        case .notEnoughDetail: "Not enough visible detail. Move closer, improve the lighting, and keep only the device inside the frame."
        case .detectionFailed: "Recognition could not process this image."
        }
    }
}

/// Labels must match a whole observed line. A common model name or partial name
/// must never turn into a unique device assignment.
enum DeviceLabelMatcher {
    static func matches(_ text: String, names: [String]) -> [Int] {
        let value = normalized(text)
        guard value.count >= 6 else { return [] }
        return names.indices.filter { normalized(names[$0]) == value }
    }
    private static func normalized(_ text: String) -> String {
        text.trimmingCharacters(in: .whitespacesAndNewlines).lowercased()
            .split(whereSeparator: \.isWhitespace).joined(separator: " ")
    }
}

struct DeviceSight: Identifiable, Sendable {
    let id: String
    let label: String
    let deviceIDs: [String]
    let corners: [CGPoint]
    let evidence: String
    var center: CGPoint {
        guard !corners.isEmpty else { return .zero }
        return CGPoint(x: corners.map(\.x).reduce(0,+) / CGFloat(corners.count),
                       y: corners.map(\.y).reduce(0,+) / CGFloat(corners.count))
    }

    /// Two reference profiles can describe the same visible object. Keep all
    /// candidate devices in one card, rather than covering one with another.
    static func group(_ matches: [DeviceVisualMatch], display: CGAffineTransform) -> [DeviceSight] {
        var groups: [[DeviceVisualMatch]] = []
        for match in matches {
            if let index = groups.firstIndex(where: { group in group.contains { overlap($0.corners, match.corners) > 0.4 } }) {
                groups[index].append(match)
            } else { groups.append([match]) }
        }
        return groups.compactMap { group in
            guard let first = group.first else { return nil }
            let ids = Array(Set(group.compactMap(\.deviceID))).sorted()
            return DeviceSight(id: first.id, label: ids.count > 1 ? "Several devices match" : first.label,
                deviceIDs: ids, corners: first.corners.map { $0.applying(display) },
                evidence: ids.count > 1 ? "Multiple appearance matches" : ids.isEmpty ? "Hardware model match" : "Saved appearance match")
        }
    }

    private static func overlap(_ a: [CGPoint], _ b: [CGPoint]) -> CGFloat {
        func box(_ points: [CGPoint]) -> CGRect {
            guard let minX = points.map(\.x).min(), let minY = points.map(\.y).min(),
                  let maxX = points.map(\.x).max(), let maxY = points.map(\.y).max() else { return .zero }
            return CGRect(x: minX, y: minY, width: maxX-minX, height: maxY-minY)
        }
        let x = box(a), y = box(b), intersection = x.intersection(y)
        guard !intersection.isNull else { return 0 }
        let area = intersection.width * intersection.height
        let union = x.width*x.height + y.width*y.height - area
        return union > 0 ? area / union : 0
    }
}

struct DeviceRecognitionReport: Sendable {
    var matches: [DeviceVisualMatch]
    var referenceCount: Int
    var keypoints: Int
    var bestMatches: Int
    var bestInliers: Int
    var stage: Int
    var usedCenterCrop = false
    var summary: String {
        "\(referenceCount) references · \(keypoints) features · \(bestMatches) matches · \(bestInliers) inliers · stage \(stage)" + (usedCenterCrop ? " · centre crop" : "")
    }
    var guidance: String {
        if referenceCount == 0 { return "No appearance references loaded." }
        if keypoints < 16 { return "Not enough detail. Hold steady in good light." }
        if matches.isEmpty { return "No verified match. Centre the board with its lens facing you." }
        return "Checking the match. Hold steady."
    }
}

/// Camera tracking quality is deliberately absent: recognizing an image does
/// not require AR world tracking. Only fresh, consistent image evidence counts.
struct DeviceSightConfirmation {
    private var previous: [String: CGPoint] = [:]
    private var timestamp: TimeInterval?
    mutating func accept(_ sights: [DeviceSight], timestamp next: TimeInterval) -> [DeviceSight] {
        guard next.isFinite else { reset(); return [] }
        let gap = timestamp.map { next - $0 }
        let eligible = gap.map { $0 > 0 && $0 < 2 } ?? false
        let accepted = eligible ? sights.filter {
            guard let last = previous[$0.id] else { return false }
            return hypot($0.center.x-last.x, $0.center.y-last.y) < 0.15
                && $0.corners.allSatisfy { $0.x.isFinite && $0.y.isFinite }
        } : []
        previous = Dictionary(sights.map { ($0.id, $0.center) }, uniquingKeysWith: { first, _ in first })
        timestamp = next
        return accepted
    }
    mutating func reset() { previous = [:]; timestamp = nil }
}
