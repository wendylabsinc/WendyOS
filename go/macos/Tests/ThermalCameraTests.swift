import XCTest
import SwiftUI
#if os(macOS)
@testable import WendyDesktop
#else
@testable import WendyMobile
#endif

final class ThermalCameraTests: XCTestCase {
    private func frame() throws -> ThermalCameraFrame {
        var metadata = WDThermalFrame()
        metadata.width = 80; metadata.height = 60
        metadata.cold_x = 0; metadata.cold_y = 0; metadata.cold_raw = 28000
        metadata.hot_x = 79; metadata.hot_y = 59; metadata.hot_raw = 32000
        metadata.timestamp_ns = 123
        var bytes = [UInt8](repeating: 255, count: 80 * 60 * 4)
        for y in 0..<60 {
            for x in 0..<80 {
                let offset = (y * 80 + x) * 4
                bytes[offset] = UInt8(x * 3)
                bytes[offset + 1] = UInt8(y * 4)
                bytes[offset + 2] = 50
            }
        }
        return try bytes.withUnsafeBufferPointer { try ThermalCameraFrame(metadata, bytes: $0.baseAddress, count: $0.count) }
    }

    func testTemperatureScalesAreExplicit() {
        XCTAssertEqual(ThermalCameraScale.counts.label(30000), "30000 counts")
        XCTAssertEqual(ThermalCameraScale.kelvin100.label(30000), "26.9°C")
        XCTAssertEqual(ThermalCameraScale.kelvin10.label(3000), "26.9°C")
    }

    func testMarkerCoordinatesIncludeLetterboxingAndPixelCenters() {
        let wide = ThermalCameraFrame.position(x: 0, y: 0, width: 80, height: 60, in: CGSize(width: 800, height: 300))
        XCTAssertEqual(wide.x, 202.5)
        XCTAssertEqual(wide.y, 2.5)
        let tall = ThermalCameraFrame.position(x: 79, y: 59, width: 80, height: 60, in: CGSize(width: 400, height: 800))
        XCTAssertEqual(tall.x, 397.5)
        XCTAssertEqual(tall.y, 547.5)
    }

    func testNearbyExtremaKeepBothLabelsVisible() {
        for size in [CGSize(width: 320, height: 240), CGSize(width: 800, height: 600)] {
            for point in [CGPoint.zero, CGPoint(x: size.width / 2, y: size.height / 2), CGPoint(x: size.width, y: size.height)] {
                let other = CGPoint(x: point.x + 1, y: point.y)
                let cold = ThermalCameraFrame.markerLabelPosition(point, avoiding: other, isMinimum: true, in: size)
                let hot = ThermalCameraFrame.markerLabelPosition(other, avoiding: point, isMinimum: false, in: size)
                XCTAssertGreaterThanOrEqual(hot.y - cold.y, 28)
                XCTAssertGreaterThanOrEqual(cold.y, 36)
                XCTAssertLessThanOrEqual(hot.y, size.height - 36)
            }
        }
    }

    func testFrameCopiesBytesAndRejectsInvalidGeometry() throws {
        let copied = try frame()
        XCTAssertEqual(copied.width, 80)
        XCTAssertEqual(copied.timestampNanoseconds, 123)
        XCTAssertNotNil(copied.image.dataProvider?.data)
        var invalid = WDThermalFrame()
        invalid.width = UInt32.max; invalid.height = UInt32.max
        XCTAssertThrowsError(try ThermalCameraFrame(invalid, bytes: nil, count: 0))
    }

    func testTOPDONGeometryAndTemperatureCapability() throws {
        var metadata = WDThermalFrame()
        metadata.width = 256; metadata.height = 192
        metadata.cold_raw = 19200; metadata.hot_raw = 20000
        metadata.hot_x = 255; metadata.hot_y = 191
        let bytes = [UInt8](repeating: 255, count: 256 * 192 * 4)
        func decode() throws -> ThermalCameraFrame {
            try bytes.withUnsafeBufferPointer { try ThermalCameraFrame(metadata, bytes: $0.baseAddress, count: $0.count) }
        }
        let counts = try decode()
        XCTAssertEqual(counts.width, 256)
        XCTAssertEqual(counts.height, 192)
        XCTAssertFalse(counts.allowScaleOverride)
        XCTAssertNil(counts.temperatureLabel(counts.coldest, scale: .counts))
        // A stale PureThermal preference must not turn HIK counts into degrees.
        XCTAssertNil(counts.temperatureLabel(19200, scale: .kelvin100))
        metadata.has_temperature = 1
        metadata.kelvin_per_count = 1.0 / 64.0
        let measured = try decode()
        XCTAssertEqual(measured.temperatureLabel(19200, scale: .counts), "26.9°C")
        metadata.has_temperature = 0
        metadata.allow_scale_override = 1
        let manual = try decode()
        XCTAssertNil(manual.temperatureLabel(30000, scale: .counts))
        XCTAssertEqual(manual.temperatureLabel(30000, scale: .kelvin100), "26.9°C")
        metadata.has_temperature = 1
        metadata.kelvin_per_count = .nan
        XCTAssertThrowsError(try decode())
        metadata.has_temperature = 0
        metadata.hot_x = 256
        XCTAssertThrowsError(try decode())
    }

