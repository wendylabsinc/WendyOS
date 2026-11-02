import Foundation
import CoreGraphics
import ImageIO
import Testing
#if os(iOS)
@testable import WendyMobile
#else
@testable import WendyDesktop
#endif

@Test func visualLabelsRequireWholeNamesAndKeepAmbiguity() {
    #expect(DeviceLabelMatcher.matches("esp-camera", names: ["esp-camera-01"]).isEmpty)
    #expect(DeviceLabelMatcher.matches("  ESP-camera-01  ", names: ["esp-camera-01"]) == [0])
    #expect(DeviceLabelMatcher.matches("esp-camera-01", names: ["esp-camera-01", "ESP-CAMERA-01"]) == [0, 1])
    #expect(DeviceLabelMatcher.matches("Seeed", names: ["Seeed"]).isEmpty)
}

@Test func visualReferenceRejectsMalformedPixelsAndCoordinates() {
    var ref = DeviceVisualReference(label: "device", deviceID: "id", width: 32, height: 32,
        gray: Data(repeating: 0, count: 1024), quad: [0,0,31,0,31,31,0,31])
    #expect(ref.valid)
    ref.gray = Data(repeating: 0, count: 1023)
    #expect(!ref.valid)
    ref.gray = Data(repeating: 0, count: 1024)
    ref.quad[0] = .nan
    #expect(!ref.valid)
    ref.quad[0] = 32
    #expect(!ref.valid)
}

@Test func bundledBoardRecognizesRotationAndRejectsBlankFrame() async throws {
    let engine = DeviceRecognitionEngine(registry: FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString))
    try await engine.load()
    let image = try referencePhoto()
    let found = try await engine.detect(gray: image.gray, width: image.width, height: image.height)
    #expect(found.contains { $0.label == "Seeed XIAO ESP32S3" })
    #expect(found.allSatisfy { $0.deviceID == nil })
    // Rotate pixels 90 degrees. The detector must return the new location,
    // not a match in the reference image's original coordinate frame.
    var rotated = Data(count: image.gray.count)
    rotated.withUnsafeMutableBytes { targetRaw in
        image.gray.withUnsafeBytes { sourceRaw in
            let target = targetRaw.bindMemory(to: UInt8.self), source = sourceRaw.bindMemory(to: UInt8.self)
            for y in 0..<image.height { for x in 0..<image.width {
                target[x*image.height+(image.height-1-y)] = source[y*image.width+x]
            } }
        }
    }
    let turned = try await engine.detect(gray: rotated, width: image.height, height: image.width)
    let first = try #require(found.first { $0.label == "Seeed XIAO ESP32S3" })
    let second = try #require(turned.first { $0.label == first.label })
    for (a,b) in zip(first.corners, second.corners) {
        #expect(abs(b.x - (1-a.y)) < 0.015)
        #expect(abs(b.y - a.x) < 0.015)
    }
    let empty = try await engine.detect(gray: Data(repeating: 127, count: 640*480), width: 640, height: 480)
    #expect(empty.isEmpty)
}

@Test func appearanceLibraryPersistsAndRemovesDeviceBindings() async throws {
    let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: directory) }
    let url = directory.appendingPathComponent("references.json")
    let engine = DeviceRecognitionEngine(registry: url)
    try await engine.load()
    var ref = try referencePhoto()
    ref.deviceID = "unique-device-id"
    ref.label = "Test board"
    try await engine.remember(ref)
    let reloaded = DeviceRecognitionEngine(registry: url)
    try await reloaded.load()
    let matches = try await reloaded.detect(gray: ref.gray, width: ref.width, height: ref.height)
    #expect(matches.contains { $0.deviceID == "unique-device-id" })
    try await reloaded.forget(ref.id)
    #expect(await reloaded.registered().isEmpty)
    let removed = DeviceRecognitionEngine(registry: url)
    try await removed.load()
    #expect(await removed.registered().isEmpty)
    let after = try await removed.detect(gray: ref.gray, width: ref.width, height: ref.height)
    #expect(!after.contains { $0.deviceID != nil })
}

@Test func appearanceLibraryDoesNotReplaceAReferenceWithBlankImage() async throws {
    let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: directory) }
    let engine = DeviceRecognitionEngine(registry: directory.appendingPathComponent("references.json"))
    try await engine.load()
    var ref = try referencePhoto()
    ref.deviceID = "device"
    try await engine.remember(ref)
    let oldID = ref.id
    ref.id = UUID()
    ref.gray = Data(repeating: 128, count: ref.width * ref.height)
    await #expect(throws: RecognitionError.self) { try await engine.remember(ref) }
    #expect(await engine.registered().map(\.id) == [oldID])
}

