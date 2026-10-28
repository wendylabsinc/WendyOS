import Foundation
import Testing
import simd
import AVFoundation

#if os(iOS)
@testable import WendyMobile
#else
@testable import WendyDesktop
#endif

@Test func cameraFOVUsesCalibrationAndOffCenterPrincipalPoint() throws {
    var lens = SiteCameraLens(width: 640, height: 480, fx: 320, fy: 320, cx: 320, cy: 240,
        distortionModel: "none", distortion: [], measured: true, note: "fixture", captureMode: "full")
    #expect(abs(try #require(lens.horizontalFOV) - 90) < 0.0001)
    #expect(abs(try #require(lens.verticalFOV) - 73.739795) < 0.0001)
    lens.cx = 200
    let expected = (atan(200.0/320) + atan(440.0/320)) * 180 / Double.pi
    #expect(abs(try #require(lens.horizontalFOV) - expected) < 0.0001)
    lens.fx = .nan
    #expect(lens.horizontalFOV == nil)
    #expect(lens.boundaryRays == nil)
}

@Test func cameraBrownDistortionIsInvertedAndChecked() throws {
    var lens = SiteCameraLens(width: 640, height: 480, fx: 500, fy: 500, cx: 320, cy: 240,
        distortionModel: "brown_conrady", distortion: [0.08, -0.01, 0, 0, 0], measured: true, note: "fixture", captureMode: "full")
    let u = 0.4, v = 0.2
    let r2 = u*u+v*v, radial = 1+0.08*0.2-0.01*0.04
    #expect(abs(r2-0.2)<0.000001)
    let ray = try #require(lens.ray(x: 320+500*u*radial, y: 240+500*v*radial))
    #expect(abs(ray.x/ray.z-u)<0.000001)
    #expect(abs(ray.y/ray.z-v)<0.000001)
    #expect(lens.boundaryRays?.count == 32)
    lens.distortion = [-100,0,0,0,0]
    #expect(lens.boundaryRays == nil)
}

@Test func cameraPoseRejectsStaleAmbiguousAndWrongModelTransforms() throws {
    let site = Site(name: "Room"), now = Date()
    var pose = SiteCameraPose(modelRevision: site.modelRevision, position: SitePoint(x: 1,y: 2,z: 3),
        rotation: SIMD4(0,1,0,0), method: 1, provenance: "test", observedAt: now, validUntil: now.addingTimeInterval(60))
    var metadata = SiteCameraMetadata(sourceID: "camera", revision: 1, lens: nil, poses: [pose])
    #expect(metadata.pose(in: site, now: now) != nil)
    #expect(abs(pose.yaw)<0.00001 && abs(pose.pitch)<0.00001)
    #expect(metadata.pose(in: site, now: now.addingTimeInterval(61)) == nil)
    var replaced = site; replaced.replaceModel()
    #expect(metadata.pose(in: replaced, now: now) == nil)
    metadata.poses.append(pose)
    #expect(metadata.pose(in: site, now: now) == nil)
    pose.rotation = SIMD4(0,2,0,0)
    #expect(!pose.isCurrent(revision: site.modelRevision, now: now))
}

@Test func movingToAnotherScanInvalidatesEveryPlacement() {
    var site = Site(
        name: "Office",
        cameras: [
            SiteCamera(
                gatewayID: "org/device",
                sourceID: "camera-1",
                name: "Door",
                position: .init(x: 1, y: 2, z: 3)
            )
        ]
    )
    let originalRevision = site.modelRevision
    site.replaceModel()
    #expect(site.modelRevision != originalRevision)
    #expect(site.cameras.first?.position == nil)
}

@Test func staleFutureAndWrongModelObservationsAreNotCurrent() {
    let camera = SiteCamera(gatewayID: "org/device", sourceID: "thermal-1", name: "Thermal")
    var site = Site(name: "Office", cameras: [camera])
    let now = Date(timeIntervalSince1970: 1000)
    var reading = SiteObservation(
        sensorID: camera.id,
        modelRevision: site.modelRevision,
        capturedAt: now.addingTimeInterval(-1),
        receivedAt: now,
        value: 24.5,
        unit: "°C"
    )
    #expect(reading.isCurrent(in: site, now: now))
    #expect(!reading.isCurrent(in: site, now: now.addingTimeInterval(6)))
    reading.capturedAt = now.addingTimeInterval(10)
    #expect(!reading.isCurrent(in: site, now: now))
    reading.capturedAt = now
    reading.value = .nan
    #expect(!reading.isCurrent(in: site, now: now))
    reading.value = 24.5
    site.replaceModel()
    #expect(!reading.isCurrent(in: site, now: now))
}

