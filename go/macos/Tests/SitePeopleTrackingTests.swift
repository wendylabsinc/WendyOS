import Foundation
import CoreGraphics
import Testing
import simd
@testable import WendyDesktop

@Suite struct SitePeopleTrackingTests {
    func camera(revision: UUID, now: Date) -> SiteCameraGeometry {
        SiteCameraGeometry(pose: SiteCameraPose(modelRevision: revision, position: .init(x: 0, y: 2, z: 0),
            rotation: SIMD4(1, 0, 0, 0), method: 1, provenance: "test", observedAt: now,
            validUntil: now.addingTimeInterval(60)),
            lens: SiteCameraLens(width: 640, height: 480, fx: 320, fy: 240, cx: 320, cy: 240,
                distortionModel: "none", distortion: [], measured: true, note: "test", captureMode: "full"))
    }

    @Test func visionAcceptsDecodedImageWithoutInventingAPerson() async throws {
        let bytes = Data(repeating: 0, count: 640 * 480)
        let provider = try #require(CGDataProvider(data: bytes as CFData))
        let image = try #require(CGImage(width: 640, height: 480, bitsPerComponent: 8, bitsPerPixel: 8,
            bytesPerRow: 640, space: CGColorSpaceCreateDeviceGray(), bitmapInfo: [], provider: provider,
            decode: nil, shouldInterpolate: false, intent: .defaultIntent))
        let result = try await BlockingWork.run(on: .global(qos: .utility)) {
            try SitePersonProjection.ankles(in: image)
        }
        #expect(result.isEmpty)
    }

    @Test func projectsVisionCoordinatesOntoSelectedFloor() throws {
        let now = Date(), revision = UUID()
        let geometry = camera(revision: revision, now: now)
        let point = try #require(SitePersonProjection.floorPoint(ankle: CGPoint(x: 0.75, y: 0.25),
            geometry: geometry, revision: revision, floorY: 0, now: now))
        #expect(abs(point.x - 2) < 0.0001)
        #expect(abs(point.y) < 0.0001)
        #expect(abs(point.z + 4) < 0.0001)
        let lower = try #require(SitePersonProjection.floorPoint(ankle: CGPoint(x: 0.75, y: 0.25),
            geometry: geometry, revision: revision, floorY: -1, now: now))
        #expect(abs(lower.y + 1) < 0.0001)
        #expect(abs(lower.z + 6) < 0.0001)
    }

    @Test func rejectsHorizonCroppedFeetExpiredCalibrationAndWrongRevision() {
        let now = Date(), revision = UUID()
        let geometry = camera(revision: revision, now: now)
        for ankle in [CGPoint(x: 0.5, y: 0.5), CGPoint(x: 0.5, y: 0.8), CGPoint(x: 0.5, y: 0),
                      CGPoint(x: -1, y: 0.25), CGPoint(x: CGFloat.nan, y: 0.25)] {
            #expect(SitePersonProjection.floorPoint(ankle: ankle, geometry: geometry,
                revision: revision, floorY: 0, now: now) == nil)
        }
        #expect(SitePersonProjection.floorPoint(ankle: CGPoint(x: 0.5, y: 0.25), geometry: geometry,
            revision: UUID(), floorY: 0, now: now) == nil)
        #expect(SitePersonProjection.floorPoint(ankle: CGPoint(x: 0.5, y: 0.25), geometry: geometry,
            revision: revision, floorY: 0, now: now.addingTimeInterval(61)) == nil)
    }

    @Test func fusesCamerasAndSmoothsWithoutRefreshingRepeatedFrames() throws {
        let now = Date(), a = UUID(), b = UUID()
        var tracker = SitePeopleTracker()
        tracker.update([.init(camera: a, position: SIMD3(0, 0, -2), observedAt: now),
                        .init(camera: b, position: SIMD3(0.2, 0, -2), observedAt: now)], now: now)
        #expect(tracker.tracks.count == 1)
        let first = try #require(tracker.tracks.first)
        #expect(abs(first.position.x - 0.1) < 0.001)
        let later = now.addingTimeInterval(0.2)
        let moved = SitePersonDetection(camera: b, position: SIMD3(0.4, 0, -2), observedAt: later)
        tracker.update([moved], now: later)
        let second = try #require(tracker.tracks.first)
        #expect(second.id == first.id)
        #expect(second.position.x > first.position.x && second.position.x < 0.4)
        tracker.update([moved], now: later.addingTimeInterval(0.2))
        #expect(tracker.tracks == [second])
        tracker.update([moved], now: later.addingTimeInterval(1.3))
        #expect(tracker.tracks.isEmpty)
    }

    @Test func keepsPeopleFromSameCameraSeparateAndRejectsInvalidTimes() {
        let now = Date(), camera = UUID()
        var tracker = SitePeopleTracker()
        tracker.update([.init(camera: camera, position: SIMD3(0, 0, 0), observedAt: now),
                        .init(camera: camera, position: SIMD3(0.3, 0, 0), observedAt: now)], now: now)
        #expect(tracker.tracks.count == 2)
        tracker.update([], now: now.addingTimeInterval(2))
        #expect(tracker.tracks.isEmpty)
        tracker.update([.init(camera: camera, position: .zero, observedAt: now.addingTimeInterval(3)),
                        .init(camera: camera, position: SIMD3(.nan, 0, 0), observedAt: now)], now: now)
        #expect(tracker.tracks.isEmpty)
    }
}