    func testCameraSummaryTemperatureAndFreshness() throws {
        var metadata = WDThermalFrame()
        metadata.width = 256; metadata.height = 192
        metadata.cold_raw = 4900; metadata.hot_raw = 5400
        metadata.summary_updated_ns = 100_000_000_000
        metadata.summary_cold_x = 19; metadata.summary_cold_y = 93
        metadata.summary_hot_x = 255; metadata.summary_hot_y = 186
        metadata.summary_cold_celsius = 19.875; metadata.summary_hot_celsius = 33.53125
        let bytes = [UInt8](repeating: 255, count: 256 * 192 * 4)
        func decode() throws -> ThermalCameraFrame {
            try bytes.withUnsafeBufferPointer { try ThermalCameraFrame(metadata, bytes: $0.baseAddress, count: $0.count) }
        }
        let frame = try decode()
        XCTAssertEqual(frame.temperatureLabel(try XCTUnwrap(frame.summaryHottest), scale: .counts), "33.5°C")
        XCTAssertEqual(frame.temperatureLabel(try XCTUnwrap(frame.summaryColdest), scale: .counts), "19.9°C")
        XCTAssertNil(frame.temperatureLabel(frame.hottest, scale: .counts))
        XCTAssertTrue(frame.summaryIsFresh(at: Date(timeIntervalSince1970: 102)))
        XCTAssertFalse(frame.summaryIsFresh(at: Date(timeIntervalSince1970: 103)))
        metadata.summary_hot_celsius = .nan
        XCTAssertThrowsError(try decode())
    }

    @MainActor
    func testThermalFeedIgnoresOldGenerationAndClearsOnStopAndFailure() throws {
        let feed = CameraFeed(camera: Camera(id: 2, name: "PureThermal", thermal: true))
        let image = try frame()
        feed.start(stream: 42)
        feed.receive(BridgeEvent(session: 1, stream: 41, payload: .thermal(image)))
        XCTAssertNil(feed.thermalFrame)
        feed.receive(BridgeEvent(session: 1, stream: 42, payload: .thermal(image)))
        XCTAssertTrue(feed.receivedVideo)
        XCTAssertNotNil(feed.thermalFrame)
        XCTAssertEqual(feed.thermalScale, .counts)
        feed.fail("Camera disconnected", stream: 42)
        XCTAssertNil(feed.thermalFrame)
        XCTAssertFalse(feed.receivedVideo)
        feed.start(stream: 43)
        feed.receive(BridgeEvent(session: 1, stream: 43, payload: .thermal(image)))
        feed.pause()
        XCTAssertNil(feed.thermalFrame)
        XCTAssertFalse(feed.running)
    }

    @MainActor
    func testThermalAutoplayRecoversAndRespectsPause() {
        let feed = CameraFeed(camera: Camera(id: 2, name: "TC001", thermal: true))
        XCTAssertTrue(feed.shouldAutoStart(at: Date()))
        feed.start(stream: 1)
        XCTAssertFalse(feed.shouldAutoStart(at: Date()))
        XCTAssertTrue(feed.shouldAutoStart(at: Date().addingTimeInterval(16)))
        feed.fail("Startup interrupted", stream: 1)
        XCTAssertTrue(feed.shouldAutoStart(at: Date().addingTimeInterval(3)))
        feed.pause(manually: true)
        XCTAssertFalse(feed.shouldAutoStart(at: Date().addingTimeInterval(60)))
        feed.allowAutoplay()
        XCTAssertTrue(feed.shouldAutoStart(at: Date().addingTimeInterval(60)))
        let ordinary = CameraFeed(camera: Camera(id: 0, name: "Webcam"))
        XCTAssertFalse(ordinary.shouldAutoStart(at: Date()))
    }

    @MainActor
    func testThermalDisplayRenders() throws {
        let feed = CameraFeed(camera: Camera(id: 2, name: "PureThermal", thermal: true))
        let image = try frame()
        feed.start(stream: 42)
        feed.receive(BridgeEvent(session: 1, stream: 42, payload: .thermal(image)))
        let renderer = ImageRenderer(content: ThermalCameraDisplay(feed: feed, frame: image).frame(width: 640, height: 480))
        XCTAssertNotNil(renderer.cgImage)
        #if os(macOS)
        if let rendered = renderer.cgImage {
            let bitmap = NSBitmapImageRep(cgImage: rendered)
            try bitmap.representation(using: .png, properties: [:])?.write(to: URL(fileURLWithPath: "/tmp/wendy-thermal-preview.png"))
        }
        #endif
    }
}