@Test func duplicateSourcesAndInvalidCoordinatesAreRejected() throws {
    let camera = SiteCamera(gatewayID: "org/device", sourceID: "camera-1", name: "Door")
    var site = Site(name: "Office", cameras: [camera])
    try site.validate()
    site.cameras.append(
        SiteCamera(gatewayID: camera.gatewayID, sourceID: camera.sourceID, name: "Duplicate")
    )
    #expect(throws: SiteError.self) { try site.validate() }
    site.cameras.removeLast()
    site.cameras[0].position = .init(x: .infinity, y: 0, z: 0)
    #expect(throws: SiteError.self) { try site.validate() }
}

@Test func savedSitesRoundTripWithoutPersistingLiveState() async throws {
    let folder = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: folder) }
    try FileManager.default.createDirectory(at: folder, withIntermediateDirectories: true)
    let model = folder.appendingPathComponent("room.usdz")
    // Store verifies package signature and size; the UI additionally parses USDZ.
    try Data([0x50, 0x4b, 0x03, 0x04, 1]).write(to: model)
    let store = SiteStore(directory: folder.appendingPathComponent("sites"))
    var saved = try await store.create(name: "Room", model: model)
    saved.site.cameras.append(
        SiteCamera(gatewayID: "org/device", sourceID: "camera-1", name: "Door")
    )
    try await store.save(saved.site)
    let loaded = try await store.load(id: saved.id)
    #expect(loaded.site == saved.site)
    #expect(try await store.list().count == 1)
    let invalid = folder.appendingPathComponent("invalid.usdz")
    try Data("not a model".utf8).write(to: invalid)
    await #expect(throws: SiteError.self) {
        try await store.create(name: "Invalid", model: invalid)
    }
    #expect(try await store.list().count == 1)
}

@Test func portableSitePreservesPlacementWithoutOverwritingExistingSite() async throws {
    let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: directory) }
    let camera = SiteCamera(
        gatewayID: "cloud-v2/tenant/gateway",
        sourceID: "sensorlink:principal:video",
        name: "Entrance",
        position: .init(x: 2, y: 3, z: -1)
    )
    let original = Site(name: "Room", cameras: [camera])
    let archive = try SiteArchive(site: original, model: Data([0x50, 0x4b, 0x03, 0x04, 1]))
    let bytes = try archive.data()
    let decoded = try SiteArchive(data: bytes)
    let store = SiteStore(directory: directory)
    let first = try await store.importArchive(decoded)
    let second = try await store.importArchive(decoded)
    #expect(first.id != second.id)
    #expect(first.site.cameras == original.cameras)
    #expect(first.site.modelRevision == original.modelRevision)
    #expect(try await store.list().count == 2)
    #expect(throws: SiteError.self) { try SiteArchive(data: bytes.prefix(13)) }
    var corrupt = bytes
    corrupt[8] = 255
    #expect(throws: SiteError.self) { try SiteArchive(data: corrupt) }
}


@Test func siteGatewayIdentityIsTenantScopedAndRejectsMalformedSelectors() {
    let tenant = "5c9e14bb-49c0-4178-9cc5-493a151f9885"
    let asset = "fad6089a-06e8-4c7b-8c09-4bcbef67f387"
    let selector = "cloud://api.wendy.dev:443/tenant/\(tenant)/asset/\(asset)"
    #expect(SiteGatewayIdentity.cloud(selector: selector) == "cloud-v2/\(tenant)/\(asset)")
    #expect(SiteGatewayIdentity.cloud(selector: selector + "?token=private") == nil)
    #expect(SiteGatewayIdentity.cloud(selector: "cloud://user:password@api.wendy.dev/tenant/\(tenant)/asset/\(asset)") == nil)
    #expect(SiteGatewayIdentity.cloud(selector: "192.168.1.2:50052") == nil)
    #expect(SiteGatewayIdentity.cloud(selector: "cloud://api.wendy.dev/tenant/other/asset/\(asset)") == nil)
}

