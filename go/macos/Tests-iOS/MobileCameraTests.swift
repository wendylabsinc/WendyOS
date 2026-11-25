import XCTest
import UIKit
@testable import WendyMobile

final class MobileCameraTests: XCTestCase {
    @MainActor
    func testFramesRespectStreamOwnershipAndFailureClearsImage() throws {
        let feed = CameraFeed(camera: Camera(id: 0, name: "QA camera"))
        let image = UIGraphicsImageRenderer(size: CGSize(width: 64, height: 48)).image { context in
            UIColor.green.setFill()
            context.fill(CGRect(x: 0, y: 0, width: 64, height: 48))
        }
        let data = try XCTUnwrap(image.jpegData(compressionQuality: 0.8))
        feed.start(stream: 11)
        feed.receive(BridgeEvent(session: 1, stream: 10, payload: .jpeg(data)))
        XCTAssertNil(feed.jpeg)
        feed.receive(BridgeEvent(session: 1, stream: 11, payload: .jpeg(data)))
        XCTAssertNotNil(feed.jpeg)
        XCTAssertNotNil(feed.lastVideoReceivedAt)
        XCTAssertNotNil(feed.resolution)
        feed.fail("Disconnected", stream: 10)
        XCTAssertTrue(feed.running)
        feed.fail("Disconnected", stream: 11)
        XCTAssertFalse(feed.running)
        XCTAssertFalse(feed.receivedVideo)
        XCTAssertNil(feed.jpeg)
        XCTAssertNil(feed.resolution)
        feed.start(stream: 12)
        feed.receive(BridgeEvent(session: 1, stream: 11, payload: .jpeg(data)))
        XCTAssertNil(feed.jpeg)
        feed.pause(manually: true)
        XCTAssertFalse(feed.shouldAutoStart(at: Date().addingTimeInterval(120)))
        XCTAssertNil(feed.lastVideoReceivedAt)
    }
}