private func referencePhoto() throws -> DeviceVisualReference {
    let url = try #require(Bundle.main.url(forResource: "seeed-xiao", withExtension: "jpg", subdirectory: "DeviceRecognition"))
    let source = try #require(CGImageSourceCreateWithURL(url as CFURL, nil))
    let image = try #require(CGImageSourceCreateImageAtIndex(source, 0, nil))
    let scale = 960 / Double(max(image.width, image.height))
    let width = Int(Double(image.width)*scale), height = Int(Double(image.height)*scale)
    var gray = Data(count: width*height)
    try gray.withUnsafeMutableBytes { raw in
        let context = try #require(CGContext(data: raw.baseAddress, width: width, height: height, bitsPerComponent: 8,
            bytesPerRow: width, space: CGColorSpaceCreateDeviceGray(), bitmapInfo: CGImageAlphaInfo.none.rawValue))
        context.interpolationQuality = .high
        context.draw(image, in: CGRect(x: 0, y: 0, width: width, height: height))
    }
    return DeviceVisualReference(label: "Seeed", deviceID: nil, width: width, height: height, gray: gray,
        quad: [480,280,1190,280,1190,1100,480,1100].map { $0*scale })
}

@Test func overlappingAppearanceProfilesKeepEveryDeviceCandidate() {
    let a = [CGPoint(x: 0.1, y: 0.1), CGPoint(x: 0.4, y: 0.1), CGPoint(x: 0.4, y: 0.4), CGPoint(x: 0.1, y: 0.4)]
    let b = a.map { CGPoint(x: $0.x+0.01, y: $0.y+0.01) }
    let c = a.map { CGPoint(x: $0.x+0.5, y: $0.y) }
    let matches = [DeviceVisualMatch(id: "model", label: "Seeed", deviceID: nil, corners: a, inliers: 30, residual: 1),
                   DeviceVisualMatch(id: "one", label: "Camera 1", deviceID: "first", corners: b, inliers: 20, residual: 1),
                   DeviceVisualMatch(id: "two", label: "Camera 2", deviceID: "second", corners: a, inliers: 22, residual: 1),
                   DeviceVisualMatch(id: "other", label: "Other", deviceID: "third", corners: c, inliers: 25, residual: 1)]
    let grouped = DeviceSight.group(matches, display: .identity)
    #expect(grouped.count == 2)
    #expect(grouped[0].deviceIDs == ["first", "second"])
    #expect(grouped[0].evidence == "Multiple appearance matches")
    #expect(grouped[1].deviceIDs == ["third"])
}

@Test func imageConfirmationAcceptsSlowFreshObservationsAndRejectsStaleOrMovingTargets() {
    let corners = [CGPoint(x: 0.2, y: 0.2), CGPoint(x: 0.4, y: 0.2), CGPoint(x: 0.4, y: 0.4), CGPoint(x: 0.2, y: 0.4)]
    let sight = DeviceSight(id: "board", label: "Board", deviceIDs: [], corners: corners, evidence: "Hardware model match")
    var confirmation = DeviceSightConfirmation()
    #expect(confirmation.accept([sight], timestamp: 0).isEmpty)
    #expect(confirmation.accept([sight], timestamp: 1.2).count == 1)
    #expect(confirmation.accept([sight], timestamp: 1.2).isEmpty)
    #expect(confirmation.accept([sight], timestamp: 4).isEmpty)
    #expect(confirmation.accept([sight], timestamp: 4.2).count == 1)
    let moved = DeviceSight(id: sight.id, label: sight.label, deviceIDs: [],
        corners: corners.map { CGPoint(x: $0.x+0.4, y: $0.y) }, evidence: sight.evidence)
    #expect(confirmation.accept([moved], timestamp: 4.4).isEmpty)
    #expect(confirmation.accept([], timestamp: 4.6).isEmpty)
    #expect(confirmation.accept([moved], timestamp: 4.8).isEmpty)
}

@Test func diagnosticsDistinguishUnmatchedImageFromMissingFeatures() async throws {
    let engine = DeviceRecognitionEngine(registry: FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString))
    try await engine.load()
    let image = try referencePhoto()
    let recognized = try await engine.inspect(gray: image.gray, width: image.width, height: image.height)
    #expect(recognized.referenceCount >= 2)
    #expect(recognized.stage == 9)
    #expect(recognized.bestInliers >= 16)
    let blank = try await engine.inspect(gray: Data(repeating: 127, count: 1920*1440), width: 1920, height: 1440)
    #expect(blank.matches.isEmpty)
    #expect(blank.keypoints == 0)
    #expect(blank.stage == 1)
    #expect(blank.usedCenterCrop)
}