@Test func removingCapturePreservesModelAndPlacementsForRestoreAfterRestart() async throws {
    let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: directory) }
    let store = SiteStore(directory: directory)
    let original = Site(name: "Bad capture", cameras: [SiteCamera(gatewayID: "gateway", sourceID: "camera", name: "Door", position: .init(x: 1, y: 2, z: 3))])
    let bytes = Data([0x50, 0x4b, 0x03, 0x04, 1])
    let capture = try await store.importArchive(SiteArchive(site: original, model: bytes))
    let other = try await store.importArchive(SiteArchive(site: Site(name: "Keep"), model: bytes))
    try await store.remove(id: capture.id)
    #expect(try await store.list().map(\.id) == [other.id])
    let reopened = SiteStore(directory: directory)
    #expect(try await reopened.removed().map(\.id) == [capture.id])
    let restored = try await reopened.restore(id: capture.id)
    #expect(restored.site == capture.site)
    #expect(try Data(contentsOf: restored.modelURL) == bytes)
    #expect(try await reopened.removed().isEmpty)
    #expect(try await reopened.list().count == 2)
    await #expect(throws: (any Error).self) { try await reopened.restore(id: capture.id) }
    #expect(try await reopened.load(id: other.id).site == other.site)
}

private func visualMapFixture(site: Site, model: Data) -> SiteVisualMap {
    var rows = [Float](repeating: 0, count: 120*131)
    for i in 0..<120 { rows[i*131] = Float(i)/100; rows[i*131+2] = 2; rows[i*131+3] = 32 }
    return SiteVisualMap(modelRevision: site.modelRevision, modelSHA256: SiteVisualMap.digest(model),
        capturedAt: Date(), frameCount: 3, landmarks: rows.withUnsafeBytes { Data($0) })
}

@Test func visualReferenceArchiveChecksExactModelAndRevision() throws {
    let site = Site(name: "References"), model = Data([0x50,0x4b,0x03,0x04,1])
    let map = visualMapFixture(site: site, model: model)
    let archive = try SiteArchive(site: site, model: model, visualMap: map)
    let bytes = try archive.data()
    #expect(bytes.prefix(8) == Data("WENDYST2".utf8))
    let decoded = try SiteArchive(data: bytes)
    #expect(decoded.visualMap?.landmarks == map.landmarks)
    #expect(decoded.visualMap?.modelRevision == site.modelRevision)
    var modified = model; modified.append(2)
    #expect(throws: (any Error).self) { try SiteArchive(site: site, model: modified, visualMap: map) }
    var other = site; other.replaceModel()
    #expect(throws: (any Error).self) { try SiteArchive(site: other, model: model, visualMap: map) }
    #expect(throws: (any Error).self) { try SiteArchive(data: Data(bytes.dropLast())) }
    var corrupt = bytes; corrupt[16] = 255
    #expect(throws: (any Error).self) { try SiteArchive(data: corrupt) }
    var trailing = bytes; trailing.append(0)
    #expect(throws: (any Error).self) { try SiteArchive(data: trailing) }
    let legacy = try SiteArchive(site: site, model: model).data()
    #expect(legacy.prefix(8) == Data("WENDYST1".utf8))
    #expect(try SiteArchive(data: legacy).visualMap == nil)
}

@Test func visualReferencesRejectMalformedValuesAndKeepRemovedCaptureRecoverable() async throws {
    let site = Site(name: "References"), model = Data([0x50,0x4b,0x03,0x04,1])
    var bad = visualMapFixture(site: site, model: model)
    var nan = Float.nan
    bad.landmarks.replaceSubrange(12..<16, with: withUnsafeBytes(of: &nan) { Data($0) })
    #expect(throws: (any Error).self) { try bad.validate(modelRevision: site.modelRevision, model: model) }
    let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: directory) }
    let store = SiteStore(directory: directory)
    let map = visualMapFixture(site: site, model: model)
    let imported = try await store.importArchive(SiteArchive(site: site, model: model, visualMap: map))
    #expect(imported.site.id != site.id)
    #expect(imported.site.modelRevision == site.modelRevision)
    #expect(imported.visualMapURL != nil)
    try await store.remove(id: imported.id)
    let restored = try await store.restore(id: imported.id)
    let recovered = try await store.archive(id: restored.id)
    #expect(recovered.visualMap?.landmarks == map.landmarks)
    #expect(recovered.model == model)
}

