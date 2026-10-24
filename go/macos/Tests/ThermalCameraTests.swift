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

    func testFrameCopiesBytesAndRejectsInvalidGeometry() throws {
        let copied = try frame()
        XCTAssertEqual(copied.width, 80)
        XCTAssertEqual(copied.timestampNanoseconds, 123)
        XCTAssertNotNil(copied.image.dataProvider?.data)
        var invalid = WDThermalFrame()
        invalid.width = UInt32.max; invalid.height = UInt32.max
        XCTAssertThrowsError(try ThermalCameraFrame(invalid, bytes: nil, count: 0))
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
