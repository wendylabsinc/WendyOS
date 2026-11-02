import XCTest
import SwiftUI
@testable import WendyDesktop

final class DownloadedModelTests: XCTestCase {
    @MainActor
    func testChatLayoutStaysWithinWindowAfterConnectionFailure() async throws {
        let chat = DeviceChatModel(session: 0) { _, _ in
            throw WendyError(message: String(repeating: "Device connection failed: no route to host. ", count: 100))
        }
        let host = NSHostingView(rootView: DeviceChatView(deviceName: "USB device", chat: chat))
        let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 900, height: 620),
                              styleMask: [.titled, .resizable], backing: .buffered, defer: false)
        window.contentView = host
        window.orderFront(nil)
        defer { window.orderOut(nil); window.contentView = nil }
        await chat.discover()
        for size in [NSSize(width: 900, height: 620), NSSize(width: 1200, height: 800), NSSize(width: 720, height: 500)] {
            window.setContentSize(size)
            try await Task.sleep(for: .milliseconds(100))
            host.layoutSubtreeIfNeeded()
            XCTAssertLessThanOrEqual(host.fittingSize.width, size.width)
            XCTAssertLessThanOrEqual(host.fittingSize.height, size.height)
            XCTAssertEqual(host.frame.size, size)
        }
        XCTAssertNotNil(chat.error)
    }

    @MainActor
    func testDiscoverySelectsDownloadedModelAndItsPort() async {
        let chat = DeviceChatModel(session: 3) { _, _ in
            [ChatModelOption(id: "qwen3:4b", name: "Qwen", port: 11435),
             ChatModelOption(id: "gemma3:4b", name: "Gemma", port: 11436)]
        }
        await chat.discover()
        XCTAssertEqual(chat.model, "qwen3:4b")
        XCTAssertEqual(chat.portNumber, 11435)
        chat.prompt = "Hello"
        XCTAssertTrue(chat.canSend)
        chat.model = "gemma3:4b"
        XCTAssertEqual(chat.portNumber, 11436)
        XCTAssertTrue(chat.canSend)
    }

    @MainActor
    func testRefreshFailurePreservesDownloadedModel() async {
        let chat = DeviceChatModel(session: 3) { _, _ in throw WendyError(message: "Offline") }
        chat.useModel("qwen3:4b", port: 11435)
        chat.prompt = "Hello"
        await chat.discover()
        XCTAssertEqual(chat.models.map(\.id), ["qwen3:4b"])
        XCTAssertEqual(chat.model, "qwen3:4b")
        XCTAssertEqual(chat.portNumber, 11435)
        XCTAssertNotNil(chat.error)
        XCTAssertTrue(chat.canSend)
        chat.port = "11436"
        await chat.discover()
        XCTAssertFalse(chat.canSend)
    }

    @MainActor
    func testDownloadReplacesOldPortForSameModel() {
        let chat = DeviceChatModel(session: 3)
        chat.useModel("qwen3:4b", port: 11434)
        chat.useModel("qwen3:4b", port: 11435)
        XCTAssertEqual(chat.models.count, 1)
        XCTAssertEqual(chat.portNumber, 11435)
    }

    @MainActor
    func testDownloadedButUnreachableModelStaysVisible() async {
        let chat = DeviceChatModel(session: 3) { _, _ in
            [ChatModelOption(id: "qwen3:4b", name: "Qwen", port: 11437, unavailableReason: "Connect through Nearby")]
        }
        await chat.discover()
        chat.prompt = "Hello"
        XCTAssertEqual(chat.model, "qwen3:4b")
        XCTAssertEqual(chat.unavailableReason, "Connect through Nearby")
        XCTAssertFalse(chat.canSend)
        XCTAssertNil(chat.error)
    }

    func testDownloadProgressUsesActualBytes() {
        XCTAssertNil(ModelDownloadProgress(status: "Preparing", completed: 0, total: 0).fraction)
        XCTAssertEqual(ModelDownloadProgress(status: "Downloading", completed: 25, total: 100).fraction, 0.25)
        XCTAssertEqual(ModelDownloadProgress(status: "Downloading", completed: 200, total: 100).fraction, 1)
    }
}