@Test func scanCoordinateParserRejectsTruncationAndUnsupportedZip() {
    for size in [0, 4, 21, 22, 128] {
        #expect(throws: (any Error).self) { try SiteScanCoordinates.meshIdentities(Data(repeating: 0, count: size)) }
    }
}


// A 64×48 H.264 IDR fixture: upper half gray 40, lower half gray 210.
// Encoded with libx264 from a generated PGM; no external media is used.
@Test @MainActor func h264AlignmentUsesDecodedKeyframesAndClearsOnReset() async throws {
    let encoded = try #require(Data(base64Encoded: "AAAAAWdkEAqsuI9CAAADAAIAAAMAZAgAAAABaO4PLIsAAAEGBf//WtxF6b3m2Ui3lizYINkj7u94MjY0IC0gY29yZSAxNjUgcjMyMjIgYjM1NjA1YSAtIEguMjY0L01QRUctNCBBVkMgY29kZWMgLSBDb3B5bGVmdCAyMDAzLTIwMjUgLSBodHRwOi8vd3d3LnZpZGVvbGFuLm9yZy94MjY0Lmh0bWwgLSBvcHRpb25zOiBjYWJhYz0xIHJlZj0xIGRlYmxvY2s9MTowOjAgYW5hbHlzZT0weDM6MHgxMTMgbWU9aGV4IHN1Ym1lPTcgcHN5PTEgcHN5X3JkPTEuMDA6MC4wMCBtaXhlZF9yZWY9MCBtZV9yYW5nZT0xNiBjaHJvbWFfbWU9MSB0cmVsbGlzPTEgOHg4ZGN0PTEgY3FtPTAgZGVhZHpvbmU9MjEsMTEgZmFzdF9wc2tpcD0xIGNocm9tYV9xcF9vZmZzZXQ9LTIgdGhyZWFkcz0xIGxvb2thaGVhZF90aHJlYWRzPTEgc2xpY2VkX3RocmVhZHM9MCBucj0wIGRlY2ltYXRlPTEgaW50ZXJsYWNlZD0wIGJsdXJheV9jb21wYXQ9MCBjb25zdHJhaW5lZF9pbnRyYT0wIGJmcmFtZXM9MCB3ZWlnaHRwPTAga2V5aW50PTEga2V5aW50X21pbj0xIHNjZW5lY3V0PTQwIGludHJhX3JlZnJlc2g9MCByYz1jcmYgbWJ0cmVlPTAgY3JmPTIzLjAgcWNvbXA9MC42MCBxcG1pbj0wIHFwbWF4PTY5IHFwc3RlcD00IGlwX3JhdGlvPTEuNDAgYXE9MToxLjAwAIAAAAFliIQEv/73rd+BTa9vdWu6Vzi0l4gBBPOUAeIoDSTzHjfsH7y8VbleGQ=="))
    let decoder = H264AnnexBDecoder()
    let layer = AVSampleBufferDisplayLayer()
    decoder.attach(layer: layer)
    decoder.calibrationFrames.setEnabled(true)
    let start = Date()
    decoder.feed(encoded + Data([0,0,0,1,9,16,0,0,0,1,9,16]))
    let deadline = ContinuousClock.now.advanced(by: .seconds(8))
    while decoder.calibrationFrames.latest == nil && ContinuousClock.now < deadline { try await Task.sleep(for: .milliseconds(50)) }
    let frame = try #require(decoder.calibrationFrames.latest)
    #expect(frame.width == 64 && frame.height == 48 && frame.codec == "H.264")
    #expect(frame.received >= start)
    #expect(abs(Int(frame.gray[8*64+32])-40) <= 4)
    #expect(abs(Int(frame.gray[40*64+32])-210) <= 4)
    #expect(frame.isFresh(at: frame.received.addingTimeInterval(2)))
    #expect(!frame.isFresh(at: frame.received.addingTimeInterval(4)))
    #expect(!frame.isFresh(at: frame.received.addingTimeInterval(-1)))
    decoder.reset()
    #expect(decoder.calibrationFrames.latest == nil)
    decoder.feed(encoded + Data([0,0,0,1,9,16,0,0,0,1,9,16]))
    decoder.calibrationFrames.setEnabled(false)
    try await Task.sleep(for: .milliseconds(200))
    #expect(decoder.calibrationFrames.latest == nil)
    decoder.detach(layer: layer)
}
